package auth

import (
	"context"
	"fmt"
	"net/http"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// PostAuthLogout implements POST /auth/logout (spec section 8): the
// presented qp_refresh cookie — still valid after qp_access expires, and
// delivered by its Path=/auth scope — resolves to its whole session family
// in one call, so every generation dies, live and already-rotated alike.
// The access JWT itself keeps verifying until its exp — the accepted
// revocation lag of spec section 4.
//
// Always 204, valid token or not: the store's boolean feeds metrics only
// and never reaches the wire, so the response carries no validity oracle —
// absent, unknown and already-dead cookies all get the same cleared pair
// plus the bare status. An absent cookie skips the store call entirely
// (an empty jar is cleared harmlessly), and the raw token never approaches
// the store — its SHA-256 hash is the lookup key.
//
// The only non-204 is a driver fault, answered through the uniform 500
// with no Set-Cookie at all — the same rule refresh's 500 follows: clearing
// on a failed revoke would log the client out of a family that is still
// live.
func (e *Endpoints) PostAuthLogout(
	ctx context.Context,
	_ authapi.PostAuthLogoutRequestObject,
) (authapi.PostAuthLogoutResponseObject, error) {
	presented, ok := refreshCookieFromContext(ctx)
	if !ok {
		return e.logoutCleared(), nil
	}
	if _, err := e.store.RevokeSessionFamilyByHash(ctx, platform.HashToken(presented)); err != nil {
		return nil, fmt.Errorf("auth: revoke session family: %w", err)
	}
	return e.logoutCleared(), nil
}

// logoutCleared derives the always-204 answer: the same cleared pair a
// refresh rejection emits, around the contract's bare status — every
// non-fault response is byte-identical whatever the cookie resolved to.
func (e *Endpoints) logoutCleared() authapi.PostAuthLogoutResponseObject {
	access, refresh := e.clearedPair()
	return logoutNoContent{access: access, refresh: refresh}
}

// logoutNoContent is the 204 with the cleared pair attached: the generated
// PostAuthLogout204Response writes status only (the contract's 204 carries
// no body), so this wrapper emits both Set-Cookie headers first — a header
// written after WriteHeader would be silently dropped — then delegates.
type logoutNoContent struct {
	access  *http.Cookie
	refresh *http.Cookie
}

// VisitPostAuthLogoutResponse emits the cleared pair before the bare 204 —
// the same ordering rule as refreshOK: headers precede WriteHeader.
func (r logoutNoContent) VisitPostAuthLogoutResponse(w http.ResponseWriter) error {
	http.SetCookie(w, r.access)
	http.SetCookie(w, r.refresh)
	return authapi.PostAuthLogout204Response{}.VisitPostAuthLogoutResponse(w)
}
