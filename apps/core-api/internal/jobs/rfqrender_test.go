package jobs_test

// Real-Postgres integration tests for the S4 rfq_render worker (spec S4):
// lock → assemble → engine render → storage artifacts → pdf_storage_key.
// The ai-engine seam and object storage are named fakes; the tenant DB,
// RLS and the audit trail are real — they are what these tests prove.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/aiengine"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/jobs"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// renderFixture is what seedRender planted: one READY rfq carrying a
// valid extraction, one quote version (DRAFT unless steered) and its line
// items.
type renderFixture struct {
	org     uuid.UUID
	slug    string
	rfq     uuid.UUID
	quote   uuid.UUID
	number  string
	version int
	pdfKey  string // expected tenants/... key after a successful render
	htmlKey string
}

// renderOpts steers the columns the stage's no-op checks read.
type renderOpts struct {
	quoteStatus string // defaults DRAFT
	pdfKey      string // pre-set pdf_storage_key; empty leaves NULL
	payload     []byte // extracted_payload_json; defaults to a valid FCL doc
	branding    []byte // organizations.branding_json; defaults {}
	lines       int    // line-item rows to plant; defaults 2
	noLines     bool   // plant zero line items (the drift fixture)
	pricingJSON []byte // pricing_result_json; defaults to a COMPLETE output
}

// validExtractionDoc is the minimal extraction payload the shipment
// assembly needs — CNSHA→BRSSZ derives IMPORT.
func validExtractionDoc(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"fields": map[string]any{
		"modal":             map[string]any{"value": "FCL"},
		"incoterm":          map[string]any{"value": "FOB"},
		"origin_locode":     map[string]any{"value": "CNSHA"},
		"dest_locode":       map[string]any{"value": "BRSSZ"},
		"equipment_or_unit": map[string]any{"value": "40HC"},
		"quantity":          map[string]any{"value": "2"},
	}})
	require.NoError(t, err)
	return raw
}

func seedRender(t *testing.T, db *tenantdb.DB, o renderOpts) renderFixture {
	t.Helper()
	f := renderFixture{org: uuid.New(), version: 1, number: "Q-" + uuid.NewString()[:8]}
	f.slug = testenv.SeedOrganization(t, f.org, "render-"+uuid.NewString()[:6])
	if o.quoteStatus == "" {
		o.quoteStatus = "DRAFT"
	}
	if o.payload == nil {
		o.payload = validExtractionDoc(t)
	}
	if o.pricingJSON == nil {
		var err error
		o.pricingJSON, err = json.Marshal(pricing.Output{
			Status:      pricing.StatusComplete,
			Warnings:    []string{"CARRIER_OFFER_EXPIRED_FALLBACK"},
			Assumptions: []pricing.Assumption{pricing.AssumptionTaxesExcluded},
		})
		require.NoError(t, err)
	}
	if o.lines == 0 && !o.noLines {
		o.lines = 2
	}
	if o.branding != nil {
		// Layer A write: the org row is seeded (and patched) as
		// qp_migrator — qp_app holds no grant on it by design.
		pool := testenv.MigratorPool(t)
		_, err := pool.Exec(context.Background(),
			`UPDATE app.organizations SET branding_json = $2 WHERE id = $1`,
			f.org.String(), o.branding)
		require.NoError(t, err)
	}
	require.NoError(t, db.InTenantTx(context.Background(), f.org, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO app.rfq_requests
				(organization_id, rfq_number, resend_message_id, customer_email, status, kind,
				 extracted_payload_json, extraction_confidence)
			VALUES ($1, (SELECT coalesce(max(rfq_number),0)+1 FROM app.rfq_requests WHERE organization_id=$1),
			        $2, 'buyer@customer.test', 'READY', 'RFQ', $3, 0.9)
			RETURNING id`,
			f.org, "resend-"+uuid.NewString(), o.payload).Scan(&f.rfq); err != nil {
			return err
		}
		var pdfArg *string
		if o.pdfKey != "" {
			pdfArg = &o.pdfKey
		}
		// The quote and its lines always land DRAFT first — the rule-9
		// triggers refuse line INSERTs on a non-DRAFT parent; a status
		// UPDATE afterwards is the only way to stage a retired version.
		if err := tx.QueryRow(ctx, `
			INSERT INTO app.quotes
				(organization_id, rfq_id, quote_number, version, status, quote_date,
				 total_cost_brl, target_margin, final_price_brl, overall_confidence,
				 pricing_result_json, pdf_storage_key)
			VALUES ($1, $2, $3, $4, 'DRAFT', $5, 100.00, 0.1500, 115.00,
			        0.9000, $6, $7)
			RETURNING id`,
			f.org, f.rfq, f.number, f.version,
			time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC), o.pricingJSON, pdfArg).Scan(&f.quote); err != nil {
			return err
		}
		for i := 0; i < o.lines; i++ {
			var flags []byte
			if i == 0 {
				flags = []byte(`["MIN_MARKUP_APPLIED"]`)
			} else {
				flags = []byte(`[]`)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO app.quote_line_items
					(organization_id, quote_id, sort_order, category, description, source_type,
					 original_currency, original_amount, fx_rate_used,
					 cost_amount_brl, sell_amount_brl, line_confidence, flags)
				VALUES ($1, $2, $3, $4::app.quote_line_category, $5, 'PRIVATE_TARIFF',
				        'USD', 900.0000, 5.450000, 4905.00, 5630.00, 0.9000, $6)`,
				f.org, f.quote, i, renderLineCategory(i), "Ocean freight "+f.number, flags); err != nil {
				return err
			}
		}
		if o.quoteStatus != "DRAFT" {
			if _, err := tx.Exec(ctx,
				`UPDATE app.quotes SET status = $2::app.quote_status WHERE id = $1`,
				f.quote, o.quoteStatus); err != nil {
				return err
			}
		}
		return nil
	}))
	f.pdfKey = "tenants/" + f.org.String() + "/quotes/" + f.quote.String() + "_v1.pdf"
	f.htmlKey = "tenants/" + f.org.String() + "/quotes/" + f.quote.String() + "_v1.html"
	return f
}

func renderLineCategory(i int) string {
	if i == 0 {
		return "INTL_FREIGHT"
	}
	return "SURCHARGE"
}

// renderJob wraps args in the job shape Work consumes; attempt 0 leaves
// JobRow nil (simulates a non-final attempt).
func renderJob(org, rfqID, quoteID uuid.UUID, attempt, maxAttempts int) *river.Job[jobs.RenderArgs] {
	j := &river.Job[jobs.RenderArgs]{Args: jobs.RenderArgs{
		OrganizationID: org, CorrelationID: uuid.New(), RfqID: rfqID, QuoteID: quoteID,
	}}
	if attempt > 0 {
		j.JobRow = &rivertype.JobRow{Attempt: attempt, MaxAttempts: maxAttempts}
	}
	return j
}

// renderPdf is a minimal valid payload — %PDF magic is what the contract
// bounds, not a full document.
func renderResult(html string) *aiengine.RenderResult {
	return &aiengine.RenderResult{PDF: []byte("%PDF-1.7 fake quote bytes"), EmailHTML: html}
}

// renderQuoteState reads back the columns S4 writes.
func renderQuoteKey(t *testing.T, db *tenantdb.DB, f renderFixture) *string {
	t.Helper()
	var key *string
	require.NoError(t, db.InTenantTx(context.Background(), f.org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT pdf_storage_key FROM app.quotes WHERE id = $1`, f.quote).Scan(&key)
	}))
	return key
}

// TestRfqRenderHappyPath is the spec-S4 sequence: render → artifacts
// under the tenant quotes prefix → pdf_storage_key → quote.rendered — all
// inside one tenant tx.
func TestRfqRenderHappyPath(t *testing.T) {
	db := testenv.Open(t)
	f := seedRender(t, db, renderOpts{branding: []byte(`{"primary_color":"#0047FF","logo_key":"brand/logo.png"}`)})
	engine := &fakeAIEngine{render: renderResult("<p>Your quote</p>")}
	storage := platform.NewFakeStorage()
	worker := jobs.NewRfqRenderWorker(db, engine, storage)

	require.NoError(t, worker.Work(context.Background(), renderJob(f.org, f.rfq, f.quote, 0, 0)))

	require.Equal(t, 1, engine.renderCalls)
	req := engine.lastRender
	// rule 2: no organization_id field exists on the request — the check
	// marshals the wire shape and asserts the key is absent, so a future
	// field addition fails loudly instead of leaking the tenant id.
	raw, err := json.Marshal(req)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(raw, &wire))
	_, hasOrg := wire["organization_id"]
	assert.False(t, hasOrg, "organization_id must never reach the render wire")
	assert.Equal(t, f.number, req.Quote.QuoteNumber)
	assert.Equal(t, 1, req.Quote.Version)
	assert.Equal(t, "2026-03-15", req.Quote.QuoteDate)
	assert.Equal(t, "FCL", req.Quote.Modal)
	assert.Equal(t, "IMPORT", req.Quote.Direction, "CNSHA→BRSSZ derives IMPORT")
	assert.Equal(t, "FOB", req.Quote.Incoterm)
	assert.Equal(t, "CNSHA", req.Quote.OriginLOCODE)
	assert.Equal(t, "BRSSZ", req.Quote.DestLOCODE)
	assert.Equal(t, "40HC", req.Quote.EquipmentOrUnit)
	require.Len(t, req.Quote.Lines, 2)
	assert.Equal(t, "INTL_FREIGHT", req.Quote.Lines[0].Category)
	assert.Equal(t, []string{"MIN_MARKUP_APPLIED"}, req.Quote.Lines[0].Flags)
	assert.Equal(t, "SURCHARGE", req.Quote.Lines[1].Category)
	assert.Equal(t, "115", req.Quote.TotalBRL.String(), "final_price_brl becomes total_brl")
	assert.Equal(t, []string{"CARRIER_OFFER_EXPIRED_FALLBACK"}, req.Quote.Warnings)
	assert.Equal(t, []string{"TAXES_EXCLUDED"}, req.Quote.Assumptions)
	// Branding rides verbatim — org content, not an identifier.
	assert.JSONEq(t, `{"primary_color":"#0047FF","logo_key":"brand/logo.png"}`, string(req.Branding))
	assert.Equal(t, "Test "+f.slug, req.Org.Name)
	assert.Equal(t, "Test "+f.slug, req.Org.FromName)
	assert.Equal(t, "ops@"+f.slug+".test", req.Org.ReplyTo)

	key := renderQuoteKey(t, db, f)
	require.NotNil(t, key)
	assert.Equal(t, f.pdfKey, *key)
	pdf, err := storage.Get(context.Background(), f.pdfKey)
	require.NoError(t, err)
	assert.Equal(t, []byte("%PDF-1.7 fake quote bytes"), pdf)
	html, err := storage.Get(context.Background(), f.htmlKey)
	require.NoError(t, err, "a non-empty email_html is stored beside the pdf")
	assert.Equal(t, "<p>Your quote</p>", string(html))

	n, meta := auditCount(t, db, f.org, "quote.rendered")
	assert.Equal(t, 1, n)
	assert.Equal(t, f.pdfKey, meta["pdf_storage_key"])
	assert.Equal(t, f.htmlKey, meta["html_storage_key"])
	assert.Equal(t, f.number, meta["quote_number"])
}

// TestRfqRenderReplayNoSecondRender: a second worker run on a rendered
// quote no-ops under the FOR UPDATE read — the engine is never called
// again (idempotent on pdf_storage_key, spec S4).
func TestRfqRenderReplayNoSecondRender(t *testing.T) {
	db := testenv.Open(t)
	f := seedRender(t, db, renderOpts{})
	engine := &fakeAIEngine{render: renderResult("")}
	worker := jobs.NewRfqRenderWorker(db, engine, platform.NewFakeStorage())

	require.NoError(t, worker.Work(context.Background(), renderJob(f.org, f.rfq, f.quote, 0, 0)))
	require.NoError(t, worker.Work(context.Background(), renderJob(f.org, f.rfq, f.quote, 0, 0)))
	assert.Equal(t, 1, engine.renderCalls, "pdf_storage_key already set collapses the replay")

	n, _ := auditCount(t, db, f.org, "quote.rendered")
	assert.Equal(t, 1, n)
}

// TestRfqRenderPreRenderedNoOp: a quote that arrives with pdf_storage_key
// already set — a retry after the commit — never re-renders.
func TestRfqRenderPreRenderedNoOp(t *testing.T) {
	db := testenv.Open(t)
	f := seedRender(t, db, renderOpts{pdfKey: "tenants/x/quotes/done.pdf"})
	engine := &fakeAIEngine{render: renderResult("")}
	worker := jobs.NewRfqRenderWorker(db, engine, platform.NewFakeStorage())

	require.NoError(t, worker.Work(context.Background(), renderJob(f.org, f.rfq, f.quote, 0, 0)))
	assert.Equal(t, 0, engine.renderCalls)
}

// TestRfqRenderNonDraftNoOp: superseded and approved versions are no-ops
// — S4 renders the DRAFT only; a retired version's artifact is history.
func TestRfqRenderNonDraftNoOp(t *testing.T) {
	db := testenv.Open(t)
	for _, status := range []string{"SUPERSEDED", "APPROVED_SENT"} {
		f := seedRender(t, db, renderOpts{quoteStatus: status})
		engine := &fakeAIEngine{render: renderResult("")}
		worker := jobs.NewRfqRenderWorker(db, engine, platform.NewFakeStorage())
		require.NoError(t, worker.Work(context.Background(), renderJob(f.org, f.rfq, f.quote, 0, 0)),
			"status %s must be an idempotent no-op", status)
		assert.Equal(t, 0, engine.renderCalls, "status %s", status)
	}
}

// TestRfqRenderMissingQuoteNoOp: a job for a quote that does not exist is
// envelope drift — nothing to audit, an idempotent no-op.
func TestRfqRenderMissingQuoteNoOp(t *testing.T) {
	db := testenv.Open(t)
	f := seedRender(t, db, renderOpts{})
	engine := &fakeAIEngine{render: renderResult("")}
	worker := jobs.NewRfqRenderWorker(db, engine, platform.NewFakeStorage())

	require.NoError(t, worker.Work(context.Background(),
		renderJob(f.org, f.rfq, uuid.New(), 0, 0)))
	assert.Equal(t, 0, engine.renderCalls)
}

// TestRfqRenderCrossTenantNoOp: the job's organization_id scopes every
// read — org B's render job over org A's quote id sees nothing (RLS) and
// produces nothing: no call, no artifact, no audit row in either tenant.
func TestRfqRenderCrossTenantNoOp(t *testing.T) {
	db := testenv.Open(t)
	f := seedRender(t, db, renderOpts{})
	other := uuid.New()
	testenv.SeedOrganization(t, other, "render-other")
	engine := &fakeAIEngine{render: renderResult("")}
	storage := platform.NewFakeStorage()
	worker := jobs.NewRfqRenderWorker(db, engine, storage)

	require.NoError(t, worker.Work(context.Background(), renderJob(other, f.rfq, f.quote, 0, 0)))
	assert.Equal(t, 0, engine.renderCalls)
	assert.Empty(t, storage.Keys(), "no artifact is written under a foreign prefix")
	n, _ := auditCount(t, db, other, "quote.render_failed")
	assert.Equal(t, 0, n, "a cross-tenant miss audits nothing")
	assert.Nil(t, renderQuoteKey(t, db, f), "the real quote is untouched")
}

// TestRfqRenderContractViolationCancels: a non-transient engine fault —
// deterministic status or invalid payload — audits quote.render_failed
// and cancels instead of burning attempts (spec §3 taxonomy).
func TestRfqRenderContractViolationCancels(t *testing.T) {
	db := testenv.Open(t)
	f := seedRender(t, db, renderOpts{})
	engine := &fakeAIEngine{renderErr: &aiengine.EngineError{
		Op: "render_quote", StatusCode: 422, Detail: "unknown template"}}
	worker := jobs.NewRfqRenderWorker(db, engine, platform.NewFakeStorage())

	err := worker.Work(context.Background(), renderJob(f.org, f.rfq, f.quote, 0, 0))
	assertJobCancel(t, err)
	assert.Equal(t, 1, engine.renderCalls)
	assert.Nil(t, renderQuoteKey(t, db, f))

	n, meta := auditCount(t, db, f.org, "quote.render_failed")
	assert.Equal(t, 1, n)
	assert.Equal(t, "contract_violation", meta["error_class"])
}

// TestRfqRenderTransientRetries: a transient engine error propagates for
// River's retry — the tx rolls back, so no key and no audit survive.
func TestRfqRenderTransientRetries(t *testing.T) {
	db := testenv.Open(t)
	f := seedRender(t, db, renderOpts{})
	engine := &fakeAIEngine{renderErr: &aiengine.EngineError{
		Op: "render_quote", StatusCode: 503, Detail: "upstream down"}}
	worker := jobs.NewRfqRenderWorker(db, engine, platform.NewFakeStorage())

	err := worker.Work(context.Background(), renderJob(f.org, f.rfq, f.quote, 0, 0))
	require.Error(t, err)
	var jce *rivertype.JobCancelError
	assert.False(t, errors.As(err, &jce), "transient must propagate for retry, not cancel")
	assert.Nil(t, renderQuoteKey(t, db, f))
	n, _ := auditCount(t, db, f.org, "quote.render_failed")
	assert.Equal(t, 0, n)
}

// TestRfqRenderFinalAttemptAuditsNil: the last attempt's transient
// failure must not discard the job with zero trace — quotes has no FAILED
// column, so quote.render_failed in a fresh tenant tx is the honest dead
// letter, and the job completes (recoverable via reprocess).
func TestRfqRenderFinalAttemptAuditsNil(t *testing.T) {
	db := testenv.Open(t)
	f := seedRender(t, db, renderOpts{})
	engine := &fakeAIEngine{renderErr: &aiengine.EngineError{
		Op: "render_quote", StatusCode: 503, Detail: "upstream down"}}
	worker := jobs.NewRfqRenderWorker(db, engine, platform.NewFakeStorage())

	require.NoError(t, worker.Work(context.Background(), renderJob(f.org, f.rfq, f.quote, 3, 3)),
		"the exhausted transient completes the job instead of discarding it silently")
	assert.Nil(t, renderQuoteKey(t, db, f))

	n, meta := auditCount(t, db, f.org, "quote.render_failed")
	assert.Equal(t, 1, n)
	assert.Equal(t, "transient_exhausted", meta["error_class"])
	assert.EqualValues(t, 3, meta["attempts"])
	assert.Contains(t, meta["error"], "exhausted 3 attempts")
}

// TestRfqRenderStoredDriftCancels: a DRAFT quote whose stored extraction
// is undecodable can only be data drift (S3b required it to reach
// COMPLETE) — audited stored_drift, cancelled, never retried.
func TestRfqRenderStoredDriftCancels(t *testing.T) {
	db := testenv.Open(t)
	f := seedRender(t, db, renderOpts{payload: []byte(`{"fields": null}`)})
	engine := &fakeAIEngine{render: renderResult("")}
	worker := jobs.NewRfqRenderWorker(db, engine, platform.NewFakeStorage())

	err := worker.Work(context.Background(), renderJob(f.org, f.rfq, f.quote, 0, 0))
	assertJobCancel(t, err)
	assert.Equal(t, 0, engine.renderCalls, "drift is detected before the paid call")
	n, meta := auditCount(t, db, f.org, "quote.render_failed")
	assert.Equal(t, 1, n)
	assert.Equal(t, "stored_drift", meta["error_class"])
}

// TestRfqRenderNoLineItemsCancels: a DRAFT quote with no line rows is
// stored drift — S3b commits lines with the quote — so it cancels as
// stored_drift before the paid call.
func TestRfqRenderNoLineItemsCancels(t *testing.T) {
	db := testenv.Open(t)
	f := seedRender(t, db, renderOpts{noLines: true})
	engine := &fakeAIEngine{render: renderResult("")}
	worker := jobs.NewRfqRenderWorker(db, engine, platform.NewFakeStorage())

	err := worker.Work(context.Background(), renderJob(f.org, f.rfq, f.quote, 0, 0))
	assertJobCancel(t, err)
	assert.Equal(t, 0, engine.renderCalls)
	n, meta := auditCount(t, db, f.org, "quote.render_failed")
	assert.Equal(t, 1, n)
	assert.Equal(t, "stored_drift", meta["error_class"])
}

// TestRfqRenderEmptyResultCancels: a render that returns no PDF bytes —
// already past the adapter's contract check, so this is the worker's
// defense against a lying seam — cancels as contract_violation.
func TestRfqRenderEmptyResultCancels(t *testing.T) {
	db := testenv.Open(t)
	f := seedRender(t, db, renderOpts{})
	engine := &fakeAIEngine{render: &aiengine.RenderResult{}}
	worker := jobs.NewRfqRenderWorker(db, engine, platform.NewFakeStorage())

	err := worker.Work(context.Background(), renderJob(f.org, f.rfq, f.quote, 0, 0))
	assertJobCancel(t, err)
	assert.Nil(t, renderQuoteKey(t, db, f))
}

// TestRfqRenderNoEmailHTML: email_html is optional — an engine that
// returns none still stores the PDF and pins the key; no .html artifact
// is written (S5 owns the send body for now).
func TestRfqRenderNoEmailHTML(t *testing.T) {
	db := testenv.Open(t)
	f := seedRender(t, db, renderOpts{})
	engine := &fakeAIEngine{render: renderResult("")}
	storage := platform.NewFakeStorage()
	worker := jobs.NewRfqRenderWorker(db, engine, storage)

	require.NoError(t, worker.Work(context.Background(), renderJob(f.org, f.rfq, f.quote, 0, 0)))

	key := renderQuoteKey(t, db, f)
	require.NotNil(t, key)
	assert.Equal(t, []string{f.pdfKey}, storage.Keys())
	_, err := storage.Get(context.Background(), f.htmlKey)
	assert.ErrorIs(t, err, platform.ErrObjectNotFound)
}

// TestRenderArgsInsertOpts pins the spec-§3 row: the render queue plus a
// three-attempt budget with entity uniqueness on QuoteID.
func TestRenderArgsInsertOpts(t *testing.T) {
	opts := jobs.RenderArgs{}.InsertOpts()
	assert.Equal(t, jobs.QueueRender, opts.Queue)
	assert.Equal(t, 3, opts.MaxAttempts)
	assert.True(t, opts.UniqueOpts.ByArgs)
}
