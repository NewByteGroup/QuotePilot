package platform

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

// accessCookie is the spec section 4 transport for the access JWT. The
// refresh cookie (qp_refresh, SameSite=Strict, Path=/auth) is scoped
// to the session endpoints; the middleware chain never reads it.
const accessCookie = "qp_access"

// OrgHeader is the tenant header ResolveTenant prefers (spec section 7).
// Demanding a custom header — never a query param, and a cookie only as
// the fallback below — is part of the CSRF posture: a cross-site form
// cannot set one (ADR-005 section 4).
const OrgHeader = "X-Organization-Id"

// OrgCookie carries the browser's active-organization choice written by
// POST /v1/session/organization (the web v1 read surface). ResolveTenant
// reads it only when OrgHeader is absent — SameSite=Lax keeps it off
// cross-site posts, so the header-first CSRF posture still guards every
// state-changing route while plain GETs resolve without JS.
const OrgCookie = "qp_org"

// requestIDHeader carries the per-request correlation id in and out;
// maxRequestIDLen bounds a client-supplied value — anything longer is
// regenerated rather than truncated, so a shared prefix cannot alias two
// requests into one log line.
const (
	requestIDHeader = "X-Request-Id"
	maxRequestIDLen = 128
)

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

// correlationKey is Correlation's context key: an unexported struct type
// keyed by type identity alone, so code outside this package cannot plant
// or overwrite the request id under it.
type correlationKey struct{}

// CorrelationID returns the request id Correlation stored on ctx; "" means
// the middleware did not run — log it anyway rather than skip the line.
//
//	cid := platform.CorrelationID(r.Context())
func CorrelationID(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey{}).(string)
	return id
}

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

// Correlation is the outermost layer: it plants one request id on the
// context and echoes it on the response, so every diagnostic the request
// emits — the membership and writeError warnings below — ties to one log
// line. An inbound X-Request-Id is reused only when it is well-formed
// (trims to non-empty, within maxRequestIDLen, free of control
// characters); an absent or malformed one gets a fresh uuid, since echoing
// an attacker-chosen string verbatim would let them smuggle log-forging
// bytes into every line the request writes. CorrelationID is the read
// side.
//
//	mux.Handle("GET /me", platform.Correlation(platform.Authenticate(cfg)(meHandler)))
func Correlation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cid := inboundRequestID(r.Header.Get(requestIDHeader))
		w.Header().Set(requestIDHeader, cid)
		ctx := context.WithValue(r.Context(), correlationKey{}, cid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// inboundRequestID returns a client-supplied id safe to reuse — non-empty
// after trimming, inside the length cap, free of control characters — or
// a fresh uuid for anything absent or malformed.
func inboundRequestID(header string) string {
	id := strings.TrimSpace(header)
	if id == "" || len(id) > maxRequestIDLen || strings.IndexFunc(id, unicode.IsControl) >= 0 {
		return uuid.NewString()
	}
	return id
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
				writeError(r.Context(), w, http.StatusUnauthorized, errUnauthenticated)
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

// resolveTenantHandler is the shared ResolveTenant body; allowDefault
// selects whether a hintless safe-method request may fall back to the
// caller's first membership.
func resolveTenantHandler(store *Store, allowDefault bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			orgID, role, ok := resolveTenant(store, w, r, allowDefault)
			if !ok {
				return
			}
			ctx := context.WithValue(r.Context(), orgCtxKey, orgID)
			ctx = context.WithValue(ctx, roleCtxKey, role)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ResolveTenant resolves the tenant to a membership the authenticated
// user holds right now — the check runs on every request and is never
// cached (ADR-005 section 5), so removing a member takes effect on their
// next call. The tenant id comes from X-Organization-Id when present, else
// the qp_org cookie POST /v1/session/organization sets; an absent or
// non-UUID hint is 400 before any lookup. A NULL from get_membership
// covers non-member and nonexistent org indistinguishably and maps to one
// 404 through the same writer — no "does the org exist" shortcut exists
// for an oracle to observe (spec section 7).
//
//	mux.Handle("GET /me/organization",
//		platform.Authenticate(cfg)(platform.ResolveTenant(store)(orgHandler)))
func ResolveTenant(store *Store) func(http.Handler) http.Handler {
	return resolveTenantHandler(store, false)
}

// ResolveTenantDefault is ResolveTenant plus one fallback: a safe-method
// request (GET/HEAD/OPTIONS) carrying no org hint at all resolves the
// caller's first membership — the same deterministic pick GET /v1/session
// makes — instead of answering missing_organization. The web inbox boots
// this way: a fresh session has a valid qp_access but no qp_org yet, and
// a 400 would dead-end the first load. The fallback only fires when no
// hint exists; a present-but-malformed header stays a hard 400 (an
// explicit transport must not paper over client bugs), and membership is
// re-checked exactly as before.
//
//	mux.Handle("GET /v1/inbox/quotes",
//		platform.Authenticate(cfg)(platform.ResolveTenantDefault(store)(authSrv)))
func ResolveTenantDefault(store *Store) func(http.Handler) http.Handler {
	return resolveTenantHandler(store, true)
}

// resolveTenant runs the transport and identity checks in order: the org
// hint is validated before any lookup, and a missing user context means
// the chain was mis-mounted without Authenticate — answered as
// unauthenticated, never passed through. false means the response is
// already written.
func resolveTenant(store *Store, w http.ResponseWriter, r *http.Request, allowDefault bool) (uuid.UUID, string, bool) {
	orgID, ok := orgHint(r)
	if !ok && allowDefault {
		var err error
		if orgID, ok, err = defaultOrgHint(store, r); err != nil {
			// A profile/membership read failure is a server fault, not a
			// missing hint — a 500 must not masquerade as the client 400.
			slog.Default().WarnContext(r.Context(), "platform: default org lookup failed",
				"correlation_id", CorrelationID(r.Context()), "error", err)
			writeError(r.Context(), w, http.StatusInternalServerError, errInternal)
			return uuid.Nil, "", false
		}
	}
	if !ok {
		writeError(r.Context(), w, http.StatusBadRequest, errMissingOrg)
		return uuid.Nil, "", false
	}
	userID, ok := UserID(r.Context())
	if !ok {
		writeError(r.Context(), w, http.StatusUnauthorized, errUnauthenticated)
		return uuid.Nil, "", false
	}
	return checkMembership(store, w, r, userID, orgID)
}

// defaultOrgHint is ResolveTenantDefault's last resort: the first
// membership of the authenticated caller. It returns ok=false — never an
// error — when the request is not a safe method, an OrgHeader is present
// (a malformed explicit hint stays a hard 400), the caller identity is
// absent, the profile row is missing, or the membership list is empty;
// only a store failure is an error. The pick is ms[0]:
// list_memberships_for_email is created_at-ordered, the same org GET
// /v1/session reports as active.
func defaultOrgHint(store *Store, r *http.Request) (uuid.UUID, bool, error) {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		return uuid.Nil, false, nil
	}
	if r.Header.Get(OrgHeader) != "" {
		return uuid.Nil, false, nil
	}
	userID, ok := UserID(r.Context())
	if !ok {
		return uuid.Nil, false, nil
	}
	_, email, _, found, err := store.GetUserProfile(r.Context(), userID)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("platform: default org: profile %s: %w", userID, err)
	}
	if !found {
		return uuid.Nil, false, nil
	}
	ms, err := store.ListMembershipsForEmail(r.Context(), email)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("platform: default org: memberships %q: %w", email, err)
	}
	if len(ms) == 0 {
		return uuid.Nil, false, nil
	}
	return ms[0].OrgID, true, nil
}

// orgHint resolves the tenant-id transport: the explicit
// X-Organization-Id header wins (API clients); an absent header falls back
// to the qp_org browser cookie. ok=false means both were absent or the
// present value was not a UUID — resolveTenant answers the uniform 400.
func orgHint(r *http.Request) (uuid.UUID, bool) {
	if h := r.Header.Get(OrgHeader); h != "" {
		id, err := uuid.Parse(h)
		return id, err == nil
	}
	c, err := r.Cookie(OrgCookie)
	if err != nil || c.Value == "" {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(c.Value)
	return id, err == nil
}

// SetOrgCookie writes the qp_org transport POST /v1/session/organization
// answers with: the same flag set as qp_access (HttpOnly, SameSite=Lax,
// Path=/, Secure from cfg.CookieSecure) at the refresh lifetime — the org
// choice must outlive the 15-minute access token and die with the session
// family. Callers only ever pass a membership-proven org UUID, never raw
// user input; ResolveTenant re-checks membership on every read.
func SetOrgCookie(cfg *Config, w http.ResponseWriter, orgID uuid.UUID) {
	// #nosec G124 -- HttpOnly and SameSite are literal; Secure is
	// intentionally config-driven (CookieSecure=false only for local http),
	// and middleware_orgcookie_test.go pins the exact flag set.
	http.SetCookie(w, &http.Cookie{
		Name:     OrgCookie,
		Value:    orgID.String(),
		Path:     "/",
		MaxAge:   int(cfg.RefreshTTL.Seconds()),
		HttpOnly: true,
		Secure:   cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

// checkMembership is the per-request membership probe: one get_membership
// call whose NULL — non-member and nonexistent org alike — maps to the
// uniform 404. A driver error is the only differently-shaped answer (500):
// it is a server failure, not information about whether the org exists.
func checkMembership(store *Store, w http.ResponseWriter, r *http.Request, userID, orgID uuid.UUID) (uuid.UUID, string, bool) {
	role, ok, err := store.GetMembership(r.Context(), userID, orgID)
	if err != nil {
		// The client sees the uniform 500 either way; the driver error
		// itself must not vanish into it — log before writing. No user or
		// tenant id rides along: the correlation id plus the wrapped error
		// is enough to find the request in the logs.
		slog.Default().WarnContext(r.Context(), "platform: membership lookup failed",
			"correlation_id", CorrelationID(r.Context()), "error", err)
		writeError(r.Context(), w, http.StatusInternalServerError, errInternal)
		return uuid.Nil, "", false
	}
	if !ok {
		writeError(r.Context(), w, http.StatusNotFound, errNotFound)
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
				writeError(r.Context(), w, http.StatusForbidden, errForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CheckOrigin is the Origin/Sec-Fetch-Site check of ADR-005 section 4's CSRF
// posture: the stdlib's CrossOriginProtection rejects non-safe cross-site
// browser requests (Sec-Fetch-Site, then Origin-vs-Host). WebURL is added as
// a trusted origin so the web app on its sibling host survives the Host
// comparison; requests with neither header are treated as non-browser and
// pass. Mount it ahead of Authenticate on state-changing routes.
//
//	mux.Handle("POST /quotes", platform.CheckOrigin(cfg)(chain(quotes)))
func CheckOrigin(cfg *Config) func(http.Handler) http.Handler {
	cop := http.NewCrossOriginProtection()
	if err := cop.AddTrustedOrigin(trustedOrigin(cfg.WebURL)); err != nil {
		// A WEB_URL the CSRF layer cannot parse is a startup bug — better a
		// loud crash than a silently weaker posture.
		panic(fmt.Errorf("platform: check origin: %w", err))
	}
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(r.Context(), w, http.StatusForbidden, errForbidden)
	}))
	return cop.Handler
}

// trustedOrigin reduces WebURL — a base URL that may carry a path — to the
// scheme://host[:port] origin AddTrustedOrigin accepts. An unparseable or
// non-http(s) WebURL is a startup bug and panics with the offending value,
// the expected shape, and the parser's diagnostic.
func trustedOrigin(webURL string) string {
	u, err := url.Parse(webURL)
	if err != nil {
		panic(fmt.Errorf("platform: trusted origin: WEB_URL %q is not a valid URL (expected scheme://host[:port]): %w", webURL, err))
	}
	if u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		panic(fmt.Errorf("platform: trusted origin: WEB_URL %q must be an http(s) URL with a host (expected scheme://host[:port])", webURL))
	}
	return u.Scheme + "://" + u.Host
}

// writeError is the single failure writer every middleware rejection routes
// through (spec section 7's uniform-failure contract): a fixed
// {"error":"<code>"} body with a fixed content type. Callers pass only the
// status and the code — there is no parameter through which distinguishing
// detail could leak, which is what makes a non-member and a nonexistent org
// byte-identical on the wire.
func writeError(ctx context.Context, w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := io.WriteString(w, `{"error":"`+code+`"}`); err != nil {
		// The wire contract cannot change mid-response; the only thing left
		// is the diagnostic, logged rather than discarded. correlation_id is
		// passed explicitly — WarnContext does not turn ctx values into
		// attributes. Rendering them (a global JSON handler) is cmd/server
		// wiring, not this package's concern.
		slog.Default().WarnContext(ctx, "platform: error response write failed",
			"correlation_id", CorrelationID(ctx),
			"status", status, "code", code, "error", err)
	}
}
