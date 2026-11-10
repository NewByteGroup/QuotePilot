package auth

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// The spec section 4 cookie pair. qp_access carries the JWT to every route —
// Authenticate reads it; qp_refresh is scoped to Path=/auth so it only ever
// travels to the session endpoints, and SameSite=Strict keeps it off
// cross-site requests entirely.
const (
	accessCookieName  = "qp_access"
	refreshCookieName = "qp_refresh"
)

// PostAuthSession implements POST /auth/session (spec section 8): the
// magic-link token is consumed exactly once — by SHA-256 hash, the raw value
// never approaches the store — and success mints refresh generation 0, signs
// the access JWT and sets the spec section 4 cookie pair around the
// profile+memberships body the org switcher renders.
//
// Used, expired and unknown tokens are one failure by construction:
// consume_login_token collapses all three to ok=false inside its row lock,
// and the handler answers with a single response object, so the wire cannot
// distinguish the cases even by accident. The two payload reads run before
// create_session so a driver fault mid-response cannot orphan a session row
// — the only write on the path happens once the body is known to assemble.
func (e *Endpoints) PostAuthSession(
	ctx context.Context,
	request authapi.PostAuthSessionRequestObject,
) (authapi.PostAuthSessionResponseObject, error) {
	if request.Body == nil || request.Body.Token == "" {
		return sessionBadRequest(), nil
	}
	userID, ok, err := e.store.ConsumeLoginToken(ctx, platform.HashToken(request.Body.Token))
	if err != nil {
		return nil, fmt.Errorf("auth: consume login token: %w", err)
	}
	if !ok {
		return sessionUnauthenticated(), nil
	}
	profile, memberships, err := e.loadSessionPayload(ctx, userID)
	if err != nil {
		return nil, err
	}
	refresh, sessionID, err := e.mintSession(ctx, userID)
	if err != nil {
		return nil, err
	}
	access, err := e.cfg.SignAccess(userID, sessionID, e.now())
	if err != nil {
		return nil, fmt.Errorf("auth: sign access token: %w", err)
	}
	return sessionOK{
		body: authapi.PostAuthSession200JSONResponse{
			Profile:     profile,
			Memberships: memberships,
		},
		access:  e.accessCookie(access),
		refresh: e.refreshCookie(refresh),
	}, nil
}

// loadSessionPayload reads the 200 body the switcher needs: the user's own
// profile plus the four-column membership list keyed on that profile's email
// (spec section 3.3 — one lookup carries id, name, slug and role). A consume
// that succeeded names a real user — the token row's FK protects it — so a
// missing profile is driver contract drift, answered 500 through the error
// path rather than an empty 200.
func (e *Endpoints) loadSessionPayload(
	ctx context.Context,
	userID uuid.UUID,
) (authapi.UserProfile, []authapi.Membership, error) {
	id, email, fullName, ok, err := e.store.GetUserProfile(ctx, userID)
	if err != nil {
		return authapi.UserProfile{}, nil, fmt.Errorf("auth: get user profile: %w", err)
	}
	if !ok {
		return authapi.UserProfile{}, nil, fmt.Errorf("auth: get user profile: no row for consumed user %s", userID)
	}
	rows, err := e.store.ListMembershipsForEmail(ctx, email)
	if err != nil {
		return authapi.UserProfile{}, nil, fmt.Errorf("auth: list memberships: %w", err)
	}
	memberships := make([]authapi.Membership, 0, len(rows))
	for _, m := range rows {
		memberships = append(memberships, toAPIMembership(m))
	}
	return authapi.UserProfile{ID: id.String(), Email: email, FullName: fullName}, memberships, nil
}

// toAPIMembership maps one platform membership row to the contract model.
// Role is cast, not validated: migration 0002's enum CHECK means only the
// three contract values can ever leave the store.
func toAPIMembership(m platform.Membership) authapi.Membership {
	return authapi.Membership{
		OrganizationID: m.OrgID.String(),
		Name:           m.Name,
		Slug:           m.Slug,
		Role:           authapi.MembershipRole(m.Role),
	}
}

// mintSession creates refresh generation 0: a fresh opaque token whose hash —
// never the token — roots the new family, expiring at now+RefreshTTL (spec
// section 4: sliding 30 days). The returned token is the cookie value; the
// session id becomes the JWT's sid claim.
func (e *Endpoints) mintSession(ctx context.Context, userID uuid.UUID) (string, uuid.UUID, error) {
	refresh, err := platform.NewOpaqueToken()
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("auth: mint refresh token: %w", err)
	}
	sessionID, err := e.store.CreateSession(
		ctx, userID, platform.HashToken(refresh), e.now().Add(e.cfg.RefreshTTL))
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("auth: create session: %w", err)
	}
	return refresh, sessionID, nil
}

// accessCookie builds the spec section 4 transport for the JWT: Path=/ so
// every route receives it, SameSite=Lax so top-level navigations still carry
// it. Secure rides cfg.CookieSecure — false only for local http.
func (e *Endpoints) accessCookie(value string) *http.Cookie {
	return &http.Cookie{
		Name:     accessCookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   int(e.cfg.AccessTTL.Seconds()),
		HttpOnly: true,
		Secure:   e.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	}
}

// refreshCookie builds the scoped transport: Path=/auth delivers it only to
// the session endpoints and SameSite=Strict pins it to first-party requests
// (spec section 4's tightening over ADR-005).
func (e *Endpoints) refreshCookie(value string) *http.Cookie {
	return &http.Cookie{
		Name:     refreshCookieName,
		Value:    value,
		Path:     "/auth",
		MaxAge:   int(e.cfg.RefreshTTL.Seconds()),
		HttpOnly: true,
		Secure:   e.cfg.CookieSecure,
		SameSite: http.SameSiteStrictMode,
	}
}

// sessionOK is the 200 with the cookie pair attached: the generated
// PostAuthSession200JSONResponse cannot carry Set-Cookie headers, so this
// wrapper writes both cookies and then delegates status and JSON body to it
// — the authapi/doc.go recipe for cookies on the wire.
type sessionOK struct {
	body    authapi.PostAuthSession200JSONResponse
	access  *http.Cookie
	refresh *http.Cookie
}

// VisitPostAuthSessionResponse emits the two Set-Cookie headers before the
// body — a header written after WriteHeader would be silently dropped, so
// cookies go first, then the generated writer sends 200 plus the JSON.
func (r sessionOK) VisitPostAuthSessionResponse(w http.ResponseWriter) error {
	http.SetCookie(w, r.access)
	http.SetCookie(w, r.refresh)
	return r.body.VisitPostAuthSessionResponse(w)
}

// sessionBadRequest is the typed 400 for a missing token field — the request
// shape failures the binder cannot see; bind failures themselves reach the
// same body through writeBadRequest.
func sessionBadRequest() authapi.PostAuthSessionResponseObject {
	return authapi.PostAuthSession400JSONResponse{
		BadRequestJSONResponse: authapi.BadRequestJSONResponse{Error: authapi.ErrorCodeBadRequest},
	}
}

// sessionUnauthenticated is the single 401 every token rejection shares —
// the store already collapsed used, expired and unknown into one ok=false,
// so this is the only unauthenticated body the endpoint can emit.
func sessionUnauthenticated() authapi.PostAuthSessionResponseObject {
	return authapi.PostAuthSession401JSONResponse{
		UnauthorizedJSONResponse: authapi.UnauthorizedJSONResponse{Error: authapi.ErrorCodeUnauthenticated},
	}
}
