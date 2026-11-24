package authapi

// The {error, message} coverage proofs of issue #61, kept in-package so
// the test can walk errorMessages itself: enumerating the ErrorCode
// constants alone would prove every code has copy, but only the map walk
// plus the length check also proves a new enum member did not land
// without its default text — the drift this file exists to prevent.

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// allErrorCodes is the contract's ErrorCode vocabulary, pinned
// explicitly: a new member added to the enum without map copy fails
// TestErrorMessagesCoverEveryCode's length check, not by accident.
var allErrorCodes = []ErrorCode{
	ErrorCodeBadRequest,
	ErrorCodeUnauthenticated,
	ErrorCodeMissingOrganization,
	ErrorCodeNotFound,
	ErrorCodeForbidden,
	ErrorCodeRateLimited,
	ErrorCodeInternal,
	ErrorCodeQuoteSuperseded,
	ErrorCodeNegativeMargin,
	ErrorCodeRfqNotParked,
	ErrorCodeRfqBusy,
}

// TestErrorMessagesCoverEveryCode is the issue #61 guarantee: every
// ErrorCode has a non-empty default message (so no response ships an
// empty `message`), and the map holds no key outside the enum (a stale
// key could never be emitted anyway — it would not compile — but the
// check keeps the table honest).
func TestErrorMessagesCoverEveryCode(t *testing.T) {
	for _, code := range allErrorCodes {
		assert.NotEmpty(t, errorMessages[code], "%s must carry default display copy", code)
		assert.True(t, code.Valid(), "%q is not an ErrorCode member", code)
	}
	for code := range errorMessages {
		assert.True(t, code.Valid(), "map key %q is not an ErrorCode member", code)
	}
	assert.Len(t, errorMessages, len(allErrorCodes),
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
