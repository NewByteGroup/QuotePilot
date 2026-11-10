package jobs_test

import (
	"bytes"
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/jobs"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// lockedBuffer makes a slog destination safe for River's concurrent writers.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestSpikeRiverJobTenantContext is gate criterion 6: a River job enqueued
// inside InTenantTx runs under the tenant's RLS context. It also records how
// River's periodic reindexer behaves without index ownership (flagged in
// infra/postgres/init/01-roles-and-schemas.sh).
func TestSpikeRiverJobTenantContext(t *testing.T) {
	testenv.RequirePostgres(t)
	testenv.EnsureMigrated(t)
	db := testenv.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	orgA := uuid.New()
	orgB := uuid.New()
	// The worker writes inbound_messages under org A's tenant context; since
	// 0002 that column is a real organizations FK, so the org rows must exist.
	testenv.SeedOrganization(t, orgA, "jobs-org-a")
	testenv.SeedOrganization(t, orgB, "jobs-org-b")

	var logs lockedBuffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	workers := river.NewWorkers()
	river.AddWorker(workers, jobs.NewEchoTenantWorker(db))

	client, err := river.NewClient(riverpgxv5.New(db.Pool()), &river.Config{
		Queues:  map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 4}},
		Workers: workers,
		Schema:  "river",
		Logger:  logger,
		// Fire the reindexer quickly so the spike observes what it does without
		// index ownership; production should disable it via river.NeverSchedule().
		ReindexerSchedule: river.PeriodicInterval(2 * time.Second),
	})
	require.NoError(t, err)
	require.NoError(t, client.Start(ctx))
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer stopCancel()
		_ = client.Stop(stopCtx)
	})

	// Enqueue inside org A's tenant transaction: the job row commits only when
	// the tenant transaction commits (ADR-004 staged jobs).
	var jobID int64
	require.NoError(t, db.InTenantTx(ctx, orgA, func(ctx context.Context, tx pgx.Tx) error {
		res, err := client.InsertTx(ctx, tx, jobs.EchoTenantArgs{OrganizationID: orgA}, nil)
		if err != nil {
			return err
		}
		jobID = res.Job.ID
		return nil
	}))
	require.NotZero(t, jobID)

	require.Eventually(t, func() bool {
		row, err := client.JobGet(ctx, jobID)
		return err == nil && row.State == rivertype.JobStateCompleted
	}, 30*time.Second, 250*time.Millisecond, "river job must complete")

	// The worker's row exists under org A and is invisible under org B.
	resendID := "job:" + strconv.FormatInt(jobID, 10)
	require.NoError(t, db.InTenantTx(ctx, orgA, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		require.NoError(t, tx.QueryRow(ctx,
			`SELECT count(*) FROM app.inbound_messages WHERE resend_message_id = $1`, resendID).Scan(&n))
		require.Equal(t, 1, n, "worker must have written under org A's tenant context")
		return nil
	}))
	require.NoError(t, db.InTenantTx(ctx, orgB, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM app.inbound_messages`).Scan(&n))
		require.Equal(t, 0, n, "worker's row must be invisible to org B")
		return nil
	}))

	// Finding for the spike report: the reindexer needs index ownership, which
	// qp_app lacks, so it logs "must be owner of index" each run.
	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "must be owner of index")
	}, 15*time.Second, 500*time.Millisecond,
		"expected reindexer to fail without index ownership; logs:\n%s", logs.String())
	t.Logf("reindexer 'must be owner of index' occurrences: %d",
		strings.Count(logs.String(), "must be owner of index"))
	assert.Contains(t, logs.String(), "must be owner of index")
}
