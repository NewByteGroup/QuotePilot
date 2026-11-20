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
// in order, each tagged with the template kind the worker switched on.
func TestFakeMailSenderRecords(t *testing.T) {
	fake := &platform.FakeMailSender{}
	ctx := context.Background()
	require.NoError(t, fake.SendMagicLink(ctx, "a@test.dev", "https://app.test/auth/exchange?token=one"))
	require.NoError(t, fake.SendPasswordReset(ctx, "b@test.dev", "https://app.test/auth/set-password?token=two"))
	require.Equal(t, []platform.MagicSend{
		{Kind: platform.MailPurposeLogin, To: "a@test.dev", URL: "https://app.test/auth/exchange?token=one"},
		{Kind: platform.MailPurposePasswordReset, To: "b@test.dev", URL: "https://app.test/auth/set-password?token=two"},
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

// TestResendMailSenderResetPost pins the reset template's wire shape: same
// /emails POST, its own subject and copy so the two mails are
// unmistakable, the tokened URL riding the html.
func TestResendMailSenderResetPost(t *testing.T) {
	const (
		apiKey = "re_test_secret_key"
		link   = "https://app.test/auth/set-password?token=abc123"
	)
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"mail_1"}`))
	}))
	defer srv.Close()

	sender := platform.NewResendMailSender(apiKey, "signin@quotepilot.test")
	sender.BaseURL = srv.URL
	require.NoError(t, sender.SendPasswordReset(context.Background(), "user@test.dev", link))

	assert.Equal(t, "Set your QuotePilot password", gotBody["subject"])
	assert.Contains(t, gotBody["html"], link)
	assert.Contains(t, gotBody["html"], "Set your QuotePilot password",
		"the reset body says what the link does — it does not sign anyone in")
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
