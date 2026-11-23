package rfqs_test

// The busy-quote refusals behind POST /v1/rfqs/{id}/reprocess (Devin
// Review: draftRenderState's lock must never wait — the endpoint already
// holds FOR UPDATE on the RFQ, and quote_send holds the quote lock across
// the provider delivery call before locking the RFQ, so waiting inverts
// their order and deadlocks). Each test holds FOR UPDATE on the DRAFT row
// inside a second tenant tx — the lock quote_send and rfq_render own
// while they run — and posts the reprocess synchronously: under NOWAIT
// the answer is a prompt 409. Every blocking point is bounded — the
// holder's tx ctx caps its acquire, the post carries postTimeout, the
// post-release resume polls under Eventually — so a plain-FOR-UPDATE
// regression fails the suite instead of hanging it. Fixtures, the rig
// and the read-back helpers live in reprocess_test.go.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// quoteLock is the held-lock fixture: the channels the holder goroutine,
// the test and the cleanup coordinate on. drained tells the cleanup the
// test already consumed the holder's result (it must not wait on an
// empty channel); once guards the release close so the test's explicit
// release and the cleanup never double-close.
type quoteLock struct {
	locked  chan struct{}
	release chan struct{}
	done    chan error
	drained bool
	once    sync.Once
}

// holdQuoteLock takes FOR UPDATE on the quote row inside a tenant tx on
// a second pooled connection — the row lock quote_send and rfq_render
// hold while their work runs — and returns the release. The cleanup is
// registered BEFORE the holder goroutine spawns, so an acquire failure
// or the wait timeout below still frees the holder's wait and drains
// done: a leaked tx would keep its pooled conn and row lock and block
// the teardown. Every wait is bounded — no regression can hang the
// suite.
func holdQuoteLock(t *testing.T, db *tenantdb.DB, org, quote uuid.UUID) func() {
	t.Helper()
	l := &quoteLock{locked: make(chan struct{}), release: make(chan struct{}), done: make(chan error, 1)}
	t.Cleanup(func() { l.releaseAndDrain(t) })
	go l.run(db, org, quote)
	select {
	case <-l.locked:
	case err := <-l.done:
		l.drained = true
		t.Fatalf("the lock holder's tenant tx failed before locking: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the lock holder never acquired the quote row")
	}
	return func() { l.once.Do(func() { close(l.release) }) }
}

// run is the holder goroutine: one tenant tx, bounded by a 15s ctx so a
// stuck acquire can never hang it, that takes FOR UPDATE on the quote,
// signals locked and parks until release. The result lands in done —
// buffered so the send never blocks after the test drained early.
func (l *quoteLock) run(db *tenantdb.DB, org, quote uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	l.done <- db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		var id uuid.UUID
		if err := tx.QueryRow(ctx,
			`SELECT id FROM app.quotes WHERE id = $1 FOR UPDATE`, quote).Scan(&id); err != nil {
			return err
		}
		close(l.locked)
		<-l.release
		return nil
	})
}

// releaseAndDrain is the cleanup: close release (Once — the test's
// explicit release may have fired already), then bound the drain of
// done so a wedged holder reports instead of deadlocking the suite.
func (l *quoteLock) releaseAndDrain(t *testing.T) {
	l.once.Do(func() { close(l.release) })
	if l.drained {
		return
	}
	select {
	case err := <-l.done:
		require.NoError(t, err, "the lock holder's tenant tx must commit clean")
	case <-time.After(15 * time.Second):
		t.Error("the lock holder's tenant tx never returned — its row lock may leak")
	}
}

// busyFixture is the contention tests' seeded world — the rig, the ids
// the variant assertions read, and the held lock's release.
type busyFixture struct {
	rig     *reprocessRig
	db      *tenantdb.DB
	org     uuid.UUID
	user    uuid.UUID
	rfq     uuid.UUID
	quote   uuid.UUID
	release func()
}

// setupBusyDraft seeds one RFQ (rfqSeed carries the status/reasons
// variant) with a DRAFT quote — rendered pins pdf_storage_key — then
// holds FOR UPDATE on the quote row in a second tenant tx, the lock a
// live quote_send/rfq_render worker owns.
func setupBusyDraft(t *testing.T, slug string, seed rfqSeed, rendered bool) busyFixture {
	t.Helper()
	db := testenv.Open(t)
	f := busyFixture{db: db, org: uuid.New(), user: seedUser(t)}
	testenv.SeedOrganization(t, f.org, slug)
	seedMembership(t, f.org, f.user, platform.RolePricingAnalyst)
	f.rig = newReprocessRig(t, db, 60*time.Second, func() time.Time { return rigNow })
	f.rfq = seedRfq(t, db, f.org, seed)
	f.quote = seedDraftQuote(t, db, f.org, f.rfq, pdfFor(f.org, rendered))
	f.release = holdQuoteLock(t, db, f.org, f.quote)
	return f
}

// pdfFor returns the pdf_storage_key a rendered DRAFT carries; nil is
// the awaiting-first-render shape the READY carveout accepts.
func pdfFor(org uuid.UUID, rendered bool) *string {
	if !rendered {
		return nil
	}
	return strptr("tenants/" + org.String() + "/quotes/x_v1.pdf")
}

// assertBusy409 posts the reprocess and pins the transient refusal
// shape: 409 with rfq_busy — the code that tells a contended quote
// apart from the permanent rfq_not_parked — and the RFQ's current
// status riding the body; nothing staged (the refusal writes nothing
// and never enqueues).
func (f busyFixture) assertBusy409(t *testing.T, wantStatus string) {
	t.Helper()
	rec := f.rig.postReprocess(t, f.user, f.org, f.rfq)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var body authapi.ReprocessConflict
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, authapi.ErrorCodeRfqBusy, body.Error)
	assert.Equal(t, wantStatus, body.Status)
	assert.Empty(t, stagedJobs(t, f.db, f.rfq), "a busy refusal enqueues nothing")
}

// releaseAndResume frees the quote lock and polls the reprocess until it
// lands. The holder's COMMIT can trail release() by a few ms — the next
// post could still read the row as held — so the succeeding call is
// polled under Eventually (2s/50ms): bounded, so a regression fails
// rather than flaking or hanging.
func (f busyFixture) releaseAndResume(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	f.release()
	var rec *httptest.ResponseRecorder
	require.Eventually(t, func() bool {
		var err error
		rec, err = f.rig.tryReprocess(f.user, f.org, f.rfq)
		return err == nil && rec.Code == http.StatusAccepted
	}, 2*time.Second, 50*time.Millisecond,
		"the released quote row must free the reprocess")
	return rec
}

// TestReprocessReadyBusyDraft409: READY + an unrendered DRAFT is the one
// reprocessable READY shape — but a live worker holding the quote row
// flips the gate's DraftAwaitingRender NOWAIT read to busy, and busy is
// honest "not reprocessable" signal: the same 409 a not-parked RFQ gets,
// not a wait that would deadlock against quote_send's quote→RFQ order.
// Releasing the row then lets a real reprocess resume the render.
func TestReprocessReadyBusyDraft409(t *testing.T) {
	f := setupBusyDraft(t, "reproc-busy-ready", rfqSeed{
		status:     "READY",
		textBody:   strptr("please quote"),
		payload:    extractionNotOwed,
		confidence: strptr("0.9100"),
	}, false)
	f.assertBusy409(t, "READY")
	assert.Equal(t, "READY", loadRfq(t, f.db, f.org, f.rfq).Status, "a 409 writes nothing")
	f.releaseAndResume(t)
	assert.Equal(t, []string{"rfq_render"}, stagedJobs(t, f.db, f.rfq))
	assert.Equal(t, f.quote.String(), stagedJobArg(t, f.db, "rfq_render", f.rfq, "quote_id"))
}

// TestReprocessParkedBusyDraft409: the same contended lock on a parked
// RFQ's DRAFT hits inside DecideResume — ErrQuoteBusy maps onto the
// not-parked 409 (a refusal, not a 500 fault) and the refusal writes
// nothing: no status flip, no cleared reason, no audit row, no job.
func TestReprocessParkedBusyDraft409(t *testing.T) {
	f := setupBusyDraft(t, "reproc-busy-parked", rfqSeed{
		status:       "NEEDS_REVIEW",
		reviewReason: strptr("NEGATIVE_MARGIN"),
		textBody:     strptr("please quote"),
		payload:      extractionNotOwed,
		confidence:   strptr("0.9100"),
	}, true)
	f.assertBusy409(t, "NEEDS_REVIEW")
	r := loadRfq(t, f.db, f.org, f.rfq)
	require.NotNil(t, r.ReviewReason)
	assert.Equal(t, "NEGATIVE_MARGIN", *r.ReviewReason, "a refused reprocess clears nothing")
	n, _, _ := reprocessedAudits(t, f.db, f.org, f.rfq)
	assert.Equal(t, 0, n, "no audit row — the refusal rolled back clean")
	f.releaseAndResume(t)
	assert.Equal(t, []string{"rfq_price"}, stagedJobs(t, f.db, f.rfq),
		"the released row resumes for real — a rendered DRAFT reprices")
}
