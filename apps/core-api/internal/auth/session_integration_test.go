package auth_test

// Real-Postgres regression for the review finding on POST /auth/session: the
// exchange is one transaction, so a failure after consume_login_token — here
// the access signing, the last step on the path — rolls the consume back and
// the magic link stays valid. The fake rig cannot see this (its inTx is a
// direct call), so this mounts the endpoints on a real qp_app pool with
// store.InStoreTx and inspects the auth tables as qp_migrator — qp_app holds
// no table privilege on them (spec 2026-11-10-tenant-resolution-api section
// 5), which is why the reads go through the migrator pool.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// TestSessionExchangeRollsBackConsume is the stranded-link proof: a valid
// token hits a signing failure deep inside the exchange, answers the uniform
// 500, and leaves auth_login_tokens.consumed_at NULL — the row's fate under
// a real transaction, which the fake rig's direct call cannot express. The
// retry then proves "survives" end-to-end: the same token earns a 200.
func TestSessionExchangeRollsBackConsume(t *testing.T) {
	db := testenv.Open(t)
	migrator := testenv.MigratorPool(t)
	ctx := context.Background()

	userID := seedSessionUser(t, migrator)
	store := platform.NewStore(db.Pool())
	token, tokenHash := plantLoginToken(t, store, userID)

	// The ghost kid fails SignAccess after the session row exists — the
	// deepest failure on the path (its fake-matrix twin lives in
	// TestSessionFailuresAreUniform500).
	ghost := sessionConfig()
	ghost.ActiveKID = "ghost"
	rec := postSessionTo(t, sessionHandler(ghost, store), `{"token":"`+token+`"}`)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, `{"error":"internal"}`+"\n", rec.Body.String())
	assert.Empty(t, rec.Header().Values("Set-Cookie"))

	var consumed bool
	require.NoError(t, migrator.QueryRow(ctx,
		`SELECT consumed_at IS NOT NULL FROM app.auth_login_tokens WHERE token_hash = $1`,
		tokenHash).Scan(&consumed))
	assert.False(t, consumed,
		"the failed exchange must roll the consume back — the link stays valid")
	var sessions int
	require.NoError(t, migrator.QueryRow(ctx,
		`SELECT count(*) FROM app.auth_sessions WHERE user_id = $1`, userID).Scan(&sessions))
	assert.Zero(t, sessions, "the rolled-back exchange must leave no orphan session row")

	// The surviving link still exchanges: a working signer earns the 200.
	rec = postSessionTo(t, sessionHandler(sessionConfig(), store), `{"token":"`+token+`"}`)
	require.Equal(t, http.StatusOK, rec.Code,
		"a rolled-back exchange must leave the magic link usable")
}

// seedSessionUser inserts an app.users row as qp_migrator — the only role
// that may write Layer A (seedAuthUser's twin in tenantdb) — and returns
// its id.
func seedSessionUser(t *testing.T, migrator *pgxpool.Pool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, migrator.QueryRow(context.Background(),
		`INSERT INTO app.users (email, full_name) VALUES ($1, $2) RETURNING id`,
		"session-rollback-"+uuid.NewString()+"@test.dev", "Rollback User").Scan(&id))
	return id
}

// plantLoginToken stores a real login token through the app function family
// (the store's own CreateLoginToken) and returns the raw token plus the
// at-rest hash — the pair production holds, the hash never the token.
func plantLoginToken(t *testing.T, store *platform.Store, userID uuid.UUID) (string, string) {
	t.Helper()
	token, err := platform.NewOpaqueToken()
	require.NoError(t, err)
	hash := platform.HashToken(token)
	_, err = store.CreateLoginToken(
		context.Background(), userID, hash, time.Now().Add(15*time.Minute))
	require.NoError(t, err)
	return token, hash
}

// sessionHandler mounts the endpoints on the real store with the production
// wiring: real store.InStoreTx, real Limiters on wall time, auth.NewServer.
func sessionHandler(cfg *platform.Config, store *platform.Store) http.Handler {
	endpoints := auth.NewEndpoints(cfg, store, store.InStoreTx, &platform.FakeMailSender{},
		platform.NewLimiter(platform.LoginIPCapacity, platform.LimitWindow, time.Now),
		platform.NewLimiter(platform.LoginEmailCapacity, platform.LimitWindow, time.Now),
		time.Now)
	return auth.NewServer(endpoints)
}

// postSessionTo fires one JSON body at a handler the test built itself —
// the rig's postSession for a surface outside the rig.
func postSessionTo(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/auth/session", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}
