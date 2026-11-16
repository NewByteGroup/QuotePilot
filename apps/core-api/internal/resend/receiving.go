// Package resend holds the Resend API clients used by the pipeline workers.
// The Receiving API (GET /emails/receiving/{id} plus the attachments list)
// is the only way to fetch the full body, headers and attachment download
// URLs that the metadata-only email.received webhook omits — which is why
// the fetch lives in the rfq_ingest job, not the webhook handler (spec S1).
// Field names follow resend's documented response: from, message_id
// (RFC 5322), headers map, text/html bodies, authentication{spf,dkim,dmarc},
// and attachments carrying download_url from the attachments-list endpoint.
package resend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// receivingBaseURL is the production API root; BaseURL on the client lets
// tests point at httptest instead.
const receivingBaseURL = "https://api.resend.com"

// receivingTimeout bounds every Receiving call (metadata, attachment list
// and downloads alike) so a hung provider cannot stall the ingest worker
// past River's job budget. Downloads share it: attachments at MVP scale are
// a few MB.
const receivingTimeout = 90 * time.Second

// maxAttachmentBytes caps one downloaded attachment so a hostile or
// corrupted payload cannot exhaust memory — the bytes are held whole for
// the storage Put.
const maxAttachmentBytes = 64 << 20 // 64 MiB

// Receiver is the seam the rfq_ingest worker depends on: the real
// ReceivingClient in production, UnconfiguredReceiver when RESEND_API_KEY
// is absent, and a fake in tests.
type Receiver interface {
	// GetInboundEmail returns the full received email for one Resend email
	// id — bodies, headers and attachments enriched with download URLs.
	GetInboundEmail(ctx context.Context, emailID string) (*InboundEmail, error)
	// DownloadAttachment fetches attachment bytes from a signed download_url.
	DownloadAttachment(ctx context.Context, downloadURL string) ([]byte, error)
}

// AuthenticationResult is the receiving server's SPF/DKIM/DMARC verdict —
// computed by Resend's mail server, so it cannot be forged by the sender.
// It is null for emails received before the field existed; nil is expected.
type AuthenticationResult struct {
	SPF   string `json:"spf"`
	DKIM  string `json:"dkim"`
	DMARC string `json:"dmarc"`
}

// InboundAttachment is one attachment's metadata plus the short-lived
// (~1h) signed download_url filled in from the attachments-list endpoint.
type InboundAttachment struct {
	ID                 string `json:"id"`
	Filename           string `json:"filename"`
	ContentType        string `json:"content_type"`
	ContentDisposition string `json:"content_disposition"`
	ContentID          string `json:"content_id"`
	Size               int64  `json:"size"`
	DownloadURL        string `json:"download_url"`
	ExpiresAt          string `json:"expires_at"`
}

// InboundEmail is the full fetched message: the fields rfq_requests persists
// plus the attachment list.
type InboundEmail struct {
	ID             string                `json:"id"`
	From           string                `json:"from"`
	To             []string              `json:"to"`
	Subject        string                `json:"subject"`
	Text           string                `json:"text"`
	HTML           string                `json:"html"`
	MessageID      string                `json:"message_id"`
	Headers        map[string]string     `json:"headers"`
	Authentication *AuthenticationResult `json:"authentication"`
	Attachments    []InboundAttachment   `json:"attachments"`
}

// FetchError reports a non-2xx Receiving API answer or a client-side
// permanent failure (Client=true, e.g. the unconfigured stub). Only 4xx and
// Client errors are permanent; transport errors never produce FetchError —
// they stay untyped and transient so River retries them.
type FetchError struct {
	Op         string // operation: "get_inbound_email", "list_attachments", "download_attachment"
	StatusCode int    // HTTP status when the API answered; 0 for client-side failures
	Detail     string
	Client     bool // true = permanent failure before/without an HTTP request
}

// Error implements error.
func (e *FetchError) Error() string {
	if e.Client {
		return fmt.Sprintf("resend: %s: %s", e.Op, e.Detail)
	}
	return fmt.Sprintf("resend: %s: status %d: %s", e.Op, e.StatusCode, e.Detail)
}

// Permanent reports whether retrying cannot help: client-side failures and
// 4xx EXCEPT 408 (request timeout) and 429 (rate limited) — both are
// transient conditions where a retry is exactly the right move.
func (e *FetchError) Permanent() bool {
	if e.Client {
		return true
	}
	if e.StatusCode == http.StatusRequestTimeout || e.StatusCode == http.StatusTooManyRequests {
		return false
	}
	return e.StatusCode >= 400 && e.StatusCode < 500
}

// IsPermanent unwraps err and reports whether it is a permanent FetchError.
func IsPermanent(err error) bool {
	var fe *FetchError
	return errors.As(err, &fe) && fe.Permanent()
}

// ReceivingClient calls the Resend Receiving API over a bounded http.Client.
type ReceivingClient struct {
	apiKey string
	client *http.Client
	// BaseURL defaults to receivingBaseURL; tests override with httptest.
	BaseURL string
}

// NewReceivingClient builds the production client.
//
//	recv := resend.NewReceivingClient(cfg.ResendAPIKey)
func NewReceivingClient(apiKey string) *ReceivingClient {
	return &ReceivingClient{
		apiKey:  apiKey,
		client:  &http.Client{Timeout: receivingTimeout},
		BaseURL: receivingBaseURL,
	}
}

var _ Receiver = (*ReceivingClient)(nil)

// apiEmail is the GET /emails/receiving/{id} body as documented — every
// field optional so missing keys parse to zero values.
type apiEmail struct {
	ID             string                `json:"id"`
	From           string                `json:"from"`
	To             []string              `json:"to"`
	Subject        string                `json:"subject"`
	Text           *string               `json:"text"`
	HTML           *string               `json:"html"`
	MessageID      string                `json:"message_id"`
	Headers        map[string]string     `json:"headers"`
	Authentication *AuthenticationResult `json:"authentication"`
	Attachments    []InboundAttachment   `json:"attachments"`
}

// apiAttachmentList is the GET .../attachments envelope.
type apiAttachmentList struct {
	Data    []InboundAttachment `json:"data"`
	HasMore bool                `json:"has_more"`
}

// GetInboundEmail fetches the message, then enriches its attachments with
// the download URLs only the attachments-list endpoint returns (the message
// payload itself carries attachment metadata without URLs).
func (c *ReceivingClient) GetInboundEmail(ctx context.Context, emailID string) (*InboundEmail, error) {
	id := strings.TrimSpace(emailID)
	if id == "" {
		return nil, &FetchError{Op: "get_inbound_email", Client: true, Detail: "email id must be non-empty"}
	}
	var ae apiEmail
	if err := c.get(ctx, "get_inbound_email", "/emails/receiving/"+url.PathEscape(id), &ae); err != nil {
		return nil, err
	}
	email := &InboundEmail{
		ID:             ae.ID,
		From:           ae.From,
		To:             ae.To,
		Subject:        ae.Subject,
		MessageID:      ae.MessageID,
		Headers:        ae.Headers,
		Authentication: ae.Authentication,
		Attachments:    ae.Attachments,
	}
	if ae.Text != nil {
		email.Text = *ae.Text
	}
	if ae.HTML != nil {
		email.HTML = *ae.HTML
	}
	if len(email.Attachments) > 0 {
		if err := c.mergeDownloadURLs(ctx, id, email); err != nil {
			return nil, err
		}
	}
	return email, nil
}

// mergeDownloadURLs fetches the attachment list and copies download_url,
// expires_at and size onto the message's attachments by id. List entries
// unknown to the message are appended so nothing downloadable is lost.
// A page longer than the 100-item limit fails loudly — silently dropping
// the tail would surface as "no download_url" retries, not as this bug.
func (c *ReceivingClient) mergeDownloadURLs(ctx context.Context, emailID string, email *InboundEmail) error {
	var list apiAttachmentList
	path := "/emails/receiving/" + url.PathEscape(emailID) + "/attachments?limit=100"
	if err := c.get(ctx, "list_attachments", path, &list); err != nil {
		return err
	}
	if list.HasMore {
		return &FetchError{Op: "list_attachments", Client: true,
			Detail: fmt.Sprintf("email %s has >100 attachments; pagination is not implemented", emailID)}
	}
	byID := map[string]int{}
	for i, a := range email.Attachments {
		byID[a.ID] = i
	}
	for _, item := range list.Data {
		if i, ok := byID[item.ID]; ok {
			email.Attachments[i].DownloadURL = item.DownloadURL
			email.Attachments[i].ExpiresAt = item.ExpiresAt
			if email.Attachments[i].Size == 0 {
				email.Attachments[i].Size = item.Size
			}
			continue
		}
		email.Attachments = append(email.Attachments, item)
	}
	return nil
}

// get issues an authenticated GET and decodes the JSON body into out.
func (c *ReceivingClient) get(ctx context.Context, op, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return &FetchError{Op: op, Client: true, Detail: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("resend: %s: %w", op, stripURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return &FetchError{Op: op, StatusCode: resp.StatusCode, Detail: resp.Status}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// stripURLError removes the *url.Error wrapper so a request URL — which may
// be a signed CDN link carrying credential parameters — never leaks into an
// error string that can land in rfq_requests.failure_reason.
func stripURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Unwrap()
	}
	return err
}

// DownloadAttachment fetches bytes from a signed download_url. The URL is a
// pre-signed CDN link — the API key must NOT ride along, so this request
// goes out bare with no Authorization header, and the URL itself is kept
// out of every error it produces.
func (c *ReceivingClient) DownloadAttachment(ctx context.Context, downloadURL string) ([]byte, error) {
	if strings.TrimSpace(downloadURL) == "" {
		return nil, &FetchError{Op: "download_attachment", Client: true, Detail: "download_url must be non-empty"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, &FetchError{Op: "download_attachment", Client: true, Detail: "invalid download_url"}
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("resend: download_attachment: %w", stripURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, &FetchError{Op: "download_attachment", StatusCode: resp.StatusCode, Detail: resp.Status}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAttachmentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("resend: download_attachment: read body: %w", err)
	}
	if len(body) > maxAttachmentBytes {
		return nil, &FetchError{Op: "download_attachment", Client: true,
			Detail: fmt.Sprintf("attachment exceeds %d bytes cap", maxAttachmentBytes)}
	}
	return body, nil
}

// UnconfiguredReceiver is the fail-soft binding used when RESEND_API_KEY is
// absent (spec: not silently off): the server still boots, and every fetch
// fails loudly with a permanent FetchError so ingest writes the RFQ shell
// with failure_reason instead of retrying a credential that cannot appear.
type UnconfiguredReceiver struct{}

var _ Receiver = UnconfiguredReceiver{}

// GetInboundEmail reports the missing configuration as permanent.
func (UnconfiguredReceiver) GetInboundEmail(context.Context, string) (*InboundEmail, error) {
	return nil, &FetchError{Op: "get_inbound_email", Client: true,
		Detail: "receiving client not configured — RESEND_API_KEY is unset"}
}

// DownloadAttachment reports the missing configuration as permanent.
func (UnconfiguredReceiver) DownloadAttachment(context.Context, string) ([]byte, error) {
	return nil, &FetchError{Op: "download_attachment", Client: true,
		Detail: "receiving client not configured — RESEND_API_KEY is unset"}
}
