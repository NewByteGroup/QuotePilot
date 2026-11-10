package auth

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// GetMeOrganization implements GET /me/organization (spec section 8) — the
// mission's end-to-end proof: Authenticate verified the qp_access JWT,
// ResolveTenant re-checked live membership and planted org and role on ctx,
// and the handler reads the registry row through get_current_organization()
// inside the tenant read transaction. The org id handed to InTenantReadTx is
// the same id RLS then scopes the GUC to, so the function's row and the
// transaction's tenant can never disagree by construction.
//
// An absent org or role context means the route was mis-mounted without the
// chain's tail; it fails closed with the same 401 the middleware emits —
// me.go's rule: a request that skipped authentication is unauthenticated,
// not a server fault. (Org and role are planted together, so the role check
// can only trip on a partial mount that cannot be built through the public
// chain today — it is checked anyway so a future planter that separates them
// still denies.)
func (e *Endpoints) GetMeOrganization(
	ctx context.Context,
	_ authapi.GetMeOrganizationRequestObject,
) (authapi.GetMeOrganizationResponseObject, error) {
	orgID, ok := platform.OrgID(ctx)
	if !ok {
		return meOrgUnauthenticated(), nil
	}
	if _, ok := platform.Role(ctx); !ok {
		return meOrgUnauthenticated(), nil
	}
	org, err := e.currentOrganization(ctx, orgID)
	if err != nil {
		return nil, err
	}
	branding, err := orgBranding(org.Branding)
	if err != nil {
		return nil, err
	}
	return authapi.GetMeOrganization200JSONResponse{
		ID:                org.ID.String(),
		Name:              org.Name,
		Slug:              org.Slug,
		InboundEmailAlias: org.InboundEmailAlias,
		BrandingJSON:      branding,
	}, nil
}

// currentOrganization is the tenant read: InTenantReadTx sets the
// app.current_organization_id GUC for orgID, and get_current_organization()
// resolves the row from that same GUC. A miss cannot mean "org does not
// exist" — ResolveTenant already proved membership — so found=false is
// contract drift, surfaced through the uniform 500 like any driver fault
// rather than as a 404 a caller could read existence into.
func (e *Endpoints) currentOrganization(ctx context.Context, orgID uuid.UUID) (platform.Organization, error) {
	var (
		org   platform.Organization
		found bool
	)
	err := e.runner.InTenantReadTx(ctx, orgID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		org, found, err = platform.NewStore(tx).GetCurrentOrganization(ctx)
		return err
	})
	if err != nil {
		return platform.Organization{}, fmt.Errorf("auth: get current organization: %w", err)
	}
	if !found {
		return platform.Organization{}, fmt.Errorf(
			"auth: get current organization: no row for member org %s", orgID)
	}
	return org, nil
}

// orgBranding decodes the jsonb branding blob for the contract's required
// branding_json field. The column is NOT NULL DEFAULT '{}' (migration 0002),
// so an empty or JSON-null blob maps to the empty object — the contract has
// no null variant for a required object. A non-object or unparseable blob is
// store contract drift and rides the same error path to the uniform 500.
func orgBranding(raw json.RawMessage) (map[string]interface{}, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]interface{}{}, nil
	}
	out := map[string]interface{}{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("auth: decode branding_json: %w", err)
	}
	return out, nil
}

// meOrgUnauthenticated is the fail-closed answer for a request that reached
// the handler without the chain's planted context — the same
// {"error":"unauthenticated"} the middleware emits for a bad token, so a
// mis-mount denies rather than leaks.
func meOrgUnauthenticated() authapi.GetMeOrganizationResponseObject {
	return authapi.GetMeOrganization401JSONResponse{
		UnauthorizedJSONResponse: authapi.UnauthorizedJSONResponse{Error: authapi.ErrorCodeUnauthenticated},
	}
}
