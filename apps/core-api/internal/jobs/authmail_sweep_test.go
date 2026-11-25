package jobs_test

// Real-Postgres proof of the auth_mail_sweep worker (migration 0014): Work
// runs sweep_auth_mail_outbox over the shared table, so an expired token's
// outbox row dies while a live token's pending send survives. The seeds
// ride one rolled-back qp_app transaction under the same advisory lock the
// platform suite's sweeper test holds, so the two table-wide DELETEs never
// race and nothing this run staged persists.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/jobs"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// TestAuthMailSweepWorkDeletesDeadRows drives Work against the real
// function: the expired-token row is physically gone after the pass —
// delete_mail_outbox's false is the Layer A existence probe — and the
// live-token row is still there to be delivered.
func TestAuthMailSweepWorkDeletesDeadRows(t *testing.T) {
	db := testenv.Open(t)
	ctx := context.Background()
	tx, err := db.Pool().Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	_, err = tx.Exec(ctx,
		`SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended('auth_mail_sweep_test', 0))`)
	require.NoError(t, err)

	var userID uuid.UUID
	require.NoError(t, testenv.MigratorPool(t).QueryRow(ctx,
		`INSERT INTO app.users (email, full_name) VALUES ($1, '') RETURNING id`,
		"sweep-worker-"+uuid.NewString()+"@test.dev").Scan(&userID))

	store := platform.NewStore(tx)
	seed := func(hash string, expiresAt time.Time) uuid.UUID {
		t.Helper()
		tokenID, err := store.CreateLoginToken(ctx, userID, hash, expiresAt)
		require.NoError(t, err)
		row, err := store.CreateMailOutbox(ctx, tokenID, "https://app.quotepilot.test/"+uuid.NewString())
		require.NoError(t, err)
		return row
	}
	deadRow := seed(platform.HashToken("sweep-dead-"+uuid.NewString()), time.Now().Add(-time.Minute))
	liveRow := seed(platform.HashToken("sweep-live-"+uuid.NewString()), time.Now().Add(15*time.Minute))

	worker := jobs.NewAuthMailSweepWorker(store)
	require.NoError(t, worker.Work(ctx, &river.Job[jobs.AuthMailSweepArgs]{}))

	gone, err := store.DeleteMailOutbox(ctx, deadRow)
	require.NoError(t, err)
	assert.False(t, gone, "the expired-token row was swept, not merely unreadable")
	kept, err := store.DeleteMailOutbox(ctx, liveRow)
	require.NoError(t, err)
	assert.True(t, kept, "the live-token row survived to be receipted")
}
