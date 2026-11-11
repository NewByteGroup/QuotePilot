package auth

import (
	"context"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// GetMe implements GET /me (spec section 8): the caller's own profile plus
// the org-switcher membership list, flattened onto one object — where POST
// /auth/session nests the same reads under `profile`, this response keeps
// them at the top level. The route mounts behind platform.Authenticate at
// the mux level (authapi/doc.go), so a verified qp_access JWT has already
// planted sub/sid on ctx: the handler never re-reads the token and never
// re-checks sid — Authenticate is deliberately stateless (spec section 4),
// so a logged-out-but-unexpired JWT still answers until its exp.
//
// An absent user context means the route was mis-mounted without
// Authenticate; it fails closed with the same 401 the middleware emits —
// ResolveTenant answers the identical bug the same way, because a request
// that skipped authentication is unauthenticated, not a server fault.
//
//	resp, err := endpoints.GetMe(ctx, authapi.GetMeRequestObject{})
func (e *Endpoints) GetMe(
	ctx context.Context,
	_ authapi.GetMeRequestObject,
) (authapi.GetMeResponseObject, error) {
	userID, ok := platform.UserID(ctx)
	if !ok {
		return meUnauthenticated(), nil
	}
	profile, memberships, err := e.loadProfile(ctx, e.store, userID)
	if err != nil {
		return nil, err
	}
	return authapi.GetMe200JSONResponse{
		ID:          profile.ID,
		Email:       profile.Email,
		FullName:    profile.FullName,
		Memberships: memberships,
	}, nil
}

// meUnauthenticated is the fail-closed answer for a request that reached
// the handler without Authenticate's planted context — the same
// {"error":"unauthenticated"} the middleware emits for a bad token, so a
// mis-mount denies rather than leaks.
func meUnauthenticated() authapi.GetMeResponseObject {
	return authapi.GetMe401JSONResponse{
		UnauthorizedJSONResponse: authapi.UnauthorizedJSONResponse{Error: authapi.ErrorCodeUnauthenticated},
	}
}
