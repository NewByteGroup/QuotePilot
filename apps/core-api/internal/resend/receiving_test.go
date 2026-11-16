package resend_test

// Client-contract tests for the Resend Receiving API: the response shapes
// here mirror resend.com/docs/api-reference/emails/retrieve-received-email
// (message payload carries attachment metadata WITHOUT download_url; the
// attachments-list endpoint supplies download_url + expires_at) and
// list-received-email-attachments.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/resend"
)

// servingFixture renders the documented response payloads.
func servingFixture(t *testing.T, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/emails/receiving/msg-1":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if status/100 != 2 {
				_, _ = w.Write([]byte(`{"message":"not found"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object":  "email",
				"id":      "msg-1",
				"to":      []string{"quotes@in.test"},
				"from":    "Ada Buyer <ada@buyer.test>",
				"subject": "RFQ: 2x40HQ NGB SZX",
				"html":    "<p>Please quote</p>",
				"text":    "Please quote",
				"headers": map[string]string{
					"from":         "Ada Buyer <ada@buyer.test>",
					"message-id":   "<m1@buyer.test>",
					"in-reply-to":  "<m0@fwd.test>",
					"references":   "<m0@fwd.test>",
					"return-path":  "ada@buyer.test",
					"mime-version": "1.0",
				},
				"authentication": map[string]string{"spf": "pass", "dkim": "pass", "dmarc": "pass"},
				"message_id":     "<m1@buyer.test>",
				"attachments": []map[string]any{{
					"id":                  "att-1",
					"filename":            "rates.pdf",
					"content_type":        "application/pdf",
					"content_disposition": "attachment",
					"content_id":          nil,
					"size":                13264,
				}},
			})
		case "/emails/receiving/msg-1/attachments":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object":   "list",
				"has_more": false,
				"data": []map[string]any{{
					"id":                  "att-1",
					"filename":            "rates.pdf",
					"content_type":        "application/pdf",
					"content_disposition": "attachment",
					"download_url":        "https://cdn.test/signed/att-1",
					"expires_at":          "2026-11-20T01:00:00Z",
					"size":                13264,
				}},
			})
		case "/signed/att-1":
			// Pre-signed CDN path — must NOT carry the API key.
			assert.Empty(t, r.Header.Get("Authorization"), "download must not send the bearer token to the CDN")
			_, _ = w.Write([]byte("PDF-BYTES"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestGetInboundEmailParsesAndMergesURLs(t *testing.T) {
	srv := servingFixture(t, http.StatusOK)
	defer srv.Close()
	client := resend.NewReceivingClient("re_test_key")
	client.BaseURL = srv.URL

	em, err := client.GetInboundEmail(context.Background(), "msg-1")
	require.NoError(t, err)
	assert.Equal(t, "msg-1", em.ID)
	assert.Equal(t, "Ada Buyer <ada@buyer.test>", em.From)
	assert.Equal(t, "RFQ: 2x40HQ NGB SZX", em.Subject)
	assert.Equal(t, "Please quote", em.Text)
	assert.Equal(t, "<p>Please quote</p>", em.HTML)
	assert.Equal(t, "<m1@buyer.test>", em.MessageID)
	require.NotNil(t, em.Authentication)
	assert.Equal(t, "pass", em.Authentication.SPF)
	require.Len(t, em.Attachments, 1)
	assert.Equal(t, "rates.pdf", em.Attachments[0].Filename)
	assert.Equal(t, "https://cdn.test/signed/att-1", em.Attachments[0].DownloadURL)
	assert.Equal(t, int64(13264), em.Attachments[0].Size)
}

func TestGetInboundEmailToleratesMissingFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Minimal payload: no headers, no authentication, text/html null,
		// no attachments.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "email", "id": "bare", "from": "x@y.test",
			"text": nil, "html": nil, "attachments": nil, "authentication": nil,
		})
	}))
	defer srv.Close()
	client := resend.NewReceivingClient("re_test_key")
	client.BaseURL = srv.URL

	em, err := client.GetInboundEmail(context.Background(), "bare")
	require.NoError(t, err)
	assert.Equal(t, "x@y.test", em.From)
	assert.Empty(t, em.Text)
	assert.Empty(t, em.HTML)
	assert.Nil(t, em.Authentication)
	assert.Empty(t, em.Attachments)
}

// TestGetInboundEmailStatusClassification pins the retry contract end to
// end: 200 parses, other 4xx are permanent (retrying a client error only
// burns attempts), but 408 and 429 stay transient alongside 5xx — Resend
// answering "slow down" or "try again" is exactly what River retry is for.
func TestGetInboundEmailStatusClassification(t *testing.T) {
	for _, c := range []struct {
		name      string
		status    int
		permanent bool
	}{
		{"ok", http.StatusOK, false},
		{"request timeout", http.StatusRequestTimeout, false},
		{"too many requests", http.StatusTooManyRequests, false},
		{"not found", http.StatusNotFound, true},
		{"forbidden", http.StatusForbidden, true},
		{"unauthorized", http.StatusUnauthorized, true},
		{"bad gateway", http.StatusBadGateway, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := servingFixture(t, c.status)
			defer srv.Close()
			client := resend.NewReceivingClient("re_test_key")
			client.BaseURL = srv.URL

			_, err := client.GetInboundEmail(context.Background(), "msg-1")
			if c.status/100 == 2 {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, c.permanent, resend.IsPermanent(err),
				"status %d permanent=%v", c.status, c.permanent)
		})
	}
}

func TestDownloadAttachment(t *testing.T) {
	srv := servingFixture(t, http.StatusOK)
	defer srv.Close()
	client := resend.NewReceivingClient("re_test_key")
	client.BaseURL = srv.URL

	body, err := client.DownloadAttachment(context.Background(), srv.URL+"/signed/att-1")
	require.NoError(t, err)
	assert.Equal(t, []byte("PDF-BYTES"), body)
}

// TestDownloadAttachmentErrorHidesSignedURL pins nit-6: the download URL is
// a credential-bearing signed link, and *url.Error echoes the whole URL —
// the wrapper is stripped so signature query params can never ride an error
// into logs or rfq_requests.failure_reason. (The hostname may still appear
// inside the transport error; only the secret query is stripped.)
func TestDownloadAttachmentErrorHidesSignedURL(t *testing.T) {
	client := resend.NewReceivingClient("re_test_key")
	// Unroutable TLD fails fast in the transport — the query is the secret.
	signed := "https://cdn.invalid/att.pdf?X-Sig=SECRET123&X-Exp=1"
	_, err := client.DownloadAttachment(context.Background(), signed)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "SECRET123")
	assert.NotContains(t, err.Error(), "X-Sig")
}

func TestUnconfiguredReceiverFailsPermanently(t *testing.T) {
	var recv resend.Receiver = resend.UnconfiguredReceiver{}
	_, err := recv.GetInboundEmail(context.Background(), "x")
	require.Error(t, err)
	assert.True(t, resend.IsPermanent(err))
	_, err = recv.DownloadAttachment(context.Background(), "https://x.test")
	require.Error(t, err)
	assert.True(t, resend.IsPermanent(err))
}
