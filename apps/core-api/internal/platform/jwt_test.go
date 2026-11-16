package platform_test

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// jwtConfig is a two-key Config so rotation (any configured kid verifies,
// only the active one signs) is exercised.
func jwtConfig() *platform.Config {
	return &platform.Config{
		JWTKeys:   map[string]string{"k1": "secret-one", "k2": "secret-two"},
		ActiveKID: "k1",
		AccessTTL: 15 * time.Minute,
	}
}

// signAny mints a token outside Config.SignAccess so tests can forge kid,
// alg and claims freely.
func signAny(t *testing.T, method jwt.SigningMethod, kid string, key any, claims jwt.Claims) string {
	t.Helper()
	token := jwt.NewWithClaims(method, claims)
	if kid != "" {
		token.Header["kid"] = kid
	}
	raw, err := token.SignedString(key)
	require.NoError(t, err)
	return raw
}

func accessClaims(userID, sessionID uuid.UUID, iat, exp time.Time) jwt.Claims {
	return jwt.MapClaims{
		"sub": userID.String(),
		"sid": sessionID.String(),
		"iat": iat.Unix(),
		"exp": exp.Unix(),
	}
}

// TestSignVerifyAccessRoundTrip proves the happy path and that the header
// carries the active kid.
func TestSignVerifyAccessRoundTrip(t *testing.T) {
	cfg := jwtConfig()
	userID, sessionID := uuid.New(), uuid.New()

	raw, err := cfg.SignAccess(userID, sessionID, time.Now())
	require.NoError(t, err)

	header := jwt.NewParser()
	unverified := jwt.MapClaims{}
	parsed, _, err := header.ParseUnverified(raw, unverified)
	require.NoError(t, err)
	assert.Equal(t, "k1", parsed.Header["kid"])
	assert.Equal(t, "HS256", parsed.Header["alg"])
	assert.Contains(t, unverified, "sid")
	assert.NotContains(t, unverified, "org", "org must never be a claim (spec section 4)")
	assert.NotContains(t, unverified, "role")

	gotUser, gotSession, err := cfg.VerifyAccess(raw)
	require.NoError(t, err)
	assert.Equal(t, userID, gotUser)
	assert.Equal(t, sessionID, gotSession)
}

// TestVerifyAccessNonActiveKey proves rotation: a token signed with a
// configured-but-inactive kid still verifies.
func TestVerifyAccessNonActiveKey(t *testing.T) {
	cfg := jwtConfig()
	userID, sessionID := uuid.New(), uuid.New()
	raw := signAny(t, jwt.SigningMethodHS256, "k2", []byte("secret-two"),
		accessClaims(userID, sessionID, time.Now(), time.Now().Add(time.Minute)))

	gotUser, gotSession, err := cfg.VerifyAccess(raw)
	require.NoError(t, err)
	assert.Equal(t, userID, gotUser)
	assert.Equal(t, sessionID, gotSession)
}

// TestVerifyAccessRejections pins the forgery contract: every failure maps
// to ErrInvalidAccessToken so middleware can answer the uniform 401.
func TestVerifyAccessRejections(t *testing.T) {
	cfg := jwtConfig()
	userID, sessionID := uuid.New(), uuid.New()
	now := time.Now()
	valid := accessClaims(userID, sessionID, now, now.Add(15*time.Minute))

	expired, err := cfg.SignAccess(userID, sessionID, now.Add(-time.Hour))
	require.NoError(t, err)

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	cases := []struct {
		name string
		raw  string
	}{
		{"malformed garbage", "this.is.not-a-jwt"},
		{"empty", ""},
		{"wrong signature", signAny(t, jwt.SigningMethodHS256, "k1", []byte("attacker-secret"), valid)},
		{"unknown kid", signAny(t, jwt.SigningMethodHS256, "ghost", []byte("secret-one"), valid)},
		{"missing kid", signAny(t, jwt.SigningMethodHS256, "", []byte("secret-one"), valid)},
		{"alg none", signAny(t, jwt.SigningMethodNone, "k1", jwt.UnsafeAllowNoneSignatureType, valid)},
		{"alg confusion RS256", signAny(t, jwt.SigningMethodRS256, "k1", rsaKey, valid)},
		{"expired", expired},
		{"missing exp", signAny(t, jwt.SigningMethodHS256, "k1", []byte("secret-one"),
			jwt.MapClaims{"sub": userID.String(), "sid": sessionID.String(), "iat": now.Unix()})},
		{"missing iat", signAny(t, jwt.SigningMethodHS256, "k1", []byte("secret-one"),
			jwt.MapClaims{"sub": userID.String(), "sid": sessionID.String(), "exp": now.Add(time.Minute).Unix()})},
		{"nil uuid sub", signAny(t, jwt.SigningMethodHS256, "k1", []byte("secret-one"),
			jwt.MapClaims{"sub": uuid.Nil.String(), "sid": sessionID.String(), "iat": now.Unix(), "exp": now.Add(time.Minute).Unix()})},
		{"sid not uuid", signAny(t, jwt.SigningMethodHS256, "k1", []byte("secret-one"),
			jwt.MapClaims{"sub": userID.String(), "sid": "not-a-uuid", "iat": now.Unix(), "exp": now.Add(time.Minute).Unix()})},
		{"sub not uuid", signAny(t, jwt.SigningMethodHS256, "k1", []byte("secret-one"),
			jwt.MapClaims{"sub": "not-a-uuid", "sid": sessionID.String(), "iat": now.Unix(), "exp": now.Add(time.Minute).Unix()})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := cfg.VerifyAccess(tc.raw)
			require.Error(t, err)
			assert.True(t, errors.Is(err, platform.ErrInvalidAccessToken),
				"%s must unwrap to ErrInvalidAccessToken, got %v", tc.name, err)
		})
	}
}
