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

// TestTruncateClassifyBody pins the ADR-006 16 KiB classify input bound:
// the cut never splits a multi-byte rune and short bodies pass through.
func TestTruncateClassifyBody(t *testing.T) {
	short := "small body"
	assert.Equal(t, short, truncateClassifyBody(short), "short body passes through")

	exact := strings.Repeat("a", classifyBodyLimit)
	assert.Equal(t, exact, truncateClassifyBody(exact), "exact limit passes through")

	over := strings.Repeat("a", classifyBodyLimit+1)
	assert.Len(t, truncateClassifyBody(over), classifyBodyLimit)

	// A multi-byte rune straddling the boundary: "é" is 2 bytes, so a
	// cut at an odd offset would split it — the helper must back off to
	// the last valid boundary.
	multibyte := strings.Repeat("a", classifyBodyLimit-1) + "é"
	got := truncateClassifyBody(multibyte)
	assert.Len(t, got, classifyBodyLimit-1, "cut backs off to the rune boundary")
	assert.True(t, utf8.ValidString(got))
}
