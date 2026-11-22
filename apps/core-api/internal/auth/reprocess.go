package auth

import (
	"context"
	"errors"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
)

// RfqReprocessor is the POST /v1/rfqs/{id}/reprocess implementation the
// strict surface delegates to — internal/rfqs provides it. oapi-codegen
// emits one StrictServerInterface for the whole contract, so this surface
// must expose reprocessRfq even though the handler lives in its own
// package.
type RfqReprocessor interface {
	ReprocessRfq(ctx context.Context, request authapi.ReprocessRfqRequestObject) (authapi.ReprocessRfqResponseObject, error)
}

// ReprocessRfq forwards to the injected RfqReprocessor. It is assigned
// post-build — the same seam assignment shape as Approver — because the
// delegate needs the River client for the in-transaction resume enqueue,
// and the client exists only after the worker registry is built. A nil
// delegate means a wiring bug, which surfaces through writeInternal like
// any other server fault rather than answering on an unwired route.
func (e *Endpoints) ReprocessRfq(ctx context.Context, req authapi.ReprocessRfqRequestObject) (authapi.ReprocessRfqResponseObject, error) {
	if e.Reprocessor == nil {
		return nil, errors.New("auth: reprocess rfq: reprocessor not wired")
	}
	return e.Reprocessor.ReprocessRfq(ctx, req)
}
