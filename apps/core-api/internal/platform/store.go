package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Querier is the narrow slice of pgx the Store needs; *pgxpool.Pool and
// pgx.Tx both satisfy it. Every query runs as qp_app against the Layer A
// SECURITY DEFINER family (migrations 0003+) — the only path to the auth
// tables (spec section 5: qp_app holds no table privilege on them).
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

// MailOutbox is one pending magic-link send as read_mail_outbox returns it:
// the link, the recipient resolved through the login token (the job carries
// only the outbox id — SendMagicLink still needs a destination), and the
// token's expiry so the worker owns the dead-link policy.
type MailOutbox struct {
	Link      string
	Email     string
	ExpiresAt time.Time
}

// Organization is the resolved tenant's registry row as
// app.get_current_organization() returns it (spec section 8). Branding is
// carried raw: the jsonb blob belongs to the caller, so the store neither
// parses nor validates it — handlers decode into their own contract shape.
type Organization struct {
	ID                uuid.UUID
	Name              string
	Slug              string
	InboundEmailAlias string
	Branding          json.RawMessage
}

// Store wraps the Layer A function family in typed methods (spec
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
//
//	store := platform.NewStore(pool)
func NewStore(q Querier) *Store { return &Store{q: q} }

// InStoreTx runs fn against a Store bound to one transaction: every write
// fn performs commits together or rolls back together — the contract
// /auth/session needs so a mid-exchange failure cannot strand a consumed
// magic link. A pool-backed Store opens a fresh transaction; a Store
// already bound to a pgx.Tx nests via savepoint (pgx's Begin on a tx),
// which keeps the same all-or-nothing contract; a fake Querier without
// Begin returns an error the caller maps to its uniform failure path.
//
//	err := store.InStoreTx(ctx, func(tx *platform.Store) error {
//		userID, ok, err := tx.ConsumeLoginToken(ctx, hash)
//		...
//	})
func (s *Store) InStoreTx(ctx context.Context, fn func(*Store) error) error {
	beginner, ok := s.q.(interface {
		Begin(context.Context) (pgx.Tx, error)
	})
	if !ok {
		return fmt.Errorf("platform: InStoreTx: querier %T has no Begin", s.q)
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return fmt.Errorf("platform: InStoreTx: begin: %w", err)
	}
	// Unconditional rollback — the tenantdb.tenantTx idiom: covers panic AND
	// runtime.Goexit, is ErrTxClosed after a successful Commit, and runs on a
	// cancel-free context so a dead caller context cannot abort it.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(&Store{q: tx}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// FindUserByEmail resolves the sole email→id lookup (spec section 3.2:
// there is no self-signup, so this is the only path). An unknown email is
// (uuid.Nil, false, nil) — not an error, and indistinguishable from any
// other miss.
//
//	userID, found, err := store.FindUserByEmail(ctx, email)
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
//
//	id, err := store.CreateLoginToken(ctx, userID, platform.HashToken(token), expiry)
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
//
//	userID, ok, err := store.ConsumeLoginToken(ctx, platform.HashToken(token))
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
//
//	sid, err := store.CreateSession(ctx, userID, platform.HashToken(refresh), expiry)
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
//
//   - found=false: unknown, revoked or expired hash — the uniform empty set;
//
//   - rotated=false, found=true: reuse of an already-exchanged generation —
//     the function killed the whole family in the same call and returned
//     NULL ids;
//
//   - rotated=true: the successor's session id and the owning user.
//
//     newSID, userID, rotated, found, err := store.RotateSession(ctx, platform.HashToken(old), newHash, expiry)
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
//
//	ok, err := store.RevokeSession(ctx, sessionID)
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
//
//	_, _ = store.RevokeSessionFamilyByHash(ctx, platform.HashToken(refresh))
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
//
//	role, member, err := store.GetMembership(ctx, userID, orgID)
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

// ResolveInboundOrg maps one Resend inbound recipient to its organization
// through app.resolve_inbound_org (ADR-005 section 7): the function
// normalizes case, angle brackets and plus-tags, then tries the org's
// inbound_email_alias before the pinned <slug>@in.quotepilot.app fallback.
// A NULL return — malformed or unmatched recipient — is ok=false, not an
// error; only driver failures propagate.
//
//	orgID, ok, err := store.ResolveInboundOrg(ctx, "Quotes+tag@in.quotepilot.app")
func (s *Store) ResolveInboundOrg(ctx context.Context, recipient string) (uuid.UUID, bool, error) {
	var id *uuid.UUID
	err := s.q.QueryRow(ctx, `SELECT app.resolve_inbound_org($1)`, recipient).Scan(&id)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("platform: resolve_inbound_org: %w", err)
	}
	if id == nil {
		return uuid.Nil, false, nil
	}
	return *id, true, nil
}

// GetUserProfile returns the GET /me identity row. An unknown user is the
// empty set, hence ok=false — not an error. full_name is nullable in the
// schema; NULL maps to "".
//
//	_, email, name, ok, err := store.GetUserProfile(ctx, userID)
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
//
//	orgs, err := store.ListMembershipsForEmail(ctx, email)
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

// CreateMailOutbox queues one magic-link send beside its login token — the
// migration-0004 half of durable delivery, meant to run in the same
// transaction as CreateLoginToken and the River enqueue so a token is never
// minted without its send. The link is a live credential bounded by the
// token's TTL; the row lives only until DeleteMailOutbox follows a
// successful send.
//
//	outboxID, err := store.CreateMailOutbox(ctx, tokenID, link)
func (s *Store) CreateMailOutbox(ctx context.Context, tokenID uuid.UUID, link string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.q.QueryRow(ctx,
		`SELECT app.create_mail_outbox($1, $2)`, tokenID, link).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("platform: create_mail_outbox: %w", err)
	}
	return id, nil
}

// ReadMailOutbox returns one pending send — link, recipient and expiry.
// Idempotent reads are the point: a River retry re-reads the row its
// predecessor crashed before deleting, so delivery is at-least-once and a
// resent link is the SAME link. ok=false means the job is done — already
// sent, never queued, or the token died — never an error.
//
//	pending, ok, err := store.ReadMailOutbox(ctx, outboxID)
func (s *Store) ReadMailOutbox(ctx context.Context, id uuid.UUID) (MailOutbox, bool, error) {
	var out MailOutbox
	err := s.q.QueryRow(ctx,
		`SELECT * FROM app.read_mail_outbox($1)`, id).
		Scan(&out.Link, &out.Email, &out.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return MailOutbox{}, false, nil
	}
	if err != nil {
		return MailOutbox{}, false, fmt.Errorf("platform: read_mail_outbox: %w", err)
	}
	return out, true, nil
}

// DeleteMailOutbox is the delivery receipt: called only after the send
// succeeds. Idempotent — ok=false just means the row is already gone.
//
//	deleted, err := store.DeleteMailOutbox(ctx, outboxID)
func (s *Store) DeleteMailOutbox(ctx context.Context, id uuid.UUID) (bool, error) {
	var deleted *bool
	err := s.q.QueryRow(ctx, `SELECT app.delete_mail_outbox($1)`, id).Scan(&deleted)
	if err != nil {
		return false, fmt.Errorf("platform: delete_mail_outbox: %w", err)
	}
	return deleted != nil, nil
}

// GetCurrentOrganization reads the resolved tenant's registry row through
// app.get_current_organization() — the migration-0002 accessor that scopes on
// the app.current_organization_id GUC, so a row can only come back inside
// InTenantTx/InTenantReadTx. Outside a tenant context the function emits an
// all-NULL composite row, which maps to (zero, false, nil) like every other
// miss in this family — callers inside a correctly-scoped tx treat found=false
// as contract drift, never as "org does not exist".
//
//	var org platform.Organization
//	var found bool
//	err := runner.InTenantReadTx(ctx, orgID, func(ctx context.Context, tx pgx.Tx) error {
//		var err error
//		org, found, err = platform.NewStore(tx).GetCurrentOrganization(ctx)
//		return err
//	})
func (s *Store) GetCurrentOrganization(ctx context.Context) (Organization, bool, error) {
	var row orgRow
	err := s.q.QueryRow(ctx, `
		SELECT id, name, slug, inbound_email_alias, branding_json
		FROM app.get_current_organization()`).
		Scan(&row.id, &row.name, &row.slug, &row.alias, &row.branding)
	if errors.Is(err, pgx.ErrNoRows) {
		return Organization{}, false, nil
	}
	if err != nil {
		return Organization{}, false, fmt.Errorf("platform: get_current_organization: %w", err)
	}
	return row.organization()
}

// orgRow is the nullable composite app.get_current_organization() emits:
// all-NULL outside a tenant context (a miss), NULL-free inside one — a NULL
// column beside a non-NULL id is driver contract drift, not a missing org.
type orgRow struct {
	id                *uuid.UUID
	name, slug, alias *string
	branding          json.RawMessage
}

// organization maps the scanned row onto the Organization result;
// found=false is the all-NULL composite the function emits outside a
// tenant context.
func (r orgRow) organization() (Organization, bool, error) {
	if r.id == nil {
		return Organization{}, false, nil
	}
	if r.name == nil || r.slug == nil || r.alias == nil {
		return Organization{}, false, fmt.Errorf("platform: get_current_organization: null column in row %s", *r.id)
	}
	return Organization{
		ID:                *r.id,
		Name:              *r.name,
		Slug:              *r.slug,
		InboundEmailAlias: *r.alias,
		Branding:          r.branding,
	}, true, nil
}
