package platform_test

// Correlation and the log diagnostics it feeds: the X-Request-Id
// reuse/generate contract, and the correlation_id attribute on the two
// warnings the middleware emits — writeError's write failure and the
// membership driver error. In its own file so middleware_test.go stays
// under the 500-line limit.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// logCapture is a slog.Handler that stores every record for later
// assertions; the middleware logs through slog.Default, so tests install
// it there. The mutex is belt-and-suspenders — ServeHTTP is synchronous in
// these tests, but -race must stay honest if that ever changes.
type logCapture struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (h *logCapture) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r.Clone())
	return nil
}

func (h *logCapture) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *logCapture) WithGroup(string) slog.Handler      { return h }

// find returns the first record carrying the given message.
func (h *logCapture) find(msg string) (slog.Record, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.recs {
		if r.Message == msg {
			return r, true
		}
	}
	return slog.Record{}, false
}

// swapLogger installs capture under slog.Default for the test and restores
// the previous logger on cleanup — the swap is process-global, so these
// tests never call t.Parallel.
func swapLogger(t *testing.T, capture *logCapture) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

// attr reads one attribute off a record; ok=false means it was never set,
// which the correlation_id assertions must distinguish from "present but
// empty" — the accessor's absent-middleware contract.
func attr(r slog.Record, key string) (string, bool) {
	var got string
	ok := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			got, ok = a.Value.String(), true
		}
		return !ok
	})
	return got, ok
}

// TestCorrelationEchoesValidID proves a well-formed inbound X-Request-Id
// is trimmed, reused, planted on the context and echoed on the response —
// client and logs then refer to the same request.
func TestCorrelationEchoesValidID(t *testing.T) {
	p := &probe{}
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("X-Request-Id", "  req-123-abc  ")
	rec := httptest.NewRecorder()
	platform.Correlation(p.handler()).ServeHTTP(rec, req)

	require.True(t, p.called)
	assert.Equal(t, "req-123-abc", rec.Header().Get("X-Request-Id"),
		"the trimmed inbound id must be echoed")
	assert.Equal(t, "req-123-abc", platform.CorrelationID(p.saw.Context()),
		"the same id must reach the handler's context")
}

// TestCorrelationGeneratesWhenAbsent proves a request without the header
// gets a fresh uuid, on the context and the response alike — and that a
// context Correlation never touched reads "".
func TestCorrelationGeneratesWhenAbsent(t *testing.T) {
	p := &probe{}
	rec := httptest.NewRecorder()
	platform.Correlation(p.handler()).ServeHTTP(
		rec, httptest.NewRequest(http.MethodGet, "/me", nil))

	require.True(t, p.called)
	got := rec.Header().Get("X-Request-Id")
	_, err := uuid.Parse(got)
	require.NoError(t, err, "a generated id must be a uuid")
	assert.Equal(t, got, platform.CorrelationID(p.saw.Context()))
	assert.Equal(t, "", platform.CorrelationID(context.Background()),
		"a context without the middleware must read empty")
}

// TestCorrelationRegeneratesMalformed proves the reuse gate: ids carrying
// control characters or past the length cap are never echoed — a fresh
// uuid takes their place so client bytes cannot reach a log line raw.
func TestCorrelationRegeneratesMalformed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"newline in value", "good-part\nforged-log-line"},
		{"nul byte", "abc\x00def"},
		{"del control char", "abc\x7f"},
		{"overlong", strings.Repeat("a", maxRequestIDLenForTest+1)},
		{"whitespace only", "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &probe{}
			req := httptest.NewRequest(http.MethodGet, "/me", nil)
			req.Header.Set("X-Request-Id", tc.value)
			rec := httptest.NewRecorder()
			platform.Correlation(p.handler()).ServeHTTP(rec, req)

			got := rec.Header().Get("X-Request-Id")
			_, err := uuid.Parse(got)
			require.NoError(t, err, "a malformed inbound id must be replaced, not echoed")
		})
	}
}

// maxRequestIDLenForTest mirrors the unexported cap the middleware applies
// to inbound ids; the overlong case pins the boundary it cannot name.
const maxRequestIDLenForTest = 128

// TestWriteErrorLogsCorrelationID proves the writeError warning carries
// the id Correlation planted: the attribute must exist — WarnContext does
// not surface ctx values on its own — and match the inbound header.
func TestWriteErrorLogsCorrelationID(t *testing.T) {
	capture := &logCapture{}
	swapLogger(t, capture)

	cfg := jwtConfig()
	cfg.WebURL = "https://web.quotepilot.test"
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("X-Request-Id", "cid-write-fail")
	platform.Correlation(platform.CheckOrigin(cfg)(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) { t.Fatal("handler must not run") },
	))).ServeHTTP(failWriter{}, req)

	logRec, ok := capture.find("platform: error response write failed")
	require.True(t, ok, "the write failure must be logged")
	cid, ok := attr(logRec, "correlation_id")
	require.True(t, ok, "correlation_id must be an attribute")
	assert.Equal(t, "cid-write-fail", cid)
	code, _ := attr(logRec, "code")
	assert.Equal(t, "forbidden", code)
}

// TestWriteErrorLogsEmptyCorrelationWithoutMiddleware proves the accessor
// contract under a mis-mounted chain: with no Correlation the attribute is
// still emitted — as "" — so log pipelines can rely on the key always
// being present.
func TestWriteErrorLogsEmptyCorrelationWithoutMiddleware(t *testing.T) {
	capture := &logCapture{}
	swapLogger(t, capture)

	cfg := jwtConfig()
	cfg.WebURL = "https://web.quotepilot.test"
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	platform.CheckOrigin(cfg)(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) { t.Fatal("handler must not run") },
	)).ServeHTTP(failWriter{}, req)

	logRec, ok := capture.find("platform: error response write failed")
	require.True(t, ok)
	cid, ok := attr(logRec, "correlation_id")
	require.True(t, ok, "the attribute must be present even with no middleware")
	assert.Equal(t, "", cid)
}

// TestMembershipLookupLogsError proves a get_membership driver error still
// answers the uniform 500 byte-for-byte but no longer vanishes: a warning
// carries the correlation id and the driver diagnostic — and nothing about
// the user or the tenant.
func TestMembershipLookupLogsError(t *testing.T) {
	capture := &logCapture{}
	swapLogger(t, capture)

	p := &probe{}
	store := platform.NewStore(errQuerier{})
	cfg := jwtConfig()
	chain := platform.Correlation(platform.Authenticate(cfg)(
		platform.ResolveTenant(store)(p.handler())))
	req := authedRequest(t, cfg, uuid.New(), http.MethodGet, "/tenant/route")
	req.Header.Set("X-Organization-Id", uuid.NewString())
	req.Header.Set("X-Request-Id", "cid-membership-fail")
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, `{"error":"internal"}`, rec.Body.String(),
		"the client-facing 500 stays byte-identical")
	assert.Equal(t, "cid-membership-fail", rec.Header().Get("X-Request-Id"))
	assert.False(t, p.called)

	logRec, ok := capture.find("platform: membership lookup failed")
	require.True(t, ok, "a driver error must produce a log line")
	cid, _ := attr(logRec, "correlation_id")
	assert.Equal(t, "cid-membership-fail", cid)
	errVal, _ := attr(logRec, "error")
	assert.Contains(t, errVal, "driver down")
}
