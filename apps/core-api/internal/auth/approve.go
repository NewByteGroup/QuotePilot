package auth

import (
	"context"
	"errors"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
)

// QuoteApprover is the POST /quotes/{id}/approve implementation the strict
// surface delegates to — internal/quotes provides it. oapi-codegen emits
// one StrictServerInterface for the whole contract, so this surface must
// expose approveQuote even though the handler lives in its own package.
type QuoteApprover interface {
	ApproveQuote(ctx context.Context, request authapi.ApproveQuoteRequestObject) (authapi.ApproveQuoteResponseObject, error)
}

// ApproveQuote forwards to the injected QuoteApprover. It is assigned
// post-build — the same seam assignment shape as
// jobs.RfqIngestWorker.Inserter — because the delegate needs the River
// client for the in-transaction send enqueue, and the client exists only
// after the worker registry is built. A nil delegate means a wiring bug,
// which surfaces through writeInternal like any other server fault rather
// than answering 200 on an unwired route.
func (e *Endpoints) ApproveQuote(ctx context.Context, req authapi.ApproveQuoteRequestObject) (authapi.ApproveQuoteResponseObject, error) {
	if e.Approver == nil {
		return nil, errors.New("auth: approve quote: approver not wired")
	}
	return e.Approver.ApproveQuote(ctx, req)
}
