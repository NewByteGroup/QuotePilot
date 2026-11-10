package platform_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// TestFakeMailSenderRecords proves the double itself: calls land in Sent()
// in order.
func TestFakeMailSenderRecords(t *testing.T) {
	fake := &platform.FakeMailSender{}
	ctx := context.Background()
	require.NoError(t, fake.SendMagicLink(ctx, "a@test.dev", "https://app.test/auth/exchange?token=one"))
	require.NoError(t, fake.SendMagicLink(ctx, "b@test.dev", "https://app.test/auth/exchange?token=two"))
	require.Equal(t, []platform.MagicSend{
		{To: "a@test.dev", URL: "https://app.test/auth/exchange?token=one"},
		{To: "b@test.dev", URL: "https://app.test/auth/exchange?token=two"},
	}, fake.Sent())
}

// TestResendMailSenderPosts pins the wire contract against a real HTTP
// server: POST /emails, bearer auth, and the documented JSON shape.
func TestResendMailSenderPosts(t *testing.T) {
	const (
		apiKey = "re_test_secret_key"
		link   = "https://app.test/auth/exchange?token=abc123"
	)
	var gotAuth, gotContentType string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/emails", r.URL.Path)
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"mail_1"}`))
	}))
	defer srv.Close()

	sender := platform.NewResendMailSender(apiKey, "signin@quotepilot.test")
	sender.BaseURL = srv.URL
	err := sender.SendMagicLink(context.Background(), "user@test.dev", link)
	require.NoError(t, err)

	assert.Equal(t, "Bearer "+apiKey, gotAuth)
	assert.Equal(t, "application/json", gotContentType)
	assert.Equal(t, "signin@quotepilot.test", gotBody["from"])
	assert.Equal(t, "user@test.dev", gotBody["to"])
	assert.NotEmpty(t, gotBody["subject"])
	assert.Contains(t, gotBody["html"], link, "the html body must carry the magic link")
}

// TestResendMailSenderError pins the failure contract: non-2xx surfaces the
// status and leaks neither the API key nor the tokened URL.
func TestResendMailSenderError(t *testing.T) {
	const (
		apiKey = "re_test_secret_key"
		link   = "https://app.test/auth/exchange?token=abc123"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"invalid from"}`))
	}))
	defer srv.Close()

	sender := platform.NewResendMailSender(apiKey, "bad-from")
	sender.BaseURL = srv.URL
	err := sender.SendMagicLink(context.Background(), "user@test.dev", link)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "422")
	assert.NotContains(t, err.Error(), apiKey, "the error must never leak the credential")
	assert.NotContains(t, err.Error(), "abc123", "the error must never leak the token")
}
