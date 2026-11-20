package resend

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// sendTimeout bounds the POST /emails call — a hung provider must not pin
// the send worker past River's job budget. Sends are small (one PDF at MVP
// scale), so 30 s is generous.
const sendTimeout = 30 * time.Second

// Sender is the seam the quote_send worker depends on: the real
// SendingClient in production, UnconfiguredSender when RESEND_API_KEY is
// absent, and FakeSender in tests.
type Sender interface {
	// Send posts one outbound email and returns the provider message id —
	// the value persisted on quotes.sent_outbound_message_id.
	Send(ctx context.Context, req SendRequest) (messageID string, err error)
}

// SendAttachment is one file on the outbound message; Content is the raw
// bytes — the client base64s them into the request payload.
type SendAttachment struct {
	Filename string
	Content  []byte
}

// SendRequest is everything a send carries: destination, the tenant's
// branding pair (From display name + Reply-To), the thread headers that
// keep the reply inside the customer's existing conversation, the PDF
// bytes and the idempotency key ({quote_id}:{version}, spec S5) that makes
// a retry after a successful send collapse at the provider.
type SendRequest struct {
	To             []string
	From           string
	ReplyTo        string
	Subject        string
	TextBody       string
	Attachments    []SendAttachment
	Headers        map[string]string
	IdempotencyKey string
}

// SendError reports a non-2xx Sending API answer or a client-side permanent
// failure (Client=true, e.g. the unconfigured stub or invalid input). The
// retry classification is FetchError's: client-side failures and 4xx — EXCEPT
// 408 and 429 — are permanent; everything else stays untyped and transient.
type SendError struct {
	Op         string // operation: "send_email"
	StatusCode int    // HTTP status when the API answered; 0 for client-side failures
	Detail     string
	Client     bool // true = permanent failure before/without an HTTP request
}

// Error implements error.
func (e *SendError) Error() string {
	if e.Client {
		return fmt.Sprintf("resend: %s: %s", e.Op, e.Detail)
	}
	return fmt.Sprintf("resend: %s: status %d: %s", e.Op, e.StatusCode, e.Detail)
}

// Permanent reports whether retrying cannot help — the same rule FetchError
// applies: 408 and 429 are the only retryable 4xx.
func (e *SendError) Permanent() bool {
	if e.Client {
		return true
	}
	if e.StatusCode == http.StatusRequestTimeout || e.StatusCode == http.StatusTooManyRequests {
		return false
	}
	return e.StatusCode >= 400 && e.StatusCode < 500
}

// SendingClient calls the Resend Sending API (POST /emails) over a bounded
// http.Client — the outbound twin of ReceivingClient.
type SendingClient struct {
	apiKey string
	client *http.Client
	// BaseURL defaults to receivingBaseURL (same API root); tests override
	// with httptest.
	BaseURL string
	// Transport replaces the RoundTripper when set — tests stub the network
	// deterministically; nil keeps the bounded default client.
	Transport http.RoundTripper
}

// NewSendingClient builds the production client.
//
//	sender := resend.NewSendingClient(cfg.ResendAPIKey)
func NewSendingClient(apiKey string) *SendingClient {
	return &SendingClient{
		apiKey:  apiKey,
		client:  &http.Client{Timeout: sendTimeout},
		BaseURL: receivingBaseURL,
	}
}

var _ Sender = (*SendingClient)(nil)

// httpClient mirrors ReceivingClient.httpClient: the bounded default, or
// one built on the stubbed Transport (timeout still applies).
func (c *SendingClient) httpClient() *http.Client {
	if c.Transport != nil {
		return &http.Client{Timeout: sendTimeout, Transport: c.Transport}
	}
	return c.client
}

// apiSendRequest is the POST /emails payload as documented: from/to/subject,
// text body, reply_to, arbitrary headers and base64 attachments.
type apiSendRequest struct {
	From        string            `json:"from"`
	To          []string          `json:"to"`
	Subject     string            `json:"subject"`
	Text        string            `json:"text,omitempty"`
	ReplyTo     string            `json:"reply_to,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Attachments []apiAttachment   `json:"attachments,omitempty"`
}

// apiAttachment carries one attachment's bytes base64-encoded, matching the
// documented "content" field.
type apiAttachment struct {
	Filename string `json:"filename"`
	Content  string `json:"content"`
}

// apiSendResponse is the documented send answer; only the id matters —
// it becomes quotes.sent_outbound_message_id.
type apiSendResponse struct {
	ID string `json:"id"`
}

// apiErrorBody is the error envelope; message carries the provider detail
// when present — never logged into tenant rows, it just annotates the error.
type apiErrorBody struct {
	Message string `json:"message"`
}

// Send posts the message. The Idempotency-Key header makes a provider-side
// replay of {quote_id}:{version} collapse onto the first send — the second
// half of the no-double-send guarantee, the first being the APPROVED_SENT
// status flip under FOR UPDATE (spec S5: one send per version).
func (c *SendingClient) Send(ctx context.Context, req SendRequest) (string, error) {
	payload := apiSendRequest{
		From:    req.From,
		To:      req.To,
		Subject: req.Subject,
		Text:    req.TextBody,
		ReplyTo: req.ReplyTo,
		Headers: req.Headers,
	}
	for _, a := range req.Attachments {
		payload.Attachments = append(payload.Attachments, apiAttachment{
			Filename: a.Filename,
			Content:  base64.StdEncoding.EncodeToString(a.Content),
		})
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", &SendError{Op: "send_email", Client: true, Detail: fmt.Sprintf("marshal request: %s", err)}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/emails", bytes.NewReader(body))
	if err != nil {
		return "", &SendError{Op: "send_email", Client: true, Detail: err.Error()}
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	if req.IdempotencyKey != "" {
		httpReq.Header.Set("Idempotency-Key", req.IdempotencyKey)
	}
	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("resend: send_email: %w", stripURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		detail := resp.Status
		var apiErr apiErrorBody
		if json.NewDecoder(resp.Body).Decode(&apiErr) == nil && apiErr.Message != "" {
			detail = apiErr.Message
		} else {
			_, _ = io.Copy(io.Discard, resp.Body)
		}
		return "", &SendError{Op: "send_email", StatusCode: resp.StatusCode, Detail: detail}
	}
	var out apiSendResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("resend: send_email: decode response: %w", err)
	}
	if strings.TrimSpace(out.ID) == "" {
		// An incomplete 2xx receipt is NOT deterministic: the provider may
		// have accepted the send and trimmed the body, so cancelling here
		// would kill a delivery an idempotent retry recovers — the retry
		// carries the same Idempotency-Key and the provider returns the
		// recorded send's id. Untyped error → transient → River retries.
		return "", fmt.Errorf("resend: send_email: 2xx response carries no message id")
	}
	return out.ID, nil
}

// UnconfiguredSender is the fail-soft binding used when RESEND_API_KEY is
// absent (same contract as UnconfiguredReceiver): the server still boots and
// every send fails loudly as a permanent SendError, so quote_send cancels —
// no credential can appear mid-retry.
type UnconfiguredSender struct{}

var _ Sender = UnconfiguredSender{}

// Send reports the missing configuration as permanent.
func (UnconfiguredSender) Send(context.Context, SendRequest) (string, error) {
	return "", &SendError{Op: "send_email", Client: true,
		Detail: "sending client not configured — RESEND_API_KEY is unset"}
}

// FakeSender is the dev/test implementation of Sender: it captures every
// SendRequest so worker tests can assert exactly what would have been mailed
// — named, not an inline stub, per the repo's fake-class rule. MessageID is
// the canned provider id; Err fails the call deterministically.
type FakeSender struct {
	mu        sync.Mutex
	MessageID string
	Err       error
	requests  []SendRequest
}

var _ Sender = (*FakeSender)(nil)

// Send records the request and answers the canned result.
func (f *FakeSender) Send(_ context.Context, req SendRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if f.Err != nil {
		return "", f.Err
	}
	return f.MessageID, nil
}

// Requests returns a copy of every recorded request, in call order.
func (f *FakeSender) Requests() []SendRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]SendRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

// Calls reports how many sends were attempted — the no-double-send assertion.
func (f *FakeSender) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}
