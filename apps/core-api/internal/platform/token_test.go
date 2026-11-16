package platform_test

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

var base64urlPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// TestNewOpaqueTokenShape pins the wire format: 32 bytes in unpadded
// base64url is always exactly 43 chars from the URL-safe alphabet.
func TestNewOpaqueTokenShape(t *testing.T) {
	token, err := platform.NewOpaqueToken()
	require.NoError(t, err)
	assert.Len(t, token, 43)
	assert.Regexp(t, base64urlPattern, token, "token must be RawURLEncoding base64url")
	assert.NotContains(t, token, "=", "no padding on the wire")
}

// TestHashTokenDeterminism pins the at-rest shape: 64 lowercase hex chars,
// stable for the same input, different inputs different hashes.
func TestHashTokenDeterminism(t *testing.T) {
	h1 := platform.HashToken("abc")
	h2 := platform.HashToken("abc")
	assert.Equal(t, h1, h2, "hash must be deterministic — lookups key on it")
	assert.Len(t, h1, 64, "SHA-256 hex is 64 chars")
	assert.Regexp(t, `^[0-9a-f]{64}$`, h1)
	assert.NotEqual(t, h1, platform.HashToken("abd"))
}

// TestNewOpaqueTokenUniqueness guards against a broken entropy source: N
// draws must be distinct.
func TestNewOpaqueTokenUniqueness(t *testing.T) {
	seen := make(map[string]struct{}, 512)
	for i := 0; i < 512; i++ {
		token, err := platform.NewOpaqueToken()
		require.NoError(t, err)
		require.NotContains(t, seen, token, "duplicate token at draw %d", i)
		seen[token] = struct{}{}
	}
}
