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

// TestHTTPSearchCarrierOffer: the S3a wire shape — correlation_id plus
// the normalized request, auth header, the raw offer handed back
// unvalidated (caller bounds it, rule 6), and a null offer decoded as
// the no-offer shape.
func TestHTTPSearchCarrierOffer(t *testing.T) {
	srv := newFakeEngine(t, 200, nil)
	srv.rawResp = `{"offer":{"carrier":"CMA_CGM","charges":[{"category":"OCEAN_FREIGHT","code":"OCEAN","amount":900,"currency":"USD","equipment_or_unit":"40HC"}],"expires_at":"2026-04-01T00:00:00Z"},"offer_reference":"CMA-123"}`
	c := newClient(srv)
	res, err := c.SearchCarrierOffer(context.Background(), aiengine.CarrierOfferSearchRequest{
		CorrelationID: uuid.New(), OriginLOCODE: "CNSHA", DestLOCODE: "BRSSZ",
		EquipmentOrUnit: "40HC", Quantity: 2, ReadyDate: "2026-03-15",
	})
	require.NoError(t, err)
	assert.Equal(t, "/v1/carrier-offers/search", srv.lastPath)
	assert.Equal(t, "test-token", srv.lastServiceKey)
	assert.Equal(t, "CNSHA", srv.lastBody["origin_locode"])
	assert.Equal(t, float64(2), srv.lastBody["quantity"])
	assert.Equal(t, "2026-03-15", srv.lastBody["ready_date"])
	_, hasOrg := srv.lastBody["organization_id"]
	assert.False(t, hasOrg, "organization_id never crosses the wire (rule 2)")
	assert.Contains(t, string(res.Offer), `"carrier":"CMA_CGM"`)
	require.NotNil(t, res.OfferReference)
	assert.Equal(t, "CMA-123", *res.OfferReference)
}

// TestHTTPSearchCarrierOfferNoOffer: the wire's null offer decodes to
// the NO_OFFER shape; an absent offer_reference decodes nil.
func TestHTTPSearchCarrierOfferNoOffer(t *testing.T) {
	srv := newFakeEngine(t, 200, nil)
	srv.rawResp = `{"offer":null,"offer_reference":null}`
	c := newClient(srv)
	res, err := c.SearchCarrierOffer(context.Background(), aiengine.CarrierOfferSearchRequest{})
	require.NoError(t, err)
	assert.Equal(t, "null", string(res.Offer))
	assert.Nil(t, res.OfferReference)
}

// TestHTTPSearchCarrierOfferErrors: the shared taxonomy applies —
// transient statuses retry, a schema-invalid body is a contract
// violation, and the unconfigured stub fails loudly.
func TestHTTPSearchCarrierOfferErrors(t *testing.T) {
	srv := newFakeEngine(t, 503, map[string]any{"error": "x"})
	c := newClient(srv)
	_, err := c.SearchCarrierOffer(context.Background(), aiengine.CarrierOfferSearchRequest{})
	require.Error(t, err)
	assert.True(t, aiengine.IsTransient(err))

	srv = newFakeEngine(t, 200, nil)
	srv.rawResp = `not json`
	c = newClient(srv)
	_, err = c.SearchCarrierOffer(context.Background(), aiengine.CarrierOfferSearchRequest{})
	require.Error(t, err)
	assert.True(t, aiengine.IsContractViolation(err))

	_, err = aiengine.UnconfiguredClient{}.SearchCarrierOffer(context.Background(), aiengine.CarrierOfferSearchRequest{})
	require.Error(t, err)
	assert.True(t, aiengine.IsContractViolation(err),
		"the unconfigured binding fails loudly, never transient")
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

// TestHTTPRenderQuote: the S4 wire shape — correlation_id plus the
// quote/branding/org blocks, auth header, no organization_id (rule 2) —
// and the response decoded to bytes only after the %PDF check passes.
func TestHTTPRenderQuote(t *testing.T) {
	srv := newFakeEngine(t, 200, nil)
	srv.rawResp = `{"pdf_b64":"JVBERi0xLjcK","email_html":"<p>hi</p>"}`
	c := newClient(srv)
	res, err := c.RenderQuote(context.Background(), aiengine.RenderRequest{
		CorrelationID: uuid.New(),
		Quote: aiengine.RenderQuoteData{
			QuoteNumber: "ACME-2026-0042", Version: 1, QuoteDate: "2026-03-15",
			Modal: "FCL", Direction: "IMPORT", Incoterm: "FOB",
			OriginLOCODE: "CNSHA", DestLOCODE: "BRSSZ", EquipmentOrUnit: "40HC",
			Lines:       []aiengine.RenderLine{{Category: "INTL_FREIGHT"}},
			Warnings:    []string{},
			Assumptions: []string{},
		},
		Branding: json.RawMessage(`{"primary_color":"#0047FF"}`),
		Org:      aiengine.RenderOrg{Name: "Acme", FromName: "Acme Ops", ReplyTo: "ops@acme.test"},
	})
	require.NoError(t, err)
	assert.Equal(t, "/v1/render/quote", srv.lastPath)
	assert.Equal(t, "test-token", srv.lastServiceKey)
	assert.Equal(t, []byte("%PDF-1.7\n"), res.PDF, "pdf_b64 is decoded for the caller")
	assert.Equal(t, "<p>hi</p>", res.EmailHTML)
	_, hasOrg := srv.lastBody["organization_id"]
	assert.False(t, hasOrg, "organization_id never crosses the wire (rule 2)")
	quote, _ := srv.lastBody["quote"].(map[string]any)
	require.NotNil(t, quote)
	assert.Equal(t, "ACME-2026-0042", quote["quote_number"])
	assert.Equal(t, "IMPORT", quote["direction"])
	assert.Equal(t, map[string]any{"primary_color": "#0047FF"}, srv.lastBody["branding"],
		"branding_json travels verbatim")
	org, _ := srv.lastBody["org"].(map[string]any)
	assert.Equal(t, "ops@acme.test", org["reply_to"])
}

// TestHTTPRenderQuotePayloadValidated: the adapter's rule-6 check —
// pdf_b64 that fails base64, magic or the size bound is a contract
// violation, never a stored artifact.
func TestHTTPRenderQuotePayloadValidated(t *testing.T) {
	oversize := base64.StdEncoding.EncodeToString(
		append([]byte("%PDF"), make([]byte, 16<<20)...))
	cases := map[string]string{
		"bad_base64":  `{"pdf_b64":"!!!","email_html":""}`,
		"not_pdf":     `{"pdf_b64":"aGVsbG8=","email_html":""}`,
		"empty_pdf":   `{"pdf_b64":"","email_html":""}`,
		"oversize":    `{"pdf_b64":"` + oversize + `"}`,
		"missing_key": `{"email_html":"<p>x</p>"}`,
	}
	for name, raw := range cases {
		srv := newFakeEngine(t, 200, nil)
		srv.rawResp = raw
		c := newClient(srv)
		_, err := c.RenderQuote(context.Background(), aiengine.RenderRequest{})
		require.Error(t, err, name)
		assert.True(t, aiengine.IsContractViolation(err), "%s must be a contract violation", name)
		assert.False(t, aiengine.IsTransient(err), "%s must not be retried", name)
	}
}

// TestHTTPRenderQuoteErrors: the shared taxonomy applies — transient
// statuses retry, a schema-invalid body violates the contract, and the
// unconfigured stub fails loudly.
func TestHTTPRenderQuoteErrors(t *testing.T) {
	srv := newFakeEngine(t, 503, map[string]any{"error": "x"})
	c := newClient(srv)
	_, err := c.RenderQuote(context.Background(), aiengine.RenderRequest{})
	require.Error(t, err)
	assert.True(t, aiengine.IsTransient(err))

	srv = newFakeEngine(t, 200, nil)
	srv.rawResp = `not json`
	c = newClient(srv)
	_, err = c.RenderQuote(context.Background(), aiengine.RenderRequest{})
	require.Error(t, err)
	assert.True(t, aiengine.IsContractViolation(err))

	_, err = aiengine.UnconfiguredClient{}.RenderQuote(context.Background(), aiengine.RenderRequest{})
	require.Error(t, err)
	assert.True(t, aiengine.IsContractViolation(err),
		"the unconfigured binding fails loudly, never transient")
}
