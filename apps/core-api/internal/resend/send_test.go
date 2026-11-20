package resend_test

// Sending client tests against httptest: the wire shape (auth, idempotency
// key, base64 attachment, headers) and the retry classification the
// quote_send worker drives off IsPermanent.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/resend"
)

// sendFixture is the request every test sends; the server asserts on it.
func sendFixture() resend.SendRequest {
	return resend.SendRequest{
		To:       []string{"buyer@customer.test"},
		From:     "Comexa <quotes@comexa.test>",
		ReplyTo:  "quotes@comexa.test",
		Subject:  "Re: RFQ: 2x40HQ",
		TextBody: "Your quote Q-1 is attached.",
		Attachments: []resend.SendAttachment{
			{Filename: "Q-1.pdf", Content: []byte("%PDF-fake")},
		},
		Headers:        map[string]string{"In-Reply-To": "<m1@buyer.test>", "References": "<m0@fwd.test> <m1@buyer.test>"},
		IdempotencyKey: "quote-1:2",
	}
}

// TestSendingClientWireShape: POST /emails carries the bearer token, the
// idempotency key and the documented payload — attachments base64.
func TestSendingClientWireShape(t *testing.T) {
	var gotAuth, gotIdem, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotIdem = r.Header.Get("Idempotency-Key")
		raw := make([]byte, 0, 4096)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			raw = append(raw, buf[:n]...)
			if err != nil {
				break
			}
		}
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"provider-msg-9"}`))
	}))
	defer srv.Close()

	c := resend.NewSendingClient("test-key")
	c.BaseURL = srv.URL
	id, err := c.Send(context.Background(), sendFixture())
	require.NoError(t, err)
	assert.Equal(t, "provider-msg-9", id)

	assert.Equal(t, "Bearer test-key", gotAuth)
	assert.Equal(t, "quote-1:2", gotIdem, "the idempotency key must ride the documented header")
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(gotBody), &payload))
	assert.Equal(t, "Comexa <quotes@comexa.test>", payload["from"])
	assert.Equal(t, "quotes@comexa.test", payload["reply_to"])
	assert.Equal(t, []any{"buyer@customer.test"}, payload["to"])
	assert.Equal(t, "Re: RFQ: 2x40HQ", payload["subject"])
	atts, ok := payload["attachments"].([]any)
	require.True(t, ok && len(atts) == 1)
	att := atts[0].(map[string]any)
	assert.Equal(t, "Q-1.pdf", att["filename"])
	decoded, err := base64.StdEncoding.DecodeString(att["content"].(string))
	require.NoError(t, err)
	assert.Equal(t, "%PDF-fake", string(decoded))
	headers := payload["headers"].(map[string]any)
	assert.Equal(t, "<m1@buyer.test>", headers["In-Reply-To"])
}

// TestSendingClientNoIdempotencyHeader: an empty key sends no header —
// the worker always sets one, but the client must not emit a blank one.
func TestSendingClientNoIdempotencyHeader(t *testing.T) {
	sent := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent = true
		_, has := r.Header["Idempotency-Key"]
		assert.False(t, has)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()
	c := resend.NewSendingClient("k")
	c.BaseURL = srv.URL
	_, err := c.Send(context.Background(), resend.SendRequest{To: []string{"a@b.c"}, From: "f", Subject: "s"})
	require.NoError(t, err)
	require.True(t, sent)
}

// TestSendingClientErrorClassification pins the spec-§3 rule on the wire:
// 4xx (not 408/429) is permanent; 429, 408 and 5xx stay transient so River
// retries them; the API message body becomes the error detail.
func TestSendingClientErrorClassification(t *testing.T) {
	cases := []struct {
		status    int
		permanent bool
	}{
		{400, true}, {401, true}, {403, true}, {404, true}, {422, true},
		{408, false}, {429, false},
		{500, false}, {502, false}, {503, false},
	}
	for _, c := range cases {
		t.Run(http.StatusText(c.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(`{"message":"bad to address"}`))
			}))
			defer srv.Close()
			cl := resend.NewSendingClient("k")
			cl.BaseURL = srv.URL
			_, err := cl.Send(context.Background(), resend.SendRequest{To: []string{"a@b.c"}, From: "f", Subject: "s"})
			require.Error(t, err)
			assert.Equal(t, c.permanent, resend.IsPermanent(err), "status %d", c.status)
			var se *resend.SendError
			require.True(t, errors.As(err, &se))
			assert.Equal(t, c.status, se.StatusCode)
			assert.Equal(t, "bad to address", se.Detail)
		})
	}
}

// TestSendingClientTransient: a transport error is untyped and therefore
// transient — River retries it.
func TestSendingClientTransient(t *testing.T) {
	c := resend.NewSendingClient("k")
	c.BaseURL = "http://127.0.0.1:1" // nothing listens — connection refused
	_, err := c.Send(context.Background(), resend.SendRequest{To: []string{"a@b.c"}, From: "f", Subject: "s"})
	require.Error(t, err)
	assert.False(t, resend.IsPermanent(err))
	assert.NotContains(t, err.Error(), "/emails", "the *url.Error wrapper — and its URL — must be stripped")
}

// TestUnconfiguredSender is the missing-credential contract: permanent,
// so quote_send cancels instead of retrying a key that cannot appear.
func TestUnconfiguredSender(t *testing.T) {
	_, err := resend.UnconfiguredSender{}.Send(context.Background(), sendFixture())
	require.Error(t, err)
	assert.True(t, resend.IsPermanent(err))
	var se *resend.SendError
	require.True(t, errors.As(err, &se) && se.Client)
}

// TestFakeSender pins the named fake's contract — the worker tests drive
// it: captures requests, returns the canned id, fails on Err.
func TestFakeSender(t *testing.T) {
	f := &resend.FakeSender{MessageID: "msg-7"}
	id, err := f.Send(context.Background(), sendFixture())
	require.NoError(t, err)
	assert.Equal(t, "msg-7", id)
	assert.Equal(t, 1, f.Calls())
	require.Len(t, f.Requests(), 1)
	assert.Equal(t, "quote-1:2", f.Requests()[0].IdempotencyKey)

	f.Err = errors.New("boom")
	_, err = f.Send(context.Background(), sendFixture())
	require.Error(t, err)
	assert.Equal(t, 2, f.Calls(), "a failed send still records the attempt")
}
