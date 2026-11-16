// Package platform holds everything pre-tenant or request-edge (spec
// 2026-11-10-tenant-resolution-api section 6): env config, the access-JWT
// signer, opaque token helpers, typed accessors for the migration-0003
// SECURITY DEFINER function family, the hand-rolled rate limiter and the
// magic-link sender. It is the only Go package allowed to name the platform
// path; it never imports internal/tenantdb — tenant context starts after
// this package has run.
package platform

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Config is the request-edge configuration, loaded once at server boot.
// Secret-adjacent fields (JWTKeys, ResendAPIKey, the DSNs) must never be
// logged or echoed into errors.
type Config struct {
	AppDSN       string            // qp_app DSN — the only credential on request paths
	AdminDSN     string            // qp_admin DSN — platform_* path only
	WebURL       string            // base URL magic links are built from
	JWTKeys      map[string]string // kid -> HS256 secret; any entry verifies
	ActiveKID    string            // kid that signs new access tokens
	AccessTTL    time.Duration     // access JWT lifetime (default 15m, spec section 4)
	RefreshTTL   time.Duration     // refresh cookie lifetime (default 30d)
	LoginTTL     time.Duration     // magic-link token lifetime (default 15m)
	ResendAPIKey string            // magic-link sender credential
	MagicFrom    string            // From: address of magic-link mail
	CookieSecure bool              // Secure flag on auth cookies; false only for local http
}

// Defaults from spec section 4. Env overrides are Go duration strings —
// time.ParseDuration has no day unit, so 30d is written "720h".
const (
	defaultAccessTTL  = 15 * time.Minute
	defaultRefreshTTL = 30 * 24 * time.Hour
	defaultLoginTTL   = 15 * time.Minute
)

// LoadConfig builds a Config from environment variables read through getenv
// (os.Getenv in production, a map in tests). Required: POSTGRES_DSN, WEB_URL
// and at least one AUTH_JWT_KEYS entry; AUTH_JWT_ACTIVE_KID defaults to the
// first listed kid. Errors name the offending variable and the expected
// shape — never a secret value.
//
//	cfg, err := platform.LoadConfig(os.Getenv)
func LoadConfig(getenv func(string) string) (Config, error) {
	cfg := defaultConfig(getenv)
	if err := cfg.loadJWTKeys(getenv); err != nil {
		return Config{}, err
	}
	if err := cfg.loadTTLs(getenv); err != nil {
		return Config{}, err
	}
	if err := cfg.loadCookieSecure(getenv); err != nil {
		return Config{}, err
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// defaultConfig reads the plain-string vars and applies the spec section 4
// defaults before the optional overrides land.
func defaultConfig(getenv func(string) string) Config {
	return Config{
		AppDSN:       getenv("POSTGRES_DSN"),
		AdminDSN:     getenv("QP_ADMIN_DSN"),
		WebURL:       getenv("WEB_URL"),
		ResendAPIKey: getenv("RESEND_API_KEY"),
		MagicFrom:    getenv("AUTH_MAGIC_FROM"),
		AccessTTL:    defaultAccessTTL,
		RefreshTTL:   defaultRefreshTTL,
		LoginTTL:     defaultLoginTTL,
		CookieSecure: true,
	}
}

// loadJWTKeys parses AUTH_JWT_KEYS and resolves the active kid
// (AUTH_JWT_ACTIVE_KID, defaulting to the first listed).
func (c *Config) loadJWTKeys(getenv func(string) string) error {
	keys, first, err := parseJWTKeys(getenv("AUTH_JWT_KEYS"))
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return errors.New("platform: AUTH_JWT_KEYS must list at least one kid=secret pair")
	}
	c.JWTKeys = keys
	c.ActiveKID = strings.TrimSpace(getenv("AUTH_JWT_ACTIVE_KID"))
	if c.ActiveKID == "" {
		c.ActiveKID = first
	}
	if _, ok := c.JWTKeys[c.ActiveKID]; !ok {
		return fmt.Errorf("platform: AUTH_JWT_ACTIVE_KID %q is not one of the AUTH_JWT_KEYS kids", c.ActiveKID)
	}
	return nil
}

// parseJWTKeys turns "kid=secret,kid=secret,..." into the key map and names
// the first kid. Entries are never echoed into errors: a malformed entry
// may itself be the secret.
func parseJWTKeys(raw string) (map[string]string, string, error) {
	keys := map[string]string{}
	first := ""
	for i, pair := range strings.Split(raw, ",") {
		kid, secret, skip, err := parseKeyPair(pair, i)
		if err != nil {
			return nil, "", err
		}
		if skip {
			continue
		}
		if err := addKey(keys, kid, secret); err != nil {
			return nil, "", err
		}
		if first == "" {
			first = kid
		}
	}
	return keys, first, nil
}

// minJWTSecretLen is the smallest accepted HS256 secret: any captured token
// can be offline-brute-forced, so a short secret is a weak password — 32
// bytes of entropy is the floor.
const minJWTSecretLen = 32

// addKey inserts one pair, rejecting a duplicate kid — a repeated kid would
// silently change which secret verifies its tokens.
func addKey(keys map[string]string, kid, secret string) error {
	if _, dup := keys[kid]; dup {
		return fmt.Errorf("platform: AUTH_JWT_KEYS: duplicate kid %q", kid)
	}
	if len(secret) < minJWTSecretLen {
		return fmt.Errorf("platform: AUTH_JWT_KEYS: kid %q's secret must be at least %d characters", kid, minJWTSecretLen)
	}
	keys[kid] = secret
	return nil
}

// parseKeyPair splits one entry on its first '=' — secrets may contain '='
// themselves. An empty entry is skipped; a malformed one is an error naming
// the position, never the content.
func parseKeyPair(pair string, index int) (kid, secret string, skip bool, err error) {
	pair = strings.TrimSpace(pair)
	if pair == "" {
		return "", "", true, nil
	}
	kid, secret, ok := strings.Cut(pair, "=")
	kid = strings.TrimSpace(kid)
	if !ok || kid == "" || secret == "" {
		return "", "", false, fmt.Errorf("platform: AUTH_JWT_KEYS entry %d must have shape kid=secret", index+1)
	}
	return kid, secret, false, nil
}

// loadTTLs applies the three optional duration overrides; a set-but-invalid
// value is a boot-time error, not a silent default.
func (c *Config) loadTTLs(getenv func(string) string) error {
	var err error
	if c.AccessTTL, err = envDuration(getenv, "AUTH_JWT_ACCESS_TTL", c.AccessTTL); err != nil {
		return err
	}
	if c.RefreshTTL, err = envDuration(getenv, "AUTH_JWT_REFRESH_TTL", c.RefreshTTL); err != nil {
		return err
	}
	if c.LoginTTL, err = envDuration(getenv, "AUTH_LOGIN_TTL", c.LoginTTL); err != nil {
		return err
	}
	return nil
}

// envDuration reads one positive Go-duration env var, falling back when unset.
func envDuration(getenv func(string) string, name string, fallback time.Duration) (time.Duration, error) {
	raw := getenv(name)
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("platform: %s must be a positive Go duration like %q, got %q", name, fallback, raw)
	}
	return d, nil
}

// loadCookieSecure reads AUTH_COOKIE_SECURE: unset means secure; "false"/"0"
// is the documented escape for local http (spec section 6).
func (c *Config) loadCookieSecure(getenv func(string) string) error {
	switch raw := getenv("AUTH_COOKIE_SECURE"); raw {
	case "", "true", "1":
		c.CookieSecure = true
	case "false", "0":
		c.CookieSecure = false
	default:
		return fmt.Errorf("platform: AUTH_COOKIE_SECURE must be true/1 or false/0, got %q", raw)
	}
	return nil
}

// validate catches config the server cannot boot with. RESEND_API_KEY and
// AUTH_MAGIC_FROM travel as a pair: the real sender needs both, and a half
// pair means a misconfigured deploy — leaving both empty selects the
// dev/test sender.
func (c *Config) validate() error {
	if c.AppDSN == "" {
		return errors.New("platform: POSTGRES_DSN is required (expected postgres://qp_app:...@host/db)")
	}
	if c.WebURL == "" {
		return errors.New("platform: WEB_URL is required — magic links are built from it")
	}
	if (c.ResendAPIKey == "") != (c.MagicFrom == "") {
		return errors.New("platform: RESEND_API_KEY and AUTH_MAGIC_FROM must be set together (or neither)")
	}
	return nil
}
