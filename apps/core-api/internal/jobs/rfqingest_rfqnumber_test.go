package jobs_test

// rfq_number allocation proofs (migration 0009): the web inbox addresses
// an RFQ by its per-company integer, allocated inside the same tenant
// transaction as the rfq_requests row — sequential per org,
// concurrency-safe by the org_rfq_counters row lock, and shared by the
// FAILED-shell path so a dead letter is still addressable.

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/resend"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// loadRfqNumber reads the allocated sequence for one resend id inside the
// tenant tx — the counter's RLS makes the org's rows the only visible ones.
func loadRfqNumber(t *testing.T, db *tenantdb.DB, org uuid.UUID, resendID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT rfq_number FROM app.rfq_requests WHERE resend_message_id = $1`, resendID).Scan(&n)
	}))
	return n
}

// loadCounter reads the org's next_seq — the allocation cursor itself.
func loadCounter(t *testing.T, db *tenantdb.DB, org uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT next_seq FROM app.org_rfq_counters WHERE organization_id = $1`, org).Scan(&n)
	}))
	return n
}

// ingestOnce promotes one receipt end to end through the real worker.
func ingestOnce(t *testing.T, rig *ingestRig, org, receipt uuid.UUID) {
	t.Helper()
	require.NoError(t, rig.worker.Work(context.Background(), ingestJob(org, uuid.New(), receipt)))
}

// TestRfqNumberSequentialPerOrg: two promoted RFQs take 1 and 2 in
// allocation order, and a second org's sequence starts back at 1 — the
// counter is per-company, never global.
func TestRfqNumberSequentialPerOrg(t *testing.T) {
	db := testenv.Open(t)
	orgA, orgB := uuid.New(), uuid.New()
	testenv.SeedOrganization(t, orgA, "rfqnum-a")
	testenv.SeedOrganization(t, orgB, "rfqnum-b")

	recv := &fakeReceiver{email: fixtureEmail(), downloads: map[string][]byte{
		"https://cdn.test/a1": []byte("PDF1"), "https://cdn.test/a2": []byte("CSV"),
	}}
	rig := newIngestRig(t, db, recv, platform.NewFakeStorage())

	r1 := seedReceipt(t, db, orgA, "num-a-1", "first")
	r2 := seedReceipt(t, db, orgA, "num-a-2", "second")
	r3 := seedReceipt(t, db, orgB, "num-b-1", "other org")

	ingestOnce(t, rig, orgA, r1)
	ingestOnce(t, rig, orgA, r2)
	ingestOnce(t, rig, orgB, r3)

	assert.Equal(t, 1, loadRfqNumber(t, db, orgA, "num-a-1"))
	assert.Equal(t, 2, loadRfqNumber(t, db, orgA, "num-a-2"))
	assert.Equal(t, 1, loadRfqNumber(t, db, orgB, "num-b-1"),
		"org B's sequence is independent — its first RFQ is 1")
	assert.Equal(t, 3, loadCounter(t, db, orgA), "the cursor sits one past the last grant")
	assert.Equal(t, 2, loadCounter(t, db, orgB))
}

// TestRfqNumberConcurrentAlloc: N ingests racing on one org must produce
// N distinct numbers with no unique violation — the counter row lock is
// the only thing that makes allocation serial.
func TestRfqNumberConcurrentAlloc(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "rfqnum-race")

	const n = 6
	receipts := make([]uuid.UUID, n)
	for i := range receipts {
		receipts[i] = seedReceipt(t, db, org, "race-"+uuid.NewString()[:8], "concurrent")
	}

	// One rig per goroutine: the shared fakes count their calls on bare
	// fields — the contention under test is Postgres's, not the fixtures'.
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i, rc := range receipts {
		wg.Add(1)
		go func(i int, receipt uuid.UUID) {
			defer wg.Done()
			recv := &fakeReceiver{email: fixtureEmail(), downloads: map[string][]byte{
				"https://cdn.test/a1": []byte("PDF1"), "https://cdn.test/a2": []byte("CSV"),
			}}
			rig := newIngestRig(t, db, recv, platform.NewFakeStorage())
			errs[i] = rig.worker.Work(context.Background(), ingestJob(org, uuid.New(), receipt))
		}(i, rc)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "concurrent ingest %d must not fail", i)
	}

	numbers := map[int]bool{}
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT rfq_number FROM app.rfq_requests`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v int
			if err := rows.Scan(&v); err != nil {
				return err
			}
			numbers[v] = true
		}
		return rows.Err()
	}))
	require.Len(t, numbers, n, "every concurrent ingest must land a distinct rfq_number")
	for i := 1; i <= n; i++ {
		assert.True(t, numbers[i], "rfq_number %d must be allocated", i)
	}
}

// TestRfqNumberFailedShellAllocates: the permanent-failure shell gets a
// number through the same CTE — a dead letter is an addressable inbox row
// too.
func TestRfqNumberFailedShellAllocates(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "rfqnum-shell")

	recv := &fakeReceiver{err: resendPermanentErr()}
	rig := newIngestRig(t, db, recv, platform.NewFakeStorage())
	receipt := seedReceipt(t, db, org, "shell-num-1", "gone")

	require.NoError(t, rig.worker.Work(context.Background(), ingestJob(org, uuid.New(), receipt)))
	assert.Equal(t, 1, loadRfqNumber(t, db, org, "shell-num-1"))
}

// resendPermanentErr fabricates the 4xx class the receiver reports for a
// dead message id — the worker maps it onto the FAILED shell, no retry.
func resendPermanentErr() error {
	return &resend.FetchError{Op: "get_inbound_email", StatusCode: 404, Detail: "404 Not Found"}
}
