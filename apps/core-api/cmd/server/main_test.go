package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// markerHandler proves a mount was reached: the wrapped middleware chain
// either lets the tag through or denies before it.
func markerHandler(tag string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(tag))
	})
}

// testConfig gives buildMux a WEB_URL CheckOrigin can trust and a JWT key
// set Authenticate can build — the two constructors that validate config.
func testConfig() *platform.Config {
	return &platform.Config{
		WebURL:    "https://app.example.test",
		JWTKeys:   map[string]string{"dev": strings.Repeat("k", 40)},
		ActiveKID: "dev",
		AccessTTL: 15 * time.Minute,
	}
}

// serve request helper: fire req at mux and return the recorder.
func fire(mux http.Handler, method, path string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestMuxAuthSubtree proves /auth/* reaches the endpoint handler through
// Correlation+CheckOrigin with no auth middleware — a non-browser caller
// (no Origin) passes, and the request id echoes back.
func TestMuxAuthSubtree(t *testing.T) {
	mux := buildMux(testConfig(), platform.NewStore(nil), markerHandler("auth"), nil)
	rec := fire(mux, "POST", "/auth/login")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "auth", rec.Body.String())
	require.NotEmpty(t, rec.Header().Get("X-Request-Id"))
}

// TestMuxOriginDenied proves CheckOrigin rejects a cross-site browser post
// before the endpoint sees it — the marker must never answer.
func TestMuxOriginDenied(t *testing.T) {
	mux := buildMux(testConfig(), platform.NewStore(nil), markerHandler("auth"), nil)
	rec := fire(mux, "POST", "/auth/login", "Origin", "https://evil.example.test")
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.NotEqual(t, "auth", rec.Body.String())
}

// TestMuxOriginTrusted proves the WEB_URL web app posts freely — the
// topology CheckOrigin exists for.
func TestMuxOriginTrusted(t *testing.T) {
	mux := buildMux(testConfig(), platform.NewStore(nil), markerHandler("auth"), nil)
	rec := fire(mux, "POST", "/auth/login", "Origin", "https://app.example.test")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "auth", rec.Body.String())
}

// TestMuxMeRequiresJWT proves Authenticate wraps /me: a request with no
// qp_access cookie dies at the middleware's uniform 401 and never reaches
// the endpoint.
func TestMuxMeRequiresJWT(t *testing.T) {
	mux := buildMux(testConfig(), platform.NewStore(nil), markerHandler("auth"), nil)
	rec := fire(mux, "GET", "/me")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.NotEqual(t, "auth", rec.Body.String())
}

// TestMuxMeOrganizationRequiresTenant proves ResolveTenant sits behind
// Authenticate on the org route: a valid JWT without X-Organization-Id gets
// the header check's 400 — ResolveTenant ran, and the endpoint never did.
func TestMuxMeOrganizationRequiresTenant(t *testing.T) {
	cfg := testConfig()
	token, err := cfg.SignAccess(uuid.New(), uuid.New(), time.Now())
	require.NoError(t, err)
	mux := buildMux(cfg, platform.NewStore(nil), markerHandler("auth"), nil)
	rec := fire(mux, "GET", "/me/organization", "Cookie", "qp_access="+token)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.NotEqual(t, "auth", rec.Body.String())
}

// TestMuxMeOrganizationRejectsBadJWT proves Authenticate precedes
// ResolveTenant on the org route: a forged cookie dies at the 401, not at
// the tenant check.
func TestMuxMeOrganizationRejectsBadJWT(t *testing.T) {
	mux := buildMux(testConfig(), platform.NewStore(nil), markerHandler("auth"), nil)
	rec := fire(mux, "GET", "/me/organization", "Cookie", "qp_access=forged")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestMuxWebhookMount covers both inbox states: unmounted when buildInbox
// returned nil (secret unset), served when the handler exists.
func TestMuxWebhookMount(t *testing.T) {
	without := buildMux(testConfig(), platform.NewStore(nil), markerHandler("auth"), nil)
	require.Equal(t, http.StatusNotFound, fire(without, "POST", "/webhooks/resend/inbound").Code)
	with := buildMux(testConfig(), platform.NewStore(nil), markerHandler("auth"), markerHandler("inbox"))
	require.Equal(t, "inbox", fire(with, "POST", "/webhooks/resend/inbound").Body.String())
}

// TestMuxStrayPath is the catch-all: nothing outside the mounts matches.
func TestMuxStrayPath(t *testing.T) {
	mux := buildMux(testConfig(), platform.NewStore(nil), markerHandler("auth"), markerHandler("inbox"))
	require.Equal(t, http.StatusNotFound, fire(mux, "GET", "/nope").Code)
}

// TestListenAddr pins the PORT convention and the dev default.
func TestListenAddr(t *testing.T) {
	t.Setenv("PORT", "")
	require.Equal(t, ":8080", listenAddr())
	t.Setenv("PORT", "9000")
	require.Equal(t, ":9000", listenAddr())
}

// TestNewMailer covers the three mail paths: real Resend, the gated dev
// sender, and the boot failure when neither is configured.
func TestNewMailer(t *testing.T) {
	resend, err := newMailer(&platform.Config{ResendAPIKey: "re_x", MagicFrom: "a@b.c"})
	require.NoError(t, err)
	require.IsType(t, &platform.ResendMailSender{}, resend)
	t.Setenv("AUTH_DEV_MAILER", "true")
	dev, err := newMailer(&platform.Config{})
	require.NoError(t, err)
	require.IsType(t, &platform.LogMailSender{}, dev)
}

// TestNewMailerRequiresOptIn pins the security rule: an unset Resend pair
// without the explicit dev flag is a boot error, not a logged link.
func TestNewMailerRequiresOptIn(t *testing.T) {
	t.Setenv("AUTH_DEV_MAILER", "")
	_, err := newMailer(&platform.Config{})
	require.ErrorContains(t, err, "no mail path")
}

// recording collects drain events in call order.
type recording struct {
	mu     sync.Mutex
	events []string
}

func (r *recording) add(e string) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

// fakeHTTPServer blocks in ListenAndServe until Shutdown closes it — the
// contract the real server gives serve's select.
type fakeHTTPServer struct {
	rec  *recording
	die  chan struct{}
	once sync.Once
	fail error
}

func (f *fakeHTTPServer) ListenAndServe() error {
	if f.fail != nil {
		return f.fail
	}
	<-f.die
	return http.ErrServerClosed
}

// Shutdown records the drain event and releases the listener, matching the
// real server's unblock-on-Shutdown behavior.
func (f *fakeHTTPServer) Shutdown(context.Context) error {
	f.rec.add("http-shutdown")
	f.once.Do(func() { close(f.die) })
	return nil
}

// fakeJobs records which stop path ran.
type fakeJobs struct{ rec *recording }

func (f *fakeJobs) Stop(context.Context) error { f.rec.add("jobs-stop"); return nil }
func (f *fakeJobs) StopAndCancel(context.Context) error {
	f.rec.add("jobs-stopcancel")
	return nil
}

// TestServeDrainsInOrder pins the shutdown sequence: HTTP Shutdown, then
// the detached sends, then the workers — the ordering the lost-link fix
// requires.
func TestServeDrainsInOrder(t *testing.T) {
	rec := &recording{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	srv := &fakeHTTPServer{rec: rec, die: make(chan struct{})}
	detached := func(context.Context) bool { rec.add("detached"); return true }
	require.NoError(t, serve(ctx, srv, detached, &fakeJobs{rec: rec}))
	require.Equal(t, []string{"http-shutdown", "detached", "jobs-stop"}, rec.events)
}

// TestServeBindFailureStopsJobs pins the leak fix: a dead listener still
// stops the River client (hard — there is no drain to wait for).
func TestServeBindFailureStopsJobs(t *testing.T) {
	rec := &recording{}
	bindErr := errors.New("bind: address in use")
	srv := &fakeHTTPServer{rec: rec, die: make(chan struct{}), fail: bindErr}
	err := serve(context.Background(), srv, func(context.Context) bool { return true }, &fakeJobs{rec: rec})
	require.ErrorIs(t, err, bindErr)
	require.Equal(t, []string{"jobs-stopcancel"}, rec.events)
}

// TestServeServerClosedIsClean pins the graceful-exit contract: a listener
// that returns ErrServerClosed on its own means shutdown already happened.
func TestServeServerClosedIsClean(t *testing.T) {
	rec := &recording{}
	srv := &fakeHTTPServer{rec: rec, die: make(chan struct{}), fail: http.ErrServerClosed}
	require.NoError(t, serve(context.Background(), srv, func(context.Context) bool { return true }, &fakeJobs{rec: rec}))
	require.Empty(t, rec.events)
}
