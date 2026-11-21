package aiengine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// httpTimeout bounds a single ai-engine call; the S2 job's own 120 s
// timeout still governs the whole attempt.
const httpTimeout = 60 * time.Second

// errSnippetLimit bounds the non-2xx body kept in EngineError.Detail — a
// 422's {"detail": ...} is exactly what the INVALID_EXTRACTION audit row
// should record, but a huge error page must not blow up the payload.
const errSnippetLimit = 512

// HTTPClient calls ai-engine over HTTP: Bearer auth on
// AI_ENGINE_SERVICE_TOKEN, JSON in/out. The three endpoints are the
// provisional transport contract (packages/contracts/openapi/ai-engine.yaml).
type HTTPClient struct {
	baseURL string
	token   string
	client  *http.Client
	// Transport replaces the RoundTripper when set — tests stub the network
	// deterministically; nil keeps the bounded default client.
	Transport http.RoundTripper
}

// NewHTTPClient builds the production adapter.
//
//	engine := aiengine.NewHTTPClient(cfg.AIEngineURL, cfg.AIEngineToken)
func NewHTTPClient(baseURL, token string) *HTTPClient {
	return &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  &http.Client{Timeout: httpTimeout},
	}
}

var _ Client = (*HTTPClient)(nil)

// httpClient returns the bounded default client, or one built on the
// stubbed Transport when a test sets it — the timeout still applies there.
func (c *HTTPClient) httpClient() *http.Client {
	if c.Transport != nil {
		return &http.Client{Timeout: httpTimeout, Transport: c.Transport}
	}
	return c.client
}

// classifyResponse mirrors POST /v1/classify's body.
type classifyResponse struct {
	Label         string             `json:"label"`
	Probabilities map[string]float64 `json:"probabilities"`
	Model         string             `json:"model"`
}

// Classify calls POST /v1/classify and validates the response before
// handing it up — a missing label or an incomplete/out-of-range
// probabilities map is a contract violation, never a routing decision.
func (c *HTTPClient) Classify(ctx context.Context, req ClassifyRequest) (*ClassifyResult, error) {
	var out classifyResponse
	if err := c.post(ctx, "classify", "/v1/classify", req, &out); err != nil {
		return nil, err
	}
	if out.Label == "" {
		return nil, &EngineError{Op: "classify", Contract: true, Detail: "response is missing label"}
	}
	if err := checkProbabilities(out.Probabilities); err != nil {
		return nil, &EngineError{Op: "classify", Contract: true, Detail: err.Error()}
	}
	return &ClassifyResult{Label: out.Label, Probabilities: out.Probabilities, Model: out.Model}, nil
}

// checkProbabilities requires the three contract keys, each in [0,1] —
// the audit row stores them, so garbage must not reach it.
func checkProbabilities(p map[string]float64) error {
	for _, k := range []string{"RFQ", "TARIFF_UPDATE", "OTHER"} {
		v, ok := p[k]
		if !ok {
			return fmt.Errorf("probabilities.%s is missing", k)
		}
		if v < 0 || v > 1 {
			return fmt.Errorf("probabilities.%s %v is outside [0,1]", k, v)
		}
	}
	return nil
}

// Extract calls POST /v1/extract and returns the RAW body — validating it
// is the caller's job (ValidateExtraction, rule 6).
func (c *HTTPClient) Extract(ctx context.Context, req ExtractRequest) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.post(ctx, "extract", "/v1/extract", req, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// parseWire is the provisional wire shape for POST /v1/attachments/parse —
// bytes travel base64-encoded inside JSON.
type parseWire struct {
	CorrelationID string `json:"correlation_id"`
	Filename      string `json:"filename"`
	ContentType   string `json:"content_type"`
	BytesB64      string `json:"bytes_b64"`
}

// parseResponse mirrors the parse endpoint's body.
type parseResponse struct {
	ParsedMarkdown string `json:"parsed_markdown"`
}

// ParseAttachment calls POST /v1/attachments/parse.
func (c *HTTPClient) ParseAttachment(ctx context.Context, req ParseAttachmentRequest) (string, error) {
	wire := parseWire{
		CorrelationID: req.CorrelationID.String(),
		Filename:      req.Filename,
		ContentType:   req.ContentType,
		BytesB64:      base64.StdEncoding.EncodeToString(req.Bytes),
	}
	var out parseResponse
	if err := c.post(ctx, "parse_attachment", "/v1/attachments/parse", wire, &out); err != nil {
		return "", err
	}
	return out.ParsedMarkdown, nil
}

// post issues an authenticated JSON POST and decodes the body into out.
// Non-2xx statuses become EngineError with the HTTP status so Transient()
// can classify them; transport failures stay transient EngineErrors.
func (c *HTTPClient) post(ctx context.Context, op, path string, in any, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return &EngineError{Op: op, Detail: fmt.Sprintf("marshal request: %s", err)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return &EngineError{Op: op, Detail: fmt.Sprintf("build request: %s", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return &EngineError{Op: op, Detail: stripURLError(err).Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return &EngineError{Op: op, StatusCode: resp.StatusCode, Detail: errDetail(resp)}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return &EngineError{Op: op, Contract: true, Detail: fmt.Sprintf("decode response: %s", err)}
	}
	return nil
}

// errDetail builds the EngineError detail for a non-2xx response: the
// HTTP status plus a bounded, whitespace-trimmed snippet of the body —
// ai-engine's own {"detail": ...} explanation is what an audit row needs.
func errDetail(resp *http.Response) string {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, errSnippetLimit))
	detail := resp.Status
	if s := strings.TrimSpace(string(snippet)); s != "" {
		detail += ": " + s
	}
	return detail
}

// stripURLError removes the *url.Error wrapper so a request URL never
// leaks into an error string (same rule as resend).
func stripURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Unwrap()
	}
	return err
}
