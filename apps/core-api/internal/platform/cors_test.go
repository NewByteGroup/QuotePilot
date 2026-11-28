package platform_test

// CORS: the browser preflight layer mounted outermost on the mux. The
// trusted origin gets a full 204 allow answer or response headers;
// everything else — no Origin, a hostile one, a non-preflight OPTIONS —
// passes through with nothing stamped. Own file for the same reason as
// the CheckOrigin matrix in middleware_origin_test.go.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// corsOrigin is the trusted origin the fixture WebURL reduces to.
const corsOrigin = "https://app.example.test"

func TestCORS(t *testing.T) {
	cfg := &platform.Config{WebURL: corsOrigin}
	p := &probe{}
	handler := platform.CORS(cfg)(p.handler())
	for _, tc := range corsCases {
		t.Run(tc.name, func(t *testing.T) { runCORSCase(t, handler, p, tc) })
	}
}

type corsCase struct {
	name        string
	method      string
	headers     map[string]string
	wantCode    int
	wantCall    bool
	wantHeaders map[string]string
	wantNoCORS  bool
}

var corsCases = []corsCase{
	{"trusted preflight answers 204 with the full allow set",
		http.MethodOptions,
		map[string]string{
			"Origin":                        corsOrigin,
			"Access-Control-Request-Method": "POST",
		},
		http.StatusNoContent, false,
		map[string]string{
			"Access-Control-Allow-Origin":      corsOrigin,
			"Access-Control-Allow-Credentials": "true",
			"Access-Control-Allow-Methods":     "GET, POST, PATCH, PUT, DELETE, OPTIONS",
			"Access-Control-Allow-Headers":     "Content-Type, " + platform.OrgHeader,
			"Access-Control-Max-Age":           "7200",
			"Vary":                             "Origin",
		}, false},
	{"untrusted preflight passes through untouched",
		http.MethodOptions,
		map[string]string{
			"Origin":                        "https://evil.example",
			"Access-Control-Request-Method": "POST",
		},
		http.StatusOK, true, nil, true},
	{"trusted origin on a normal request gets response headers",
		http.MethodGet,
		map[string]string{"Origin": corsOrigin},
		http.StatusOK, true,
		map[string]string{
			"Access-Control-Allow-Origin":      corsOrigin,
			"Access-Control-Allow-Credentials": "true",
			"Vary":                             "Origin",
		}, false},
	{"request without an Origin passes through untouched",
		http.MethodGet, nil,
		http.StatusOK, true, nil, true},
	// Not a preflight: an OPTIONS that declares no requested method is an
	// ordinary request — it reaches the handler, still stamped because the
	// Origin itself is trusted.
	{"OPTIONS without a requested method is not a preflight",
		http.MethodOptions,
		map[string]string{"Origin": corsOrigin},
		http.StatusOK, true,
		map[string]string{
			"Access-Control-Allow-Origin":      corsOrigin,
			"Access-Control-Allow-Credentials": "true",
			"Vary":                             "Origin",
		}, false},
}

func runCORSCase(t *testing.T, handler http.Handler, p *probe, tc corsCase) {
	t.Helper()
	p.called = false
	req := httptest.NewRequest(tc.method, "/v1/inbox/quotes", nil)
	for k, v := range tc.headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, tc.wantCode, rec.Code)
	assert.Equal(t, tc.wantCall, p.called)
	for k, v := range tc.wantHeaders {
		assert.Equal(t, v, rec.Header().Get(k), "header %s", k)
	}
	if tc.wantNoCORS {
		assertNoCORSHeaders(t, rec.Header())
	}
}

// assertNoCORSHeaders proves the pass-through path stamped nothing — an
// untrusted origin must never find its value reflected in an allow header.
func assertNoCORSHeaders(t *testing.T, h http.Header) {
	t.Helper()
	for name := range h {
		assert.NotContains(t, name, "Access-Control", "header %s must not be set", name)
	}
}
