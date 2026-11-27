package auth

import (
	"context"
	"errors"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
)

// OpsAPI is the ops console's read surface the strict server delegates to —
// internal/ops provides it (GET /ops/quarantine, /ops/connectors,
// /ops/healing). oapi-codegen emits one StrictServerInterface for the whole
// contract, so this surface must expose all three even though the handlers
// live in their own package — the same seam shape as WebAPI and
// RfqReprocessor. The RequirePlatformAdmin middleware, not the handlers,
// owns the admission decision; it mounts around this surface in
// cmd/server's buildMux.
type OpsAPI interface {
	ListOpsQuarantine(ctx context.Context, request authapi.ListOpsQuarantineRequestObject) (authapi.ListOpsQuarantineResponseObject, error)
	ListOpsConnectors(ctx context.Context, request authapi.ListOpsConnectorsRequestObject) (authapi.ListOpsConnectorsResponseObject, error)
	ListOpsHealingSessions(ctx context.Context, request authapi.ListOpsHealingSessionsRequestObject) (authapi.ListOpsHealingSessionsResponseObject, error)
}

// errOpsNotWired means cmd/server forgot the post-build Ops assignment —
// a wiring bug that surfaces through writeInternal like any other server
// fault rather than answering on an unwired route.
var errOpsNotWired = errors.New("auth: ops delegate not wired")

// ListOpsQuarantine forwards to the injected OpsAPI; see OpsAPI's doc for
// why the delegate exists.
func (e *Endpoints) ListOpsQuarantine(ctx context.Context, req authapi.ListOpsQuarantineRequestObject) (authapi.ListOpsQuarantineResponseObject, error) {
	if e.Ops == nil {
		return nil, errOpsNotWired
	}
	return e.Ops.ListOpsQuarantine(ctx, req)
}

// ListOpsConnectors forwards to the injected OpsAPI.
func (e *Endpoints) ListOpsConnectors(ctx context.Context, req authapi.ListOpsConnectorsRequestObject) (authapi.ListOpsConnectorsResponseObject, error) {
	if e.Ops == nil {
		return nil, errOpsNotWired
	}
	return e.Ops.ListOpsConnectors(ctx, req)
}

// ListOpsHealingSessions forwards to the injected OpsAPI.
func (e *Endpoints) ListOpsHealingSessions(ctx context.Context, req authapi.ListOpsHealingSessionsRequestObject) (authapi.ListOpsHealingSessionsResponseObject, error) {
	if e.Ops == nil {
		return nil, errOpsNotWired
	}
	return e.Ops.ListOpsHealingSessions(ctx, req)
}
