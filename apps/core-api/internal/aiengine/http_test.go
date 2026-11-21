package aiengine_test

// Unit tests for the provisional HTTP adapter: endpoints, auth header,
// request shapes and the transient/violation error classification the S2
// worker's retry budget depends on (spec section 3).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/aiengine"
)

// fakeEngineServer records the request (method, path, auth headers,
// decoded body) and replies with a canned status + JSON body.
type fakeEngineServer struct {
	*httptest.Server
	lastPath       string
	lastServiceKey string
	lastAuthz      string
	lastBody       map[string]any
	status         int
	resp           any
	rawResp        string
}

func newFakeEngine(t *testing.T, status int, resp any) *fakeEngineServer {
	t.Helper()
	f := &fakeEngineServer{status: status, resp: resp}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastPath = r.URL.Path
		f.lastServiceKey = r.Header.Get("X-Internal-Service-Key")
		f.lastAuthz = r.Header.Get("Authorization")
		f.lastBody = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&f.lastBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		if f.rawResp != "" {
			_, _ = w.Write([]byte(f.rawResp))
			return
		}
		_ = json.NewEncoder(w).Encode(f.resp)
	}))
	t.Cleanup(f.Close)
	return f
}

func newClient(srv *fakeEngineServer) *aiengine.HTTPClient {
	return aiengine.NewHTTPClient(srv.URL, "test-token")
}

func TestHTTPClassify(t *testing.T) {
	srv := newFakeEngine(t, 200, map[string]any{
		"label":         "RFQ",
		"probabilities": map[string]any{"RFQ": 0.9, "TARIFF_UPDATE": 0.05, "OTHER": 0.05},
		"model":         "jev-1",
	})
	c := newClient(srv)
	res, err := c.Classify(context.Background(), aiengine.ClassifyRequest{
		CorrelationID: uuid.New(), Subject: "RFQ 2x40HQ", Sender: "a@b.c",
		BodyText:        "please quote",
		AttachmentMetas: []aiengine.AttachmentMeta{{Filename: "a.pdf", ContentType: "application/pdf"}},
	})
	require.NoError(t, err)
	assert.Equal(t, "RFQ", res.Label)
	assert.Equal(t, "/v1/classify", srv.lastPath)
	assert.Equal(t, "test-token", srv.lastServiceKey,
		"auth travels as X-Internal-Service-Key (ADR-005 §10)")
	assert.Empty(t, srv.lastAuthz, "no Authorization header is sent")
	assert.Equal(t, "a@b.c", srv.lastBody["sender"])
	_, hasHTML := srv.lastBody["body_html"]
	assert.False(t, hasHTML, "the contract carries no body_html — core converts before sending")
	metas, _ := srv.lastBody["attachment_metas"].([]any)
	require.Len(t, metas, 1)
	assert.Equal(t, "a.pdf", metas[0].(map[string]any)["filename"])
}

// TestHTTPClassifyProbabilitiesValidated: the adapter validates the
// probabilities before handing the result up — a missing key or an
// out-of-range value is a contract violation, never a routing decision.
func TestHTTPClassifyProbabilitiesValidated(t *testing.T) {
	cases := map[string]map[string]any{
		"missing_key":       {"label": "RFQ", "probabilities": map[string]any{"RFQ": 0.9, "OTHER": 0.1}},
		"out_of_range":      {"label": "RFQ", "probabilities": map[string]any{"RFQ": 1.5, "TARIFF_UPDATE": 0.0, "OTHER": 0.0}},
		"nil_probabilities": {"label": "RFQ"},
	}
	for name, resp := range cases {
		srv := newFakeEngine(t, 200, resp)
		c := newClient(srv)
		_, err := c.Classify(context.Background(), aiengine.ClassifyRequest{})
		require.Error(t, err, name)
		assert.True(t, aiengine.IsContractViolation(err), "%s must be a contract violation", name)
		assert.False(t, aiengine.IsTransient(err), "%s must not retry forever", name)
	}
}

// TestHTTPExtract: the wire shape carries {filename, markdown} objects —
// the real filename is what evidence cites — plus the classification the
// engine echoes in the ExtractionResult.
func TestHTTPExtract(t *testing.T) {
	srv := newFakeEngine(t, 200, nil)
	srv.rawResp = `{"contract_version":"1.0.0","fields":{}}`
	c := newClient(srv)
	raw, err := c.Extract(context.Background(), aiengine.ExtractRequest{
		CorrelationID: uuid.New(), Subject: "s", Sender: "a@b.c",
		ParsedMarkdowns: []aiengine.ParsedMarkdown{{Filename: "rates.xlsx", Markdown: "# md"}},
		Classification: aiengine.ClassifyResult{
			Label:         "RFQ",
			Probabilities: map[string]float64{"RFQ": 0.9, "TARIFF_UPDATE": 0.05, "OTHER": 0.05},
			Model:         "jev-1",
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "/v1/extract", srv.lastPath)
	assert.JSONEq(t, `{"contract_version":"1.0.0","fields":{}}`, string(raw))
	mds, _ := srv.lastBody["parsed_markdown"].([]any)
	require.Len(t, mds, 1)
	assert.Equal(t, "rates.xlsx", mds[0].(map[string]any)["filename"])
	assert.Equal(t, "# md", mds[0].(map[string]any)["markdown"])
	cls, _ := srv.lastBody["classification"].(map[string]any)
	require.NotNil(t, cls, "the extract request must carry the classify result")
	assert.Equal(t, "RFQ", cls["label"])
	assert.Equal(t, "jev-1", cls["model"])
	_, hasHTML := srv.lastBody["body_html"]
	assert.False(t, hasHTML)
}

func TestHTTPParseAttachment(t *testing.T) {
	srv := newFakeEngine(t, 200, map[string]any{"parsed_markdown": "# table"})
	c := newClient(srv)
	md, err := c.ParseAttachment(context.Background(), aiengine.ParseAttachmentRequest{
		CorrelationID: uuid.New(), Filename: "a.pdf", ContentType: "application/pdf",
		Bytes: []byte("PDF-DATA"),
	})
	require.NoError(t, err)
	assert.Equal(t, "# table", md)
	assert.Equal(t, "/v1/attachments/parse", srv.lastPath)
	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte("PDF-DATA")), srv.lastBody["bytes_b64"])
}

// TestHTTPErrorClassification pins the spec's retry rule per status:
// transient retries, the rest is a contract violation (one retry max).
func TestHTTPErrorClassification(t *testing.T) {
	cases := []struct {
		status    int
		transient bool
	}{
		{408, true}, {425, true}, {429, true}, {500, true}, {503, true},
		{400, false}, {404, false}, {422, false},
	}
	for _, tc := range cases {
		srv := newFakeEngine(t, tc.status, map[string]any{"error": "x"})
		c := newClient(srv)
		_, err := c.Classify(context.Background(), aiengine.ClassifyRequest{})
		require.Error(t, err, "status %d", tc.status)
		var ee *aiengine.EngineError
		require.True(t, errors.As(err, &ee))
		assert.Equal(t, tc.status, ee.StatusCode)
		assert.Equal(t, tc.transient, aiengine.IsTransient(err),
			"status %d transient classification", tc.status)
	}
}

// TestHTTPTransportErrorIsTransient: a connection-level failure is always
// transient — the retry is exactly what it needs.
func TestHTTPTransportErrorIsTransient(t *testing.T) {
	c := aiengine.NewHTTPClient("http://127.0.0.1:1", "tok")
	_, err := c.Classify(context.Background(), aiengine.ClassifyRequest{})
	require.Error(t, err)
	assert.True(t, aiengine.IsTransient(err))
}

// TestHTTPErrorDetailCarriesBody: a non-2xx response's body is kept (up
// to a bound) in the error Detail — a 422's {"detail": ...} is exactly
// what the INVALID_EXTRACTION audit row should record.
func TestHTTPErrorDetailCarriesBody(t *testing.T) {
	srv := newFakeEngine(t, 422, nil)
	srv.rawResp = `{"detail":"body_text exceeds the 16KB limit"}`
	c := newClient(srv)
	_, err := c.Classify(context.Background(), aiengine.ClassifyRequest{})
	require.Error(t, err)
	var ee *aiengine.EngineError
	require.True(t, errors.As(err, &ee))
	assert.Equal(t, 422, ee.StatusCode)
	assert.Contains(t, ee.Detail, "422", "the HTTP status stays in the detail")
	assert.Contains(t, ee.Detail, "body_text exceeds the 16KB limit",
		"the response body is appended for audit")
}

// TestHTTPErrorDetailBodyBounded: the snippet is bounded — a huge error
// page must not blow up the audit payload or an error string.
func TestHTTPErrorDetailBodyBounded(t *testing.T) {
	srv := newFakeEngine(t, 500, nil)
	srv.rawResp = strings.Repeat("x", 10000)
	c := newClient(srv)
	_, err := c.Classify(context.Background(), aiengine.ClassifyRequest{})
	require.Error(t, err)
	var ee *aiengine.EngineError
	require.True(t, errors.As(err, &ee))
	assert.Less(t, len(ee.Detail), 1024, "the snippet stays bounded")
}

// TestHTTPInvalidBodyIsViolation: a schema-invalid body is a contract
// violation, never a transient retry.
func TestHTTPInvalidBodyIsViolation(t *testing.T) {
	srv := newFakeEngine(t, 200, nil)
	srv.rawResp = `not json at all`
	c := newClient(srv)
	_, err := c.Classify(context.Background(), aiengine.ClassifyRequest{})
	require.Error(t, err)
	assert.False(t, aiengine.IsTransient(err))
	assert.True(t, aiengine.IsContractViolation(err))
}

// TestUnconfiguredClient: without AI_ENGINE_URL every call is a loud,
// non-transient failure — never a retry loop on a missing credential.
func TestUnconfiguredClient(t *testing.T) {
	var c aiengine.Client = aiengine.UnconfiguredClient{}
	_, err := c.Classify(context.Background(), aiengine.ClassifyRequest{})
	require.Error(t, err)
	assert.False(t, aiengine.IsTransient(err))
}
