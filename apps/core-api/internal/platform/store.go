package platform

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Querier is the narrow slice of pgx the Store needs; *pgxpool.Pool and
// pgx.Tx both satisfy it. Every query runs as qp_app against the
// migration-0003 SECURITY DEFINER family — the only path to the Layer A
// auth tables (spec section 5: qp_app holds no table privilege on them).
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Membership is one row of the org-switcher list (spec section 3.3): the
// display name and slug ride along with the membership row.
type Membership struct {
	OrgID uuid.UUID
	Name  string
	Slug  string
	Role  string
}

// Store wraps the migration-0003 function family in typed methods (spec
// section 6). NULL results and empty sets become (zero, false, nil) so
// callers never handle pgx NULLs — the indistinguishability spec section 7
// requires is preserved by returning found=false, never an error, for an
// unknown or dead token.
//
//	store := platform.NewStore(pool)
type Store struct {
	q Querier
}

// NewStore binds the function family to one Querier.
func NewStore(q Querier) *Store { return &Store{q: q} }

// FindUserByEmail resolves the sole email→id lookup (spec section 3.2:
// there is no self-signup, so this is the only path). An unknown email is
// (uuid.Nil, false, nil) — not an error, and indistinguishable from any
// other miss.
func (s *Store) FindUserByEmail(ctx context.Context, email string) (uuid.UUID, bool, error) {
	var id *uuid.UUID
	err := s.q.QueryRow(ctx, `SELECT app.find_user_by_email($1)`, email).Scan(&id)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("platform: find_user_by_email: %w", err)
	}
	if id == nil {
		return uuid.Nil, false, nil
	}
	return *id, true, nil
}

// CreateLoginToken inserts one magic-link token and returns its id; the
// caller computed the SHA-256 hash and the LoginTTL expiry (spec section 5).
func (s *Store) CreateLoginToken(ctx context.Context, userID uuid.UUID, tokenHash string, expiresAt time.Time) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.q.QueryRow(ctx,
		`SELECT app.create_login_token($1, $2, $3)`, userID, tokenHash, expiresAt).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("platform: create_login_token: %w", err)
	}
	return id, nil
}

// ConsumeLoginToken spends a magic-link token. The single-use guarantee is
// the function's atomic UPDATE row lock; consumed, expired and unknown
// hashes all collapse to (uuid.Nil, false, nil).
func (s *Store) ConsumeLoginToken(ctx context.Context, tokenHash string) (uuid.UUID, bool, error) {
	var userID *uuid.UUID
	err := s.q.QueryRow(ctx, `SELECT app.consume_login_token($1)`, tokenHash).Scan(&userID)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("platform: consume_login_token: %w", err)
	}
	if userID == nil {
		return uuid.Nil, false, nil
	}
	return *userID, true, nil
}

// CreateSession inserts refresh generation 0 — the family roots at its own
// id — and returns the session id.
func (s *Store) CreateSession(ctx context.Context, userID uuid.UUID, refreshHash string, expiresAt time.Time) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.q.QueryRow(ctx,
		`SELECT app.create_session($1, $2, $3)`, userID, refreshHash, expiresAt).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("platform: create_session: %w", err)
	}
	return id, nil
}

// RotateSession exchanges one refresh generation for the next — the only
// atomic consume-and-create on the session path (spec section 5). Exactly
// one outcome per call:
//   - found=false: unknown, revoked or expired hash — the uniform empty set;
//   - rotated=false, found=true: reuse of an already-exchanged generation —
//     the function killed the whole family in the same call and returned
//     NULL ids;
//   - rotated=true: the successor's session id and the owning user.
func (s *Store) RotateSession(
	ctx context.Context,
	presentedHash, newHash string,
	newExpiresAt time.Time,
) (sessionID, userID uuid.UUID, rotated, found bool, err error) {
	rows, err := s.q.Query(ctx,
		`SELECT session_id, user_id, rotated FROM app.rotate_session($1, $2, $3)`,
		presentedHash, newHash, newExpiresAt)
	if err != nil {
		return uuid.Nil, uuid.Nil, false, false, fmt.Errorf("platform: rotate_session: %w", err)
	}
	defer rows.Close()
	return scanRotateOutcome(rows)
}

// scanRotateOutcome consumes the single row rotate_session may return and
// applies the (rotated, found) contract documented on RotateSession. It runs
// while rows is open — the caller owns Close.
func scanRotateOutcome(rows pgx.Rows) (uuid.UUID, uuid.UUID, bool, bool, error) {
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return uuid.Nil, uuid.Nil, false, false, fmt.Errorf("platform: rotate_session rows: %w", err)
		}
		return uuid.Nil, uuid.Nil, false, false, nil
	}
	var sid, uid *uuid.UUID
	var rotated bool
	if err := rows.Scan(&sid, &uid, &rotated); err != nil {
		return uuid.Nil, uuid.Nil, false, false, fmt.Errorf("platform: rotate_session scan: %w", err)
	}
	return rotateOutcome(sid, uid, rotated)
}

// errRotateContract means the function returned rotated=true with NULL ids —
// a shape migration 0003 cannot produce, so the contract drifted.
var errRotateContract = errors.New("platform: rotate_session returned rotated=true with NULL ids")

// rotateOutcome maps one scanned row to the method contract: NULL ids are
// legal only on the reuse (rotated=false) shape. A NULL under rotated=true
// fails loudly rather than mint a session for user uuid.Nil.
func rotateOutcome(sid, uid *uuid.UUID, rotated bool) (uuid.UUID, uuid.UUID, bool, bool, error) {
	if !rotated {
		return uuid.Nil, uuid.Nil, false, true, nil
	}
	if sid == nil || uid == nil {
		return uuid.Nil, uuid.Nil, false, false, errRotateContract
	}
	return *sid, *uid, true, true, nil
}

// RevokeSession kills one session generation by id (single-row revoke; the
// JWT's sid is always the family's live generation). False means the row
// was already revoked or never existed.
func (s *Store) RevokeSession(ctx context.Context, sessionID uuid.UUID) (bool, error) {
	var ok bool
	err := s.q.QueryRow(ctx, `SELECT app.revoke_session($1)`, sessionID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("platform: revoke_session: %w", err)
	}
	return ok, nil
}

// RevokeSessionFamilyByHash is logout's revocation interface: the opaque
// refresh cookie resolves to its whole family in one call, killing every
// generation — live and already-rotated alike. The boolean feeds metrics
// only; the endpoint always answers 204.
func (s *Store) RevokeSessionFamilyByHash(ctx context.Context, refreshHash string) (bool, error) {
	var ok bool
	err := s.q.QueryRow(ctx, `SELECT app.revoke_session_family_by_hash($1)`, refreshHash).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("platform: revoke_session_family_by_hash: %w", err)
	}
	return ok, nil
}

// GetMembership is the per-request tenant check (ADR-005 section 5). NULL —
// returned indistinguishably for a non-member and a nonexistent
// organization — maps to ("" , false, nil); that indistinguishability is
// the security property.
func (s *Store) GetMembership(ctx context.Context, userID, orgID uuid.UUID) (role string, ok bool, err error) {
	var r *string
	err = s.q.QueryRow(ctx, `SELECT app.get_membership($1, $2)::text`, userID, orgID).Scan(&r)
	if err != nil {
		return "", false, fmt.Errorf("platform: get_membership: %w", err)
	}
	if r == nil {
		return "", false, nil
	}
	return *r, true, nil
}

// GetUserProfile returns the GET /me identity row. An unknown user is the
// empty set, hence ok=false — not an error. full_name is nullable in the
// schema; NULL maps to "".
func (s *Store) GetUserProfile(ctx context.Context, userID uuid.UUID) (id uuid.UUID, email, fullName string, ok bool, err error) {
	var name *string
	err = s.q.QueryRow(ctx,
		`SELECT id, email, full_name FROM app.get_user_profile($1)`, userID).Scan(&id, &email, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, "", "", false, nil
	}
	if err != nil {
		return uuid.Nil, "", "", false, fmt.Errorf("platform: get_user_profile: %w", err)
	}
	if name == nil {
		return id, email, "", true, nil
	}
	return id, email, *name, true, nil
}

// ListMembershipsForEmail returns the org-switcher rows (spec section 3.3):
// organization id, display name, slug and role. An unknown email — or a
// user with no memberships — is an empty slice, never an error.
func (s *Store) ListMembershipsForEmail(ctx context.Context, email string) ([]Membership, error) {
	rows, err := s.q.Query(ctx,
		`SELECT organization_id, name, slug, role::text FROM app.list_memberships_for_email($1)`, email)
	if err != nil {
		return nil, fmt.Errorf("platform: list_memberships_for_email: %w", err)
	}
	defer rows.Close()
	out := []Membership{}
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.OrgID, &m.Name, &m.Slug, &m.Role); err != nil {
			return nil, fmt.Errorf("platform: list_memberships_for_email scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("platform: list_memberships_for_email rows: %w", err)
	}
	return out, nil
}
