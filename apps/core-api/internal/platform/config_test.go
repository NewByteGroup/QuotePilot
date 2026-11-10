package platform_test

import (
	"maps"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// envOf adapts a map to LoadConfig's getenv parameter.
func envOf(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// baseEnv is the minimal valid environment; tests mutate a copy.
func baseEnv() map[string]string {
	return map[string]string{
		"POSTGRES_DSN":  "postgres://qp_app:pw@127.0.0.1:5432/quotepilot",
		"WEB_URL":       "https://app.example.test",
		"AUTH_JWT_KEYS": "k1=secret-one-0123456789abcdef012345678,k2=secret-two-0123456789abcdef012345678",
	}
}

func envWith(overrides map[string]string) map[string]string {
	env := baseEnv()
	maps.Copy(env, overrides)
	return env
}

// TestLoadConfigValid covers the happy path: defaults applied, first kid
// active, explicit overrides honoured.
func TestLoadConfigValid(t *testing.T) {
	cfg, err := platform.LoadConfig(envOf(baseEnv()))
	require.NoError(t, err)
	assert.Equal(t, "postgres://qp_app:pw@127.0.0.1:5432/quotepilot", cfg.AppDSN)
	assert.Equal(t, "https://app.example.test", cfg.WebURL)
	assert.Equal(t, "k1", cfg.ActiveKID, "first AUTH_JWT_KEYS entry signs by default")
	assert.Equal(t, map[string]string{"k1": "secret-one-0123456789abcdef012345678", "k2": "secret-two-0123456789abcdef012345678"}, cfg.JWTKeys)
	assert.Equal(t, 15*time.Minute, cfg.AccessTTL)
	assert.Equal(t, 30*24*time.Hour, cfg.RefreshTTL)
	assert.Equal(t, 15*time.Minute, cfg.LoginTTL)
	assert.True(t, cfg.CookieSecure, "cookie Secure defaults to true")

	cfg, err = platform.LoadConfig(envOf(envWith(map[string]string{
		"QP_ADMIN_DSN":         "postgres://qp_admin:pw@127.0.0.1:5432/quotepilot",
		"AUTH_JWT_ACTIVE_KID":  "k2",
		"AUTH_JWT_ACCESS_TTL":  "5m",
		"AUTH_JWT_REFRESH_TTL": "168h",
		"AUTH_LOGIN_TTL":       "10m",
		"AUTH_COOKIE_SECURE":   "0",
		"RESEND_API_KEY":       "re_test",
		"AUTH_MAGIC_FROM":      "signin@example.test",
	})))
	require.NoError(t, err)
	assert.Equal(t, "k2", cfg.ActiveKID, "AUTH_JWT_ACTIVE_KID overrides the first entry")
	assert.Equal(t, 5*time.Minute, cfg.AccessTTL)
	assert.Equal(t, 168*time.Hour, cfg.RefreshTTL)
	assert.Equal(t, 10*time.Minute, cfg.LoginTTL)
	assert.False(t, cfg.CookieSecure, "0 disables Secure for local http")
	assert.Equal(t, "re_test", cfg.ResendAPIKey)
	assert.Equal(t, "signin@example.test", cfg.MagicFrom)
}

// TestLoadConfigErrors pins the failure contract: the error names the
// offending variable but never a secret value.
func TestLoadConfigErrors(t *testing.T) {
	cases := []struct {
		name      string
		overrides map[string]string
		wantVar   string
	}{
		{"missing dsn", map[string]string{"POSTGRES_DSN": ""}, "POSTGRES_DSN"},
		{"missing web url", map[string]string{"WEB_URL": ""}, "WEB_URL"},
		{"empty keys", map[string]string{"AUTH_JWT_KEYS": ""}, "AUTH_JWT_KEYS"},
		{"malformed entry", map[string]string{"AUTH_JWT_KEYS": "k1=ok-0123456789abcdef0123456789ab,nosecret"}, "AUTH_JWT_KEYS"},
		{"short secret", map[string]string{"AUTH_JWT_KEYS": "k1=short"}, "AUTH_JWT_KEYS"},
		{"empty secret", map[string]string{"AUTH_JWT_KEYS": "k1="}, "AUTH_JWT_KEYS"},
		{"duplicate kid", map[string]string{"AUTH_JWT_KEYS": "k1=a,k1=b"}, "AUTH_JWT_KEYS"},
		{"unknown active kid", map[string]string{"AUTH_JWT_ACTIVE_KID": "ghost"}, "AUTH_JWT_ACTIVE_KID"},
		{"bad access ttl", map[string]string{"AUTH_JWT_ACCESS_TTL": "soon"}, "AUTH_JWT_ACCESS_TTL"},
		{"negative ttl", map[string]string{"AUTH_JWT_REFRESH_TTL": "-1h"}, "AUTH_JWT_REFRESH_TTL"},
		{"zero ttl", map[string]string{"AUTH_LOGIN_TTL": "0s"}, "AUTH_LOGIN_TTL"},
		{"bad cookie flag", map[string]string{"AUTH_COOKIE_SECURE": "yes"}, "AUTH_COOKIE_SECURE"},
		{"half resend pair", map[string]string{"RESEND_API_KEY": "re_test"}, "AUTH_MAGIC_FROM"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := platform.LoadConfig(envOf(envWith(tc.overrides)))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantVar)
		})
	}
}

// TestLoadConfigNoSecretInError proves a malformed keys entry is reported by
// position and shape, never by echoing the entry — it may be the secret.
func TestLoadConfigNoSecretInError(t *testing.T) {
	_, err := platform.LoadConfig(envOf(envWith(map[string]string{
		"AUTH_JWT_KEYS": "k1=good-0123456789abcdef0123456789a,malformed-entry",
	})))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "malformed-entry")
	assert.NotContains(t, err.Error(), "good-0123456789abcdef0123456789a")
}
