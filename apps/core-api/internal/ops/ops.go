// Package ops implements the ops console's read surface (ADR-005 platform
// path): GET /ops/quarantine, GET /ops/connectors and GET /ops/healing.
// These are platform reads — admission is RequirePlatformAdmin's per-request
// users.is_platform_admin check, not tenant membership, and every query
// runs as qp_app with no tenant RLS context: the Layer C registry tables
// (app.connectors, app.healing_sessions) are plain SELECTs through inboxdb
// — qp_app's only privilege on them — while the Layer A quarantine table
// is reachable solely through migration 0018's app.list_inbound_quarantine
// (the platform.Store function family, like every other Layer A access).
//
// oapi-codegen emits one StrictServerInterface for the whole contract, so
// these handlers mount on the auth strict surface through the auth.OpsAPI
// delegate seam — the same post-build wiring as the approver and the web
// surface (internal/quotes, internal/webapi).
package ops

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/inbox/inboxdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// quarantine page bounds — the contract documents 1..200 with a 50 default;
// the generated binder enforces neither, and the function clamps a second
// time (migration 0018). Clamping rather than 400ing matches the `q`
// truncation precedent on the web surface (internal/webapi).
const (
	defaultQuarantineLimit = 50
	maxQuarantineLimit     = 200
)

// CatalogQueries is the Layer C read seam — *inboxdb.Queries satisfies it,
// and the fake rig substitutes recorded rows. The ops surface has no tenant
// context, so these run on the bare pool: RLS is not the boundary here, the
// platform-admin gate is.
type CatalogQueries interface {
	ListOpsConnectors(ctx context.Context) ([]inboxdb.ListOpsConnectorsRow, error)
	ListOpsHealingSessions(ctx context.Context) ([]inboxdb.ListOpsHealingSessionsRow, error)
}

// QuarantineSource is the Layer A read seam — *platform.Store satisfies it;
// the fake rig substitutes recorded pages. app.inbound_quarantine grants
// qp_app nothing, so the function family is the only door (migration 0016).
type QuarantineSource interface {
	ListInboundQuarantine(ctx context.Context, limit, offset int) ([]platform.InboundQuarantineRow, int64, error)
}

// Endpoints implements the three ops operations the strict surface
// delegates. No tenant runner is held on purpose: the ops reads must NOT
// enter InTenantTx — a tenant GUC has no meaning on the platform path and
// pinning one would scope nothing while implying a tenant boundary that
// does not exist (ADR-005).
type Endpoints struct {
	catalog    CatalogQueries
	quarantine QuarantineSource
}

// NewEndpoints binds the surface. Production wires the sqlc catalog over
// the qp_app pool and the pool-backed Store:
//
//	endpoints.Ops = ops.NewEndpoints(inboxdb.New(db.Pool()), store)
func NewEndpoints(catalog CatalogQueries, quarantine QuarantineSource) *Endpoints {
	return &Endpoints{catalog: catalog, quarantine: quarantine}
}

// ListOpsConnectors implements GET /ops/connectors: the connector registry,
// slug-ordered. RequirePlatformAdmin already proved the caller; a missing
// user context means the route was mis-mounted without the chain — the
// fail-closed 401 reprocess.go set the precedent for.
func (e *Endpoints) ListOpsConnectors(
	ctx context.Context,
	_ authapi.ListOpsConnectorsRequestObject,
) (authapi.ListOpsConnectorsResponseObject, error) {
	if _, ok := platform.UserID(ctx); !ok {
		return connectorsUnauthenticated(), nil
	}
	rows, err := e.catalog.ListOpsConnectors(ctx)
	if err != nil {
		return nil, fmt.Errorf("ops: list connectors: %w", err)
	}
	items := make([]authapi.OpsConnector, 0, len(rows))
	for _, r := range rows {
		items = append(items, connectorItem(r))
	}
	return authapi.ListOpsConnectors200JSONResponse{Items: items}, nil
}

// ListOpsHealingSessions implements GET /ops/healing: every healing session,
// newest created_at first.
func (e *Endpoints) ListOpsHealingSessions(
	ctx context.Context,
	_ authapi.ListOpsHealingSessionsRequestObject,
) (authapi.ListOpsHealingSessionsResponseObject, error) {
	if _, ok := platform.UserID(ctx); !ok {
		return healingUnauthenticated(), nil
	}
	rows, err := e.catalog.ListOpsHealingSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("ops: list healing sessions: %w", err)
	}
	items := make([]authapi.OpsHealingSession, 0, len(rows))
	for _, r := range rows {
		items = append(items, healingItem(r))
	}
	return authapi.ListOpsHealingSessions200JSONResponse{Items: items}, nil
}

// ListOpsQuarantine implements GET /ops/quarantine: one page of the inbound
// drop table plus the full-table total.
func (e *Endpoints) ListOpsQuarantine(
	ctx context.Context,
	req authapi.ListOpsQuarantineRequestObject,
) (authapi.ListOpsQuarantineResponseObject, error) {
	if _, ok := platform.UserID(ctx); !ok {
		return quarantineUnauthenticated(), nil
	}
	limit, offset := quarantinePage(req.Params)
	rows, total, err := e.quarantine.ListInboundQuarantine(ctx, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("ops: list quarantine: %w", err)
	}
	items := make([]authapi.OpsQuarantineItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, quarantineItem(r))
	}
	return authapi.ListOpsQuarantine200JSONResponse{Items: items, Total: int(total)}, nil
}

// quarantinePage clamps the wire page request to the function's bounds —
// absent params take the defaults, out-of-range values clamp, so the
// function's own LEAST/GREATEST is the second fence, not the only one.
func quarantinePage(p authapi.ListOpsQuarantineParams) (limit, offset int) {
	limit, offset = defaultQuarantineLimit, 0
	if p.Limit != nil {
		limit = *p.Limit
	}
	if p.Offset != nil {
		offset = *p.Offset
	}
	if limit < 1 {
		limit = 1
	}
	if limit > maxQuarantineLimit {
		limit = maxQuarantineLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// connectorItem maps one sqlc row to the contract's camelCase shape;
// confidence_score renders as a decimal string — never a float (AGENTS.md),
// the Money/marginPercent precedent.
func connectorItem(r inboxdb.ListOpsConnectorsRow) authapi.OpsConnector {
	var score *string
	if r.ConfidenceScore != nil {
		s := r.ConfidenceScore.String()
		score = &s
	}
	return authapi.OpsConnector{
		ID:              openapi_types.UUID(r.ID.Bytes),
		Slug:            r.Slug,
		DisplayName:     r.DisplayName,
		TargetURL:       r.TargetUrl,
		Status:          authapi.OpsConnectorStatus(r.Status),
		ConfidenceScore: score,
		LastScrapedAt:   pgTime(r.LastScrapedAt),
		CreatedAt:       r.CreatedAt.Time,
		UpdatedAt:       r.UpdatedAt.Time,
	}
}

// healingItem maps one healing_sessions row; the nullable columns pass
// through as pointers — null means the session never reached that step.
func healingItem(r inboxdb.ListOpsHealingSessionsRow) authapi.OpsHealingSession {
	return authapi.OpsHealingSession{
		ID:                       openapi_types.UUID(r.ID.Bytes),
		ConnectorID:              openapi_types.UUID(r.ConnectorID.Bytes),
		Status:                   authapi.OpsHealingStatus(r.Status),
		FailingSnapshotID:        pgUUID(r.FailingSnapshotID),
		FailingPayloadHash:       r.FailingPayloadHash,
		FailingPayloadStorageKey: r.FailingPayloadStorageKey,
		DevinSessionID:           r.DevinSessionID,
		DevinSessionURL:          r.DevinSessionUrl,
		GithubPrURL:              r.GithubPrUrl,
		PytestPassed:             r.PytestPassed,
		CreatedAt:                r.CreatedAt.Time,
		ResolvedAt:               pgTime(r.ResolvedAt),
	}
}

// quarantineItem maps one function row to the contract item; matched_org_ids
// converts element-wise because openapi_types.UUID is a defined type, not an
// alias of uuid.UUID.
func quarantineItem(r platform.InboundQuarantineRow) authapi.OpsQuarantineItem {
	matched := make([]openapi_types.UUID, 0, len(r.MatchedOrgIDs))
	for _, id := range r.MatchedOrgIDs {
		matched = append(matched, openapi_types.UUID(id))
	}
	return authapi.OpsQuarantineItem{
		ID:              openapi_types.UUID(r.ID),
		ResendMessageID: r.ResendMessageID,
		Reason:          authapi.OpsQuarantineReason(r.Reason),
		Recipients:      r.Recipients,
		MatchedOrgIds:   matched,
		Subject:         r.Subject,
		FromAddr:        r.FromAddr,
		ReceivedAt:      r.ReceivedAt,
		QuarantinedAt:   r.QuarantinedAt,
	}
}

// pgTime renders a nullable timestamptz as the contract's *time.Time.
func pgTime(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

// pgUUID renders a nullable uuid as the contract's *openapi_types.UUID.
func pgUUID(u pgtype.UUID) *openapi_types.UUID {
	if !u.Valid {
		return nil
	}
	id := openapi_types.UUID(u.Bytes)
	return &id
}

// The mis-mount 401s — one per operation because the response objects are
// per-operation types (the same body the middleware emits for a bad token).
func connectorsUnauthenticated() authapi.ListOpsConnectorsResponseObject {
	return authapi.ListOpsConnectors401JSONResponse{
		UnauthorizedJSONResponse: authapi.UnauthorizedJSONResponse(authapi.NewError(authapi.ErrorCodeUnauthenticated)),
	}
}

func healingUnauthenticated() authapi.ListOpsHealingSessionsResponseObject {
	return authapi.ListOpsHealingSessions401JSONResponse{
		UnauthorizedJSONResponse: authapi.UnauthorizedJSONResponse(authapi.NewError(authapi.ErrorCodeUnauthenticated)),
	}
}

func quarantineUnauthenticated() authapi.ListOpsQuarantineResponseObject {
	return authapi.ListOpsQuarantine401JSONResponse{
		UnauthorizedJSONResponse: authapi.UnauthorizedJSONResponse(authapi.NewError(authapi.ErrorCodeUnauthenticated)),
	}
}
