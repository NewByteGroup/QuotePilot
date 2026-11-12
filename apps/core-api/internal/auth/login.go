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
