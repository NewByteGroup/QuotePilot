package auth_test

// Test doubles for the POST /auth/session proofs in session_test.go: an
// in-memory platform.Querier enforcing consume_login_token's single-use
// row-lock semantics, a minimal pgx.Rows for the switcher query, and the
// mounted-server rig. Same seams as loginQuerier — the real platform.Store
// runs over the fake, so the SQL text and NULL handling are production's.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// tokenRec is one auth_login_tokens row as consume_login_token sees it.
type tokenRec struct {
	userID    uuid.UUID
	expiresAt time.Time
	consumed  bool
}

// profileRec is one get_user_profile row; fullName "" stands in for NULL —
// the users column is nullable.
type profileRec struct {
	email    string
	fullName string
}

// sessionCreate records one create_session call plus the id the fake
// returned — the access JWT's sid must equal it.
type sessionCreate struct {
	userID    uuid.UUID
	hash      string
	expiresAt time.Time
	sessionID uuid.UUID
}

// stubRows is a minimal in-memory pgx.Rows over [][]any: Next walks the
// slice, Scan assigns the current row positionally, and the methods the
// store never calls answer their zero value. pgx.Rows in v5.12 carries ten
// methods — all are implemented so the fake satisfies the real interface.
type stubRows struct {
	rows [][]any
	i    int
}

func (r *stubRows) Close()                                       {}
func (r *stubRows) Err() error                                   { return nil }
func (r *stubRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *stubRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *stubRows) Conn() *pgx.Conn                              { return nil }
func (r *stubRows) TypeMap() *pgtype.Map                         { return pgtype.NewMap() }
func (r *stubRows) Values() ([]any, error)                       { return r.rows[r.i-1], nil }
func (r *stubRows) RawValues() [][]byte                          { return nil }

func (r *stubRows) Next() bool {
	if r.i >= len(r.rows) {
		return false
	}
	r.i++
	return true
}

// Scan assigns the current row's columns positionally; only the dest types
// the auth store scans are handled — uuid, *uuid for NULL-able columns
// (rotate_session's reuse row returns NULL ids), string and bool — anything
// else fails the test rather than silently skipping a column.
func (r *stubRows) Scan(dest ...any) error {
	row := r.rows[r.i-1]
	for j, v := range row {
		switch d := dest[j].(type) {
		case *uuid.UUID:
			*d = v.(uuid.UUID)
		case **uuid.UUID:
			*d = v.(*uuid.UUID)
		case *string:
			*d = v.(string)
		case *bool:
			*d = v.(bool)
		default:
			return fmt.Errorf("stubRows: unhandled dest %T", dest[j])
		}
	}
	return nil
}

// sessionQuerier is an in-memory platform.Querier for the session and /me
// paths: a login-token table keyed by hash enforcing the function's
// single-use row lock, a profiles table, an org-switcher list, call records
// and per-function driver-error injection.
type sessionQuerier struct {
	now          func() time.Time
	tokens       map[string]*tokenRec
	profiles     map[uuid.UUID]profileRec
	members      map[string][]platform.Membership
	fail         map[string]error
	consumes     []string
	creates      []sessionCreate
	listed       []string
	profileCalls []uuid.UUID
}

var _ platform.Querier = (*sessionQuerier)(nil)

func (q *sessionQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "consume_login_token"):
		return q.consumeRow(args)
	case strings.Contains(sql, "get_user_profile"):
		return q.profileRow(args)
	case strings.Contains(sql, "create_session"):
		return q.createRow(args)
	default:
		return stubRow{scan: func(...any) error {
			return fmt.Errorf("sessionQuerier: unexpected sql %q", sql)
		}}
	}
}

func (q *sessionQuerier) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	if !strings.Contains(sql, "list_memberships_for_email") {
		return nil, fmt.Errorf("sessionQuerier: unexpected sql %q", sql)
	}
	if err := q.fail["list_memberships_for_email"]; err != nil {
		return nil, err
	}
	email := args[0].(string)
	q.listed = append(q.listed, email)
	rows := [][]any{}
	for _, m := range q.members[email] {
		rows = append(rows, []any{m.OrgID, m.Name, m.Slug, m.Role})
	}
	return &stubRows{rows: rows}, nil
}

// consumeRow answers SELECT app.consume_login_token($1) with the function's
// atomic semantics: a hit marks the token consumed and a second consume —
// like an expired or unknown hash — scans NULL, the (Nil, false, nil) miss
// the Store maps.
func (q *sessionQuerier) consumeRow(args []any) pgx.Row {
	return stubRow{scan: func(dest ...any) error {
		if err := q.fail["consume_login_token"]; err != nil {
			return err
		}
		hash := args[0].(string)
		q.consumes = append(q.consumes, hash)
		out := dest[0].(**uuid.UUID)
		tok, ok := q.tokens[hash]
		if !ok || tok.consumed || !q.now().Before(tok.expiresAt) {
			*out = nil
			return nil
		}
		tok.consumed = true
		*out = &tok.userID
		return nil
	}}
}

// profileRow answers SELECT id, email, full_name FROM
// app.get_user_profile($1): a known user scans its row, an unknown one hits
// the pgx.ErrNoRows the Store maps to ok=false.
func (q *sessionQuerier) profileRow(args []any) pgx.Row {
	return stubRow{scan: func(dest ...any) error {
		if err := q.fail["get_user_profile"]; err != nil {
			return err
		}
		q.profileCalls = append(q.profileCalls, args[0].(uuid.UUID))
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

// createRow answers SELECT app.create_session($1, $2, $3): it records the
// arguments (so tests can diff hash vs cookie value and check the expiry)
// and returns a fresh session id.
func (q *sessionQuerier) createRow(args []any) pgx.Row {
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
		q.creates = append(q.creates, call)
		*(dest[0].(*uuid.UUID)) = call.sessionID
		return nil
	}}
}

// sessionRig is a mounted auth surface plus the test's windows into it.
type sessionRig struct {
	handler http.Handler
	clock   *fakeClock
	cfg     *platform.Config
	q       *sessionQuerier
}

// sessionConfig is the rig's request-edge config: one JWT key at the 32-byte
// floor, the spec section 4 TTLs and Secure cookies on.
func sessionConfig() *platform.Config {
	return &platform.Config{
		WebURL:       "https://web.test",
		JWTKeys:      map[string]string{"k1": strings.Repeat("s", 32)},
		ActiveKID:    "k1",
		AccessTTL:    15 * time.Minute,
		RefreshTTL:   30 * 24 * time.Hour,
		LoginTTL:     15 * time.Minute,
		CookieSecure: true,
	}
}

// directInTx is the fake rigs' InTxFunc: the in-memory Querier has no Begin,
// so "the transaction" is just the call — what the fake matrix proves is the
// flow through fn, never atomicity. Real rollback is pinned against Postgres
// in session_integration_test.go.
func directInTx(store *platform.Store) auth.InTxFunc {
	return func(_ context.Context, fn func(*platform.Store) error) error {
		return fn(store)
	}
}

// newSessionRig wires the endpoints the way cmd/server will: real Store over
// the fake Querier, real Limiters on the fake clock, mounted through
// auth.NewServer. The clock starts at real now — VerifyAccess checks exp
// against wall time, so a fixed epoch would mint an already-expired JWT.
func newSessionRig(t *testing.T, cfg *platform.Config) *sessionRig {
	t.Helper()
	clock := &fakeClock{now: time.Now()}
	q := &sessionQuerier{
		now:      clock.Now,
		tokens:   map[string]*tokenRec{},
		profiles: map[uuid.UUID]profileRec{},
		members:  map[string][]platform.Membership{},
		fail:     map[string]error{},
	}
	store := platform.NewStore(q)
	endpoints := auth.NewEndpoints(cfg,
		store, directInTx(store), &platform.FakeMailSender{},
		platform.NewLimiter(platform.LoginIPCapacity, platform.LimitWindow, clock.Now),
		platform.NewLimiter(platform.LoginEmailCapacity, platform.LimitWindow, clock.Now),
		clock.Now)
	return &sessionRig{handler: auth.NewServer(endpoints), clock: clock, cfg: cfg, q: q}
}

// plantToken records a login token the way the login handler's store call
// left it: the map key is the SHA-256 hash, never the raw token. It returns
// the token for the request body.
func (r *sessionRig) plantToken(t *testing.T, userID uuid.UUID, expiresAt time.Time) string {
	t.Helper()
	token, err := platform.NewOpaqueToken()
	require.NoError(t, err)
	r.q.tokens[platform.HashToken(token)] = &tokenRec{userID: userID, expiresAt: expiresAt}
	return token
}

// postSession fires one JSON body at the mounted mux.
func (r *sessionRig) postSession(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/auth/session", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	return rec
}

// findCookie pulls one named cookie out of the recorder's Set-Cookie
// headers; nil means the handler never wrote it.
func findCookie(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}
