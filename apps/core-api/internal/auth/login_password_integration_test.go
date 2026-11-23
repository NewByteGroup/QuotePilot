package auth_test

// Real-Postgres proofs of the password path (migration 0006, ADR-005
// amendment 2026-11-16): a seeded user with a password_hash set logs in
// end-to-end — the credential lookup runs as qp_app through
// app.get_user_auth_by_email (Layer A stays function-gated: qp_app cannot
// SELECT app.users at all), the session row lands in app.auth_sessions, and
// the wire carries the cookie pair plus the switcher body. The hash itself
// is written through qp_migrator, the only role that may write Layer A.
// Skipped under -short like the rest of the suite.

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// TestLoginPasswordEndToEnd exercises the whole credential path against the
// real role model: seed + set the hash as qp_migrator, read it back through
// the function as qp_app, then POST /auth/login — the 200 body, the cookie
// pair and the committed session row are the amendment's contract.
func TestLoginPasswordEndToEnd(t *testing.T) {
	db := testenv.Open(t)
	migrator := testenv.MigratorPool(t)
	ctx := context.Background()
	email := "pw-login-" + uuid.NewString() + "@test.dev"
	userID := seedSessionUser(t, migrator, email)
	hash := testenv.PasswordHashPHC(t, "s3cret-pilot")
	_, err := migrator.Exec(ctx,
		`UPDATE app.users SET password_hash = $1 WHERE id = $2`, hash, userID)
	require.NoError(t, err)
	store := platform.NewStore(db.Pool())

	// The store sees the row through app.get_user_auth_by_email — found —
	// and an unknown email is the function's empty set, not an error.
	gotID, gotHash, found, err := store.GetUserAuthByEmail(ctx, email)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, userID, gotID)
	assert.Equal(t, hash, gotHash)
	_, _, found, err = store.GetUserAuthByEmail(ctx, "nobody-"+uuid.NewString()+"@test.dev")
	require.NoError(t, err)
	assert.False(t, found)

	handler := loginHandler(t, sessionConfig(), store, mustNotEnqueue(t))
	rec := postLoginTo(t, handler, `{"email":"`+email+`","password":"s3cret-pilot"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"email":"`+email+`"`)
	assert.NotNil(t, findCookie(t, rec, "qp_access"))
	assert.NotNil(t, findCookie(t, rec, "qp_refresh"))

	var sessions int
	require.NoError(t, migrator.QueryRow(ctx,
		`SELECT count(*) FROM app.auth_sessions WHERE user_id = $1`, userID).Scan(&sessions))
	assert.Equal(t, 1, sessions, "the committed password login lands one session row")

	// The same user with a wrong password and a user with no hash at all
	// answer the uniform 401 — the wire cannot tell the cases apart.
	rec = postLoginTo(t, handler, `{"email":"`+email+`","password":"wrong"}`)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, `{"error":"unauthenticated"}`+"\n", rec.Body.String())
	assert.Empty(t, rec.Header().Values("Set-Cookie"))

	noHashEmail := "pw-nohash-" + uuid.NewString() + "@test.dev"
	seedSessionUser(t, migrator, noHashEmail)
	rec = postLoginTo(t, handler, `{"email":"`+noHashEmail+`","password":"s3cret-pilot"}`)
	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"a NULL password_hash must reject like a wrong password")
}
