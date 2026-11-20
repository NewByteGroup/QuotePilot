package auth_test

// Test doubles for the POST /auth/login proofs in login_test.go and
// login_password_test.go: an in-memory platform.Querier covering both
// login paths — the magic-link find/token/outbox family and the password
// path's credential/profile/session reads — plus the mounted-server rig.
// Same seams as sessionQuerier: the real platform.Store runs over the
// fake, so the SQL text and NULL handling are production's.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

var errBoom = errors.New("boom")

// fakeClock is the injected time source shared by both limiters and the
// token expiry, so rate-limit and TTL assertions are exact.
type fakeClock struct{ now time.Time }

func (f *fakeClock) Now() time.Time          { return f.now }
func (f *fakeClock) Advance(d time.Duration) { f.now = f.now.Add(d) }

// stubRow is a one-row pgx.Row whose Scan the test controls.
type stubRow struct{ scan func(dest ...any) error }

func (r stubRow) Scan(dest ...any) error { return r.scan(dest...) }

// createCall records one create_login_token invocation: the hash the store
// was asked to keep, the expiry the handler computed and the row id the
// fake returned — the id the outbox row must link to inside the same tx.
type createCall struct {
	userID    uuid.UUID
	hash      string
	expiresAt time.Time
	id        uuid.UUID
}

// outboxCall records one create_mail_outbox invocation: the login token row
// id the outbox links to, the rendered link, and the outbox id the fake
// returned — the value the handler must hand the enqueue.
type outboxCall struct {
	tokenID  uuid.UUID
	link     string
	outboxID uuid.UUID
}

// credRec is one get_user_auth_by_email row: the user id plus the stored
// argon2id PHC string — "" stands in for NULL, which is what a provisioned
// account without a set password looks like (migration 0006).
type credRec struct {
	userID uuid.UUID
	hash   string
}

// passwordSet records one set_user_password call — the user id and the PHC
// the handler hashed (never the raw password; the fake must see only what
// the store sees).
type passwordSet struct {
	userID uuid.UUID
	hash   string
}

// loginQuerier is an in-memory platform.Querier for the login path: a users
// table keyed on the function's own lower/btrim normalization, a credential
// table for the password path, the profile/membership/session rows the
// session-issue tail reads and writes, a record of find/create/stage calls,
// and per-function driver-error injection. The real platform.Store runs
// over it, so the SQL text and NULL handling are the production ones —
// only the transport is faked.
type loginQuerier struct {
	now            func() time.Time
	users          map[string]uuid.UUID
	creds          map[string]credRec
	profiles       map[uuid.UUID]profileRec
	members        map[string][]platform.Membership
	tokens         map[string]*tokenRec
	fail           map[string]error
	finds          []string
	authFinds      []string
	creates        []createCall
	resetCreates   []createCall
	loginConsumes  []string
	resetConsumes  []string
	passwordSets   []passwordSet
	sessionRevokes []uuid.UUID
	pendingRevokes []uuid.UUID
	userLocks      []uuid.UUID
	sessions       []sessionCreate
	outboxCreates  []outboxCall
	credsShift     map[string]credRec
	order          []string
}

var _ platform.Querier = (*loginQuerier)(nil)

func (q *loginQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "get_user_auth_by_email"):
		return q.authRow(args)
	case strings.Contains(sql, "find_user_by_email"):
		return q.findRow(args)
	case strings.Contains(sql, "get_user_profile"):
		return q.profileRow(args)
	case strings.Contains(sql, "create_password_reset_token"):
		return q.resetCreateRow(args)
	case strings.Contains(sql, "create_login_token"):
		return q.createRow(args)
	case strings.Contains(sql, "consume_password_reset_token"):
		return q.resetConsumeRow(args)
	case strings.Contains(sql, "consume_login_token"):
		return q.loginConsumeRow(args)
	case strings.Contains(sql, "pg_advisory_xact_lock"):
		return q.userLockRow(args)
	case strings.Contains(sql, "set_user_password"):
		return q.passwordSetRow(args)
	case strings.Contains(sql, "revoke_user_sessions"):
		return q.sessionRevokeRow(args)
	case strings.Contains(sql, "revoke_user_pending_tokens"):
		return q.pendingRevokeRow(args)
	case strings.Contains(sql, "create_session"):
		return q.sessionRow(args)
	case strings.Contains(sql, "create_mail_outbox"):
		return q.outboxRow(args)
	default:
		return stubRow{scan: func(...any) error {
			return fmt.Errorf("loginQuerier: unexpected sql %q", sql)
		}}
	}
}

func (q *loginQuerier) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	if !strings.Contains(sql, "list_memberships_for_email") {
		return nil, fmt.Errorf("loginQuerier: unexpected sql %q", sql)
	}
	if err := q.fail["list_memberships_for_email"]; err != nil {
		return nil, err
	}
	email := args[0].(string)
	rows := [][]any{}
	for _, m := range q.members[email] {
		rows = append(rows, []any{m.OrgID, m.Name, m.Slug, m.Role})
	}
	return &stubRows{rows: rows}, nil
}

// findRow answers SELECT app.find_user_by_email($1): the normalized lookup
// hits q.users or scans NULL — the (Nil, false, nil) miss the Store maps.
func (q *loginQuerier) findRow(args []any) pgx.Row {
	return stubRow{scan: func(dest ...any) error {
		if err := q.fail["find_user_by_email"]; err != nil {
			return err
		}
		email := args[0].(string)
		q.finds = append(q.finds, email)
		id, ok := q.users[strings.ToLower(strings.TrimSpace(email))]
		out := dest[0].(**uuid.UUID)
		if !ok {
			*out = nil
			return nil
		}
		*out = &id
		return nil
	}}
}

// authRow answers SELECT id, password_hash FROM
// app.get_user_auth_by_email($1): the same lower/btrim normalization the
// find family applies; an unknown email hits pgx.ErrNoRows — the
// (Nil, "", false, nil) miss the Store maps — and a "" hash scans NULL.
func (q *loginQuerier) authRow(args []any) pgx.Row {
	return stubRow{scan: func(dest ...any) error {
		if err := q.fail["get_user_auth_by_email"]; err != nil {
			return err
		}
		email := args[0].(string)
		q.authFinds = append(q.authFinds, email)
		norm := strings.ToLower(strings.TrimSpace(email))
		cred, ok := q.creds[norm]
		// credsShift is the stale-credential seam: once set for an email,
		// every lookup AFTER the first returns the shifted row — the way a
		// reset committing between the outside read and the in-tx re-read
		// presents to passwordLogin's recheck.
		if shifted, has := q.credsShift[norm]; has && countOf(q.authFinds, email) > 1 {
			cred, ok = shifted, true
		}
		if !ok {
			return pgx.ErrNoRows
		}
		*(dest[0].(*uuid.UUID)) = cred.userID
		hash := dest[1].(**string)
		if cred.hash == "" {
			*hash = nil
			return nil
		}
		*hash = &cred.hash
		return nil
	}}
}

// profileRow answers SELECT id, email, full_name FROM
// app.get_user_profile($1) — sessionQuerier's twin, needed because the
// password path's session-issue tail reads the profile inside inTx.
func (q *loginQuerier) profileRow(args []any) pgx.Row {
	return stubRow{scan: func(dest ...any) error {
		if err := q.fail["get_user_profile"]; err != nil {
			return err
		}
		p, ok := q.profiles[args[0].(uuid.UUID)]
		if !ok {
			return pgx.ErrNoRows
		}
		*(dest[0].(*uuid.UUID)) = args[0].(uuid.UUID)
		*(dest[1].(*string)) = p.email
		name := dest[2].(**string)
		if p.fullName == "" {
			*name = nil
			return nil
		}
		*name = &p.fullName
		return nil
	}}
}

// sessionRow answers SELECT app.create_session($1, $2, $3): records the
// arguments (refresh hash and expiry) and returns a fresh session id.
func (q *loginQuerier) sessionRow(args []any) pgx.Row {
	return stubRow{scan: func(dest ...any) error {
		if err := q.fail["create_session"]; err != nil {
			return err
		}
		call := sessionCreate{
			userID:    args[0].(uuid.UUID),
			hash:      args[1].(string),
			expiresAt: args[2].(time.Time),
			sessionID: uuid.New(),
		}
		q.sessions = append(q.sessions, call)
		q.order = append(q.order, "create_session")
		*(dest[0].(*uuid.UUID)) = call.sessionID
		return nil
	}}
}

// loginConsumeRow answers SELECT app.consume_login_token($1): the reset
// rig's half of the cross-purpose proof — a planted 'password_reset' row
// misses the login filter, so /auth/session answers 401 on a reset token.
func (q *loginQuerier) loginConsumeRow(args []any) pgx.Row {
	return stubRow{scan: func(dest ...any) error {
		if err := q.fail["consume_login_token"]; err != nil {
			return err
		}
		hash := args[0].(string)
		q.loginConsumes = append(q.loginConsumes, hash)
		out := dest[0].(**uuid.UUID)
		tok, ok := q.tokens[hash]
		if !ok || tok.purpose != platform.MailPurposeLogin || tok.consumed || !q.now().Before(tok.expiresAt) {
			*out = nil
			return nil
		}
		tok.consumed = true
		*out = &tok.userID
		return nil
	}}
}

// createRow answers SELECT app.create_login_token($1, $2, $3): it records the
// arguments (so tests can diff hash vs token) and returns a fresh row id.
func (q *loginQuerier) createRow(args []any) pgx.Row {
	return stubRow{scan: func(dest ...any) error {
		if err := q.fail["create_login_token"]; err != nil {
			return err
		}
		call := createCall{
			userID:    args[0].(uuid.UUID),
			hash:      args[1].(string),
			expiresAt: args[2].(time.Time),
			id:        uuid.New(),
		}
		q.creates = append(q.creates, call)
		*(dest[0].(*uuid.UUID)) = call.id
		return nil
	}}
}

// outboxRow answers SELECT app.create_mail_outbox($1, $2): it records the
// (token row id, link) pair and returns a fresh outbox id. Like migration
// 0004's function, a blank link is refused — it would queue a send that can
// never deliver.
func (q *loginQuerier) outboxRow(args []any) pgx.Row {
	return stubRow{scan: func(dest ...any) error {
		if err := q.fail["create_mail_outbox"]; err != nil {
			return err
		}
		link := args[1].(string)
		if strings.TrimSpace(link) == "" {
			return errors.New("create_mail_outbox: link_url must be a non-empty string")
		}
		call := outboxCall{tokenID: args[0].(uuid.UUID), link: link, outboxID: uuid.New()}
		q.outboxCreates = append(q.outboxCreates, call)
		*(dest[0].(*uuid.UUID)) = call.outboxID
		return nil
	}}
}

// rig is a mounted auth surface plus the test's windows into it.
type rig struct {
	handler http.Handler
	clock   *fakeClock
	cfg     *platform.Config
	q       *loginQuerier
	enq     *fakeEnqueue
}

// newRig wires the endpoints the way cmd/server will: real Store over the
// fake Querier, real Limiters at the given capacities on the fake clock, the
// recording EnqueueMailFunc standing in for the River InsertTx closure,
// mounted through auth.NewServer. The config is sessionConfig — the password
// path signs a real JWT and sets cookies, so it needs the key and both TTLs;
// the magic-link assertions only read WebURL and LoginTTL, which it shares.
// The clock starts at real now: VerifyAccess checks exp against wall time,
// so a fixed epoch would mint an already-expired access JWT on the password
// path — limiter assertions use Advance against that start.
func newRig(t *testing.T, ipCap, emailCap int) *rig {
	t.Helper()
	clock := &fakeClock{now: time.Now()}
	cfg := sessionConfig()
	q := &loginQuerier{
		now:        clock.Now,
		users:      map[string]uuid.UUID{},
		creds:      map[string]credRec{},
		profiles:   map[uuid.UUID]profileRec{},
		members:    map[string][]platform.Membership{},
		tokens:     map[string]*tokenRec{},
		fail:       map[string]error{},
		credsShift: map[string]credRec{},
	}
	enq := &fakeEnqueue{}
	store := platform.NewStore(q)
	endpoints := auth.NewEndpoints(cfg,
		store, directInTx(store), &neverRunner{t}, enq.run,
		platform.NewLimiter(ipCap, platform.LimitWindow, clock.Now),
		platform.NewLimiter(emailCap, platform.LimitWindow, clock.Now),
		clock.Now)
	return &rig{handler: auth.NewServer(endpoints), clock: clock, cfg: cfg, q: q, enq: enq}
}

// postLogin fires one JSON body at the mounted mux. The default
// httptest.NewRequest RemoteAddr is "192.0.2.1:1234" — IP "192.0.2.1".
func (r *rig) postLogin(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	return r.postPath(t, "/auth/login", body)
}

// countOf reports how many times needle appears in hay — the credsShift
// seam counts prior get_user_auth_by_email calls for the same email.
func countOf(hay []string, needle string) int {
	n := 0
	for _, v := range hay {
		if v == needle {
			n++
		}
	}
	return n
}
