package auth_test

// Real-Postgres proofs of the forgot/set-password pair (migration 0007,
// ADR-005 amendment 2026-11-16): the reset request stages a
// purpose='password_reset' token and the set-password outbox link as
// qp_app through the Layer A functions, the consume+write+revoke commit in
// one transaction, and the new password then authenticates through
// /auth/login's password path — with no session minted at reset time. The
// cross-purpose half proves the token classes stay disjoint against the
// real functions: a reset token cannot consume at /auth/session and a
// login token cannot consume at /auth/password. Skipped under -short like
// the rest of the suite.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// TestPasswordResetEndToEnd runs the whole amendment arc on the real role
// model: request reset → read_mail_outbox carries purpose and the
// set-password link → POST /auth/password stores the PHC and revokes the
// user's live session → the new password logs in through the password
// path → the spent token answers 401 on reuse.
func TestPasswordResetEndToEnd(t *testing.T) {
	db := testenv.Open(t)
	migrator := testenv.MigratorPool(t)
	ctx := context.Background()
	email := "pw-reset-" + uuid.NewString() + "@test.dev"
	userID := seedSessionUser(t, migrator, email)
	cfg := sessionConfig()
	store := platform.NewStore(db.Pool())

	// A live session exists BEFORE the reset — the OWASP leg must kill it —
	// and a pending magic link rides alongside: the pending-token sweep is
	// the same credential-class kill. The refresh value is a fresh opaque
	// token, not a literal — its hash is UNIQUE in auth_sessions, and a
	// fixed string would collide on re-run against a persistent dev DB.
	staleRefresh, err := platform.NewOpaqueToken()
	require.NoError(t, err)
	sessionID, err := store.CreateSession(
		ctx, userID, platform.HashToken(staleRefresh), time.Now().Add(24*time.Hour))
	require.NoError(t, err)
	_, pendingHash := plantLoginToken(t, store, userID)

	handler := loginHandler(t, cfg, store, noopEnqueue)
	rec := postAuthTo(t, handler, "/auth/password-reset", `{"email":"`+email+`"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, `{"status":"ok"}`+"\n", rec.Body.String())

	// The outbox holds the set-password link and read_mail_outbox carries
	// the purpose the worker will switch its template on — as qp_app.
	var outboxID uuid.UUID
	var link string
	require.NoError(t, migrator.QueryRow(ctx, `
		SELECT m.id, m.link_url FROM app.auth_mail_outbox m
		JOIN app.auth_login_tokens tk ON tk.id = m.login_token_id
		WHERE tk.user_id = $1 AND tk.purpose = 'password_reset'`, userID).
		Scan(&outboxID, &link))
	assert.True(t, strings.HasPrefix(link, cfg.WebURL+"/auth/set-password?token="),
		"the reset link lands on the set-password page, got %q", link)

	pending, found, err := store.ReadMailOutbox(ctx, outboxID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, platform.MailPurposePasswordReset, pending.Purpose,
		"the purpose flows through read_mail_outbox so the worker picks the template")
	assert.Equal(t, link, pending.Link)
	assert.Equal(t, email, pending.Email)

	token := strings.TrimPrefix(link, cfg.WebURL+"/auth/set-password?token=")
	rec = postAuthTo(t, handler, "/auth/password",
		`{"token":"`+token+`","password":"n3w-pilot-pass"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, `{"status":"ok"}`+"\n", rec.Body.String())
	assert.Empty(t, rec.Result().Cookies(), "set-password does not start a session")

	// The PHC landed through set_user_password — readable only via the
	// credential function — and the pre-reset session died with it.
	_, hash, found, err := store.GetUserAuthByEmail(ctx, email)
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, strings.HasPrefix(hash, "$argon2id$v=19$"), "an argon2id PHC at rest, got %q", hash)
	var revokedAt *time.Time
	require.NoError(t, migrator.QueryRow(ctx,
		`SELECT revoked_at FROM app.auth_sessions WHERE id = $1`, sessionID).Scan(&revokedAt))
	assert.NotNil(t, revokedAt, "the reset revokes sessions minted on the old credential")

	// The pending-token sweep killed the outstanding magic link — same
	// credential class as the password — so its consume now misses too.
	var consumedAt *time.Time
	require.NoError(t, migrator.QueryRow(ctx,
		`SELECT consumed_at FROM app.auth_login_tokens WHERE token_hash = $1`, pendingHash).
		Scan(&consumedAt))
	assert.NotNil(t, consumedAt, "a pending magic link dies with the credential reset")

	// Refresh on the revoked family answers the dead-token shape: the new
	// rotate_session's FOR UPDATE read sees revoked_at and returns empty.
	// The real user-advisory barrier inside the function ran for this call
	// — a concurrent interleave is not forceable in a deterministic test,
	// so what this pins is the serialized outcome the lock guarantees.
	nextRefresh, err := platform.NewOpaqueToken()
	require.NoError(t, err)
	_, _, rotated, found, err := store.RotateSession(
		ctx, platform.HashToken(staleRefresh), platform.HashToken(nextRefresh),
		time.Now().Add(24*time.Hour))
	require.NoError(t, err)
	assert.False(t, found, "a revoked generation cannot rotate — refresh answers 401")
	assert.False(t, rotated)

	// The new password authenticates through the login password path —
	// the functional verify of the stored PHC.
	rec = postLoginTo(t, handler, `{"email":"`+email+`","password":"n3w-pilot-pass"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotNil(t, findCookie(t, rec, "qp_access"))
	assert.NotNil(t, findCookie(t, rec, "qp_refresh"))

	// The spent token answers the uniform 401 on reuse — single use is
	// the consume's row lock, not a read-then-write.
	rec = postAuthTo(t, handler, "/auth/password",
		`{"token":"`+token+`","password":"other-pass-99"}`)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, `{"error":"unauthenticated"}`+"\n", rec.Body.String())
}

// TestTokenPurposesDoNotExchangePostgres is the migration-0007 invariant
// against the real functions: consume_login_token's purpose filter means a
// reset token mints no session, and consume_password_reset_token's filter
// means a magic link writes no credential — both the uniform 401.
func TestTokenPurposesDoNotExchangePostgres(t *testing.T) {
	db := testenv.Open(t)
	migrator := testenv.MigratorPool(t)
	ctx := context.Background()
	email := "pw-xpurpose-" + uuid.NewString() + "@test.dev"
	userID := seedSessionUser(t, migrator, email)
	store := platform.NewStore(db.Pool())
	handler := loginHandler(t, sessionConfig(), store, noopEnqueue)

	// A real reset token presented to /auth/session: the login consume's
	// purpose filter is the miss — 401, no cookie, no session row.
	resetToken, _ := plantResetToken(t, store, userID)
	rec := postAuthTo(t, handler, "/auth/session", `{"token":"`+resetToken+`"}`)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, `{"error":"unauthenticated"}`+"\n", rec.Body.String())
	assert.Empty(t, rec.Header().Values("Set-Cookie"))
	var sessions int
	require.NoError(t, migrator.QueryRow(ctx,
		`SELECT count(*) FROM app.auth_sessions WHERE user_id = $1`, userID).Scan(&sessions))
	assert.Zero(t, sessions, "a reset token must never mint a session")

	// A real login token presented to /auth/password: the reset consume's
	// filter is the symmetric miss — 401, no credential written.
	loginToken, _ := plantLoginToken(t, store, userID)
	rec = postAuthTo(t, handler, "/auth/password",
		`{"token":"`+loginToken+`","password":"n3w-pilot-pass"}`)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, `{"error":"unauthenticated"}`+"\n", rec.Body.String())
	var hash *string
	require.NoError(t, migrator.QueryRow(ctx,
		`SELECT password_hash FROM app.users WHERE id = $1`, userID).Scan(&hash))
	assert.Nil(t, hash, "a magic link must never set a password")
}

// plantResetToken stores a real purpose='password_reset' token through the
// app function family (the store's own CreatePasswordResetToken) and
// returns the raw token plus its at-rest hash — plantLoginToken's twin.
func plantResetToken(t *testing.T, store *platform.Store, userID uuid.UUID) (string, string) {
	t.Helper()
	token, err := platform.NewOpaqueToken()
	require.NoError(t, err)
	hash := platform.HashToken(token)
	_, err = store.CreatePasswordResetToken(
		context.Background(), userID, hash, time.Now().Add(15*time.Minute))
	require.NoError(t, err)
	return token, hash
}

// noopEnqueue is the EnqueueMailFunc for the reset proofs: the staging
// needs a successful enqueue inside the transaction but the send itself
// belongs to the worker, which these tests read through the outbox row.
func noopEnqueue(context.Context, pgx.Tx, uuid.UUID) error { return nil }

// postAuthTo fires one JSON body at any route on the mounted surface —
// postLoginTo's generalization for the reset pair.
func postAuthTo(t *testing.T, handler http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}
