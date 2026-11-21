package jobs_test

// Failure/edge paths for S2 rfq_extract (shared fakes and helpers live in
// rfqextract_test.go): contract violations, sanity bounds, transient
// retries, idempotency and RLS isolation.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/aiengine"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/jobs"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// TestRfqExtractSanityBounds: contract-valid shape but insane probabilities
// (sum far from 1) is also a contract violation — never written.
func TestRfqExtractSanityBounds(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "extract-bounds")
	rfqID := seedRfq(t, db, org, "ext-bounds", "PARSING", "please quote", "")

	raw := validExtraction(t, 0.9)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(raw, &doc))
	doc["classification"].(map[string]any)["probabilities"] = map[string]any{
		"RFQ": 1.9, "TARIFF_UPDATE": 0.5, "OTHER": 0.5,
	}
	raw, _ = json.Marshal(doc)

	engine := &fakeAIEngine{classify: classifyResult("RFQ"), extract: raw}
	worker := newExtractRig(t, db, engine, platform.NewFakeStorage())
	require.NoError(t, worker.Work(context.Background(), extractJob(org, uuid.New(), rfqID, 2)))

	r := loadExtractRfq(t, db, org, rfqID)
	assert.Equal(t, "NEEDS_REVIEW", r.Status)
	assert.Nil(t, r.Payload)
}

// TestRfqExtractTransientRetries: a transient engine error propagates for
// River retry — no status flip, nothing parked.
func TestRfqExtractTransientRetries(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "extract-trans")
	rfqID := seedRfq(t, db, org, "ext-trans", "PARSING", "please quote", "")

	engine := &fakeAIEngine{
		classifyErr: &aiengine.EngineError{Op: "classify", StatusCode: 503, Detail: "upstream"},
	}
	worker := newExtractRig(t, db, engine, platform.NewFakeStorage())
	err := worker.Work(context.Background(), extractJob(org, uuid.New(), rfqID, 1))
	require.Error(t, err, "transient errors must propagate for retry")

	r := loadExtractRfq(t, db, org, rfqID)
	assert.Equal(t, "PARSING", r.Status, "transient must not park the RFQ")
	assert.Nil(t, r.ReviewReason)
	assert.Equal(t, 0, countRateJobs(t, db, org, rfqID))
}

// TestRfqExtractTerminalNoOp: a job arriving on a terminal-status RFQ is an
// idempotent no-op (replay / manual re-enqueue).
func TestRfqExtractTerminalNoOp(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "extract-noop")
	rfqID := seedRfq(t, db, org, "ext-noop", "PRICING", "please quote", "")

	engine := &fakeAIEngine{classify: classifyResult("RFQ"), extract: validExtraction(t, 0.9)}
	worker := newExtractRig(t, db, engine, platform.NewFakeStorage())
	require.NoError(t, worker.Work(context.Background(), extractJob(org, uuid.New(), rfqID, 0)))

	assert.Equal(t, 0, engine.classifyCalls, "terminal status = no work")
	r := loadExtractRfq(t, db, org, rfqID)
	assert.Equal(t, "PRICING", r.Status)
}

// TestRfqExtractOrgIsolation: org B's job referencing org A's rfq_id sees
// nothing (RLS) and writes nothing to org A.
func TestRfqExtractOrgIsolation(t *testing.T) {
	db := testenv.Open(t)
	orgA := uuid.New()
	orgB := uuid.New()
	testenv.SeedOrganization(t, orgA, "extract-orga")
	testenv.SeedOrganization(t, orgB, "extract-orgb")
	rfqA := seedRfq(t, db, orgA, "ext-orga", "PARSING", "please quote", "")

	engine := &fakeAIEngine{classify: classifyResult("RFQ"), extract: validExtraction(t, 0.9)}
	worker := newExtractRig(t, db, engine, platform.NewFakeStorage())
	require.NoError(t, worker.Work(context.Background(), extractJob(orgB, uuid.New(), rfqA, 0)))

	assert.Equal(t, 0, engine.classifyCalls, "org B cannot reach org A's row")
	r := loadExtractRfq(t, db, orgA, rfqA)
	assert.Equal(t, "PARSING", r.Status, "org A's row untouched")
}

// TestRfqExtractUnknownLabelIsViolation: an unrecognized classify label
// ("HAM", "", garbage) is a contract violation — it must NOT fall through
// into the paid extraction call.
func TestRfqExtractUnknownLabelIsViolation(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "extract-label")
	rfqID := seedRfq(t, db, org, "ext-label", "PARSING", "please quote", "")

	engine := &fakeAIEngine{classify: classifyResult("HAM")}
	worker := newExtractRig(t, db, engine, platform.NewFakeStorage())

	// Attempt 1: the violation propagates for the single retry.
	err := worker.Work(context.Background(), extractJob(org, uuid.New(), rfqID, 1))
	require.Error(t, err)
	assert.Equal(t, 0, engine.extractCalls, "unknown label must never reach extract")

	// Attempt 2: parks INVALID_EXTRACTION.
	require.NoError(t, worker.Work(context.Background(), extractJob(org, uuid.New(), rfqID, 2)))
	r := loadExtractRfq(t, db, org, rfqID)
	assert.Equal(t, "NEEDS_REVIEW", r.Status)
	require.NotNil(t, r.ReviewReason)
	assert.Equal(t, string(jobs.ReviewReasonInvalidExtraction), *r.ReviewReason)
	assert.Equal(t, 0, engine.extractCalls)
}

// TestRfqExtractEngineUnavailableFinalAttempt: a transient engine error on
// the job's FINAL attempt parks ENGINE_UNAVAILABLE and commits — returning
// the error would roll back the status write and leave the RFQ stuck
// invisible in PARSING.
func TestRfqExtractEngineUnavailableFinalAttempt(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "extract-unavail")
	rfqID := seedRfq(t, db, org, "ext-unavail", "PARSING", "please quote", "")

	engine := &fakeAIEngine{
		classifyErr: &aiengine.EngineError{Op: "classify", StatusCode: 503, Detail: "upstream"},
	}
	worker := newExtractRig(t, db, engine, platform.NewFakeStorage())

	// Attempts 1-2 still propagate for retry.
	err := worker.Work(context.Background(), extractJob(org, uuid.New(), rfqID, 2))
	require.Error(t, err, "non-final transient failures still retry")
	r := loadExtractRfq(t, db, org, rfqID)
	assert.Equal(t, "PARSING", r.Status)

	// Final attempt (3 == MaxAttempts): parks and commits.
	require.NoError(t, worker.Work(context.Background(), extractJob(org, uuid.New(), rfqID, 3)))
	r = loadExtractRfq(t, db, org, rfqID)
	assert.Equal(t, "NEEDS_REVIEW", r.Status)
	require.NotNil(t, r.ReviewReason)
	assert.Equal(t, string(jobs.ReviewReasonEngineUnavailable), *r.ReviewReason)
	assert.Equal(t, 1, countAudit(t, db, org, "rfq.parked"))
}

// TestRfqExtractClassifyRequestShape: the classify call carries body_html
// for HTML-only emails and bounds body_text at the ADR-006 16 KiB limit.
func TestRfqExtractClassifyRequestShape(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "extract-reqshape")
	bigBody := strings.Repeat("é", 17000) // ~34 KB of 2-byte runes
	rfqID := seedRfq(t, db, org, "ext-reqshape", "PARSING", bigBody, "<p>html only</p>")

	engine := &fakeAIEngine{classify: classifyResult("OTHER")}
	worker := newExtractRig(t, db, engine, platform.NewFakeStorage())
	require.NoError(t, worker.Work(context.Background(), extractJob(org, uuid.New(), rfqID, 0)))

	assert.Equal(t, "<p>html only</p>", engine.lastClassify.BodyHTML,
		"classify must see the HTML body too")
	assert.LessOrEqual(t, len(engine.lastClassify.BodyText), 16*1024,
		"body_text is bounded at the ADR-006 limit")
	assert.True(t, utf8.ValidString(engine.lastClassify.BodyText),
		"truncation must not split a multi-byte rune")
}

// TestExtractReenqueueAfterCompletion is the reclassify invariant: a
// completed rfq_extract job's rfq_id CAN be re-enqueued (dedupe must cover
// in-flight work, not history), while a live duplicate still collapses.
func TestExtractReenqueueAfterCompletion(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "extract-reenq")
	rfqID := seedRfq(t, db, org, "ext-reenq", "PARSING", "please quote", "")

	client, err := river.NewClient(riverpgxv5.New(db.Pool()), &river.Config{
		Schema:              "river",
		ReindexerSchedule:   river.NeverSchedule(),
		SkipUnknownJobCheck: true,
	})
	require.NoError(t, err)
	ctx := context.Background()
	args := jobs.ExtractArgs{OrganizationID: org, CorrelationID: uuid.New(), RfqID: rfqID}

	// Insert inside a tenant tx (the shape reclassify uses), then complete
	// the job out-of-band like the executor would.
	var first *rivertype.JobInsertResult
	require.NoError(t, db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		first, err = client.InsertTx(ctx, tx, args, nil)
		return err
	}))
	require.False(t, first.UniqueSkippedAsDuplicate)

	// A live duplicate collapses.
	var dup *rivertype.JobInsertResult
	require.NoError(t, db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		dup, err = client.InsertTx(ctx, tx, jobs.ExtractArgs{
			OrganizationID: org, CorrelationID: uuid.New(), RfqID: rfqID,
		}, nil)
		return err
	}))
	assert.True(t, dup.UniqueSkippedAsDuplicate, "in-flight duplicate must dedupe")

	// Completed ⇒ re-enqueue lands (the reclassify action needs this).
	// The river_job CHECK requires finalized_at on finalized states.
	_, err = db.Pool().Exec(ctx,
		`UPDATE river.river_job SET state = 'completed', finalized_at = now() WHERE id = $1`, first.Job.ID)
	require.NoError(t, err)
	var second *rivertype.JobInsertResult
	require.NoError(t, db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		second, err = client.InsertTx(ctx, tx, args, nil)
		return err
	}))
	assert.False(t, second.UniqueSkippedAsDuplicate,
		"a terminal job must not block the re-enqueue")
	assert.NotEqual(t, first.Job.ID, second.Job.ID)
}
