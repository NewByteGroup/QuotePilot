package jobs_test

// Real-Postgres tests for the quote_send claim row (issue #65): the
// app.quote_send_claims insert/arbitrate/steal/delete lifecycle that
// replaced lock-across-provider mutual exclusion. Every assertion runs
// under the real qp_app role and tenant RLS — the claim table is a Camada
// B table, so testenv's tenant tx is the only honest way to drive it.

import (
	"context"
	"errors"
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
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/resend"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// seedClaim writes a claim row the way a rival or a crashed predecessor
// would leave it — inside the fixture's own tenant tx.
func seedClaim(t *testing.T, db *tenantdb.DB, f sendFixture, jobID int64) {
	t.Helper()
	require.NoError(t, db.InTenantTx(context.Background(), f.org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.quote_send_claims (quote_id, organization_id, claimed_by)
			VALUES ($1, $2, $3)`, f.quote, f.org, jobID)
		return err
	}))
}

// claimHolder reads back the claim row for the fixture's quote; exists
// reports whether any row survives.
func claimHolder(t *testing.T, db *tenantdb.DB, f sendFixture) (claimedBy int64, exists bool) {
	t.Helper()
	require.NoError(t, db.InTenantTx(context.Background(), f.org, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`SELECT claimed_by FROM app.quote_send_claims WHERE quote_id = $1`, f.quote).
			Scan(&claimedBy)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		exists = true
		return nil
	}))
	return claimedBy, exists
}

// stageRiverJob inserts a real river_job row (state 'available') through
// an insert-only client — the liveness anchor a live claim names. The job
// is never worked: it stays 'available', a live state.
func stageRiverJob(t *testing.T, db *tenantdb.DB, org uuid.UUID) int64 {
	t.Helper()
	client, err := river.NewClient(riverpgxv5.New(db.Pool()), &river.Config{
		Schema:              "river",
		ReindexerSchedule:   river.NeverSchedule(),
		SkipUnknownJobCheck: true,
	})
	require.NoError(t, err)
	var id int64
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		res, err := client.InsertTx(ctx, tx,
			jobs.EchoTenantArgs{OrganizationID: org}, nil)
		if err != nil {
			return err
		}
		id = res.Job.ID
		return nil
	}))
	return id
}

// deadJobID is a river_job.id no sequence can have issued yet — the
// "claim row without a job" stale shape.
const deadJobID = int64(1) << 60

// sendJobAs wraps args with a JobRow carrying a specific river_job id —
// the shape Work consumes when River really runs the job.
func sendJobAs(org, quote uuid.UUID, jobID int64) *river.Job[jobs.SendArgs] {
	j := sendJob(org, quote)
	j.JobRow = &rivertype.JobRow{ID: jobID, Attempt: 1, MaxAttempts: 5}
	return j
}

// TestQuoteSendClaimLifecycle: a healthy send leaves no claim behind —
// the record tx deletes its own row in the same commit that flips the
// quote, so a delivered quote can never hold a live claim.
func TestQuoteSendClaimLifecycle(t *testing.T) {
	db := testenv.Open(t)
	f := seedSend(t, db, healthyFixture())
	storage := platform.NewFakeStorage()
	require.NoError(t, storage.Put(context.Background(), f.pdfKey, []byte("pdf")))
	sender := &resend.FakeSender{MessageID: "out-msg-" + uuid.NewString()}
	rig := newSendRig(db, sender, storage)

	require.NoError(t, rig.worker.Work(context.Background(), sendJob(f.org, f.quote)))

	assert.Equal(t, 1, sender.Calls())
	_, exists := claimHolder(t, db, f)
	assert.False(t, exists, "a committed send must delete its claim row")
	s := loadSendState(t, db, f)
	assert.Equal(t, "APPROVED_SENT", s.Status)
}

// TestQuoteSendRivalLiveClaimStays: a claim whose claimed_by names a LIVE
// river_job is an in-flight send — this job returns a retryable error
// (never JobCancel), sends nothing, and leaves the rival's row alone so
// the retry converges when the holder finishes.
func TestQuoteSendRivalLiveClaimStays(t *testing.T) {
	db := testenv.Open(t)
	f := seedSend(t, db, healthyFixture())
	storage := platform.NewFakeStorage()
	require.NoError(t, storage.Put(context.Background(), f.pdfKey, []byte("pdf")))
	sender := &resend.FakeSender{MessageID: "out-msg-" + uuid.NewString()}
	rig := newSendRig(db, sender, storage)
	rivalID := stageRiverJob(t, db, f.org)
	seedClaim(t, db, f, rivalID)

	err := rig.worker.Work(context.Background(), sendJob(f.org, f.quote))

	require.Error(t, err)
	var jce *rivertype.JobCancelError
	assert.False(t, errors.As(err, &jce), "a live rival is retryable, not a cancel")
	assert.Contains(t, err.Error(), "in flight")
	assert.Equal(t, 0, sender.Calls())
	held, exists := claimHolder(t, db, f)
	assert.True(t, exists, "the rival's live claim must survive")
	assert.Equal(t, rivalID, held)
	s := loadSendState(t, db, f)
	assert.Equal(t, "DRAFT", s.Status)
}

// TestQuoteSendStaleClaimStolen: a claim whose claimed_by names no
// river_job row is dead weight — the crashed worker's bookkeeping can
// never commit, so this job steals the row and sends.
func TestQuoteSendStaleClaimStolen(t *testing.T) {
	db := testenv.Open(t)
	f := seedSend(t, db, healthyFixture())
	storage := platform.NewFakeStorage()
	require.NoError(t, storage.Put(context.Background(), f.pdfKey, []byte("pdf")))
	sender := &resend.FakeSender{MessageID: "out-msg-" + uuid.NewString()}
	rig := newSendRig(db, sender, storage)
	seedClaim(t, db, f, deadJobID)

	require.NoError(t, rig.worker.Work(context.Background(), sendJob(f.org, f.quote)))

	assert.Equal(t, 1, sender.Calls(), "a stale claim is stolen, not obeyed")
	_, exists := claimHolder(t, db, f)
	assert.False(t, exists, "the steal ends in the record tx's own delete")
	s := loadSendState(t, db, f)
	assert.Equal(t, "APPROVED_SENT", s.Status)
}

// TestQuoteSendTerminalClaimStolen: a claim naming a river_job in a
// TERMINAL state is equally dead — liveness is a state check, not an
// existence check.
func TestQuoteSendTerminalClaimStolen(t *testing.T) {
	db := testenv.Open(t)
	f := seedSend(t, db, healthyFixture())
	storage := platform.NewFakeStorage()
	require.NoError(t, storage.Put(context.Background(), f.pdfKey, []byte("pdf")))
	sender := &resend.FakeSender{MessageID: "out-msg-" + uuid.NewString()}
	rig := newSendRig(db, sender, storage)
	rivalID := stageRiverJob(t, db, f.org)
	// The river_job CHECK requires finalized_at on terminal states.
	_, err := db.Pool().Exec(context.Background(),
		`UPDATE river.river_job SET state = 'completed', finalized_at = now() WHERE id = $1`, rivalID)
	require.NoError(t, err)
	seedClaim(t, db, f, rivalID)

	require.NoError(t, rig.worker.Work(context.Background(), sendJob(f.org, f.quote)))

	assert.Equal(t, 1, sender.Calls(), "a terminal claimant is stealable")
	_, exists := claimHolder(t, db, f)
	assert.False(t, exists)
}

// TestQuoteSendOwnClaimContinues: a claim this job itself owns is always
// valid for it — the retry-continuation path that makes every attempt a
// single writer (claimed_by == our job.ID, no liveness check consulted).
func TestQuoteSendOwnClaimContinues(t *testing.T) {
	db := testenv.Open(t)
	f := seedSend(t, db, healthyFixture())
	storage := platform.NewFakeStorage()
	require.NoError(t, storage.Put(context.Background(), f.pdfKey, []byte("pdf")))
	sender := &resend.FakeSender{MessageID: "out-msg-" + uuid.NewString()}
	rig := newSendRig(db, sender, storage)
	// The id only has to match JobRow — own-claim wins before any
	// river_job lookup, so an unissued id is fine here.
	const ourJob = int64(424242)
	seedClaim(t, db, f, ourJob)

	require.NoError(t, rig.worker.Work(context.Background(), sendJobAs(f.org, f.quote, ourJob)))

	assert.Equal(t, 1, sender.Calls(), "own claim proceeds to the provider phase")
	_, exists := claimHolder(t, db, f)
	assert.False(t, exists)
	s := loadSendState(t, db, f)
	assert.Equal(t, "APPROVED_SENT", s.Status)
}

// TestQuoteSendRefusalDropsClaim: a deterministic refusal released the
// stale claim it found in the same tx that wrote the refusal audit —
// terminal paths never leave a claim for a successor to arbitrate.
func TestQuoteSendRefusalDropsClaim(t *testing.T) {
	db := testenv.Open(t)
	o := healthyFixture()
	o.pdfKey = "" // missing render → refuseMissingRender
	f := seedSend(t, db, o)
	sender := &resend.FakeSender{MessageID: "out-msg-" + uuid.NewString()}
	rig := newSendRig(db, sender, platform.NewFakeStorage())
	seedClaim(t, db, f, deadJobID)

	err := rig.worker.Work(context.Background(), sendJob(f.org, f.quote))
	assertJobCancel(t, err)
	assert.Equal(t, 0, sender.Calls())

	_, exists := claimHolder(t, db, f)
	assert.False(t, exists, "the refusal's own commit drops the claim")
	n, meta := auditCount(t, db, f.org, "quote.send_refused")
	assert.Equal(t, 1, n)
	assert.Equal(t, "missing_render", meta["reason"])
}

// blockingSender is a resend.Sender that parks inside Send until the test
// releases it — the way to prove the provider phase holds no quote lock.
type blockingSender struct {
	started     chan struct{}
	release     chan struct{}
	once        sync.Once
	releaseOnce sync.Once
}

var _ resend.Sender = (*blockingSender)(nil)

// Release unparks Send — idempotent so a failed assertion mid-test can
// never strand the worker goroutine.
func (b *blockingSender) Release() { b.releaseOnce.Do(func() { close(b.release) }) }

func (b *blockingSender) Send(ctx context.Context, _ resend.SendRequest) (string, error) {
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.release:
		return "out-msg-" + uuid.NewString(), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// TestQuoteSendQuoteLockFreeDuringProvider is the test the claim row
// exists for: with Send parked mid-call, a NOWAIT probe takes the quote's
// row lock instantly — under the old single-tx send the same probe died
// on 55P03. Mutual exclusion mid-send is the claim row, nothing else.
func TestQuoteSendQuoteLockFreeDuringProvider(t *testing.T) {
	db := testenv.Open(t)
	f := seedSend(t, db, healthyFixture())
	storage := platform.NewFakeStorage()
	require.NoError(t, storage.Put(context.Background(), f.pdfKey, []byte("pdf")))
	blocker := &blockingSender{started: make(chan struct{}), release: make(chan struct{})}
	defer blocker.Release() // a failed probe must not strand the worker goroutine
	worker := jobs.NewQuoteSendWorker(db, blocker, storage, rigOutboundFrom)

	done := make(chan error, 1)
	go func() { done <- worker.Work(context.Background(), sendJob(f.org, f.quote)) }()

	select {
	case <-blocker.started:
	case <-time.After(10 * time.Second):
		t.Fatal("provider call never started")
	}

	require.NoError(t, db.InTenantTx(context.Background(), f.org, func(ctx context.Context, tx pgx.Tx) error {
		var id uuid.UUID
		return tx.QueryRow(ctx,
			`SELECT id FROM app.quotes WHERE id = $1 FOR UPDATE NOWAIT`, f.quote).Scan(&id)
	}))
	_, held := claimHolder(t, db, f)
	assert.True(t, held, "the claim, not the lock, excludes rivals mid-send")

	blocker.Release()
	require.NoError(t, <-done)
	s := loadSendState(t, db, f)
	assert.Equal(t, "APPROVED_SENT", s.Status)
}

// TestQuoteSendLastAttemptLiveClaimKeepsRfq: a job that dead-letters on
// the in-flight error leaves the RFQ and the rival's claim alone — the
// live rival still owns the send's outcome, so writeSendFailed must not
// run for it.
func TestQuoteSendLastAttemptLiveClaimKeepsRfq(t *testing.T) {
	db := testenv.Open(t)
	f := seedSend(t, db, healthyFixture())
	rivalID := stageRiverJob(t, db, f.org)
	seedClaim(t, db, f, rivalID)
	sender := &resend.FakeSender{MessageID: "out-msg-" + uuid.NewString()}
	rig := newSendRig(db, sender, platform.NewFakeStorage())

	job := sendJobAs(f.org, f.quote, deadJobID+1)
	job.Attempt = job.MaxAttempts // final attempt: dead-letter path
	err := rig.worker.Work(context.Background(), job)
	require.Error(t, err)

	s := loadSendState(t, db, f)
	assert.NotEqual(t, "FAILED", s.RfqState, "a live rival still owns the send's outcome")
	held, exists := claimHolder(t, db, f)
	assert.True(t, exists)
	assert.Equal(t, rivalID, held, "the rival's claim survives")
	assert.Equal(t, 0, sender.Calls())
}
