package auth

// The argon2id helpers and the timing-mitigation dummy are package-private
// by design — the endpoint is the only production caller — so this is the
// package's one internal test file: it pins the PHC contract and the dummy
// without widening the API. What it proves: hashPassword emits the
// RFC 9106-profile argon2id PHC, verifyPassword honors the parameters inside
// the string, a tampered or foreign hash verifies false, and the dummy the
// unknown-email path burns time on can never authenticate.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHashPasswordPHCShape pins the at-rest format: the $argon2id$v=19$
// prefix, the RFC 9106 memory-constrained parameters in the middle section,
// and a fresh salt per call — two hashes of one password must differ (the
// salt is what makes equal passwords hash differently).
func TestHashPasswordPHCShape(t *testing.T) {
	first, err := hashPassword("correct horse battery staple")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(first, "$argon2id$v=19$m=19456,t=2,p=1$"),
		"the stored format must be the RFC 9106-profile PHC, got %q", first)

	second, err := hashPassword("correct horse battery staple")
	require.NoError(t, err)
	assert.NotEqual(t, first, second, "a fresh salt means equal passwords hash differently")
}

// TestVerifyPasswordRoundTrip is the encode→verify contract: the right
// password verifies, a wrong one does not.
func TestVerifyPasswordRoundTrip(t *testing.T) {
	phc, err := hashPassword("s3cret-pilot")
	require.NoError(t, err)

	assert.True(t, verifyPassword("s3cret-pilot", phc))
	assert.False(t, verifyPassword("s3cret-pil0t", phc), "one wrong byte must fail")
	assert.False(t, verifyPassword("", phc), "an empty password is just a wrong one")
}

// TestVerifyPasswordForeignHashes rejects the shapes the column must never
// authenticate: a tampered key byte, non-PHC text, the argon2i variant, and
// a truncated string all verify false — never panic, never error a caller
// that maps false to the uniform 401.
func TestVerifyPasswordForeignHashes(t *testing.T) {
	phc, err := hashPassword("pw")
	require.NoError(t, err)

	// Flip one character inside the key (the last PHC section).
	tampered := phc[:len(phc)-2] + "AA"
	cases := []struct {
		name string
		phc  string
	}{
		{"tampered key", tampered},
		{"empty", ""},
		{"not a hash", "bcrypt$2y$10$..."},
		{"argon2i variant", "$argon2i$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		{"missing sections", "$argon2id$v=19$m=19456,t=2,p=1$"},
		{"bad base64 salt", "$argon2id$v=19$m=19456,t=2,p=1$!!!$AAAA"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.False(t, verifyPassword("pw", tc.phc))
		})
	}
}

// TestDummyPasswordHashNeverVerifies pins the timing-mitigation contract:
// the dummy must parse as well-formed PHC (the unknown-email path must pay
// a real argon2id run, not the fast malformed-hash reject) and must verify
// false for anything presented.
func TestDummyPasswordHashNeverVerifies(t *testing.T) {
	_, _, _, err := parsePHC(dummyPasswordHash)
	require.NoError(t, err, "the dummy must be well-formed PHC — a parse failure would be a free early exit")
	assert.False(t, verifyPassword("anything-at-all", dummyPasswordHash))
	assert.False(t, verifyPassword("", dummyPasswordHash))
}
