package rfqs_test

// Per-stage resume cases for POST /v1/rfqs/{id}/reprocess (spec 2026-11-11
// section 4 "Reprocess"): the checkpoint walk's one accepted path per
// resumed stage, the ingest-shell resurrection, and the enqueue races the
// dedupe window opens. Fixtures, the rig and the read-back helpers live in
// reprocess_test.go — same package.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/jobs"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// ---------------------------------------------------------------------------
// Happy paths — one per resumed stage (spec Reprocess checkpoint walk)
// ---------------------------------------------------------------------------

// TestReprocessResumeExtractFromFailed: a dead-lettered FAILED shell
// whose receipt is already gone can't be resurrected — the step-0 shell
// signature fires but the inbound_messages lookup misses, so the walk
// falls through to S2, the RFQ goes back to PARSING and the rfq_extract
// job is staged in the same tx. The audit row carries the actor, the
// cleared failure_reason and the stage.
func TestReprocessResumeExtractFromFailed(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "reproc-failed")
	user := seedUser(t)
	seedMembership(t, org, user, platform.RolePricingAnalyst)
	rig := newReprocessRig(t, db, 60*time.Second, func() time.Time { return rigNow })
	rfq := seedRfq(t, db, org, rfqSeed{
		status:        "FAILED",
		failureReason: strptr("rfq_extract: ai-engine unavailable after 3 attempts"),
	})

	rec := rig.postReprocess(t, user, org, rfq)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var body authapi.ReprocessRfqResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, authapi.Accepted, body.Status)
	assert.Equal(t, authapi.RfqExtract, body.ResumedStage)

	r := loadRfq(t, db, org, rfq)
	assert.Equal(t, "PARSING", r.Status, "the resumed stage's active status")
	assert.Nil(t, r.ReviewReason)
	assert.Nil(t, r.FailureReason, "the cleared failure_reason")

	assert.Equal(t, []string{"rfq_extract"}, stagedJobs(t, db, rfq))
	assert.Equal(t, org.String(), stagedJobArg(t, db, "rfq_extract", rfq, "organization_id"))

	n, meta, actor := reprocessedAudits(t, db, org, rfq)
	assert.Equal(t, 1, n)
	require.NotNil(t, actor)
	assert.Equal(t, user, *actor, "the audit row carries the acting user")
	assert.Equal(t, user.String(), meta["actor_user_id"])
	assert.Equal(t, "rfq_extract: ai-engine unavailable after 3 attempts", meta["cleared_reason"])
	assert.Equal(t, "rfq_extract", meta["resumed_stage"])
}

// TestReprocessResumeExtractTariffUpdate: a classifier-parked
// TARIFF_UPDATE never reached the extraction write — kind and reason stay
// visible until the walk re-runs S2 (spec Reprocess: re-extraction is the
// only honest resume when nothing usable exists downstream).
func TestReprocessResumeExtractTariffUpdate(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "reproc-tariff")
	user := seedUser(t)
	seedMembership(t, org, user, platform.RoleOwner)
	rig := newReprocessRig(t, db, 60*time.Second, func() time.Time { return rigNow })
	rfq := seedRfq(t, db, org, rfqSeed{
		status:       "NEEDS_REVIEW",
		kind:         "TARIFF_UPDATE",
		reviewReason: strptr("TARIFF_UPDATE"),
		textBody:     strptr("please quote"),
	})

	rec := rig.postReprocess(t, user, org, rfq)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var body authapi.ReprocessRfqResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, authapi.RfqExtract, body.ResumedStage)
	assert.Equal(t, []string{"rfq_extract"}, stagedJobs(t, db, rfq))
	_, meta, _ := reprocessedAudits(t, db, org, rfq)
	assert.Equal(t, "TARIFF_UPDATE", meta["cleared_reason"])
}

// TestReprocessResumeRates: usable extraction + owed-but-missing carrier
// checkpoint — the recomputed S3a gate (FCL + derived IMPORT + FOB) has
// no outcome row, so the walk lands on rfq_fetch_rates and the RFQ goes
// back to PRICING. A LOW_CONFIDENCE park resumes downstream, never at
// the paid extraction — the human reprocess IS the gate override.
func TestReprocessResumeRates(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "reproc-rates")
	user := seedUser(t)
	seedMembership(t, org, user, platform.RolePricingAnalyst)
	rig := newReprocessRig(t, db, 60*time.Second, func() time.Time { return rigNow })
	rfq := seedRfq(t, db, org, rfqSeed{
		status:       "NEEDS_REVIEW",
		reviewReason: strptr("LOW_CONFIDENCE"),
		textBody:     strptr("please quote"),
		payload:      extractionOwed,
		confidence:   strptr("0.6200"),
	})

	rec := rig.postReprocess(t, user, org, rfq)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var body authapi.ReprocessRfqResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, authapi.RfqFetchRates, body.ResumedStage,
		"LOW_CONFIDENCE resumes at the owed carrier fetch, never at the paid extraction")

	r := loadRfq(t, db, org, rfq)
	assert.Equal(t, "PRICING", r.Status)
	assert.Nil(t, r.ReviewReason)
	assert.Equal(t, []string{"rfq_fetch_rates"}, stagedJobs(t, db, rfq))
}

// TestReprocessResumePriceDoneFetch: a carrier checkpoint row for the
// recomputed request_hash means the completed fetch is never repeated —
// the walk falls through to rfq_price.
func TestReprocessResumePriceDoneFetch(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "reproc-done-fetch")
	user := seedUser(t)
	seedMembership(t, org, user, platform.RolePricingAnalyst)
	rig := newReprocessRig(t, db, 60*time.Second, func() time.Time { return rigNow })
	rfq := seedRfq(t, db, org, rfqSeed{
		status:       "NEEDS_REVIEW",
		reviewReason: strptr("MISSING_TARIFF"),
		textBody:     strptr("please quote"),
		payload:      extractionOwed,
		confidence:   strptr("0.8800"),
	})
	seedCarrierCheckpoint(t, db, org, rfq, owedHash())

	rec := rig.postReprocess(t, user, org, rfq)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var body authapi.ReprocessRfqResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, authapi.RfqPrice, body.ResumedStage,
		"the recorded NO_OFFER outcome completes the fetch checkpoint")
	assert.Equal(t, []string{"rfq_price"}, stagedJobs(t, db, rfq))
}

// TestReprocessResumePriceNoOwedFetch: the S3a gate recomputes false for
// LCL — no fetch was ever owed — so the missing DRAFT resumes at
// rfq_price directly.
func TestReprocessResumePriceNoOwedFetch(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "reproc-price")
	user := seedUser(t)
	seedMembership(t, org, user, platform.RoleAdmin)
	rig := newReprocessRig(t, db, 60*time.Second, func() time.Time { return rigNow })
	rfq := seedRfq(t, db, org, rfqSeed{
		status:       "NEEDS_REVIEW",
		reviewReason: strptr("PRICING_INCOMPLETE"),
		textBody:     strptr("please quote"),
		payload:      extractionNotOwed,
		confidence:   strptr("0.9100"),
	})

	rec := rig.postReprocess(t, user, org, rfq)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var body authapi.ReprocessRfqResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, authapi.RfqPrice, body.ResumedStage)
	assert.Equal(t, []string{"rfq_price"}, stagedJobs(t, db, rfq))
	r := loadRfq(t, db, org, rfq)
	assert.Equal(t, "PRICING", r.Status)
}

// TestReprocessResumeRender: a parked RFQ whose DRAFT never got its PDF —
// the render checkpoint is missing, so rfq_render re-runs for the version
// and the RFQ takes READY (it is already past pricing).
func TestReprocessResumeRender(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "reproc-render")
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
	quote := seedDraftQuote(t, db, org, rfq, nil)

	rec := rig.postReprocess(t, user, org, rfq)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var body authapi.ReprocessRfqResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, authapi.RfqRender, body.ResumedStage)

	r := loadRfq(t, db, org, rfq)
	assert.Equal(t, "READY", r.Status, "render re-runs for the version; the RFQ is past pricing")
	assert.Equal(t, []string{"rfq_render"}, stagedJobs(t, db, rfq))
	assert.Equal(t, quote.String(), stagedJobArg(t, db, "rfq_render", rfq, "quote_id"),
		"rfq_render is quote-keyed — the DRAFT version rides the args")
}

// TestReprocessRepriceCompleteDraft: every checkpoint exists (usable
// extraction, no owed fetch, DRAFT with pdf_storage_key) — the park
// questions the quote itself (NEGATIVE_MARGIN at send, a dead-lettered
// delivery), so the honest resume is rfq_price: S3b supersedes the stale
// DRAFT into a new version (rule 9).
func TestReprocessRepriceCompleteDraft(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "reproc-reprice")
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
	seedDraftQuote(t, db, org, rfq, &pdf)

	rec := rig.postReprocess(t, user, org, rfq)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var body authapi.ReprocessRfqResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, authapi.RfqPrice, body.ResumedStage,
		"all checkpoints present ⇒ reprice — the parked reason questions the quote itself")
	assert.Equal(t, []string{"rfq_price"}, stagedJobs(t, db, rfq))
	r := loadRfq(t, db, org, rfq)
	assert.Equal(t, "PRICING", r.Status)
}

// TestReprocessResumeIngestShell: the S1 dead-letter shell (FAILED, no
// bodies — the permanent-fetch-failure shape) with its receipt still in
// inbound_messages resurrects through rfq_ingest: the RFQ goes back to
// RECEIVED and the receipt-keyed job is staged in the same tx. The audit
// row records the cleared failure_reason and the ingest stage.
func TestReprocessResumeIngestShell(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "reproc-shell")
	user := seedUser(t)
	seedMembership(t, org, user, platform.RolePricingAnalyst)
	rig := newReprocessRig(t, db, 60*time.Second, func() time.Time { return rigNow })
	resendID := "resend-shell-" + uuid.NewString()
	receipt := seedReceipt(t, db, org, resendID)
	rfq := seedRfq(t, db, org, rfqSeed{
		status:        "FAILED",
		failureReason: strptr("rfq_ingest: fetch: 404 Not Found"),
		resendID:      resendID,
		// no bodies, no payload — the shell signature
	})

	rec := rig.postReprocess(t, user, org, rfq)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var body authapi.ReprocessRfqResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, authapi.RfqIngest, body.ResumedStage,
		"a fetchable shell resurrects through S1, not a hopeless extraction")

	r := loadRfq(t, db, org, rfq)
	assert.Equal(t, "RECEIVED", r.Status, "the ingest stage's active status")
	assert.Nil(t, r.FailureReason)

	assert.Equal(t, []string{"rfq_ingest"}, stagedJobsBy(t, db, "inbound_message_id", receipt.String()))
	var receiptArg string
	require.NoError(t, db.Pool().QueryRow(context.Background(),
		`SELECT args->>'inbound_message_id' FROM river.river_job WHERE kind = 'rfq_ingest'
		 AND args->>'organization_id' = $1`, org.String()).Scan(&receiptArg))
	assert.Equal(t, receipt.String(), receiptArg)

	_, meta, _ := reprocessedAudits(t, db, org, rfq)
	assert.Equal(t, "rfq_ingest: fetch: 404 Not Found", meta["cleared_reason"])
	assert.Equal(t, "rfq_ingest", meta["resumed_stage"])
}

// TestReprocessResumeRenderReadyGate: S4's terminal failure strands the
// RFQ at READY with an unrendered DRAFT — the one READY shape the gate
// accepts. The walk lands on rfq_render for the DRAFT version and the
// RFQ stays READY (it is already past pricing).
func TestReprocessResumeRenderReadyGate(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "reproc-ready")
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

	rec := rig.postReprocess(t, user, org, rfq)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var body authapi.ReprocessRfqResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, authapi.RfqRender, body.ResumedStage,
		"READY + unrendered DRAFT is a dead-lettered S4 — render re-runs")

	r := loadRfq(t, db, org, rfq)
	assert.Equal(t, "READY", r.Status)
	assert.Equal(t, []string{"rfq_render"}, stagedJobs(t, db, rfq))
	assert.Equal(t, quote.String(), stagedJobArg(t, db, "rfq_render", rfq, "quote_id"))
}

// ---------------------------------------------------------------------------
// Enqueue race — the dedupe window (spec Reprocess)
// ---------------------------------------------------------------------------

// TestReprocessDedupeRunningForcesNewJob: the staged re-enqueue dedupes
// on live states, so a same-key river_job still 'running' would swallow
// the insert — and strand the RFQ the moment that job completes (it holds
// no row lock any more: the endpoint's FOR UPDATE already owns it, so the
// running job is by definition about-to-complete). The enqueue detects
// the running dedupe hit and force-inserts past it: two rfq_extract rows
// prove the new job exists.
func TestReprocessDedupeRunningForcesNewJob(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "reproc-race")
	user := seedUser(t)
	seedMembership(t, org, user, platform.RolePricingAnalyst)
	rig := newReprocessRig(t, db, 60*time.Second, func() time.Time { return rigNow })
	rfq := seedRfq(t, db, org, rfqSeed{
		status:        "FAILED",
		failureReason: strptr("rfq_extract: ai-engine unavailable after 3 attempts"),
	})

	// The stale job: a real river insert (standard unique key, standard
	// dedupe window) flipped to 'running' — the state a finalizing worker
	// leaves until its own commit lands.
	ctx := context.Background()
	tx, err := db.Pool().Begin(ctx)
	require.NoError(t, err)
	_, err = rig.inserter.InsertTx(ctx, tx, jobs.ExtractArgs{
		OrganizationID: org, CorrelationID: uuid.New(), RfqID: rfq,
	}, nil)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	_, err = testenv.MigratorPool(t).Exec(ctx,
		`UPDATE river.river_job SET state = 'running'
		 WHERE kind = 'rfq_extract' AND args->>'rfq_id' = $1`, rfq.String())
	require.NoError(t, err)

	rec := rig.postReprocess(t, user, org, rfq)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	assert.Equal(t, []string{"rfq_extract", "rfq_extract"}, stagedJobs(t, db, rfq),
		"the running dedupe hit must not strand the RFQ — a new job is forced in")
}

// TestReprocessDedupeLiveJobAccepted: the same dedupe hit in a non-running
// live state is correct — the existing job will run the resumed work, so
// no second job is forced in.
func TestReprocessDedupeLiveJobAccepted(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "reproc-live")
	user := seedUser(t)
	seedMembership(t, org, user, platform.RolePricingAnalyst)
	rig := newReprocessRig(t, db, 60*time.Second, func() time.Time { return rigNow })
	rfq := seedRfq(t, db, org, rfqSeed{
		status:        "FAILED",
		failureReason: strptr("rfq_extract: ai-engine unavailable after 3 attempts"),
	})

	ctx := context.Background()
	tx, err := db.Pool().Begin(ctx)
	require.NoError(t, err)
	_, err = rig.inserter.InsertTx(ctx, tx, jobs.ExtractArgs{
		OrganizationID: org, CorrelationID: uuid.New(), RfqID: rfq,
	}, nil)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	// The seeded job stays 'available' — a live job owns the resume.

	rec := rig.postReprocess(t, user, org, rfq)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	assert.Equal(t, []string{"rfq_extract"}, stagedJobs(t, db, rfq),
		"an available dedupe hit already owns the resumed work")
}
