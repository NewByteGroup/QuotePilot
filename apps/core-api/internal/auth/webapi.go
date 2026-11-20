package auth

import (
	"context"
	"errors"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
)

// WebAPI is the web v1 read surface the strict server delegates to —
// internal/webapi provides it (GET /v1/session, POST
// /v1/session/organization, GET /v1/inbox/quotes, GET /v1/quotes/{id}).
// oapi-codegen emits one StrictServerInterface for the whole contract, so
// this surface must expose all four even though the handlers live in
// their own package — the same seam shape as QuoteApprover.
type WebAPI interface {
	GetSession(ctx context.Context, request authapi.GetSessionRequestObject) (authapi.GetSessionResponseObject, error)
	SwitchOrganization(ctx context.Context, request authapi.SwitchOrganizationRequestObject) (authapi.SwitchOrganizationResponseObject, error)
	ListInboxQuotes(ctx context.Context, request authapi.ListInboxQuotesRequestObject) (authapi.ListInboxQuotesResponseObject, error)
	GetQuoteDetail(ctx context.Context, request authapi.GetQuoteDetailRequestObject) (authapi.GetQuoteDetailResponseObject, error)
}

// errWebNotWired means cmd/server forgot the post-build Web assignment —
// a wiring bug that surfaces through writeInternal like any other server
// fault rather than answering on an unwired route.
var errWebNotWired = errors.New("auth: webapi delegate not wired")

// GetSession forwards to the injected WebAPI; see WebAPI's doc for why
// the delegate exists.
func (e *Endpoints) GetSession(ctx context.Context, req authapi.GetSessionRequestObject) (authapi.GetSessionResponseObject, error) {
	if e.Web == nil {
		return nil, errWebNotWired
	}
	return e.Web.GetSession(ctx, req)
}

// SwitchOrganization forwards to the injected WebAPI.
func (e *Endpoints) SwitchOrganization(ctx context.Context, req authapi.SwitchOrganizationRequestObject) (authapi.SwitchOrganizationResponseObject, error) {
	if e.Web == nil {
		return nil, errWebNotWired
	}
	return e.Web.SwitchOrganization(ctx, req)
}

// ListInboxQuotes forwards to the injected WebAPI.
func (e *Endpoints) ListInboxQuotes(ctx context.Context, req authapi.ListInboxQuotesRequestObject) (authapi.ListInboxQuotesResponseObject, error) {
	if e.Web == nil {
		return nil, errWebNotWired
	}
	return e.Web.ListInboxQuotes(ctx, req)
}

// GetQuoteDetail forwards to the injected WebAPI.
func (e *Endpoints) GetQuoteDetail(ctx context.Context, req authapi.GetQuoteDetailRequestObject) (authapi.GetQuoteDetailResponseObject, error) {
	if e.Web == nil {
		return nil, errWebNotWired
	}
	return e.Web.GetQuoteDetail(ctx, req)
}
