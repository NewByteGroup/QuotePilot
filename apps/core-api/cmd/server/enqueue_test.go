package main

// Real-Postgres proofs of enqueueAuthMail — the adapter that turns the
// River client's InsertTx into auth.EnqueueMailFunc. The auth package's
// login_integration_test.go proves the transaction contract with its own
// closure; these tests pin the production adapter itself: the job lands in
// river.river_job through store.InStorePgxTx with the outbox id in args,
// and an insert failure wraps with that id so a failed login's 500 log can
// name the send it stranded. Both run as qp_app: river_job is
// qp_app-readable and -writable because the driver owns its lifecycle.

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// insertOnlyRiverClient builds the same client shape startJobs would, minus
// the workers and Start: an unstarted client still executes InsertTx, and a
// queued job must sit for the assertions instead of being drained mid-test.
func insertOnlyRiverClient(t *testing.T, pool *pgxpool.Pool) *river.Client[pgx.Tx] {
	t.Helper()
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: "river"})
	require.NoError(t, err)
	return client
}

// TestEnqueueAuthMailInsertsJob proves the adapter stages the
// auth_mail_send job inside the caller's transaction: committed through
// store.InStorePgxTx, the river_job row carries kind and the outbox id —
// the only handle the worker needs and the only thing qp_app may read back.
func TestEnqueueAuthMailInsertsJob(t *testing.T) {
	db := testenv.Open(t)
	ctx := context.Background()
	store := platform.NewStore(db.Pool())
	client := insertOnlyRiverClient(t, db.Pool())

	outboxID := uuid.New()
	t.Cleanup(func() {
		_, err := db.Pool().Exec(context.Background(),
			`DELETE FROM river.river_job WHERE kind = 'auth_mail_send' AND args->>'outbox_id' = $1`,
			outboxID.String())
		assert.NoError(t, err, "the staged river_job row must be removable as qp_app")
	})

	require.NoError(t, store.InStorePgxTx(ctx, func(_ *platform.Store, raw pgx.Tx) error {
		return enqueueAuthMail(client)(ctx, raw, outboxID)
	}))

	var kind, argID string
	require.NoError(t, db.Pool().QueryRow(ctx, `
		SELECT kind, args->>'outbox_id' FROM river.river_job
		WHERE kind = 'auth_mail_send' AND args->>'outbox_id' = $1`, outboxID.String()).
		Scan(&kind, &argID))
	assert.Equal(t, "auth_mail_send", kind)
	assert.Equal(t, outboxID.String(), argID,
		"the job carries the outbox id alone — never the link")
}

// TestEnqueueAuthMailRollbackLeavesNoJob proves a staged insert dies with
// its transaction: an in-tx InsertTx followed by a fn error rolls the
// river_job row back with everything else. The count is scoped to this
// test's own outbox id — a global count cannot tell this job from a
// concurrent package's insert (go test runs package binaries in parallel
// against the same database).
func TestEnqueueAuthMailRollbackLeavesNoJob(t *testing.T) {
	db := testenv.Open(t)
	ctx := context.Background()
	client := insertOnlyRiverClient(t, db.Pool())
	outboxID := uuid.New()

	errBoom := fmt.Errorf("boom")
	err := platform.NewStore(db.Pool()).InStorePgxTx(ctx, func(_ *platform.Store, raw pgx.Tx) error {
		if err := enqueueAuthMail(client)(ctx, raw, outboxID); err != nil {
			return err
		}
		return errBoom
	})
	require.ErrorIs(t, err, errBoom)

	var n int
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM river.river_job WHERE args->>'outbox_id' = $1`, outboxID.String()).
		Scan(&n))
	assert.Zero(t, n, "the rolled-back tx must take its staged job with it")
}

// TestEnqueueAuthMailErrorWraps proves the adapter's error path keeps the
// outbox id: an insert against a dead transaction surfaces wrapped as
// "server: enqueue auth mail <id>: …", so the login endpoint's 500 log can
// correlate the abandoned send without a second lookup.
func TestEnqueueAuthMailErrorWraps(t *testing.T) {
	db := testenv.Open(t)
	ctx := context.Background()
	client := insertOnlyRiverClient(t, db.Pool())

	// A rolled-back tx makes InsertTx fail fast (pgx.ErrTxClosed) without
	// staging anything — the deterministic error the wrap must annotate.
	tx, err := db.Pool().Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback(ctx))

	outboxID := uuid.New()
	err = enqueueAuthMail(client)(ctx, tx, outboxID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "enqueue auth mail")
	assert.Contains(t, err.Error(), outboxID.String(),
		"the wrap must name the outbox id — it is the failed send's only handle")
}
