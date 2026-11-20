package auth

// The forgot/set-password pair (ADR-005 amendment 2026-11-16): "forgot
// password" reuses the shipped machinery — the reset link goes out through
// the platform outbox + token path (same hash-at-rest, single-use, short
// expiry) and lands on a set-password page rather than starting a session.
// POST /auth/password-reset is deliverMagicLink's twin with the purpose
// flipped; POST /auth/password is runExchange's sibling whose transaction
// writes a credential and kills sessions instead of minting one.

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// The contract's 8..128 password policy (PasswordSetRequest.minLength/
// maxLength): the generated binder decodes the string but does not enforce
// schema bounds, so the endpoint does — a violation is the uniform 400,
// never an argon2id run on a value that can never become a credential.
const (
	passwordMinLen = 8
	passwordMaxLen = 128
)

// PostAuthPasswordReset implements POST /auth/password-reset — the
// magic-link request's exact contract with the purpose flipped: any
// well-formed email earns the uniform 200 {"status":"ok"}; a provisioned
// user gets a reset link staged through the same outbox, an unknown one
// gets nothing, and the two answer with the same response object. Check
// order is login's verbatim: IP bucket first, then the per-email bucket on
// a well-formed recipient only.
func (e *Endpoints) PostAuthPasswordReset(
	ctx context.Context,
	request authapi.PostAuthPasswordResetRequestObject,
) (authapi.PostAuthPasswordResetResponseObject, error) {
	ip, ok := clientIPFromContext(ctx)
	if !ok {
		return nil, errNoClientIP
	}
	if request.Body == nil {
		return resetBadRequest(), nil
	}
	if !e.ipLimiter.Allow(ip) {
		return resetRateLimited(), nil
	}
	email, ok := parseLoginEmail(request.Body.Email)
	if !ok {
		return resetBadRequest(), nil
	}
	if !e.emailLimiter.Allow(email) {
		return resetRateLimited(), nil
	}
	if err := e.deliverPasswordReset(ctx, email); err != nil {
		return nil, err
	}
	return resetStatusOK()
}

// PostAuthPassword implements POST /auth/password — the set-password side.
// The reset token consumes inside ONE transaction together with the
// credential write and the session kill (OWASP: a reset invalidates
// existing sessions, and the three must commit or roll back together so a
// mid-failure cannot strand a consumed-but-unwritten token). Success is
// {"status":"ok"} only — the page does not start a session; a used,
// expired, unknown or wrong-purpose token is the session endpoint's
// byte-identical 401. The IP bucket is spent before the consume; there is
// no email key on this route.
func (e *Endpoints) PostAuthPassword(
	ctx context.Context,
	request authapi.PostAuthPasswordRequestObject,
) (authapi.PostAuthPasswordResponseObject, error) {
	ip, ok := clientIPFromContext(ctx)
	if !ok {
		return nil, errNoClientIP
	}
	if request.Body == nil {
		return passwordSetBadRequest(), nil
	}
	if !e.ipLimiter.Allow(ip) {
		return passwordSetRateLimited(), nil
	}
	if request.Body.Token == "" || !passwordWithinPolicy(request.Body.Password) {
		return passwordSetBadRequest(), nil
	}
	// Hash outside the transaction: the ~50ms argon2id run must not hold a
	// tx open, and a dead token's 401 paying the same cost as a live one's
	// keeps the consume off the timing channel — every request hashes.
	phc, err := hashPassword(request.Body.Password)
	if err != nil {
		return nil, fmt.Errorf("auth: hash new password: %w", err)
	}
	applied, err := e.applyPasswordReset(ctx, platform.HashToken(request.Body.Token), phc)
	if err != nil {
		return nil, err
	}
	if !applied {
		return passwordSetUnauthenticated(), nil
	}
	return passwordSetStatusOK()
}

// passwordWithinPolicy is the contract's 8..128 bound — enforced here
// because the binder does not apply minLength/maxLength. minLength counts
// JSON Schema characters (code points), so the check counts runes, not
// bytes — a multibyte password must not slip under the bound on width.
func passwordWithinPolicy(password string) bool {
	n := utf8.RuneCountInString(password)
	return n >= passwordMinLen && n <= passwordMaxLen
}

// deliverPasswordReset is deliverMagicLink's twin (login.go documents the
// why): an unknown email is not an error and still earns the uniform 200,
// the lookup stays outside the transaction, and the send never touches the
// response path — the auth_mail_send worker owns it.
func (e *Endpoints) deliverPasswordReset(ctx context.Context, email string) error {
	userID, found, err := e.store.FindUserByEmail(ctx, email)
	if err != nil {
		return fmt.Errorf("auth: password reset lookup: %w", err)
	}
	if !found {
		return nil
	}
	token, err := platform.NewOpaqueToken()
	if err != nil {
		return fmt.Errorf("auth: mint reset token: %w", err)
	}
	link := resetLinkURL(e.cfg.WebURL, token)
	return e.inTx(ctx, func(tx *platform.Store, raw pgx.Tx) error {
		return e.stagePasswordReset(ctx, tx, raw, userID, token, link)
	})
}

// stagePasswordReset is stageMagicLink's twin (login.go documents the
// why): reset token row, outbox row and the auth_mail_send job commit
// together or roll back together. The expiry reuses cfg.LoginTTL — a reset
// token is the same short-lived credential class as a login token (same
// at-rest hash, same single use), so it inherits the same TTL rather than
// minting a second dial; the amendment pins only "short expiry".
func (e *Endpoints) stagePasswordReset(
	ctx context.Context,
	tx *platform.Store,
	raw pgx.Tx,
	userID uuid.UUID,
	token, link string,
) error {
	expiresAt := e.now().Add(e.cfg.LoginTTL)
	tokenID, err := tx.CreatePasswordResetToken(ctx, userID, platform.HashToken(token), expiresAt)
	if err != nil {
		return fmt.Errorf("auth: store reset token: %w", err)
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

// applyPasswordReset is the set-password transaction body: consume the
// reset hash (the purpose filter keeps magic-link tokens out), write the
// argon2id PHC, then revoke the user's live sessions — the OWASP leg that
// keeps a session minted on a stolen password from outliving the
// credential. All three writes commit together or roll back together;
// (false, nil) is the dead-token shape the caller maps to the uniform 401.
func (e *Endpoints) applyPasswordReset(
	ctx context.Context,
	tokenHash, passwordHash string,
) (bool, error) {
	var applied bool
	err := e.inTx(ctx, func(tx *platform.Store, _ pgx.Tx) error {
		userID, ok, err := tx.ConsumePasswordResetToken(ctx, tokenHash)
		if err != nil {
			return fmt.Errorf("auth: consume reset token: %w", err)
		}
		if !ok {
			return nil
		}
		if err := tx.SetUserPassword(ctx, userID, passwordHash); err != nil {
			return fmt.Errorf("auth: set user password: %w", err)
		}
		if err := tx.RevokeUserSessions(ctx, userID); err != nil {
			return fmt.Errorf("auth: revoke user sessions: %w", err)
		}
		applied = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return applied, nil
}

// resetLinkURL builds the amendment's set-password link — magicLinkURL's
// twin aimed at /auth/set-password, the page that posts this token to
// /auth/password. Same base64url-no-escaping and trailing-slash guard.
func resetLinkURL(webURL, token string) string {
	return strings.TrimRight(webURL, "/") + "/auth/set-password?token=" + token
}

// resetStatusOK is the reset request's uniform 200 {"status":"ok"} — the
// same body loginStatusOK emits on the magic-link path.
func resetStatusOK() (authapi.PostAuthPasswordResetResponseObject, error) {
	return authapi.PostAuthPasswordReset200JSONResponse{Status: authapi.Ok}, nil
}

// resetBadRequest is the reset request's typed 400 — same body as every
// other route's request-shape failure.
func resetBadRequest() authapi.PostAuthPasswordResetResponseObject {
	return authapi.PostAuthPasswordReset400JSONResponse{
		BadRequestJSONResponse: authapi.BadRequestJSONResponse{Error: authapi.ErrorCodeBadRequest},
	}
}

// resetRateLimited is the reset request's typed 429 — same body as login's.
func resetRateLimited() authapi.PostAuthPasswordResetResponseObject {
	return authapi.PostAuthPasswordReset429JSONResponse{
		RateLimitedJSONResponse: authapi.RateLimitedJSONResponse{Error: authapi.ErrorCodeRateLimited},
	}
}

// passwordSetStatusOK is the set-password 200 {"status":"ok"} — the
// amendment's no-session answer: the caller logs in through the password
// path next.
func passwordSetStatusOK() (authapi.PostAuthPasswordResponseObject, error) {
	return authapi.PostAuthPassword200JSONResponse{Status: authapi.Ok}, nil
}

// passwordSetBadRequest covers the request-shape failures on the
// set-password route: absent token or a password outside the 8..128
// policy.
func passwordSetBadRequest() authapi.PostAuthPasswordResponseObject {
	return authapi.PostAuthPassword400JSONResponse{
		BadRequestJSONResponse: authapi.BadRequestJSONResponse{Error: authapi.ErrorCodeBadRequest},
	}
}

// passwordSetUnauthenticated is the set-password route's single 401 — the
// same {"error":"unauthenticated"} the session endpoint emits for a dead
// token, so used, expired, unknown and wrong-purpose tokens are
// byte-identical on the wire (the amendment's anti-enumeration contract).
func passwordSetUnauthenticated() authapi.PostAuthPasswordResponseObject {
	return authapi.PostAuthPassword401JSONResponse{
		UnauthorizedJSONResponse: authapi.UnauthorizedJSONResponse{Error: authapi.ErrorCodeUnauthenticated},
	}
}

// passwordSetRateLimited is the IP bucket's denial on the set-password
// route — the same body login's limiters emit.
func passwordSetRateLimited() authapi.PostAuthPasswordResponseObject {
	return authapi.PostAuthPassword429JSONResponse{
		RateLimitedJSONResponse: authapi.RateLimitedJSONResponse{Error: authapi.ErrorCodeRateLimited},
	}
}
