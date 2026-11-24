package authapi

// errorMessages is the default display copy each ErrorCode carries on the
// wire (issue #61): the converged body is {error, message} — the code for
// machines, this string for people. One entry per enum member keeps the
// guarantee that no response ever ships an empty message; a handler may
// still override Message with contextual text on its own response type.
var errorMessages = map[ErrorCode]string{
	ErrorCodeBadRequest:          "the request is malformed or invalid",
	ErrorCodeUnauthenticated:     "missing or invalid credentials",
	ErrorCodeMissingOrganization: "an organization hint is required — pass the X-Organization-Id header or the qp_org cookie",
	ErrorCodeNotFound:            "the requested resource does not exist",
	ErrorCodeForbidden:           "you do not have permission to perform this action",
	ErrorCodeRateLimited:         "too many requests — retry later",
	ErrorCodeInternal:            "an unexpected server error occurred",
	ErrorCodeQuoteSuperseded:     "this quote version was superseded — approve the latest version",
	ErrorCodeNegativeMargin:      "the pinned price is below cost — reprice before approving",
	ErrorCodeRfqNotParked:        "the RFQ is not in a reprocessable state",
	ErrorCodeRfqBusy:             "a worker holds this RFQ's quote lock — retry shortly",
}

// fallbackErrorMessage is the last-resort copy for a code absent from
// errorMessages — a new enum member that landed without its text must
// still answer a non-empty, display-safe string.
const fallbackErrorMessage = "an unexpected error occurred"

// ErrorMessage returns the default human-readable text for code — the
// `message` member of the contract's {error, message} body (issue #61).
// An unknown code yields fallbackErrorMessage, never "".
//
//	msg := authapi.ErrorMessage(authapi.ErrorCodeRateLimited)
func ErrorMessage(code ErrorCode) string {
	if msg, ok := errorMessages[code]; ok {
		return msg
	}
	return fallbackErrorMessage
}

// NewError builds the contract's {error, message} pair for code with its
// default copy — the value every Error-derived response struct embeds.
//
//	return authapi.NotFoundJSONResponse(authapi.NewError(authapi.ErrorCodeNotFound))
func NewError(code ErrorCode) Error {
	return Error{Error: code, Message: ErrorMessage(code)}
}
