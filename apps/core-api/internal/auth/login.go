package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// ctxKey mirrors platform's private context-key type: a distinct struct type
// so no key set outside this package can collide with or forge the value.
type ctxKey struct{ name string }

// clientIPKey carries the caller IP ExtractClientIP plants for PostAuthLogin.
var clientIPKey = ctxKey{"client-ip"}

// errNoClientIP means the strict handler ran without ExtractClientIP — a
// mounting bug, answered as a 500 rather than an unthrottled login.
var errNoClientIP = errors.New("auth: client IP missing on context — ExtractClientIP middleware not mounted")

// ExtractClientIP is the StrictMiddlewareFunc that plants the caller's IP on
// the context — strict handlers see only (ctx, request), so anything derived
// from *http.Request enters through a middleware (authapi/doc.go). It runs
// for every operation; only POST /auth/login consumes the value today.
//
//	authapi.NewStrictHandlerWithOptions(endpoints, []authapi.StrictMiddlewareFunc{auth.ExtractClientIP}, opts)
func ExtractClientIP(next authapi.StrictHandlerFunc, _ string) authapi.StrictHandlerFunc {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
		return next(context.WithValue(ctx, clientIPKey, hostOnly(r.RemoteAddr)), w, r, request)
	}
}

// hostOnly strips the port from a host:port RemoteAddr. A bare address (no
// port) is used as-is — the limiter keys on whatever identity the edge can
// offer. Trusted-proxy headers (X-Forwarded-For) are deliberately not read:
// RemoteAddr is the only client identity this deployment can trust until the
// ingress story exists.
func hostOnly(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// clientIPFromContext returns the IP ExtractClientIP planted; ok=false means
// the middleware did not run.
func clientIPFromContext(ctx context.Context) (string, bool) {
	ip, ok := ctx.Value(clientIPKey).(string)
	return ip, ok
}

// PostAuthLogin implements POST /auth/login's two paths (spec section 8,
// ADR-005 amendment 2026-11-16). `password` absent is the magic-link path:
// any well-formed email earns the uniform 200 {"status":"ok"} — a
// provisioned user gets a link, an unknown one gets nothing, and the two
// answer with the same response object so the wire cannot tell them apart.
// `password` present — even empty — is the password path: verify the
// argon2id hash and answer exactly like POST /auth/session on success, or
// the single uniform 401 on any failure. An explicit JSON `null` or a
// non-string value is neither path — it is a malformed request and earns a
// 400, so the generated field stays json.RawMessage to keep null distinct
// from absent (*string and *json.RawMessage both collapse both to nil).
//
// Check order is the same on both paths: the IP bucket is spent before the
// email is even parsed — spec section 8.1 meters requests and the route
// chain is rate-limit → handler — while the per-email bucket is spent only
// on a well-formed recipient, so a malformed "email" can never drain a real
// user's budget and every password attempt pays both buckets.
func (e *Endpoints) PostAuthLogin(
	ctx context.Context,
	request authapi.PostAuthLoginRequestObject,
) (authapi.PostAuthLoginResponseObject, error) {
	ip, ok := clientIPFromContext(ctx)
	if !ok {
		return nil, errNoClientIP
	}
	if request.Body == nil {
		return loginBadRequest(), nil
	}
	if !e.ipLimiter.Allow(ip) {
		return loginRateLimited(), nil
	}
	email, ok := parseLoginEmail(request.Body.Email)
	if !ok {
		return loginBadRequest(), nil
	}
	if !e.emailLimiter.Allow(email) {
		return loginRateLimited(), nil
	}
	password, present, valid := decodeLoginPassword(request.Body.Password)
	if !valid {
		return loginBadRequest(), nil
	}
	if present {
		return e.passwordLogin(ctx, email, password)
	}
	if err := e.deliverMagicLink(ctx, email); err != nil {
		return nil, err
	}
	return loginStatusOK()
}

// decodeLoginPassword maps the raw `password` member onto the handler's
// three-way dispatch: an absent member (empty slice) requests a magic link;
// a JSON string — "" included — is a password attempt; `null` and
// non-strings are malformed and must reach neither path, or a client that
// sent a broken credential would get the magic-link 200.
func decodeLoginPassword(raw json.RawMessage) (password string, present, valid bool) {
	if len(raw) == 0 {
		return "", false, true
	}
	var pw *string
	if err := json.Unmarshal(raw, &pw); err != nil || pw == nil {
		return "", false, false
	}
	return *pw, true, true
}

// parseLoginEmail accepts only a bare RFC 5322 addr-spec — no display name,
// no angle form — and returns the lower-cased address used as the limiter
// key, the lookup argument and the recipient. find_user_by_email repeats the
// lower/btrim normalization inside Postgres; doing it here keeps every layer
// of this handler keyed on one canonical form.
func parseLoginEmail(raw string) (string, bool) {
	email := strings.TrimSpace(raw)
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return "", false
	}
	return strings.ToLower(email), true
}

// deliverMagicLink runs the lookup-and-stage for a throttled, well-formed
// email. An unknown email is not an error: nothing is created or queued and
// the caller still gets the uniform 200 (spec section 8 — the only
// observable difference is inside the platform, never in the response). The
// lookup stays outside the transaction: it is the one read both paths pay
// before the known path's writes begin. The send itself never touches the
// response path — the auth_mail_send worker owns it (migration 0004), so a
// mail provider's round trip cannot make a provisioned address measurably
// slower than an unknown one, a timing oracle identical bodies cannot hide.
func (e *Endpoints) deliverMagicLink(ctx context.Context, email string) error {
	userID, found, err := e.store.FindUserByEmail(ctx, email)
	if err != nil {
		return fmt.Errorf("auth: login lookup: %w", err)
	}
	if !found {
		return nil
	}
	token, err := platform.NewOpaqueToken()
	if err != nil {
		return fmt.Errorf("auth: mint login token: %w", err)
	}
	link := magicLinkURL(e.cfg.WebURL, token)
	return e.inTx(ctx, func(tx *platform.Store, raw pgx.Tx) error {
		return e.stageMagicLink(ctx, tx, raw, userID, token, link)
	})
}

// stageMagicLink is the login transaction body (migration 0004, ADR-005):
// the token row, the outbox row carrying the rendered link and the
// auth_mail_send job commit together or roll back together — a token is
// never minted without its send queued, so a crash between mint and
// delivery can no longer strand a link. The job arg is the outbox id alone:
// river_job is qp_app-readable at rest, and a tokened URL there would be a
// second live-credential copy outside the outbox's retention story.
func (e *Endpoints) stageMagicLink(
	ctx context.Context,
	tx *platform.Store,
	raw pgx.Tx,
	userID uuid.UUID,
	token, link string,
) error {
	expiresAt := e.now().Add(e.cfg.LoginTTL)
	tokenID, err := tx.CreateLoginToken(ctx, userID, platform.HashToken(token), expiresAt)
	if err != nil {
		return fmt.Errorf("auth: store login token: %w", err)
	}
	outboxID, err := tx.CreateMailOutbox(ctx, tokenID, link)
	if err != nil {
		return fmt.Errorf("auth: stage mail outbox: %w", err)
	}
	if err := e.enqueue(ctx, raw, outboxID); err != nil {
		return fmt.Errorf("auth: enqueue mail send %s: %w", outboxID, err)
	}
	return nil
}

// magicLinkURL builds the spec section 8 exchange link; the token is
// base64url (RawURLEncoding), so it needs no query escaping. A trailing
// slash on WEB_URL would otherwise double the path's first separator.
func magicLinkURL(webURL, token string) string {
	return strings.TrimRight(webURL, "/") + "/auth/exchange?token=" + token
}

// passwordLogin is the amendment's credential path. Rejection collapses to
// the session endpoint's single 401 — unknown email, a NULL/empty stored
// hash and a wrong password are indistinguishable on the wire — and every
// rejection pays one argon2id verification: the dummy hash stands in for a
// missing row or column value, so the store's answer cannot be timed apart
// from a real wrong-password verify (password.go carries the why).
//
// The session-issue mirrors runExchange: profile reads and the session row
// commit inside ONE transaction through e.inTx, so a signing or insert
// failure leaves nothing half-created behind — only the response/cookie
// assembly stays outside it.
//
// The ~50ms argon2id verify stays outside (it must not hold the lock), so
// the transaction opens with the per-user advisory lock and then RE-READS
// the credential: a reset landing between the outside lookup and the lock
// would otherwise let this login mint a session on the just-killed
// password (Devin Review). Under the lock the stored hash must equal the
// one verified — a mid-flight credential change (reset committed while we
// waited, user gone, hash nulled) is the same uniform 401 as a wrong
// password. One cheap SELECT inside tx pays for the airtightness.
func (e *Endpoints) passwordLogin(
	ctx context.Context,
	email, password string,
) (authapi.PostAuthLoginResponseObject, error) {
	userID, stored, found, err := e.store.GetUserAuthByEmail(ctx, email)
	if err != nil {
		return nil, fmt.Errorf("auth: password login lookup: %w", err)
	}
	hash := stored
	if !found || hash == "" {
		hash = dummyPasswordHash
	}
	verified := verifyPassword(password, hash)
	if !found || stored == "" || !verified {
		return loginUnauthenticated(), nil
	}
	var grant *sessionGrant
	var stale bool
	err = e.inTx(ctx, func(tx *platform.Store, _ pgx.Tx) error {
		if err := tx.LockUserSession(ctx, userID); err != nil {
			return fmt.Errorf("auth: lock user session: %w", err)
		}
		_, rechecked, ok, err := tx.GetUserAuthByEmail(ctx, email)
		if err != nil {
			return fmt.Errorf("auth: password login re-check: %w", err)
		}
		if !ok || rechecked != stored {
			stale = true
			return nil
		}
		profile, memberships, err := e.loadProfile(ctx, tx, userID)
		if err != nil {
			return err
		}
		grant, err = e.issueSession(ctx, tx, userID, profile, memberships)
		return err
	})
	if err != nil {
		return nil, err
	}
	if stale {
		return loginUnauthenticated(), nil
	}
	return e.passwordGrantResponse(grant)
}

// passwordGrantResponse builds the password path's 200: the contract's oneOf
// second arm is SessionResponse — the same switcher payload POST
// /auth/session returns — wrapped with the cookie pair the generated
// response object cannot carry (sessionOK's recipe, on this operation's
// visit method). A union marshal cannot fail on this shape; if it ever does,
// the error is a plain 500 like every other path failure.
func (e *Endpoints) passwordGrantResponse(grant *sessionGrant) (authapi.PostAuthLoginResponseObject, error) {
	var body authapi.PostAuthLogin200JSONResponse
	if err := body.FromSessionResponse(authapi.SessionResponse{
		Profile:     grant.profile,
		Memberships: grant.memberships,
	}); err != nil {
		return nil, fmt.Errorf("auth: encode password login response: %w", err)
	}
	return loginSessionOK{
		body:    body,
		access:  e.accessCookie(grant.access),
		refresh: e.refreshCookie(grant.refresh),
	}, nil
}

// loginStatusOK is the magic-link path's uniform 200 {"status":"ok"} — the
// contract's oneOf first arm. An encode failure is theoretically a 500 like
// any other path fault; the constant shape cannot produce one in practice.
func loginStatusOK() (authapi.PostAuthLoginResponseObject, error) {
	var body authapi.PostAuthLogin200JSONResponse
	if err := body.FromLoginResponse(authapi.LoginResponse{Status: authapi.Ok}); err != nil {
		return nil, fmt.Errorf("auth: encode login response: %w", err)
	}
	return body, nil
}

// loginSessionOK is the password path's cookie-carrying 200 — sessionOK's
// twin for this operation (authapi/doc.go recipe: the generated type cannot
// emit Set-Cookie, so a wrapper writes both cookies first, then delegates
// status and body to it).
type loginSessionOK struct {
	body    authapi.PostAuthLogin200JSONResponse
	access  *http.Cookie
	refresh *http.Cookie
}

// VisitPostAuthLoginResponse emits the two Set-Cookie headers before the
// body — a header written after WriteHeader would be silently dropped.
func (r loginSessionOK) VisitPostAuthLoginResponse(w http.ResponseWriter) error {
	http.SetCookie(w, r.access)
	http.SetCookie(w, r.refresh)
	return r.body.VisitPostAuthLoginResponse(w)
}

// loginUnauthenticated is the password path's single 401 — the same
// {"error":"unauthenticated","message":…} body POST /auth/session emits for a dead
// token, so a wrong password, an unknown email and an unset password are
// byte-identical on the wire (ADR-005 amendment's anti-enumeration contract).
func loginUnauthenticated() authapi.PostAuthLoginResponseObject {
	return authapi.PostAuthLogin401JSONResponse{
		UnauthorizedJSONResponse: authapi.UnauthorizedJSONResponse(authapi.NewError(authapi.ErrorCodeUnauthenticated)),
	}
}

// loginBadRequest is the typed 400 the handler returns for request-shape
// failures the binder cannot see (absent or ill-formed email); bind failures
// reach the same body through writeBadRequest.
func loginBadRequest() authapi.PostAuthLoginResponseObject {
	return authapi.PostAuthLogin400JSONResponse{
		BadRequestJSONResponse: authapi.BadRequestJSONResponse(authapi.NewError(authapi.ErrorCodeBadRequest)),
	}
}

// loginRateLimited is the typed 429 both limiter denials share — IP and email
// exhaustion are one body (spec section 8.1 leaves the code unpinned; the
// contract fixes "rate_limited").
func loginRateLimited() authapi.PostAuthLoginResponseObject {
	return authapi.PostAuthLogin429JSONResponse{
		RateLimitedJSONResponse: authapi.RateLimitedJSONResponse(authapi.NewError(authapi.ErrorCodeRateLimited)),
	}
}
