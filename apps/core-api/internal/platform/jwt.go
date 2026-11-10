package platform

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// ErrInvalidAccessToken is the umbrella for every VerifyAccess failure:
// unknown kid, wrong alg, bad signature, malformed input, expiry, or claims
// that are not uuids. Callers map it to the uniform 401; the wrapped cause
// is for logs only and never reaches the client.
var ErrInvalidAccessToken = errors.New("platform: invalid access token")

// accessClaims carries exactly the spec section 4 claims: sub (user), sid
// (session), exp, iat — never org, never role (ADR-005 section 5: membership
// is re-checked per request and must not be cached in the token).
type accessClaims struct {
	jwt.RegisteredClaims
	SID string `json:"sid"`
}

// SignAccess mints an AccessTTL HS256 JWT stamped with the active kid. now is
// injected so tests can mint already-expired tokens without sleeping.
//
//	raw, err := cfg.SignAccess(userID, sessionID, time.Now())
func (c *Config) SignAccess(userID, sessionID uuid.UUID, now time.Time) (string, error) {
	claims := accessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			ExpiresAt: jwt.NewNumericDate(now.Add(c.AccessTTL)),
			IssuedAt:  jwt.NewNumericDate(now),
		},
		SID: sessionID.String(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token.Header["kid"] = c.ActiveKID
	secret, ok := c.JWTKeys[c.ActiveKID]
	if !ok {
		return "", fmt.Errorf("platform: sign access token: active kid %q has no configured key", c.ActiveKID)
	}
	raw, err := token.SignedString([]byte(secret))
	if err != nil {
		return "", fmt.Errorf("platform: sign access token: %w", err)
	}
	return raw, nil
}

// VerifyAccess parses and validates raw: HS256 only (alg=none and algorithm
// confusion fail like any forgery), the kid must name a configured key, and
// exp/iat are enforced by the parser. Any key in JWTKeys verifies, so
// rotation runs with old and new keys valid at once (spec section 4).
//
//	userID, sessionID, err := cfg.VerifyAccess(cookie.Value)
func (c *Config) VerifyAccess(raw string) (uuid.UUID, uuid.UUID, error) {
	claims := &accessClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, c.keyFor,
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt())
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("%w: %w", ErrInvalidAccessToken, err)
	}
	if claims.IssuedAt == nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("%w: iat claim missing", ErrInvalidAccessToken)
	}
	userID, err := uuid.Parse(claims.Subject)
	if err != nil || userID == uuid.Nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("%w: sub claim %q is not a uuid", ErrInvalidAccessToken, claims.Subject)
	}
	sessionID, err := uuid.Parse(claims.SID)
	if err != nil || sessionID == uuid.Nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("%w: sid claim %q is not a uuid", ErrInvalidAccessToken, claims.SID)
	}
	return userID, sessionID, nil
}

// keyFor resolves the token's kid header to its configured secret. The
// SigningMethodHMAC assertion pins HMAC inside the keyfunc too, so the check
// does not rest on parser options alone.
func (c *Config) keyFor(token *jwt.Token) (any, error) {
	if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
		return nil, fmt.Errorf("%w: alg %q is not HMAC", ErrInvalidAccessToken, token.Method.Alg())
	}
	kid, _ := token.Header["kid"].(string)
	secret, ok := c.JWTKeys[kid]
	if !ok {
		return nil, fmt.Errorf("%w: unknown kid %q", ErrInvalidAccessToken, kid)
	}
	return []byte(secret), nil
}
