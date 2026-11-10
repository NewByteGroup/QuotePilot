package platform

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// opaqueTokenBytes is the entropy spec section 4 fixes for login and refresh
// tokens: 32 bytes from crypto/rand. The token itself travels only in the
// email link or the cookie and is never stored or logged — Postgres keeps
// its SHA-256 hex.
const opaqueTokenBytes = 32

// NewOpaqueToken returns a fresh 32-byte CSPRNG token in base64url
// (RawURLEncoding: no padding, URL- and cookie-safe), always 43 chars.
//
//	token, err := platform.NewOpaqueToken()
func NewOpaqueToken() (string, error) {
	var b [opaqueTokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("platform: read crypto/rand for opaque token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// HashToken returns the SHA-256 hex of a token — the only shape Postgres
// ever sees (spec section 4). Deterministic by design: consume and rotate
// look rows up by it.
//
//	hash := platform.HashToken(token) // store this; never the token
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
