package rfqs_test

// The busy-quote refusals behind POST /v1/rfqs/{id}/reprocess (Devin
// Review: draftRenderState's lock must never wait — the endpoint already
// holds FOR UPDATE on the RFQ, and quote_send holds the quote lock across
// the provider delivery call before locking the RFQ, so waiting inverts
// their order and deadlocks). Each test holds FOR UPDATE on the DRAFT row
// inside a second tenant tx — the lock quote_send and rfq_render own
// while they run — and posts the reprocess synchronously: under NOWAIT
// the answer is a prompt 409; a plain-FOR-UPDATE regression would hang
// on the held row forever, so the return itself is the NOWAIT assertion.
// Fixtures, the rig and the read-back helpers live in reprocess_test.go.

import (
	"context"
	"encoding/json"
	"net/http"
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

// holdQuoteLock takes FOR UPDATE on the quote row inside a tenant tx on
// a second pooled connection — the row lock quote_send and rfq_render
// hold while their work runs — and returns the release. The tx commits
// at test cleanup either way, so a failed assert never leaks the pooled
// connection.
func holdQuoteLock(t *testing.T, db *tenantdb.DB, org, quote uuid.UUID) func() {
	t.Helper()
	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
			var id uuid.UUID
			if err := tx.QueryRow(ctx,
				`SELECT id FROM app.quotes WHERE id = $1 FOR UPDATE`, quote).Scan(&id); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	select {
	case <-locked:
	case err := <-done:
		t.Fatalf("the lock holder's tenant tx failed before locking: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("the lock holder never acquired the quote row")
	}
	var once sync.Once
	t.Cleanup(func() {
		once.Do(func() { close(release) })
		require.NoError(t, <-done, "the lock holder's tenant tx must commit clean")
	})
	return func() { once.Do(func() { close(release) }) }
}

// TestReprocessReadyBusyDraft409: READY + an unrendered DRAFT is the one
// reprocessable READY shape — but a live worker holding the quote row
// flips the gate's DraftAwaitingRender NOWAIT read to busy, and busy is
// honest "not reprocessable" signal: the same 409 a not-parked RFQ gets,
// not a wait that would deadlock against quote_send's quote→RFQ order.
// Releasing the row then lets a real reprocess resume the render.
func TestReprocessReadyBusyDraft409(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "reproc-busy-ready")
	user := seedUser(t)
	seedMembership(t, org, user, platform.RolePricingAnalyst)
	rig := newReprocessRig(t, db, 60*time.Second, func() time.Time { return rigNow })
	rfq := seedRfq(t, db, org, rfqSeed{
		status:     "READY",
		textBody:   strptr("please quote"),
		payload:    extractionNotOwed,
		confidence: strptr("0.9100"),
	})
	quote := seedDraftQuote(t, db, org, rfq, nil)
	release := holdQuoteLock(t, db, org, quote)

	rec := rig.postReprocess(t, user, org, rfq)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var body authapi.ReprocessConflict
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, authapi.ErrorCodeRfqNotParked, body.Error)
	assert.Equal(t, "READY", body.Status)
	assert.Empty(t, stagedJobs(t, db, rfq), "a busy refusal enqueues nothing")
	r := loadRfq(t, db, org, rfq)
	assert.Equal(t, "READY", r.Status, "a 409 writes nothing")

	release()
	rec = rig.postReprocess(t, user, org, rfq)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String(),
		"the released quote row lets the READY carveout resume the render")
	assert.Equal(t, []string{"rfq_render"}, stagedJobs(t, db, rfq))
	assert.Equal(t, quote.String(), stagedJobArg(t, db, "rfq_render", rfq, "quote_id"))
}

// TestReprocessParkedBusyDraft409: the same contended lock on a parked
// RFQ's DRAFT hits inside DecideResume — ErrQuoteBusy maps onto the
// not-parked 409 (a refusal, not a 500 fault) and the refusal writes
// nothing: no status flip, no cleared reason, no audit row, no job.
func TestReprocessParkedBusyDraft409(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "reproc-busy-parked")
	user := seedUser(t)
	seedMembership(t, org, user, platform.RolePricingAnalyst)
	rig := newReprocessRig(t, db, 60*time.Second, func() time.Time { return rigNow })
	rfq := seedRfq(t, db, org, rfqSeed{
		status:       "NEEDS_REVIEW",
		reviewReason: strptr("NEGATIVE_MARGIN"),
		textBody:     strptr("please quote"),
		payload:      extractionNotOwed,
		confidence:   strptr("0.9100"),
	})
	pdf := "tenants/" + org.String() + "/quotes/x_v1.pdf"
	quote := seedDraftQuote(t, db, org, rfq, &pdf)
	release := holdQuoteLock(t, db, org, quote)

	rec := rig.postReprocess(t, user, org, rfq)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var body authapi.ReprocessConflict
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, authapi.ErrorCodeRfqNotParked, body.Error)
	assert.Equal(t, "NEEDS_REVIEW", body.Status,
		"ErrQuoteBusy rides the same 409 a not-parked RFQ gets — with the RFQ's real status")

	r := loadRfq(t, db, org, rfq)
	assert.Equal(t, "NEEDS_REVIEW", r.Status)
	require.NotNil(t, r.ReviewReason)
	assert.Equal(t, "NEGATIVE_MARGIN", *r.ReviewReason, "a refused reprocess clears nothing")
	n, _, _ := reprocessedAudits(t, db, org, rfq)
	assert.Equal(t, 0, n, "no audit row — the refusal rolled back clean")
	assert.Empty(t, stagedJobs(t, db, rfq), "no job is staged")

	release()
	rec = rig.postReprocess(t, user, org, rfq)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"rfq_price"}, stagedJobs(t, db, rfq),
		"the released row resumes for real — a rendered DRAFT reprices")
}
