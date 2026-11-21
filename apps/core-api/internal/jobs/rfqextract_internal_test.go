package jobs

// Internal unit tests for the unexported helpers — the repo's internal-
// test convention (webapi/map_internal_test.go) covers unexported logic
// the external suite can only reach through the full worker.

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

// TestTruncateBodyBytes pins the rune-safe byte cap shared by the
// classify (16 KiB) and extract (64 KiB) limits: the cut never splits a
// multi-byte rune and short bodies pass through.
func TestTruncateBodyBytes(t *testing.T) {
	short := "small body"
	assert.Equal(t, short, truncateBodyBytes(short, classifyBodyLimit), "short body passes through")

	exact := strings.Repeat("a", classifyBodyLimit)
	assert.Equal(t, exact, truncateBodyBytes(exact, classifyBodyLimit), "exact limit passes through")

	over := strings.Repeat("a", classifyBodyLimit+1)
	assert.Len(t, truncateBodyBytes(over, classifyBodyLimit), classifyBodyLimit)

	// A multi-byte rune straddling the boundary: "é" is 2 bytes, so a
	// cut at an odd offset would split it — the helper must back off to
	// the last valid boundary.
	multibyte := strings.Repeat("a", classifyBodyLimit-1) + "é"
	got := truncateBodyBytes(multibyte, classifyBodyLimit)
	assert.Len(t, got, classifyBodyLimit-1, "cut backs off to the rune boundary")
	assert.True(t, utf8.ValidString(got))

	// The limit is the caller's, not a constant: under the wider extract
	// cap the same input passes through untouched.
	assert.Equal(t, over, truncateBodyBytes(over, extractBodyLimit))
}
