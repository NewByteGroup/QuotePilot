package auth

import (
	"context"
	"fmt"
	"net/http"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/google/uuid"
)

// refreshCookieKey carries the raw qp_refresh cookie value
// ExtractRefreshCookie plants for PostAuthRefresh and PostAuthLogout —
// logout resolves the same cookie to its session family.
var refreshCookieKey = ctxKey{"refresh-cookie"}

// ExtractRefreshCookie is the StrictMiddlewareFunc that reads the
// qp_refresh cookie off the wire and plants its raw value on the context —
// strict handlers see only (ctx, request), so anything derived from
// *http.Request enters through a middleware (authapi/doc.go). An absent
// cookie plants nothing: the handler's ok=false branch, not the binder,
// answers it, so a missing cookie gets the same uniform 401 as a dead one —
// the contract deliberately carries no cookie parameter because a required
// one would emit a 400 where spec section 8 mandates the 401.
//
//	authapi.NewStrictHandlerWithOptions(endpoints, []authapi.StrictMiddlewareFunc{auth.ExtractRefreshCookie}, opts)
func ExtractRefreshCookie(next authapi.StrictHandlerFunc, _ string) authapi.StrictHandlerFunc {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
		if c, err := r.Cookie(refreshCookieName); err == nil {
			ctx = context.WithValue(ctx, refreshCookieKey, c.Value)
		}
		return next(ctx, w, r, request)
	}
}

// refreshCookieFromContext returns the raw qp_refresh value
// ExtractRefreshCookie planted; ok=false means the request carried no such
// cookie — a case the wire cannot tell apart from a dead one.
func refreshCookieFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(refreshCookieKey).(string)
	return v, ok
}

// PostAuthRefresh implements POST /auth/refresh (spec section 8): the
// presented cookie is exchanged — by SHA-256 hash, the raw value never
// approaches the store — for the next generation of its session family, and
// success re-issues the whole spec section 4 pair: a fresh access JWT on
// the successor's session id plus the rotated opaque refresh cookie.
//
// Every rejection is one response: absent, unknown, revoked, expired and
// already-rotated (reuse — the store killed the whole family in the same
// atomic call) all answer the same 401 with both cookies cleared, so the
// wire cannot distinguish a dead token from a replayed one.
//
// Ordering: the successor token is minted before the rotate because its
// hash is an argument, and the access JWT is signed after because the
// successor's session id is its sid claim — so rotate_session commits
// before SignAccess runs. A sign failure therefore strands the client
// behind a 500: its presented generation is already exchanged and a replay
// is the reuse case. The successor itself is inert — only its hash ever
// left the handler — so the stranding is safe, not a leak (C2 review).
//
//	resp, err := endpoints.PostAuthRefresh(ctx, authapi.PostAuthRefreshRequestObject{})
func (e *Endpoints) PostAuthRefresh(
	ctx context.Context,
	_ authapi.PostAuthRefreshRequestObject,
) (authapi.PostAuthRefreshResponseObject, error) {
	presented, ok := refreshCookieFromContext(ctx)
	if !ok {
		return e.refreshUnauthenticated(), nil
	}
	return e.rotateGeneration(ctx, presented)
}

// rotateGeneration orchestrates the exchange: mint the successor opaque
// token, hand both hashes to the store's atomic rotate, then answer with
// the signed pair — or the uniform 401 when the store refused.
func (e *Endpoints) rotateGeneration(
	ctx context.Context, presented string,
) (authapi.PostAuthRefreshResponseObject, error) {
	successor, err := platform.NewOpaqueToken()
	if err != nil {
		return nil, fmt.Errorf("auth: mint refresh token: %w", err)
	}
	sessionID, userID, rotated, err := e.rotateStored(ctx, presented, successor)
	if err != nil {
		return nil, err
	}
	if !rotated {
		return e.refreshUnauthenticated(), nil
	}
	return e.rotatedResponse(userID, sessionID, successor)
}

// rotateStored runs the store's atomic exchange and collapses the two
// rejection shapes it reports — the reuse kill (found, rotated=false) and
// the unknown/revoked/expired empty set (not found) — into the single
// rotated=false the handler is allowed to act on.
func (e *Endpoints) rotateStored(
	ctx context.Context, presented, successor string,
) (sessionID, userID uuid.UUID, rotated bool, err error) {
	sessionID, userID, changed, found, err := e.store.RotateSession(
		ctx, platform.HashToken(presented), platform.HashToken(successor),
		e.now().Add(e.cfg.RefreshTTL))
	if err != nil {
		return uuid.Nil, uuid.Nil, false, fmt.Errorf("auth: rotate session: %w", err)
	}
	return sessionID, userID, found && changed, nil
}

// rotatedResponse signs the successor's access JWT and attaches the rotated
// pair. A sign failure strands the client: the successor is already
// committed, so replaying the presented generation is reuse and kills the
// family — inert, never a leak (C2 review).
func (e *Endpoints) rotatedResponse(
	userID, sessionID uuid.UUID, successor string,
) (authapi.PostAuthRefreshResponseObject, error) {
	access, err := e.cfg.SignAccess(userID, sessionID, e.now())
	if err != nil {
		return nil, fmt.Errorf("auth: sign access token: %w", err)
	}
	return refreshOK{
		access:  e.accessCookie(access),
		refresh: e.refreshCookie(successor),
	}, nil
}

// refreshOK is the 200 with the rotated cookie pair attached: the generated
// PostAuthRefresh200Response writes status only (the contract's 200 carries
// no body), so this wrapper emits both Set-Cookie headers first — a header
// written after WriteHeader would be silently dropped — then delegates.
type refreshOK struct {
	access  *http.Cookie
	refresh *http.Cookie
}

// VisitPostAuthRefreshResponse emits the rotated pair before the bare 200.
func (r refreshOK) VisitPostAuthRefreshResponse(w http.ResponseWriter) error {
	http.SetCookie(w, r.access)
	http.SetCookie(w, r.refresh)
	return authapi.PostAuthRefresh200Response{}.VisitPostAuthRefreshResponse(w)
}

// refreshUnauthenticated is the single rejection shape — the
// sessionUnauthenticated twin plus the cleared pair spec section 8
// mandates: reuse kills the family server-side, but the client keeps
// presenting its pair until it is told to drop it, and a dead or absent
// cookie must empty the same jar.
func (e *Endpoints) refreshUnauthenticated() authapi.PostAuthRefreshResponseObject {
	access, refresh := e.clearedPair()
	return refreshRejected{access: access, refresh: refresh}
}

// clearedPair derives the deletion headers from the real cookie builders —
// same names, paths and flags as the set versions, so the two can never
// drift apart — with an empty value and MaxAge=-1, which serializes as
// "Max-Age=0": the browser-side delete.
func (e *Endpoints) clearedPair() (access, refresh *http.Cookie) {
	// #nosec G124 -- the builders' flag set is pinned by test; only Value and
	// MaxAge differ here, which is exactly the browser-delete contract.
	access = e.accessCookie("")
	access.MaxAge = -1
	// #nosec G124 -- same justification as the access cookie above.
	refresh = e.refreshCookie("")
	refresh.MaxAge = -1
	return access, refresh
}

// refreshRejected is the uniform 401 with both cookies cleared — every
// rejection (absent, unknown, revoked, expired, replayed) answers this one
// object, so body and Set-Cookie headers are byte-identical across cases.
type refreshRejected struct {
	access  *http.Cookie
	refresh *http.Cookie
}

// VisitPostAuthRefreshResponse clears the pair, then delegates the 401 body
// — the same ordering rule as refreshOK: headers precede WriteHeader.
func (r refreshRejected) VisitPostAuthRefreshResponse(w http.ResponseWriter) error {
	http.SetCookie(w, r.access)
	http.SetCookie(w, r.refresh)
	return authapi.PostAuthRefresh401JSONResponse{
		UnauthorizedJSONResponse: authapi.UnauthorizedJSONResponse{
			Error: authapi.ErrorCodeUnauthenticated,
		},
	}.VisitPostAuthRefreshResponse(w)
}
