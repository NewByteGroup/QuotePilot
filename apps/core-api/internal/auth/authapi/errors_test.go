package authapi

// The {error, message} coverage proofs of issue #61, kept in-package so
// the test can walk errorMessages itself: enumerating the ErrorCode
// constants alone would prove every code has copy, but only the map walk
// plus the length check also proves a new enum member did not land
// without its default text — the drift this file exists to prevent.

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// declaredErrorCodes reads the enum straight out of authapi.gen.go — the
// constants the contract itself declares. A handwritten list would let a
// new ErrorCode member land without map copy and stay green (the list
// and the map would both be stale); reading the generated declarations
// makes the coverage check self-updating.
func declaredErrorCodes(t *testing.T) []ErrorCode {
	t.Helper()
	src, err := os.ReadFile("authapi.gen.go")
	require.NoError(t, err)
	decls := regexp.MustCompile(`ErrorCode\w+\s+ErrorCode = "(\w+)"`).FindAllStringSubmatch(string(src), -1)
	require.NotEmpty(t, decls, "no ErrorCode declarations found in authapi.gen.go")
	codes := make([]ErrorCode, len(decls))
	for i, d := range decls {
		codes[i] = ErrorCode(d[1])
	}
	return codes
}

// TestErrorMessagesCoverEveryCode is the issue #61 guarantee: every
// ErrorCode has a non-empty default message (so no response ships an
// empty `message`), and the map holds no key outside the enum (a stale
// key could never be emitted anyway — it would not compile — but the
// check keeps the table honest).
func TestErrorMessagesCoverEveryCode(t *testing.T) {
	declared := declaredErrorCodes(t)
	for _, code := range declared {
		assert.NotEmpty(t, errorMessages[code], "%s must carry default display copy", code)
		assert.True(t, code.Valid(), "%q is not an ErrorCode member", code)
	}
	for code := range errorMessages {
		assert.True(t, code.Valid(), "map key %q is not an ErrorCode member", code)
	}
	assert.Len(t, errorMessages, len(declared),
		"every ErrorCode needs a default message — add copy for a new enum member")
}

// TestErrorMessageUnknownCode pins the fallback: a code the map does not
// know still answers a non-empty, display-safe string — the contract's
// required `message` can never ship empty.
func TestErrorMessageUnknownCode(t *testing.T) {
	assert.Equal(t, fallbackErrorMessage, ErrorMessage(ErrorCode("future_code")))
	assert.NotEmpty(t, ErrorMessage(""))
}

// TestNewError pins the pair NewError builds: the code verbatim and its
// default copy — the two required members of the converged body.
func TestNewError(t *testing.T) {
	body := NewError(ErrorCodeNotFound)
	assert.Equal(t, ErrorCodeNotFound, body.Error)
	assert.Equal(t, errorMessages[ErrorCodeNotFound], body.Message)
}
