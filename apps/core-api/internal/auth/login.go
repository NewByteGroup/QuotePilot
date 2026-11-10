package auth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"strings"

	"github.com/google/uuid"

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

// PostAuthLogin implements POST /auth/login (spec section 8): any well-formed
// email earns the uniform 200 {"status":"ok"} — a provisioned user gets a
// magic link, an unknown one gets nothing, and the two paths answer with the
// same response object so the wire cannot tell them apart.
//
// Check order: the IP bucket is spent before the email is even parsed — spec
// section 8.1 meters requests and the route chain is rate-limit → handler —
// while the per-email bucket is spent only on a well-formed recipient, so a
// malformed "email" can never drain a real user's magic-link budget.
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
	if err := e.deliverMagicLink(ctx, email); err != nil {
		return nil, err
	}
	return authapi.PostAuthLogin200JSONResponse{Status: authapi.Ok}, nil
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

// deliverMagicLink runs the lookup-and-send for a throttled, well-formed
// email. An unknown email is not an error: nothing is created or sent and the
// caller still gets the uniform 200 (spec section 8 — the only observable
// difference is inside the platform, never in the response).
func (e *Endpoints) deliverMagicLink(ctx context.Context, email string) error {
	userID, found, err := e.store.FindUserByEmail(ctx, email)
	if err != nil {
		return fmt.Errorf("auth: login lookup: %w", err)
	}
	if !found {
		return nil
	}
	token, err := e.issueLoginToken(ctx, userID)
	if err != nil {
		return err
	}
	if err := e.mailer.SendMagicLink(ctx, email, magicLinkURL(e.cfg.WebURL, token)); err != nil {
		return fmt.Errorf("auth: send magic link: %w", err)
	}
	return nil
}

// issueLoginToken mints a fresh opaque token and stores only its SHA-256 hex
// (spec section 4: the token travels in the link, Postgres keeps the hash).
// It returns the token itself for the magic-link URL — never the hash.
func (e *Endpoints) issueLoginToken(ctx context.Context, userID uuid.UUID) (string, error) {
	token, err := platform.NewOpaqueToken()
	if err != nil {
		return "", fmt.Errorf("auth: mint login token: %w", err)
	}
	expiresAt := e.now().Add(e.cfg.LoginTTL)
	if _, err := e.store.CreateLoginToken(ctx, userID, platform.HashToken(token), expiresAt); err != nil {
		return "", fmt.Errorf("auth: store login token: %w", err)
	}
	return token, nil
}

// magicLinkURL builds the spec section 8 exchange link; the token is
// base64url (RawURLEncoding), so it needs no query escaping. A trailing
// slash on WEB_URL would otherwise double the path's first separator.
func magicLinkURL(webURL, token string) string {
	return strings.TrimRight(webURL, "/") + "/auth/exchange?token=" + token
}

// loginBadRequest is the typed 400 the handler returns for request-shape
// failures the binder cannot see (absent or ill-formed email); bind failures
// reach the same body through writeBadRequest.
func loginBadRequest() authapi.PostAuthLoginResponseObject {
	return authapi.PostAuthLogin400JSONResponse{
		BadRequestJSONResponse: authapi.BadRequestJSONResponse{Error: authapi.ErrorCodeBadRequest},
	}
}

// loginRateLimited is the typed 429 both limiter denials share — IP and email
// exhaustion are one body (spec section 8.1 leaves the code unpinned; the
// contract fixes "rate_limited").
func loginRateLimited() authapi.PostAuthLoginResponseObject {
	return authapi.PostAuthLogin429JSONResponse{
		RateLimitedJSONResponse: authapi.RateLimitedJSONResponse{Error: authapi.ErrorCodeRateLimited},
	}
}
