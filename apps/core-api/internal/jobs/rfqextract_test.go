package jobs_test

// Real-Postgres integration tests for the S2 rfq_extract worker (spec S2):
// classify → extract → confidence gate. The ai-engine transport seam and
// object storage are fakes (named fakes per repo rule, never inline stubs);
// the tenant DB, RLS and the in-transaction RateArgs enqueue are real —
// they are what these tests exist to prove.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

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
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// fakeAIEngine is the named fake for the aiengine.Client seam: it answers
// classify/extract/parse from configured fixtures and records what the
// worker sent (the request must never carry organization_id — rule 2).
type fakeAIEngine struct {
	classify    *aiengine.ClassifyResult
	classifyErr error
	extract     json.RawMessage
	extractErr  error
	parses      map[string]string // filename -> markdown
	parseErr    error

	classifyCalls int
	extractCalls  int
	parseCalls    int
	lastClassify  aiengine.ClassifyRequest
	lastExtract   aiengine.ExtractRequest
	lastParse     aiengine.ParseAttachmentRequest
}

func (f *fakeAIEngine) Classify(_ context.Context, req aiengine.ClassifyRequest) (*aiengine.ClassifyResult, error) {
	f.classifyCalls++
	f.lastClassify = req
	if f.classifyErr != nil {
		return nil, f.classifyErr
	}
	return f.classify, nil
}

func (f *fakeAIEngine) Extract(_ context.Context, req aiengine.ExtractRequest) (json.RawMessage, error) {
	f.extractCalls++
	f.lastExtract = req
	if f.extractErr != nil {
		return nil, f.extractErr
	}
	return f.extract, nil
}

func (f *fakeAIEngine) ParseAttachment(_ context.Context, req aiengine.ParseAttachmentRequest) (string, error) {
	f.parseCalls++
	f.lastParse = req
	if f.parseErr != nil {
		return "", f.parseErr
	}
	if md, ok := f.parses[req.Filename]; ok {
		return md, nil
	}
	return "", errors.New("fakeAIEngine: no parse fixture for " + req.Filename)
}

// newExtractRig builds the worker on the real tenant runner plus an
// insert-only river client (same pattern as newIngestRig).
func newExtractRig(t *testing.T, db *tenantdb.DB, engine aiengine.Client, store *platform.FakeStorage) *jobs.RfqExtractWorker {
	t.Helper()
	worker := jobs.NewRfqExtractWorker(db, engine, store)
	client, err := river.NewClient(riverpgxv5.New(db.Pool()), &river.Config{
		Schema:            "river",
		ReindexerSchedule: river.NeverSchedule(),
	})
	require.NoError(t, err)
	worker.Inserter = client
	return worker
}

// extractJob wraps args in the job shape; attempt 0 leaves JobRow nil
// (simulates the first attempt).
func extractJob(org, corr, rfqID uuid.UUID, attempt int) *river.Job[jobs.ExtractArgs] {
	j := &river.Job[jobs.ExtractArgs]{Args: jobs.ExtractArgs{
		OrganizationID: org, CorrelationID: corr, RfqID: rfqID,
	}}
	if attempt > 0 {
		j.JobRow = &rivertype.JobRow{Attempt: attempt, MaxAttempts: 3}
	}
	return j
}

// seedRfq inserts one rfq_requests row inside the tenant tx — the same row
// S1 leaves behind (status PARSING by default in tests).
func seedRfq(t *testing.T, db *tenantdb.DB, org uuid.UUID, resendID, status, text, html string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO app.rfq_requests
				(organization_id, rfq_number, resend_message_id, customer_email, subject, status, raw_text_body, raw_html_body)
			VALUES ($1, (SELECT coalesce(max(rfq_number),0)+1 FROM app.rfq_requests WHERE organization_id=$1),
			        $2, 'customer@test.test', 'RFQ subject', $3, NULLIF($4,''), NULLIF($5,''))
			RETURNING id`, org, resendID, status, text, html).Scan(&id)
	}))
	return id
}

// seedAttachment inserts one rfq_attachments row; parsed=nil leaves
// parsed_markdown NULL (the case S2 must handle).
func seedAttachment(t *testing.T, db *tenantdb.DB, org, rfqID uuid.UUID, filename, storageKey string, parsed *string) {
	t.Helper()
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.rfq_attachments (organization_id, rfq_id, storage_key, filename, mime_type, size_bytes, parsed_markdown)
			VALUES ($1, $2, $3, $4, 'application/pdf', 100, $5)`,
			org, rfqID, storageKey, filename, parsed)
		return err
	}))
}

// extractRfqRow mirrors the columns S2 touches.
type extractRfqRow struct {
	Status        string
	Kind          string
	ReviewReason  *string
	FailureReason *string
	Payload       []byte
	Confidence    *float64
}

func loadExtractRfq(t *testing.T, db *tenantdb.DB, org, rfqID uuid.UUID) extractRfqRow {
	t.Helper()
	var r extractRfqRow
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT status::text, kind::text, review_reason, failure_reason, extracted_payload_json, extraction_confidence::float8
			FROM app.rfq_requests WHERE id = $1`, rfqID).
			Scan(&r.Status, &r.Kind, &r.ReviewReason, &r.FailureReason, &r.Payload, &r.Confidence)
	}))
	return r
}

// markdownStr is a test convenience for seeding parsed markdown.
func markdownStr(s string) *string { return &s }

// fieldEnvelope builds one valid FieldValue envelope.
func fieldEnvelope(confidence float64) map[string]any {
	return map[string]any{
		"value":      "BRSSZ",
		"evidence":   map[string]any{"source": "EMAIL_BODY", "exact_quote": "Santos"},
		"inferred":   false,
		"confidence": confidence,
		"reasons":    []string{},
	}
}

// validExtraction returns a contract-valid ExtractionResult; the single
// field carries the given auditor confidence (= the persisted minimum).
func validExtraction(t *testing.T, confidence float64) json.RawMessage {
	t.Helper()
	doc := map[string]any{
		"contract_version": "1.0.0",
		"classification": map[string]any{
			"label":         "RFQ",
			"probabilities": map[string]any{"RFQ": 0.9, "TARIFF_UPDATE": 0.05, "OTHER": 0.05},
			"model":         "typesafe/jev-1.14",
		},
		"fields": map[string]any{
			"origin_locode": fieldEnvelope(confidence),
		},
		"reference_dataset_version": "2026-11-10",
		"model_info": map[string]any{
			"extractor_model": "anthropic/claude-haiku-5.5",
			"auditor_model":   "perplexity/pplx-decider-v1.2",
		},
	}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	return raw
}

func classifyResult(label string) *aiengine.ClassifyResult {
	return &aiengine.ClassifyResult{
		Label:         label,
		Probabilities: map[string]float64{"RFQ": 0.9, "TARIFF_UPDATE": 0.05, "OTHER": 0.05},
		Model:         "typesafe/jev-1.14",
	}
}

// countRateJobs tallies enqueued rfq_fetch_rates jobs for one RFQ.
func countRateJobs(t *testing.T, db *tenantdb.DB, org, rfqID uuid.UUID) int {
	t.Helper()
	return countRows(t, db, org,
		`SELECT count(*) FROM river.river_job WHERE kind = 'rfq_fetch_rates' AND queue = 'price'
		 AND args ->> 'organization_id' = $1 AND args ->> 'rfq_id' = $2`, org.String(), rfqID.String())
}

// countAudit tallies tenant_audit_logs rows for an action.
func countAudit(t *testing.T, db *tenantdb.DB, org uuid.UUID, action string) int {
	t.Helper()
	return countRows(t, db, org,
		`SELECT count(*) FROM app.tenant_audit_logs WHERE action = $1`, action)
}

// TestRfqExtractHappyPath: RFQ → valid extraction above the gate → PRICING
// + payload + confidence + rfq_fetch_rates enqueued transactionally + audit.
func TestRfqExtractHappyPath(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "extract-happy")
	rfqID := seedRfq(t, db, org, "ext-happy", "PARSING", "please quote 2x40HQ Santos->Hamburg", "")

	engine := &fakeAIEngine{classify: classifyResult("RFQ"), extract: validExtraction(t, 0.9)}
	worker := newExtractRig(t, db, engine, platform.NewFakeStorage())
	require.NoError(t, worker.Work(context.Background(), extractJob(org, uuid.New(), rfqID, 0)))

	r := loadExtractRfq(t, db, org, rfqID)
	assert.Equal(t, "PRICING", r.Status)
	assert.Equal(t, "RFQ", r.Kind)
	assert.Nil(t, r.ReviewReason)
	require.NotNil(t, r.Payload, "validated payload must persist")
	require.NotNil(t, r.Confidence)
	assert.InDelta(t, 0.9, *r.Confidence, 0.001)
	assert.Equal(t, 1, countRateJobs(t, db, org, rfqID), "rfq_fetch_rates must be enqueued")
	assert.Equal(t, 1, countAudit(t, db, org, "rfq.extracted"))
	assert.Equal(t, 1, engine.classifyCalls)
	assert.Equal(t, 1, engine.extractCalls)
}

// TestRfqExtractOtherIgnored: OTHER → IGNORED, done — no extract call, no
// downstream job, classification audited.
func TestRfqExtractOtherIgnored(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "extract-other")
	rfqID := seedRfq(t, db, org, "ext-other", "PARSING", "unsubscribe me", "")

	engine := &fakeAIEngine{classify: classifyResult("OTHER")}
	worker := newExtractRig(t, db, engine, platform.NewFakeStorage())
	require.NoError(t, worker.Work(context.Background(), extractJob(org, uuid.New(), rfqID, 0)))

	r := loadExtractRfq(t, db, org, rfqID)
	assert.Equal(t, "IGNORED", r.Status)
	assert.Equal(t, "OTHER", r.Kind)
	assert.Nil(t, r.Payload)
	assert.Equal(t, 0, countRateJobs(t, db, org, rfqID))
	assert.Equal(t, 1, countAudit(t, db, org, "rfq.ignored"))
	assert.Equal(t, 0, engine.extractCalls, "OTHER never pays the extraction call")
}

// TestRfqExtractTariffUpdateParked — the spec invariant: a TARIFF_UPDATE
// parks NEEDS_REVIEW and MUST NOT reach pricing (a real RFQ misclassified
// as tariff would otherwise park invisibly / a tariff must never be priced).
func TestRfqExtractTariffUpdateParked(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "extract-tariff")
	rfqID := seedRfq(t, db, org, "ext-tariff", "PARSING", "new tariff effective next week", "")

	engine := &fakeAIEngine{classify: classifyResult("TARIFF_UPDATE")}
	worker := newExtractRig(t, db, engine, platform.NewFakeStorage())
	require.NoError(t, worker.Work(context.Background(), extractJob(org, uuid.New(), rfqID, 0)))

	r := loadExtractRfq(t, db, org, rfqID)
	assert.Equal(t, "NEEDS_REVIEW", r.Status)
	assert.Equal(t, "TARIFF_UPDATE", r.Kind)
	require.NotNil(t, r.ReviewReason)
	assert.Equal(t, string(jobs.ReviewReasonTariffUpdate), *r.ReviewReason)
	assert.Equal(t, 0, countRateJobs(t, db, org, rfqID),
		"invariant: nothing parked via the tariff path may reach pricing")
	assert.Equal(t, 0, engine.extractCalls)
	assert.Equal(t, 1, countAudit(t, db, org, "rfq.parked"))
}

// TestRfqExtractBelowGate: valid extraction under the confidence gate parks
// LOW_CONFIDENCE — fields are retained (human queue is pre-filled).
func TestRfqExtractBelowGate(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "extract-low")
	rfqID := seedRfq(t, db, org, "ext-low", "PARSING", "vague request", "")

	engine := &fakeAIEngine{classify: classifyResult("RFQ"), extract: validExtraction(t, 0.5)}
	worker := newExtractRig(t, db, engine, platform.NewFakeStorage())
	require.NoError(t, worker.Work(context.Background(), extractJob(org, uuid.New(), rfqID, 0)))

	r := loadExtractRfq(t, db, org, rfqID)
	assert.Equal(t, "NEEDS_REVIEW", r.Status)
	require.NotNil(t, r.ReviewReason)
	assert.Equal(t, string(jobs.ReviewReasonLowConfidence), *r.ReviewReason)
	require.NotNil(t, r.Payload, "fields stay persisted for the review UI")
	require.NotNil(t, r.Confidence)
	assert.InDelta(t, 0.5, *r.Confidence, 0.001)
	assert.Equal(t, 0, countRateJobs(t, db, org, rfqID))
}

// TestRfqExtractNoUsableText: no bodies and nothing parsed ⇒ park
// NO_USABLE_TEXT without paying any LLM call.
func TestRfqExtractNoUsableText(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "extract-notext")
	rfqID := seedRfq(t, db, org, "ext-notext", "PARSING", "", "")

	engine := &fakeAIEngine{classify: classifyResult("RFQ"), extract: validExtraction(t, 0.9)}
	worker := newExtractRig(t, db, engine, platform.NewFakeStorage())
	require.NoError(t, worker.Work(context.Background(), extractJob(org, uuid.New(), rfqID, 0)))

	r := loadExtractRfq(t, db, org, rfqID)
	assert.Equal(t, "NEEDS_REVIEW", r.Status)
	require.NotNil(t, r.ReviewReason)
	assert.Equal(t, string(jobs.ReviewReasonNoUsableText), *r.ReviewReason)
	assert.Equal(t, 0, engine.classifyCalls, "no LLM call on nothing")
	assert.Equal(t, 0, countRateJobs(t, db, org, rfqID))
}

// TestRfqExtractAttachmentParseDegrades: one failing attachment stays NULL,
// is audited, and the pipeline still proceeds on the other inputs.
func TestRfqExtractAttachmentParseDegrades(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "extract-att")
	rfqID := seedRfq(t, db, org, "ext-att", "PARSING", "please quote", "")
	seedAttachment(t, db, org, rfqID, "ok.pdf", "tenants/x/ok.pdf", nil)
	seedAttachment(t, db, org, rfqID, "bad.pdf", "tenants/x/bad.pdf", nil)

	store := platform.NewFakeStorage()
	require.NoError(t, store.Put(context.Background(), "tenants/x/ok.pdf", []byte("PDF-OK")))
	require.NoError(t, store.Put(context.Background(), "tenants/x/bad.pdf", []byte("PDF-BAD")))

	engine := &fakeAIEngine{
		classify: classifyResult("RFQ"),
		extract:  validExtraction(t, 0.9),
		parses:   map[string]string{"ok.pdf": "# parsed table"},
	}
	worker := newExtractRig(t, db, engine, store)
	require.NoError(t, worker.Work(context.Background(), extractJob(org, uuid.New(), rfqID, 0)))

	r := loadExtractRfq(t, db, org, rfqID)
	assert.Equal(t, "PRICING", r.Status, "one bad attachment must not stop the pipeline")
	assert.Equal(t, 2, engine.parseCalls)
	assert.Equal(t, 1, countAudit(t, db, org, "rfq.attachment_parse_failed"))

	// ok.pdf persisted its markdown; bad.pdf stays NULL.
	var okMd, badMd *string
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT parsed_markdown FROM app.rfq_attachments WHERE filename = 'ok.pdf'`).Scan(&okMd); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT parsed_markdown FROM app.rfq_attachments WHERE filename = 'bad.pdf'`).Scan(&badMd)
	}))
	require.NotNil(t, okMd)
	assert.Equal(t, "# parsed table", *okMd)
	assert.Nil(t, badMd, "failed parse leaves the column NULL")

	// The extract call carries the successful parse only, under its name.
	require.Len(t, engine.lastExtract.ParsedMarkdowns, 1)
	assert.Equal(t, "ok.pdf", engine.lastExtract.ParsedMarkdowns[0].Filename)
	assert.Equal(t, "# parsed table", engine.lastExtract.ParsedMarkdowns[0].Markdown)
}

// TestRfqExtractAttachmentOnlyRfq: an attachment-only RFQ uses the parsed
// markdowns as its usable text and flows to pricing.
func TestRfqExtractAttachmentOnlyRfq(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "extract-attonly")
	rfqID := seedRfq(t, db, org, "ext-attonly", "PARSING", "", "")
	md := "# booking table\nSantos -> Hamburg 2x40HQ"
	seedAttachment(t, db, org, rfqID, "booking.pdf", "tenants/x/booking.pdf", markdownStr(md))

	engine := &fakeAIEngine{classify: classifyResult("RFQ"), extract: validExtraction(t, 0.9)}
	worker := newExtractRig(t, db, engine, platform.NewFakeStorage())
	require.NoError(t, worker.Work(context.Background(), extractJob(org, uuid.New(), rfqID, 0)))

	r := loadExtractRfq(t, db, org, rfqID)
	assert.Equal(t, "PRICING", r.Status)
	assert.Equal(t, 0, engine.parseCalls, "already-parsed rows are not re-parsed")
	require.Len(t, engine.lastExtract.ParsedMarkdowns, 1)
	assert.Equal(t, "booking.pdf", engine.lastExtract.ParsedMarkdowns[0].Filename,
		"the real filename rides so evidence can cite ATTACHMENT:<filename>")
	assert.Equal(t, md, engine.lastExtract.ParsedMarkdowns[0].Markdown)
	assert.Equal(t, "RFQ", engine.lastExtract.Classification.Label,
		"the classify result is sent for the engine to echo")
}

// TestRfqExtractHTMLOnlyBodyConverted: with no usable text part, both
// classify and extract receive the HTML reduced to text — raw markup
// never crosses the wire (the contract carries no body_html), and table
// cells stay separate tokens for extraction.
func TestRfqExtractHTMLOnlyBodyConverted(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "extract-html")
	rfqID := seedRfq(t, db, org, "ext-html", "PARSING", "",
		`<div>Please quote</div><table><tr><td>Santos</td><td>Hamburg</td></tr></table>`)

	engine := &fakeAIEngine{classify: classifyResult("RFQ"), extract: validExtraction(t, 0.9)}
	worker := newExtractRig(t, db, engine, platform.NewFakeStorage())
	require.NoError(t, worker.Work(context.Background(), extractJob(org, uuid.New(), rfqID, 0)))

	r := loadExtractRfq(t, db, org, rfqID)
	assert.Equal(t, "PRICING", r.Status)
	assert.Contains(t, engine.lastClassify.BodyText, "Please quote")
	assert.Contains(t, engine.lastClassify.BodyText, "Santos Hamburg",
		"table cells stay separate tokens")
	assert.NotContains(t, engine.lastClassify.BodyText, "<", "raw markup never reaches the engine")
	assert.Equal(t, engine.lastClassify.BodyText, engine.lastExtract.BodyText,
		"extract gets the same converted body (wider cap does not matter here)")
}

// TestRfqExtractWhitespaceTextFallsBackToHTML: a whitespace-only text part
// counts as "no text" — the HTML part supplies the body.
func TestRfqExtractWhitespaceTextFallsBackToHTML(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "extract-ws")
	rfqID := seedRfq(t, db, org, "ext-ws", "PARSING", "   ", "<p>real html body</p>")

	engine := &fakeAIEngine{classify: classifyResult("OTHER")}
	worker := newExtractRig(t, db, engine, platform.NewFakeStorage())
	require.NoError(t, worker.Work(context.Background(), extractJob(org, uuid.New(), rfqID, 0)))

	assert.Contains(t, engine.lastClassify.BodyText, "real html body")
}

// TestRfqExtractInvalidPayloadRetriesOnce: a contract-violating extraction
// is never written; attempt 1 returns the error, attempt 2 parks
// INVALID_EXTRACTION (at most one retry — each attempt costs money).
func TestRfqExtractInvalidPayloadRetriesOnce(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "extract-invalid")
	rfqID := seedRfq(t, db, org, "ext-invalid", "PARSING", "please quote", "")

	broken := json.RawMessage(`{"contract_version": "9.9.9", "classification": null}`)
	engine := &fakeAIEngine{classify: classifyResult("RFQ"), extract: broken}
	worker := newExtractRig(t, db, engine, platform.NewFakeStorage())

	// Attempt 1: the validation error propagates so River retries once.
	err := worker.Work(context.Background(), extractJob(org, uuid.New(), rfqID, 1))
	require.Error(t, err, "first invalid payload must return an error for the single retry")
	r := loadExtractRfq(t, db, org, rfqID)
	assert.Equal(t, "PARSING", r.Status, "still owned by the retry")
	assert.Nil(t, r.Payload, "invalid payload is never written")

	// Attempt 2: parks INVALID_EXTRACTION, job completes.
	require.NoError(t, worker.Work(context.Background(), extractJob(org, uuid.New(), rfqID, 2)))
	r = loadExtractRfq(t, db, org, rfqID)
	assert.Equal(t, "NEEDS_REVIEW", r.Status)
	require.NotNil(t, r.ReviewReason)
	assert.Equal(t, string(jobs.ReviewReasonInvalidExtraction), *r.ReviewReason)
	assert.Nil(t, r.Payload, "invalid payload is never written even on park")
	assert.Equal(t, 0, countRateJobs(t, db, org, rfqID))
	assert.Equal(t, 1, countAudit(t, db, org, "rfq.parked"))
}
