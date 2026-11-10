package platform

import (
	"context"
	"io"
	"net/http"
	"slices"

	"github.com/google/uuid"
)

// accessCookie is the spec section 4 transport for the access JWT. The
// refresh cookie (qp_refresh, SameSite=Strict, Path=/auth/refresh) is scoped
// to the session endpoints; the middleware chain never reads it.
const accessCookie = "qp_access"

// orgHeader is the tenant header ResolveTenant requires (spec section 7).
// Demanding a custom header — never a cookie or a query param — is part of
// the CSRF posture: a cross-site form cannot set one (ADR-005 section 4).
const orgHeader = "X-Organization-Id"

// Membership roles, verbatim from the app.membership_role enum (migration
// 0002): GetMembership returns the enum text unchanged, so RequireRole
// compares against these constants — never a re-typed parallel list.
const (
	RoleOwner          = "OWNER"
	RoleAdmin          = "ADMIN"
	RolePricingAnalyst = "PRICING_ANALYST"
)

// Failure codes — the whole spec section 7 vocabulary. Every rejection in
// this file writes exactly one of them through writeError, so each failure
// class emits exactly one possible body.
const (
	errUnauthenticated = "unauthenticated"
	errMissingOrg      = "missing_organization"
	errNotFound        = "not_found"
	errForbidden       = "forbidden"
	errInternal        = "internal"
)

// ctxKey is the package-private context key type: a distinct struct type
// (not a string or int) so keys set by any other package cannot collide with
// or forge these.
type ctxKey struct{ name string }

var (
	userCtxKey    = ctxKey{"user"}
	sessionCtxKey = ctxKey{"session"}
	orgCtxKey     = ctxKey{"org"}
	roleCtxKey    = ctxKey{"role"}
)

// UserID returns the sub claim Authenticate stored on ctx; ok=false means
// the middleware did not run or the token failed to verify.
//
//	userID, ok := platform.UserID(r.Context())
func UserID(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(userCtxKey).(uuid.UUID)
	return id, ok
}

// SessionID returns the sid claim Authenticate stored on ctx — the session
// family's live generation. ok=false mirrors UserID's contract.
//
//	sessionID, ok := platform.SessionID(r.Context())
func SessionID(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(sessionCtxKey).(uuid.UUID)
	return id, ok
}

// OrgID returns the organization ResolveTenant validated for this request;
// ok=false when no tenant check ran. It is the id callers hand to InTenantTx.
//
//	orgID, ok := platform.OrgID(r.Context())
func OrgID(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(orgCtxKey).(uuid.UUID)
	return id, ok
}

// Role returns the caller's membership role in the resolved org, stored by
// ResolveTenant from the same get_membership row — never from the JWT
// (ADR-005 section 5: org and role must not be cached in the token).
//
//	role, ok := platform.Role(r.Context())
func Role(ctx context.Context) (string, bool) {
	role, ok := ctx.Value(roleCtxKey).(string)
	return role, ok
}

// Authenticate requires a valid qp_access cookie and stores its sub/sid
// claims on the request context for ResolveTenant and the handlers. It is
// deliberately stateless — no DB access: logout kills the refresh path
// instantly while an access token verifies until its exp (≤ AccessTTL); that
// is the revocation-lag residual spec section 4 documents, not an oversight.
// Missing, malformed, expired, wrong-kid and forged cookies all funnel into
// the same writeError(401), so no failure mode is distinguishable from the
// outside.
//
//	mux.Handle("GET /me", platform.Authenticate(cfg)(meHandler))
func Authenticate(cfg *Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID, sessionID, ok := verifyAccessCookie(cfg, r)
			if !ok {
				writeError(w, http.StatusUnauthorized, errUnauthenticated)
				return
			}
			ctx := context.WithValue(r.Context(), userCtxKey, userID)
			ctx = context.WithValue(ctx, sessionCtxKey, sessionID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// verifyAccessCookie collapses every authentication failure — absent cookie,
// malformed token, bad signature, unknown kid, expired — into one false, so
// the middleware cannot answer them differently even by accident.
func verifyAccessCookie(cfg *Config, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	cookie, err := r.Cookie(accessCookie)
	if err != nil {
		return uuid.Nil, uuid.Nil, false
	}
	userID, sessionID, err := cfg.VerifyAccess(cookie.Value)
	if err != nil {
		return uuid.Nil, uuid.Nil, false
	}
	return userID, sessionID, true
}

// ResolveTenant resolves X-Organization-Id to a membership the authenticated
// user holds right now — the check runs on every request and is never cached
// (ADR-005 section 5), so removing a member takes effect on their next call.
// An absent or non-UUID header is 400, before any lookup. A NULL from
// get_membership covers non-member and nonexistent org indistinguishably and
// maps to one 404 through the same writer — no "does the org exist" shortcut
// exists for an oracle to observe (spec section 7).
//
//	mux.Handle("GET /me/organization",
//		platform.Authenticate(cfg)(platform.ResolveTenant(store)(orgHandler)))
func ResolveTenant(store *Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			orgID, role, ok := resolveTenant(store, w, r)
			if !ok {
				return
			}
			ctx := context.WithValue(r.Context(), orgCtxKey, orgID)
			ctx = context.WithValue(ctx, roleCtxKey, role)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// resolveTenant runs the header and identity checks in order: the org header
// is validated before any lookup, and a missing user context means the chain
// was mis-mounted without Authenticate — answered as unauthenticated, never
// passed through. false means the response is already written.
func resolveTenant(store *Store, w http.ResponseWriter, r *http.Request) (uuid.UUID, string, bool) {
	orgID, err := uuid.Parse(r.Header.Get(orgHeader))
	if err != nil {
		writeError(w, http.StatusBadRequest, errMissingOrg)
		return uuid.Nil, "", false
	}
	userID, ok := UserID(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, errUnauthenticated)
		return uuid.Nil, "", false
	}
	return checkMembership(store, w, r, userID, orgID)
}

// checkMembership is the per-request membership probe: one get_membership
// call whose NULL — non-member and nonexistent org alike — maps to the
// uniform 404. A driver error is the only differently-shaped answer (500):
// it is a server failure, not information about whether the org exists.
func checkMembership(store *Store, w http.ResponseWriter, r *http.Request, userID, orgID uuid.UUID) (uuid.UUID, string, bool) {
	role, ok, err := store.GetMembership(r.Context(), userID, orgID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errInternal)
		return uuid.Nil, "", false
	}
	if !ok {
		writeError(w, http.StatusNotFound, errNotFound)
		return uuid.Nil, "", false
	}
	return orgID, role, true
}

// RequireRole admits the request only when the role ResolveTenant stored is
// in the list; anything else is 403. That answer leaks nothing — the caller
// already proved membership to carry a role at all (spec section 7). No role
// on the context means the chain was mis-mounted; it denies the same way
// rather than fail open.
//
//	mux.Handle("POST /quotes/approve",
//		chain(platform.RequireRole(platform.RoleOwner, platform.RoleAdmin)(approve)))
func RequireRole(roles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			role, ok := Role(r.Context())
			if !ok || !slices.Contains(roles, role) {
				writeError(w, http.StatusForbidden, errForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// writeError is the single failure writer every middleware rejection routes
// through (spec section 7's uniform-failure contract): a fixed
// {"error":"<code>"} body with a fixed content type. Callers pass only the
// status and the code — there is no parameter through which distinguishing
// detail could leak, which is what makes a non-member and a nonexistent org
// byte-identical on the wire.
func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":"`+code+`"}`)
}
