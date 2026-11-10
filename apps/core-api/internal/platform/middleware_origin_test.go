package platform_test

// CheckOrigin and the writeError failure path: the cross-origin denial
// matrix of ADR-005 section 4, kept in its own file to stay under the
// 500-line limit.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// CheckOrigin is ADR-005 section 4's Origin check: a cross-site unsafe
// request is denied with the middleware's own 403 body, safe methods and
// non-browser requests pass, and the configured WebURL survives the
// sibling-host comparison the stdlib performs.
func TestCheckOrigin(t *testing.T) {
	cfg := jwtConfig()
	cfg.WebURL = "https://web.quotepilot.test"
	p := &probe{}
	handler := platform.CheckOrigin(cfg)(p.handler())
	for _, tc := range originCases {
		t.Run(tc.name, func(t *testing.T) { runOriginCase(t, handler, p, tc) })
	}
}

type originCase struct {
	name     string
	method   string
	headers  map[string]string
	wantCode int
	wantCall bool
}

var originCases = []originCase{
	{"cross-site fetch on unsafe method is denied",
		http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden, false},
	{"cross-site fetch on safe method passes",
		http.MethodGet, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusOK, true},
	{"same-site without a trusted origin is still cross-origin",
		http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden, false},
	{"same-site fetch with the trusted WebURL origin passes",
		http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://web.quotepilot.test"}, http.StatusOK, true},
	{"trusted WebURL origin passes",
		http.MethodPost, map[string]string{"Origin": "https://web.quotepilot.test"}, http.StatusOK, true},
	{"untrusted origin denied",
		http.MethodPost, map[string]string{"Origin": "https://evil.test"}, http.StatusForbidden, false},
	{"no fetch or origin headers treated as non-browser",
		http.MethodPost, nil, http.StatusOK, true},
}

func runOriginCase(t *testing.T, handler http.Handler, p *probe, tc originCase) {
	t.Helper()
	p.called = false
	req := httptest.NewRequest(tc.method, "/tenant/route", nil)
	for k, v := range tc.headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, tc.wantCode, rec.Code)
	assert.Equal(t, tc.wantCall, p.called)
	if tc.wantCode == http.StatusForbidden {
		assert.Equal(t, `{"error":"forbidden"}`, rec.Body.String())
	}
}

// A WEB_URL carrying a path — a legitimate deployment shape — must still
// register: only its origin is trusted, the path is dropped.
func TestCheckOriginWebURLWithPath(t *testing.T) {
	cfg := jwtConfig()
	cfg.WebURL = "https://web.quotepilot.test/app"
	p := &probe{}
	handler := platform.CheckOrigin(cfg)(p.handler())
	req := httptest.NewRequest(http.MethodPost, "/tenant/route", nil)
	req.Header.Set("Origin", "https://web.quotepilot.test")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, p.called)
}

// A denied response that cannot reach the wire must not panic — the write
// failure is logged and dropped.
func TestWriteErrorWriteFailure(t *testing.T) {
	cfg := jwtConfig()
	cfg.WebURL = "https://web.quotepilot.test"
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	platform.CheckOrigin(cfg)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler must not run")
	})).ServeHTTP(failWriter{}, req)
}

type failWriter struct{}

func (failWriter) Header() http.Header       { return http.Header{} }
func (failWriter) WriteHeader(int)           {}
func (failWriter) Write([]byte) (int, error) { return 0, errDriver }
