package auth_test

// POST /auth/login proofs (spec 2026-11-10-tenant-resolution-api sections 8
// and 9 — the "login proofs" block): known and unknown emails answer
// byte-identically, and only the known one stages the three writes of
// migration 0004 inside one transaction — the token row holding the SHA-256
// hash, the outbox row holding the rendered link, and the auth_mail_send
// job carrying the outbox id alone. Malformed bodies get the uniform 400,
// driver and enqueue failures the uniform 500, an exhausted bucket the
// uniform 429. Everything runs through auth.NewServer — the same strict
// handler, middleware and error hooks production mounts; the doubles live
// in login_fake_test.go, the password path's own proofs in
// login_password_test.go.

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// TestLoginKnownEmailQueuesMagicLink covers the provisioned path: the
// uniform 200 body plus the three-write staging of migration 0004 — the
// store keeps only the token's SHA-256 hex with the LoginTTL expiry, the
// outbox row links that token row to the rendered link, and the queued job
// carries the outbox id alone (spec sections 4 and 8).
func TestLoginKnownEmailQueuesMagicLink(t *testing.T) {
	r := newRig(t, platform.LoginIPCapacity, platform.LoginEmailCapacity)
	userID := uuid.New()
	r.q.users["ada@test.dev"] = userID

	rec := r.postLogin(t, `{"email":"  ADA@Test.dev  "}`) // padded, upper-cased: normalization proof

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, `{"status":"ok"}`+"\n", rec.Body.String())

	require.Len(t, r.enq.ids, 1, "the staged send must be queued")
	require.Len(t, r.q.outboxCreates, 1)
	assert.Equal(t, r.q.outboxCreates[0].outboxID, r.enq.ids[0],
		"the job carries the outbox id the tx returned — never the link")

	prefix := r.cfg.WebURL + "/auth/exchange?token="
	link := r.q.outboxCreates[0].link
	require.True(t, strings.HasPrefix(link, prefix),
		"the outbox link must be ${WEB_URL}/auth/exchange?token=…, got %q", link)
	token := strings.TrimPrefix(link, prefix)
	raw, err := base64.RawURLEncoding.DecodeString(token)
	require.NoError(t, err)
	assert.Len(t, raw, 32, "the link carries a 32-byte opaque token")

	require.Len(t, r.q.finds, 1)
	assert.Equal(t, "ada@test.dev", r.q.finds[0], "the store sees the normalized email")
	require.Len(t, r.q.creates, 1)
	assert.Equal(t, userID, r.q.creates[0].userID)
	assert.Equal(t, platform.HashToken(token), r.q.creates[0].hash,
		"the store holds the SHA-256 hash of the mailed token")
	assert.NotContains(t, link, r.q.creates[0].hash,
		"the hash must never appear in the rendered link")
	assert.Equal(t, r.clock.now.Add(r.cfg.LoginTTL), r.q.creates[0].expiresAt)
	assert.Equal(t, r.q.creates[0].id, r.q.outboxCreates[0].tokenID,
		"the outbox row links the login token row inside the same tx")
}

// TestLoginUniform200 is the no-enumeration proof: a known and an unknown
// email get byte-identical status, headers and body, and the unknown path
// stages nothing — no token, no outbox row, no job.
func TestLoginUniform200(t *testing.T) {
	r := newRig(t, platform.LoginIPCapacity, platform.LoginEmailCapacity)
	r.q.users["known@test.dev"] = uuid.New()

	known := r.postLogin(t, `{"email":"known@test.dev"}`)
	unknown := r.postLogin(t, `{"email":"stranger@test.dev"}`)

	require.Equal(t, http.StatusOK, unknown.Code)
	assert.Equal(t, known.Code, unknown.Code)
	assert.Equal(t, known.Header().Get("Content-Type"), unknown.Header().Get("Content-Type"))
	assert.Equal(t, known.Body.String(), unknown.Body.String(),
		"known and unknown emails must answer byte-identically")
	assert.Equal(t, `{"status":"ok"}`+"\n", unknown.Body.String())

	assert.Len(t, r.q.creates, 1, "exactly one token row was stored")
	assert.Len(t, r.q.outboxCreates, 1, "exactly one send was staged — the known user's")
	assert.Len(t, r.enq.ids, 1, "exactly one job was queued")
	assert.Len(t, r.q.finds, 2, "both requests still pay the same lookup")
}

// TestLoginBadRequests covers every request-shape failure: bodies the JSON
// binder rejects (routed by RequestErrorHandlerFunc) and bodies that bind but
// carry an absent or ill-formed email (the handler's own 400). All share the
// one {"error":"bad_request"} body and none reach the store.
func TestLoginBadRequests(t *testing.T) {
	r := newRig(t, platform.LoginIPCapacity, platform.LoginEmailCapacity)
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
	assert.Empty(t, r.q.finds, "no rejected request may reach the store")
	assert.Empty(t, r.q.creates)
	assert.Empty(t, r.q.outboxCreates)
	assert.Empty(t, r.enq.ids, "a rejected request queues no send")
}

// TestLoginFailuresAreUniform500 proves a failure is the same 500
// {"error":"internal"} wherever it strikes — the lookup, the token insert,
// the outbox insert or the enqueue itself: a server fault is not an
// enumeration signal. A provider mail failure is deliberately absent here:
// the send runs in the auth_mail_send worker off the response path, so it
// can never reach the wire (migration 0004).
func TestLoginFailuresAreUniform500(t *testing.T) {
	cases := []struct {
		name   string
		failAt string // "" drives the enqueue-failure leg instead
	}{
		{"lookup driver error", "find_user_by_email"},
		{"token insert driver error", "create_login_token"},
		{"outbox insert driver error", "create_mail_outbox"},
		{"enqueue failure", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, platform.LoginIPCapacity, platform.LoginEmailCapacity)
			r.q.users["ada@test.dev"] = uuid.New()
			if tc.failAt != "" {
				r.q.fail[tc.failAt] = errBoom
			} else {
				r.enq.err = errBoom
			}
			rec := r.postLogin(t, `{"email":"ada@test.dev"}`)
			assert.Equal(t, http.StatusInternalServerError, rec.Code)
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			assert.Equal(t, `{"error":"internal"}`+"\n", rec.Body.String())
		})
	}
}

// TestLoginOversizedBodyIsUniform400 caps the parse cost of a hostile body:
// limitBody sits between the mux and the generated binder, so a body past
// maxJSONBody dies as the uniform 400 — spending no limiter budget and no
// store work.
func TestLoginOversizedBodyIsUniform400(t *testing.T) {
	r := newRig(t, platform.LoginIPCapacity, platform.LoginEmailCapacity)

	body := `{"email":"` + strings.Repeat("a", 9<<10) + `@test.dev"}`
	rec := r.postLogin(t, body)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, `{"error":"bad_request"}`+"\n", rec.Body.String())
	assert.Empty(t, r.q.finds, "an oversized body must never reach the store")
	assert.Empty(t, r.enq.ids)
}

// TestLoginIPRateLimit drains the per-IP bucket (spec section 8.1): the
// over-budget request gets the uniform 429 and does no store or staging
// work, another IP carries its own bucket, and a denied call consumes
// nothing — exactly one refilled token later the same caller is admitted
// again.
func TestLoginIPRateLimit(t *testing.T) {
	r := newRig(t, 2, 100)
	r.q.users["ada@test.dev"] = uuid.New()

	require.Equal(t, http.StatusOK, r.postLogin(t, `{"email":"ada@test.dev"}`).Code)
	require.Equal(t, http.StatusOK, r.postLogin(t, `{"email":"ada@test.dev"}`).Code)
	require.Len(t, r.enq.ids, 2)

	rec := r.postLogin(t, `{"email":"ada@test.dev"}`)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, `{"error":"rate_limited"}`+"\n", rec.Body.String())
	assert.Len(t, r.enq.ids, 2, "a denied request stages nothing")
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
	assert.Len(t, r.enq.ids, 4)
}

// TestLoginEmailRateLimit drains the per-recipient bucket: the same address
// in another case is the same key (the limiter sees the normalized form), a
// different recipient is unthrottled, and the denial consumed nothing —
// one refilled token admits the retry.
func TestLoginEmailRateLimit(t *testing.T) {
	r := newRig(t, 100, 1)
	r.q.users["ada@test.dev"] = uuid.New()
	r.q.users["grace@test.dev"] = uuid.New()

	require.Equal(t, http.StatusOK, r.postLogin(t, `{"email":"ada@test.dev"}`).Code)
	require.Len(t, r.enq.ids, 1)

	rec := r.postLogin(t, `{"email":"ADA@Test.dev"}`)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, `{"error":"rate_limited"}`+"\n", rec.Body.String())
	assert.Len(t, r.enq.ids, 1, "a denied request stages nothing")

	assert.Equal(t, http.StatusOK, r.postLogin(t, `{"email":"grace@test.dev"}`).Code,
		"the email bucket is keyed per normalized recipient")

	// Capacity 1 per window: one full window earns exactly one token back.
	r.clock.Advance(platform.LimitWindow)
	assert.Equal(t, http.StatusOK, r.postLogin(t, `{"email":"ada@test.dev"}`).Code,
		"a denied call consumed nothing — one refilled token admits the retry")
	assert.Len(t, r.enq.ids, 3)
}

// TestLoginWithoutClientIPMiddlewareFailsLoud pins the mis-mount guard: if
// ExtractClientIP is dropped from the strict middlewares, login must not fall
// back to a shared "" limiter bucket — the missing context is the loud 500.
func TestLoginWithoutClientIPMiddlewareFailsLoud(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	cfg := &platform.Config{WebURL: "https://web.test", LoginTTL: 15 * time.Minute}
	q := &loginQuerier{users: map[string]uuid.UUID{}, fail: map[string]error{}}
	store := platform.NewStore(q)
	endpoints := auth.NewEndpoints(cfg, store, directInTx(store), &neverRunner{t}, mustNotEnqueue(t),
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
	enq := &fakeEnqueue{}
	store := platform.NewStore(q)
	endpoints := auth.NewEndpoints(cfg, store, directInTx(store), &neverRunner{t}, enq.run,
		platform.NewLimiter(30, platform.LimitWindow, clock.Now),
		platform.NewLimiter(5, platform.LimitWindow, clock.Now),
		clock.Now)
	handler := auth.NewServer(endpoints)

	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"email":"ada@test.dev"}`))
	req.RemoteAddr = "198.51.100.9:443"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Len(t, q.outboxCreates, 1)
	assert.True(t, strings.HasPrefix(q.outboxCreates[0].link, "https://web.test/auth/exchange?token="),
		"trailing-slash WEB_URL must not produce //auth/exchange, got %q", q.outboxCreates[0].link)
}

// TestLoginTrailingGarbageIsUniform400 pins the declared-length reject: a
// valid object riding before an over-cap tail must die before the binder —
// MaxBytesReader alone bounds reads, but the decoder never reads the tail.
func TestLoginTrailingGarbageIsUniform400(t *testing.T) {
	r := newRig(t, platform.LoginIPCapacity, platform.LoginEmailCapacity)

	rec := r.postLogin(t, `{"email":"ada@test.dev"}`+strings.Repeat(" ", 9<<10))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, `{"error":"bad_request"}`+"\n", rec.Body.String())
	assert.Empty(t, r.q.creates, "a rejected body never reaches the store")
	assert.Empty(t, r.enq.ids)
}
