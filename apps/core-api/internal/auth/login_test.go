package auth_test

// POST /auth/login proofs (spec 2026-11-10-tenant-resolution-api sections 8
// and 9 — the "login proofs" block): known and unknown emails answer
// byte-identically, only the known one triggers a token row plus a mail
// carrying the opaque token (the store keeps its hash), malformed bodies get
// the uniform 400, driver failures the uniform 500, and an exhausted bucket
// the uniform 429. Everything runs through auth.NewServer — the same strict
// handler, middleware and error hooks production mounts.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
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
// was asked to keep and the expiry the handler computed.
type createCall struct {
	userID    uuid.UUID
	hash      string
	expiresAt time.Time
}

// loginQuerier is an in-memory platform.Querier for the login path: a users
// table keyed on the function's own lower/btrim normalization, a record of
// find/create calls, and per-function driver-error injection. The real
// platform.Store runs over it, so the SQL text and NULL handling are the
// production ones — only the transport is faked.
type loginQuerier struct {
	users   map[string]uuid.UUID
	fail    map[string]error
	finds   []string
	creates []createCall
}

var _ platform.Querier = (*loginQuerier)(nil)

func (q *loginQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "find_user_by_email"):
		return q.findRow(args)
	case strings.Contains(sql, "create_login_token"):
		return q.createRow(args)
	default:
		return stubRow{scan: func(...any) error {
			return fmt.Errorf("loginQuerier: unexpected sql %q", sql)
		}}
	}
}

func (q *loginQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("loginQuerier: Query is not on the login path")
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

// createRow answers SELECT app.create_login_token($1, $2, $3): it records the
// arguments (so tests can diff hash vs token) and returns a fresh row id.
func (q *loginQuerier) createRow(args []any) pgx.Row {
	return stubRow{scan: func(dest ...any) error {
		if err := q.fail["create_login_token"]; err != nil {
			return err
		}
		q.creates = append(q.creates, createCall{
			userID:    args[0].(uuid.UUID),
			hash:      args[1].(string),
			expiresAt: args[2].(time.Time),
		})
		*(dest[0].(*uuid.UUID)) = uuid.New()
		return nil
	}}
}

// failMailer is a MailSender that always errors — FakeMailSender never fails,
// so the detached send-failure branch needs its own double. calls counts the
// attempts so tests prove the retry budget rather than guess at goroutine
// timing.
type failMailer struct {
	err   error
	calls atomic.Int32
}

func (f *failMailer) SendMagicLink(context.Context, string, string) error {
	f.calls.Add(1)
	return f.err
}

// recordLog is a slog.Handler that captures record messages — the detached
// send logs from a goroutine, so a test asserts on the capture with
// require.Eventually rather than on the response.
type recordLog struct {
	mu       sync.Mutex
	messages []string
}

func (h *recordLog) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordLog) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, r.Message)
	return nil
}

func (h *recordLog) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordLog) WithGroup(string) slog.Handler      { return h }

// contains reports whether any captured message carries the needle — read
// under the same mutex Handle writes with.
func (h *recordLog) contains(needle string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.messages {
		if strings.Contains(m, needle) {
			return true
		}
	}
	return false
}

// awaitSend polls for one recorded fake send: the mail leaves the response
// path on a detached goroutine, so a synchronous Len would race it.
func awaitSend(t *testing.T, mailer *platform.FakeMailSender, n int) []platform.MagicSend {
	t.Helper()
	require.Eventually(t, func() bool { return len(mailer.Sent()) == n },
		time.Second, 5*time.Millisecond, "expected %d recorded send(s)", n)
	return mailer.Sent()
}

// rig is a mounted auth surface plus the test's windows into it.
type rig struct {
	handler http.Handler
	clock   *fakeClock
	cfg     *platform.Config
	q       *loginQuerier
}

// newRig wires the endpoints the way cmd/server will: real Store over the
// fake Querier, real Limiters at the given capacities on the fake clock, the
// caller's MailSender, mounted through auth.NewServer.
func newRig(t *testing.T, mailer platform.MailSender, ipCap, emailCap int) *rig {
	t.Helper()
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	cfg := &platform.Config{WebURL: "https://web.test", LoginTTL: 15 * time.Minute}
	q := &loginQuerier{users: map[string]uuid.UUID{}, fail: map[string]error{}}
	store := platform.NewStore(q)
	endpoints := auth.NewEndpoints(cfg,
		store, directInTx(store), mailer,
		platform.NewLimiter(ipCap, platform.LimitWindow, clock.Now),
		platform.NewLimiter(emailCap, platform.LimitWindow, clock.Now),
		clock.Now)
	return &rig{handler: auth.NewServer(endpoints), clock: clock, cfg: cfg, q: q}
}

// postLogin fires one JSON body at the mounted mux. The default
// httptest.NewRequest RemoteAddr is "192.0.2.1:1234" — IP "192.0.2.1".
func (r *rig) postLogin(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	return rec
}

// TestLoginKnownEmailSendsMagicLink covers the provisioned path: the uniform
// 200 body, one send to the normalized address, a link carrying a fresh
// 32-byte opaque token, and the store holding only its SHA-256 hex with the
// LoginTTL expiry (spec sections 4 and 8).
func TestLoginKnownEmailSendsMagicLink(t *testing.T) {
	mailer := &platform.FakeMailSender{}
	r := newRig(t, mailer, platform.LoginIPCapacity, platform.LoginEmailCapacity)
	userID := uuid.New()
	r.q.users["ada@test.dev"] = userID

	rec := r.postLogin(t, `{"email":"  ADA@Test.dev  "}`) // padded, upper-cased: normalization proof

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, `{"status":"ok"}`+"\n", rec.Body.String())

	sent := awaitSend(t, mailer, 1)
	assert.Equal(t, "ada@test.dev", sent[0].To, "the recipient is the normalized address")

	prefix := r.cfg.WebURL + "/auth/exchange?token="
	require.True(t, strings.HasPrefix(sent[0].URL, prefix),
		"magic link must be ${WEB_URL}/auth/exchange?token=…, got %q", sent[0].URL)
	token := strings.TrimPrefix(sent[0].URL, prefix)
	raw, err := base64.RawURLEncoding.DecodeString(token)
	require.NoError(t, err)
	assert.Len(t, raw, 32, "the link carries a 32-byte opaque token")

	require.Len(t, r.q.finds, 1)
	assert.Equal(t, "ada@test.dev", r.q.finds[0], "the store sees the normalized email")
	require.Len(t, r.q.creates, 1)
	assert.Equal(t, userID, r.q.creates[0].userID)
	assert.Equal(t, platform.HashToken(token), r.q.creates[0].hash,
		"the store holds the SHA-256 hash of the mailed token")
	assert.NotContains(t, sent[0].URL, r.q.creates[0].hash,
		"the hash must never cross the wire")
	assert.Equal(t, r.clock.now.Add(r.cfg.LoginTTL), r.q.creates[0].expiresAt)
}

// TestLoginUniform200 is the no-enumeration proof: a known and an unknown
// email get byte-identical status, headers and body, and the unknown path
// creates nothing and sends nothing.
func TestLoginUniform200(t *testing.T) {
	mailer := &platform.FakeMailSender{}
	r := newRig(t, mailer, platform.LoginIPCapacity, platform.LoginEmailCapacity)
	r.q.users["known@test.dev"] = uuid.New()

	known := r.postLogin(t, `{"email":"known@test.dev"}`)
	unknown := r.postLogin(t, `{"email":"stranger@test.dev"}`)

	require.Equal(t, http.StatusOK, unknown.Code)
	assert.Equal(t, known.Code, unknown.Code)
	assert.Equal(t, known.Header().Get("Content-Type"), unknown.Header().Get("Content-Type"))
	assert.Equal(t, known.Body.String(), unknown.Body.String(),
		"known and unknown emails must answer byte-identically")
	assert.Equal(t, `{"status":"ok"}`+"\n", unknown.Body.String())

	awaitSend(t, mailer, 1)
	assert.Len(t, mailer.Sent(), 1, "exactly one link went out — the known user's")
	assert.Len(t, r.q.creates, 1, "exactly one token row was stored")
	assert.Len(t, r.q.finds, 2, "both requests still pay the same lookup")
}

// TestLoginBadRequests covers every request-shape failure: bodies the JSON
// binder rejects (routed by RequestErrorHandlerFunc) and bodies that bind but
// carry an absent or ill-formed email (the handler's own 400). All share the
// one {"error":"bad_request"} body and none reach the store.
func TestLoginBadRequests(t *testing.T) {
	mailer := &platform.FakeMailSender{}
	r := newRig(t, mailer, platform.LoginIPCapacity, platform.LoginEmailCapacity)
	r.q.users["ada@test.dev"] = uuid.New()

	cases := []struct {
		name string
		body string
	}{
		{"malformed json", `{"email":`},
		{"empty body", ``},
		{"email wrong type", `{"email":42}`},
		{"missing email", `{}`},
		{"empty email", `{"email":""}`},
		{"not an addr-spec", `{"email":"not-an-email"}`},
		{"display name rejected", `{"email":"Ada <ada@test.dev>"}`},
		{"angle form rejected", `{"email":"<ada@test.dev>"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := r.postLogin(t, tc.body)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			assert.Equal(t, `{"error":"bad_request"}`+"\n", rec.Body.String())
		})
	}
	assert.Empty(t, mailer.Sent())
	assert.Empty(t, r.q.finds, "no rejected request may reach the store")
	assert.Empty(t, r.q.creates)
}

// TestLoginFailuresAreUniform500 proves a driver failure is the same 500
// {"error":"internal"} wherever it strikes — a server fault is not an
// enumeration signal. A mail failure is deliberately absent here: the send
// is detached, so it can never reach the wire — see
// TestLoginMailFailureIsLoggedNotSurfaced.
func TestLoginFailuresAreUniform500(t *testing.T) {
	cases := []struct {
		name   string
		mailer platform.MailSender
		failAt string
	}{
		{"lookup driver error", &platform.FakeMailSender{}, "find_user_by_email"},
		{"token insert driver error", &platform.FakeMailSender{}, "create_login_token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, tc.mailer, platform.LoginIPCapacity, platform.LoginEmailCapacity)
			r.q.users["ada@test.dev"] = uuid.New()
			if tc.failAt != "" {
				r.q.fail[tc.failAt] = errBoom
			}
			rec := r.postLogin(t, `{"email":"ada@test.dev"}`)
			assert.Equal(t, http.StatusInternalServerError, rec.Code)
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			assert.Equal(t, `{"error":"internal"}`+"\n", rec.Body.String())
		})
	}
}

// TestLoginMailFailureIsLoggedNotSurfaced proves a dead mailer cannot unmask
// a provisioned address: the send runs off the response path, so the client
// still gets the uniform 200 and the failure only ever reaches the log.
func TestLoginMailFailureIsLoggedNotSurfaced(t *testing.T) {
	logs := &recordLog{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(prev) })

	mailer := &failMailer{err: errBoom}
	r := newRig(t, mailer, platform.LoginIPCapacity, platform.LoginEmailCapacity)
	r.q.users["ada@test.dev"] = uuid.New()

	rec := r.postLogin(t, `{"email":"ada@test.dev"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, `{"status":"ok"}`+"\n", rec.Body.String(),
		"a mail failure must not change the uniform answer")

	require.Eventually(t, func() bool { return logs.contains("magic link send failed") },
		time.Second, 5*time.Millisecond, "the detached send failure must be logged")
	assert.Len(t, r.q.creates, 1, "the token was already stored before the send ran")
	require.Eventually(t, func() bool { return mailer.calls.Load() == 3 },
		5*time.Second, 10*time.Millisecond, "a failed send retries up to the 3-attempt budget")
}

// TestLoginOversizedBodyIsUniform400 caps the parse cost of a hostile body:
// limitBody sits between the mux and the generated binder, so a body past
// maxJSONBody dies as the uniform 400 — spending no limiter budget and no
// store work.
func TestLoginOversizedBodyIsUniform400(t *testing.T) {
	mailer := &platform.FakeMailSender{}
	r := newRig(t, mailer, platform.LoginIPCapacity, platform.LoginEmailCapacity)

	body := `{"email":"` + strings.Repeat("a", 9<<10) + `@test.dev"}`
	rec := r.postLogin(t, body)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, `{"error":"bad_request"}`+"\n", rec.Body.String())
	assert.Empty(t, mailer.Sent())
	assert.Empty(t, r.q.finds, "an oversized body must never reach the store")
}

// TestLoginIPRateLimit drains the per-IP bucket (spec section 8.1): the
// over-budget request gets the uniform 429 and does no store or mail work,
// another IP carries its own bucket, and a denied call consumes nothing —
// exactly one refilled token later the same caller is admitted again.
func TestLoginIPRateLimit(t *testing.T) {
	mailer := &platform.FakeMailSender{}
	r := newRig(t, mailer, 2, 100)
	r.q.users["ada@test.dev"] = uuid.New()

	require.Equal(t, http.StatusOK, r.postLogin(t, `{"email":"ada@test.dev"}`).Code)
	require.Equal(t, http.StatusOK, r.postLogin(t, `{"email":"ada@test.dev"}`).Code)
	awaitSend(t, mailer, 2)

	rec := r.postLogin(t, `{"email":"ada@test.dev"}`)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, `{"error":"rate_limited"}`+"\n", rec.Body.String())
	assert.Len(t, mailer.Sent(), 2, "a denied request sends nothing")
	assert.Len(t, r.q.creates, 2, "a denied request stores no token")

	// A different RemoteAddr is a different bucket.
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"email":"ada@test.dev"}`))
	req.RemoteAddr = "198.51.100.7:9999"
	rec2 := httptest.NewRecorder()
	r.handler.ServeHTTP(rec2, req)
	assert.Equal(t, http.StatusOK, rec2.Code, "the IP bucket is keyed per caller")

	// At capacity 2 per window one token refills in half a window; had the
	// 429 above consumed budget the balance would still be short.
	r.clock.Advance(platform.LimitWindow / 2)
	assert.Equal(t, http.StatusOK, r.postLogin(t, `{"email":"ada@test.dev"}`).Code,
		"a denied call consumed nothing — one refilled token admits the retry")
	awaitSend(t, mailer, 4)
}

// TestLoginEmailRateLimit drains the per-recipient bucket: the same address
// in another case is the same key (the limiter sees the normalized form), a
// different recipient is unthrottled, and the denial consumed nothing —
// one refilled token admits the retry.
func TestLoginEmailRateLimit(t *testing.T) {
	mailer := &platform.FakeMailSender{}
	r := newRig(t, mailer, 100, 1)
	r.q.users["ada@test.dev"] = uuid.New()
	r.q.users["grace@test.dev"] = uuid.New()

	require.Equal(t, http.StatusOK, r.postLogin(t, `{"email":"ada@test.dev"}`).Code)
	awaitSend(t, mailer, 1)

	rec := r.postLogin(t, `{"email":"ADA@Test.dev"}`)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, `{"error":"rate_limited"}`+"\n", rec.Body.String())
	assert.Len(t, mailer.Sent(), 1, "a denied request sends nothing")

	assert.Equal(t, http.StatusOK, r.postLogin(t, `{"email":"grace@test.dev"}`).Code,
		"the email bucket is keyed per normalized recipient")

	// Capacity 1 per window: one full window earns exactly one token back.
	r.clock.Advance(platform.LimitWindow)
	assert.Equal(t, http.StatusOK, r.postLogin(t, `{"email":"ada@test.dev"}`).Code,
		"a denied call consumed nothing — one refilled token admits the retry")
	awaitSend(t, mailer, 3)
	assert.Len(t, mailer.Sent(), 3)
}

// TestUnimplementedEndpointsFailLoud pins the stub contract: the contract
// mounts all six routes, so the three still unimplemented must fail loudly
// through the uniform 500 — never a silent 200 or 204 (a 204 on logout
// would claim a revocation that did not happen).
func TestUnimplementedEndpointsFailLoud(t *testing.T) {
	r := newRig(t, &platform.FakeMailSender{}, platform.LoginIPCapacity, platform.LoginEmailCapacity)

	cases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/auth/logout", `{}`},
		{http.MethodGet, "/me", ``},
		{http.MethodGet, "/me/organization", ``},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			r.handler.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusInternalServerError, rec.Code)
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			assert.Equal(t, `{"error":"internal"}`+"\n", rec.Body.String())
		})
	}
	assert.Empty(t, r.q.finds, "a stub must never reach the store")
}

// TestLoginWithoutClientIPMiddlewareFailsLoud pins the mis-mount guard: if
// ExtractClientIP is dropped from the strict middlewares, login must not fall
// back to a shared "" limiter bucket — the missing context is the loud 500.
func TestLoginWithoutClientIPMiddlewareFailsLoud(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	cfg := &platform.Config{WebURL: "https://web.test", LoginTTL: 15 * time.Minute}
	q := &loginQuerier{users: map[string]uuid.UUID{}, fail: map[string]error{}}
	store := platform.NewStore(q)
	endpoints := auth.NewEndpoints(cfg, store, directInTx(store), &platform.FakeMailSender{},
		platform.NewLimiter(30, platform.LimitWindow, clock.Now),
		platform.NewLimiter(5, platform.LimitWindow, clock.Now),
		clock.Now)
	handler := authapi.HandlerWithOptions(
		authapi.NewStrictHandler(endpoints, nil), // no middlewares — the mount bug under test
		authapi.StdHTTPServerOptions{})

	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"email":"ada@test.dev"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, q.finds, "a mis-mounted login must not reach the store")
}

// A WEB_URL carrying a trailing slash must not leak a double separator into
// the magic link — the link is what the user actually clicks.
func TestLoginTrimsTrailingSlashOnWebURL(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	q := &loginQuerier{users: map[string]uuid.UUID{"ada@test.dev": uuid.New()}, fail: map[string]error{}}
	cfg := &platform.Config{WebURL: "https://web.test/", LoginTTL: 15 * time.Minute}
	mailer := &platform.FakeMailSender{}
	store := platform.NewStore(q)
	endpoints := auth.NewEndpoints(cfg, store, directInTx(store), mailer,
		platform.NewLimiter(30, platform.LimitWindow, clock.Now),
		platform.NewLimiter(5, platform.LimitWindow, clock.Now),
		clock.Now)
	handler := auth.NewServer(endpoints)

	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"email":"ada@test.dev"}`))
	req.RemoteAddr = "198.51.100.9:443"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	sent := awaitSend(t, mailer, 1)
	assert.True(t, strings.HasPrefix(sent[0].URL, "https://web.test/auth/exchange?token="),
		"trailing-slash WEB_URL must not produce //auth/exchange, got %q", sent[0].URL)
}

// TestLoginTrailingGarbageIsUniform400 pins the declared-length reject: a
// valid object riding before an over-cap tail must die before the binder —
// MaxBytesReader alone bounds reads, but the decoder never reads the tail.
func TestLoginTrailingGarbageIsUniform400(t *testing.T) {
	mailer := &platform.FakeMailSender{}
	r := newRig(t, mailer, platform.LoginIPCapacity, platform.LoginEmailCapacity)

	rec := r.postLogin(t, `{"email":"ada@test.dev"}`+strings.Repeat(" ", 9<<10))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, `{"error":"bad_request"}`+"\n", rec.Body.String())
	assert.Empty(t, r.q.creates, "a rejected body never reaches the store")
	assert.Empty(t, mailer.Sent())
}
