package auth_test

// Real-Postgres proofs of the durable-send transaction (migration 0004,
// ADR-004): POST /auth/login stages the login token row, the
// auth_mail_outbox row and the auth_mail_send river_job in ONE transaction
// — a failure at the last step (the enqueue) leaves no token and no pending
// send, and the committed path lands all three. The fake rig cannot see
// this (its inTx is a direct call with a nil tx), so the endpoints mount on
// a real qp_app pool with store.InStorePgxTx and, for the commit half, a
// real insert-only River client. Table reads go through qp_migrator —
// qp_app holds no privilege on the Layer A tables — while the outbox and
// river_job reads run as qp_app itself.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/jobs"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// TestLoginOutboxTxRollbackOnEnqueueFailure is the stranded-link proof for
// the durable path: the enqueue erroring inside the transaction answers the
// uniform 500, and the whole staging rolls back — no auth_login_tokens row,
// no auth_mail_outbox row, no river_job row. The user simply requests
// another link; nothing half-committed waits for a worker.
func TestLoginOutboxTxRollbackOnEnqueueFailure(t *testing.T) {
	db := testenv.Open(t)
	migrator := testenv.MigratorPool(t)
	ctx := context.Background()
	email := "login-outbox-rb-" + uuid.NewString() + "@test.dev"
	userID := seedSessionUser(t, migrator, email)
	store := platform.NewStore(db.Pool())

	failEnqueue := func(context.Context, pgx.Tx, uuid.UUID) error { return errBoom }
	rec := postLoginTo(t, loginHandler(t, sessionConfig(), store, failEnqueue), `{"email":"`+email+`"}`)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, `{"error":"internal"}`+"\n", rec.Body.String())

	var tokens int
	require.NoError(t, migrator.QueryRow(ctx,
		`SELECT count(*) FROM app.auth_login_tokens WHERE user_id = $1`, userID).Scan(&tokens))
	assert.Zero(t, tokens, "the failed enqueue must roll the token insert back")
	var pending int
	require.NoError(t, migrator.QueryRow(ctx, `
		SELECT count(*) FROM app.auth_mail_outbox m
		JOIN app.auth_login_tokens tk ON tk.id = m.login_token_id
		WHERE tk.user_id = $1`, userID).Scan(&pending))
	assert.Zero(t, pending, "the aborted tx must leave no pending send behind")
	// No river_job count here: the enqueue errored before InsertTx ran, and
	// a rolled-back job's outbox id is unattributable — a global count would
	// race any concurrent package inserting the same kind (go test runs
	// package binaries in parallel). The scoped proof lives in
	// cmd/server's TestEnqueueAuthMailRollbackLeavesNoJob.
}

// TestLoginOutboxTxCommitLandsJob is the happy-path commit: the same
// transaction lands the token row, the outbox row — readable back through
// app.read_mail_outbox as qp_app with recipient and expiry resolved — and
// the auth_mail_send job carrying the outbox id in its qp_app-readable
// args, exactly the shape the worker claims.
func TestLoginOutboxTxCommitLandsJob(t *testing.T) {
	db := testenv.Open(t)
	migrator := testenv.MigratorPool(t)
	ctx := context.Background()
	email := "login-outbox-ok-" + uuid.NewString() + "@test.dev"
	userID := seedSessionUser(t, migrator, email)
	cfg := sessionConfig()
	store := platform.NewStore(db.Pool())

	// An insert-only client (no Workers, no Start) — the job must sit
	// queued for the assertions, not be drained mid-test.
	client, err := river.NewClient(riverpgxv5.New(db.Pool()), &river.Config{Schema: "river"})
	require.NoError(t, err)
	enqueue := func(ctx context.Context, tx pgx.Tx, outboxID uuid.UUID) error {
		_, err := client.InsertTx(ctx, tx, jobs.AuthMailArgs{OutboxID: outboxID}, nil)
		return err
	}

	rec := postLoginTo(t, loginHandler(t, cfg, store, enqueue), `{"email":"`+email+`"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, `{"status":"ok"}`+"\n", rec.Body.String())

	var outboxID uuid.UUID
	var link string
	require.NoError(t, migrator.QueryRow(ctx, `
		SELECT m.id, m.link_url FROM app.auth_mail_outbox m
		JOIN app.auth_login_tokens tk ON tk.id = m.login_token_id
		WHERE tk.user_id = $1`, userID).Scan(&outboxID, &link))
	assert.True(t, strings.HasPrefix(link, cfg.WebURL+"/auth/exchange?token="),
		"the outbox holds the rendered link, got %q", link)

	pending, found, err := store.ReadMailOutbox(ctx, outboxID)
	require.NoError(t, err)
	require.True(t, found, "app.read_mail_outbox must see the committed row as qp_app")
	assert.Equal(t, link, pending.Link)
	assert.Equal(t, email, pending.Email, "the recipient joins through the login token")
	assert.WithinDuration(t, time.Now().Add(cfg.LoginTTL), pending.ExpiresAt, 30*time.Second)

	var kind, argID string
	require.NoError(t, db.Pool().QueryRow(ctx, `
		SELECT kind, args->>'outbox_id' FROM river.river_job
		WHERE kind = 'auth_mail_send' AND args->>'outbox_id' = $1`, outboxID.String()).
		Scan(&kind, &argID))
	assert.Equal(t, "auth_mail_send", kind)
	assert.Equal(t, outboxID.String(), argID,
		"the job carries the outbox id alone — never the link")
}

// loginHandler mounts the login path on a real store: the production
// InStorePgxTx seam, real Limiters on wall time, the caller's enqueue.
func loginHandler(
	t *testing.T,
	cfg *platform.Config,
	store *platform.Store,
	enqueue auth.EnqueueMailFunc,
) http.Handler {
	t.Helper()
	endpoints := auth.NewEndpoints(cfg, store, store.InStorePgxTx, &neverRunner{t}, enqueue,
		platform.NewLimiter(platform.LoginIPCapacity, platform.LimitWindow, time.Now),
		platform.NewLimiter(platform.LoginEmailCapacity, platform.LimitWindow, time.Now),
		time.Now)
	return auth.NewServer(endpoints)
}

// postLoginTo fires one JSON body at a handler the test built itself —
// the rig's postLogin for a surface outside the rig.
func postLoginTo(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}
