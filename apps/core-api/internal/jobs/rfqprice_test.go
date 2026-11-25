package jobs_test

// Real-Postgres integration tests for the S3b rfq_price worker (spec S3b):
// every tenant read/write happens inside InTenantTx, so only the qp_app
// pool and the real role model can prove it — no mocked database.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
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
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// pricedNow pins the worker clock so QuoteDate/PricedAt and the quote
// number's year are asserted, never guessed.
var pricedNow = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

// priceRig bundles the worker with the insert-only River client the
// ingest tests use: InsertTx works without Start, so the rfq_render
// enqueue still lands in river_job inside the tenant tx.
type priceRig struct {
	worker *jobs.RfqPriceWorker
}

func newPriceRig(t *testing.T, db *tenantdb.DB) *priceRig {
	t.Helper()
	worker := jobs.NewRfqPriceWorker(db, func() time.Time { return pricedNow })
	client, err := river.NewClient(riverpgxv5.New(db.Pool()), &river.Config{
		Schema:            "river",
		ReindexerSchedule: river.NeverSchedule(),
	})
	require.NoError(t, err)
	worker.Inserter = client
	return &priceRig{worker: worker}
}

func priceJob(org, corr, rfq uuid.UUID) *river.Job[jobs.PriceArgs] {
	return &river.Job[jobs.PriceArgs]{Args: jobs.PriceArgs{
		OrganizationID: org, CorrelationID: corr, RfqID: rfq,
	}}
}

// ---------------------------------------------------------------------------
// Seeds — all writes as qp_app inside the tenant tx.
// ---------------------------------------------------------------------------

// extractionDoc renders the FieldValue envelope buildShipment reads: the
// worker only consumes {"fields": {k: {"value": v}}}; evidence and
// metadata are contract noise it must ignore.
func extractionDoc(fields map[string]string) string {
	doc := `{"fields":{`
	i := 0
	for k, v := range fields {
		if i > 0 {
			doc += ","
		}
		doc += fmt.Sprintf("%q:{\"value\":%q}", k, v)
		i++
	}
	return doc + "}}"
}

// importFields is a complete DAP FCL import extraction — direction
// derives IMPORT (BR destination), and DAP prices the customs group
// only, so one CUSTOMS tariff row completes it.
func importFields() map[string]string {
	return map[string]string{
		"incoterm":          "DAP",
		"modal":             "FCL",
		"origin_locode":     "CNSHA",
		"dest_locode":       "BRSSZ",
		"equipment_or_unit": "40HC",
		"quantity":          "2",
		"ready_date":        "2026-03-15",
	}
}

// seedPricedRFQ inserts one rfq_requests row in PRICING — the S2/S3a
// checkpoint state — carrying payload and confidence; an empty
// confidence inserts NULL (the drift case the cancel test needs).
func seedPricedRFQ(t *testing.T, db *tenantdb.DB, org uuid.UUID, payload string, confidence string) uuid.UUID {
	t.Helper()
	var conf any
	if confidence != "" {
		conf = confidence
	}
	var id uuid.UUID
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO app.rfq_requests
				(organization_id, rfq_number, resend_message_id, customer_email, status,
				 extracted_payload_json, extraction_confidence)
			VALUES ($1, (SELECT coalesce(max(rfq_number),0)+1 FROM app.rfq_requests WHERE organization_id=$1),
			        $2, 'cust@example.test', 'PRICING', $3::jsonb, $4)
			RETURNING id`,
			org, "msg-"+uuid.NewString(), payload, conf).Scan(&id)
	}))
	return id
}

// seedPriceRule inserts a pricing_rules row; modal is the pricing_modal
// enum, empty locodes/routeMargin map to NULL (the NULLS NOT DISTINCT
// uniqueness makes the modal/global slot real).
func seedPriceRule(t *testing.T, db *tenantdb.DB, org uuid.UUID, modal, origin, dest, margin, routeMargin string) {
	t.Helper()
	nilEmpty := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.pricing_rules
				(organization_id, modal, default_target_margin, fx_safety_spread,
				 origin_locode, dest_locode, route_target_margin)
			VALUES ($1, $2::app.pricing_modal, $3, 0, $4, $5, $6)`,
			org, modal, margin, nilEmpty(origin), nilEmpty(dest), nilEmpty(routeMargin))
		return err
	}))
}

// tariffSeed is one private_tariffs row behind an ACTIVE batch (the
// engine never sees unactivated candidates).
type tariffSeed struct {
	Modal      string
	Origin     string
	Dest       string
	Equipment  string
	Category   string
	Code       string
	Currency   string
	Amount     string
	ValidFrom  string
	ValidUntil string
}

func seedTariff(t *testing.T, db *tenantdb.DB, org uuid.UUID, s tariffSeed) uuid.UUID {
	t.Helper()
	var batch, id uuid.UUID
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO app.private_tariff_batches (organization_id, source_type, status, activated_at)
			 VALUES ($1, 'CSV_UPLOAD', 'ACTIVE', '2026-01-01T00:00:00Z') RETURNING id`, org).Scan(&batch); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO app.private_tariffs
				(organization_id, batch_id, carrier_or_agent, modal, origin_locode, dest_locode,
				 equipment_or_unit, charge_category, charge_code, currency, net_amount,
				 valid_from, valid_until, search_description)
			VALUES ($1,$2,'CARRIER_A',$3::app.freight_modal,$4,$5,$6,$7::app.charge_category,
			        $8,$9,$10,$11,$12,$13) RETURNING id`,
			org, batch, s.Modal, s.Origin, s.Dest, s.Equipment, s.Category,
			s.Code, s.Currency, s.Amount, s.ValidFrom, s.ValidUntil, "seed tariff").Scan(&id)
	}))
	return id
}

// customsTariff matches the importFields route+equipment.
func customsTariff(validUntil string) tariffSeed {
	return tariffSeed{
		Modal: "FCL", Origin: "CNSHA", Dest: "BRSSZ", Equipment: "40HC",
		Category: "CUSTOMS", Code: "CUSTOMS_FEE", Currency: "BRL", Amount: "350.00",
		ValidFrom: "2026-01-01", ValidUntil: validUntil,
	}
}

// seedOffer inserts one OFFER_FOUND checkpoint row the worker pins.
func seedOffer(t *testing.T, db *tenantdb.DB, org, rfq uuid.UUID, payload string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO app.carrier_offers
				(organization_id, rfq_id, carrier, outcome, request_hash, offer_payload_json, fetched_at)
			VALUES ($1,$2,'HAPAG','OFFER_FOUND','req-hash',$3::jsonb, now())
			RETURNING id`, org, rfq, payload).Scan(&id)
	}))
	return id
}

// setRFQStatus rewinds an RFQ to a stage — what reprocess does.
func setRFQStatus(t *testing.T, db *tenantdb.DB, org, rfq uuid.UUID, status string) {
	t.Helper()
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE app.rfq_requests SET status = $2::app.rfq_status WHERE id = $1`,
			rfq, status)
		return err
	}))
}

// setRFQReceivedAt pins received_at — the request-hash ready_date
// fallback's source — so day-drift tests control which day "the RFQ
// arrived" independent of the test's wall clock.
func setRFQReceivedAt(t *testing.T, db *tenantdb.DB, org, rfq uuid.UUID, at time.Time) {
	t.Helper()
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE app.rfq_requests SET received_at = $2 WHERE id = $1`, rfq, at)
		return err
	}))
}

// rfqState reads the row's status and review_reason inside the tenant tx.
func rfqState(t *testing.T, db *tenantdb.DB, org, rfq uuid.UUID) (status, review string) {
	t.Helper()
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT status::text, coalesce(review_reason,'') FROM app.rfq_requests WHERE id = $1`,
			rfq).Scan(&status, &review)
	}))
	return status, review
}

// quoteRow mirrors the quote columns the worker asserts on.
type quoteRow struct {
	ID            uuid.UUID
	Number        string
	Version       int
	Status        string
	QuoteDate     time.Time
	PricedAt      time.Time
	InputsHash    string
	CarrierOffer  *uuid.UUID
	CargoCIF      *string
	TotalCost     string
	TargetMargin  string
	FinalPrice    string
	Confidence    string
	PricingResult []byte
}

// loadQuotes returns the org's quotes for one RFQ ordered by version.
func loadQuotes(t *testing.T, db *tenantdb.DB, org, rfq uuid.UUID) []quoteRow {
	t.Helper()
	var out []quoteRow
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, quote_number, version, status::text, quote_date, priced_at,
			       coalesce(inputs_hash,''), carrier_offer_id, cargo_cif_brl::text,
			       total_cost_brl::text, target_margin::text, final_price_brl::text,
			       overall_confidence::text, coalesce(pricing_result_json::text,'')
			FROM app.quotes WHERE rfq_id = $1 ORDER BY version`, rfq)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var q quoteRow
			var payload string
			if err := rows.Scan(&q.ID, &q.Number, &q.Version, &q.Status, &q.QuoteDate,
				&q.PricedAt, &q.InputsHash, &q.CarrierOffer, &q.CargoCIF,
				&q.TotalCost, &q.TargetMargin, &q.FinalPrice, &q.Confidence, &payload); err != nil {
				return err
			}
			q.PricingResult = []byte(payload)
			out = append(out, q)
		}
		return rows.Err()
	}))
	return out
}

// lineRow mirrors the quote_line_items columns the worker asserts on.
type lineRow struct {
	SortOrder  int
	Category   string
	SourceType string
	TariffID   *uuid.UUID
	Currency   string
	Amount     string
	FXRate     string
	CostBRL    string
	SellBRL    string
	Confidence string
}

func loadLines(t *testing.T, db *tenantdb.DB, org, quote uuid.UUID) []lineRow {
	t.Helper()
	var out []lineRow
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT sort_order, category::text, source_type::text, private_tariff_id,
			       original_currency, original_amount::text, fx_rate_used::text,
			       cost_amount_brl::text, sell_amount_brl::text, line_confidence::text
			FROM app.quote_line_items WHERE quote_id = $1 ORDER BY sort_order`, quote)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l lineRow
			if err := rows.Scan(&l.SortOrder, &l.Category, &l.SourceType, &l.TariffID,
				&l.Currency, &l.Amount, &l.FXRate, &l.CostBRL, &l.SellBRL, &l.Confidence); err != nil {
				return err
			}
			out = append(out, l)
		}
		return rows.Err()
	}))
	return out
}

// auditCountFor counts tenant_audit_logs rows for one action on one entity.
func auditCountFor(t *testing.T, db *tenantdb.DB, org, rfq uuid.UUID, action string) int {
	t.Helper()
	return countRows(t, db, org,
		`SELECT count(*) FROM app.tenant_audit_logs WHERE action = $1 AND entity_id = $2`,
		action, rfq.String())
}

var quoteNumberPattern = regexp.MustCompile(`^[A-Z0-9-]+-\d{4}-\d{4}$`)

// ---------------------------------------------------------------------------
// The stage's contract tests
// ---------------------------------------------------------------------------

// TestRfqPriceHappyPath: PRICING + extraction + rule + customs tariff →
// DRAFT v1, its lines, RFQ READY, rfq_render enqueued in the same tx.
func TestRfqPriceHappyPath(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	slug := testenv.SeedOrganization(t, org, "price-happy")
	rig := newPriceRig(t, db)
	rfq := seedPricedRFQ(t, db, org, extractionDoc(importFields()), "0.8500")
	seedPriceRule(t, db, org, "ALL", "", "", "0.1800", "")
	tariffID := seedTariff(t, db, org, customsTariff("2026-12-31"))

	require.NoError(t, rig.worker.Work(context.Background(), priceJob(org, uuid.New(), rfq)))

	status, review := rfqState(t, db, org, rfq)
	assert.Equal(t, "READY", status)
	assert.Empty(t, review, "a priced RFQ carries no review reason")

	quotes := loadQuotes(t, db, org, rfq)
	require.Len(t, quotes, 1)
	q := quotes[0]
	assert.Equal(t, 1, q.Version)
	assert.Equal(t, "DRAFT", q.Status)
	assert.Regexp(t, quoteNumberPattern, q.Number)
	// SeedOrganization slugs are lowercase; quote_number uppercases them.
	assert.Equal(t, fmt.Sprintf("%s-2026-0001", strings.ToUpper(slug)), q.Number)
	assert.Equal(t, time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC), q.QuoteDate,
		"quote_date is the extracted ready_date")
	assert.Equal(t, pricedNow, q.PricedAt.UTC())
	assert.NotEmpty(t, q.InputsHash)
	assert.Nil(t, q.CarrierOffer)
	assert.Nil(t, q.CargoCIF, "cargo_cif_brl stays NULL — the engine exposes no CIF base")
	assert.Equal(t, "350.00", q.TotalCost)
	assert.Equal(t, "0.1800", q.TargetMargin)
	assert.Equal(t, "350.00", q.FinalPrice, "pass-through groups sell at cost")
	assert.Equal(t, "0.8500", q.Confidence, "extraction confidence carries to the quote")
	assert.Contains(t, string(q.PricingResult), `"fallback_reason": null`,
		"no fallback happened — the provenance key is still explicit")

	lines := loadLines(t, db, org, q.ID)
	require.Len(t, lines, 1)
	l := lines[0]
	assert.Equal(t, 0, l.SortOrder)
	assert.Equal(t, "CUSTOMS_DISPATCH", l.Category)
	assert.Equal(t, "PRIVATE_TARIFF", l.SourceType)
	require.NotNil(t, l.TariffID)
	assert.Equal(t, tariffID, *l.TariffID)
	assert.Equal(t, "BRL", l.Currency)
	assert.Equal(t, "1.000000", l.FXRate)
	assert.Equal(t, "0.8500", l.Confidence, "line confidence carries the quote's overall")

	// rfq_render landed in the same commit, addressed by quote id.
	n := countRows(t, db, org,
		`SELECT count(*) FROM river.river_job WHERE kind = 'rfq_render' AND queue = 'render'
		 AND args ->> 'quote_id' = $1`, q.ID.String())
	assert.Equal(t, 1, n)
	assert.Equal(t, 1, auditCountFor(t, db, org, rfq, "rfq.priced"))
	// The audit names the quote and records the provenance fields.
	var meta string
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT coalesce(metadata_json::text,'') FROM app.tenant_audit_logs
			 WHERE action = 'rfq.priced' AND entity_id = $1`, rfq).Scan(&meta)
	}))
	assert.Contains(t, meta, `"version": 1`)
	assert.Contains(t, meta, `"fallback_reason"`)
	assert.Contains(t, meta, q.InputsHash)
}

// TestRfqPriceQuoteNumberFormat pins '{SLUG}-{year}-{seq:04d}' exactly.
func TestRfqPriceQuoteNumberFormat(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	slug := testenv.SeedOrganization(t, org, "price-num")
	rig := newPriceRig(t, db)
	rfq := seedPricedRFQ(t, db, org, extractionDoc(importFields()), "0.9000")
	seedPriceRule(t, db, org, "ALL", "", "", "0.1800", "")
	seedTariff(t, db, org, customsTariff("2026-12-31"))

	require.NoError(t, rig.worker.Work(context.Background(), priceJob(org, uuid.New(), rfq)))
	quotes := loadQuotes(t, db, org, rfq)
	require.Len(t, quotes, 1)
	want := fmt.Sprintf("%s-2026-0001", strings.ToUpper(slug))
	assert.Equal(t, want, quotes[0].Number)
}

// TestRfqPriceReprocessVersions: a reprocessed RFQ builds a new quote
// version — the prior DRAFT is SUPERSEDED, never overwritten (rule 9).
func TestRfqPriceReprocessVersions(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "price-reproc")
	rig := newPriceRig(t, db)
	rfq := seedPricedRFQ(t, db, org, extractionDoc(importFields()), "0.8000")
	seedPriceRule(t, db, org, "ALL", "", "", "0.1800", "")
	seedTariff(t, db, org, customsTariff("2026-12-31"))

	require.NoError(t, rig.worker.Work(context.Background(), priceJob(org, uuid.New(), rfq)))
	setRFQStatus(t, db, org, rfq, "PRICING")
	require.NoError(t, rig.worker.Work(context.Background(), priceJob(org, uuid.New(), rfq)))

	quotes := loadQuotes(t, db, org, rfq)
	require.Len(t, quotes, 2)
	assert.Equal(t, 1, quotes[0].Version)
	assert.Equal(t, "SUPERSEDED", quotes[0].Status)
	assert.Equal(t, 2, quotes[1].Version)
	assert.Equal(t, "DRAFT", quotes[1].Status)
	assert.NotEqual(t, quotes[0].Number, quotes[1].Number,
		"each version burns its own org sequence")

	status, _ := rfqState(t, db, org, rfq)
	assert.Equal(t, "READY", status)
}

// TestRfqPriceExportParks: a BR origin derives EXPORT — the export matrix
// is open, so the RFQ parks with its own reason (spec S3b).
func TestRfqPriceExportParks(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "price-export")
	rig := newPriceRig(t, db)
	fields := importFields()
	fields["origin_locode"] = "BRSSZ"
	fields["dest_locode"] = "CNSHA"
	rfq := seedPricedRFQ(t, db, org, extractionDoc(fields), "0.9000")
	seedPriceRule(t, db, org, "ALL", "", "", "0.1800", "")

	require.NoError(t, rig.worker.Work(context.Background(), priceJob(org, uuid.New(), rfq)))
	status, review := rfqState(t, db, org, rfq)
	assert.Equal(t, "NEEDS_REVIEW", status)
	assert.Equal(t, "EXPORT_MATRIX_OPEN", review)
	assert.Empty(t, loadQuotes(t, db, org, rfq), "a parked RFQ writes no quote")
	assert.Equal(t, 1, auditCountFor(t, db, org, rfq, "rfq.parked"))
}

// TestRfqPriceMissingTariffParks: the required customs group has no rows
// at all — MISSING_TARIFF (spec S3b's missing-vs-stale split).
func TestRfqPriceMissingTariffParks(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "price-missing")
	rig := newPriceRig(t, db)
	rfq := seedPricedRFQ(t, db, org, extractionDoc(importFields()), "0.9000")
	seedPriceRule(t, db, org, "ALL", "", "", "0.1800", "")

	require.NoError(t, rig.worker.Work(context.Background(), priceJob(org, uuid.New(), rfq)))
	status, review := rfqState(t, db, org, rfq)
	assert.Equal(t, "NEEDS_REVIEW", status)
	assert.Equal(t, "MISSING_TARIFF", review)
}

// TestRfqPriceStaleTariffParks: the required group's rows exist but all
// expired before the quote date — STALE_TARIFF, not MISSING_TARIFF.
func TestRfqPriceStaleTariffParks(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "price-stale")
	rig := newPriceRig(t, db)
	rfq := seedPricedRFQ(t, db, org, extractionDoc(importFields()), "0.9000")
	seedPriceRule(t, db, org, "ALL", "", "", "0.1800", "")
	// Expired before the 2026-03-15 quote date (the whole window sits in
	// the past): the engine's dumb filter drops it, the re-query without
	// validity still sees it.
	stale := customsTariff("2025-12-31")
	stale.ValidFrom = "2025-01-01"
	seedTariff(t, db, org, stale)

	require.NoError(t, rig.worker.Work(context.Background(), priceJob(org, uuid.New(), rfq)))
	status, review := rfqState(t, db, org, rfq)
	assert.Equal(t, "NEEDS_REVIEW", status)
	assert.Equal(t, "STALE_TARIFF", review)
}

// TestRfqPriceGenericIncompleteParks: an INCOMPLETE reason without a
// dedicated code (here MODAL_OUT_OF_MVP via LCL) parks
// PRICING_INCOMPLETE — the engine's reasons ride the audit row.
func TestRfqPriceGenericIncompleteParks(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "price-generic")
	rig := newPriceRig(t, db)
	fields := importFields()
	fields["modal"] = "LCL"
	rfq := seedPricedRFQ(t, db, org, extractionDoc(fields), "0.9000")
	seedPriceRule(t, db, org, "ALL", "", "", "0.1800", "")

	require.NoError(t, rig.worker.Work(context.Background(), priceJob(org, uuid.New(), rfq)))
	status, review := rfqState(t, db, org, rfq)
	assert.Equal(t, "NEEDS_REVIEW", status)
	assert.Equal(t, "PRICING_INCOMPLETE", review)
	var meta string
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT coalesce(metadata_json::text,'') FROM app.tenant_audit_logs
			 WHERE action = 'rfq.parked' AND entity_id = $1`, rfq).Scan(&meta)
	}))
	assert.Contains(t, meta, "MODAL_OUT_OF_MVP", "the engine reason survives verbatim")
}

// TestRfqPriceRequiredFieldMissing: a required extraction key absent
// parks REQUIRED_FIELD_MISSING — the extraction is the analyst's problem,
// not the pipeline's.
func TestRfqPriceRequiredFieldMissing(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "price-req")
	rig := newPriceRig(t, db)
	fields := importFields()
	delete(fields, "modal")
	rfq := seedPricedRFQ(t, db, org, extractionDoc(fields), "0.9000")

	require.NoError(t, rig.worker.Work(context.Background(), priceJob(org, uuid.New(), rfq)))
	status, review := rfqState(t, db, org, rfq)
	assert.Equal(t, "NEEDS_REVIEW", status)
	assert.Equal(t, "REQUIRED_FIELD_MISSING", review)
}

// TestRfqPriceDirectionAmbiguous: neither endpoint is Brazilian, so no
// direction can be derived — the RFQ parks rather than guess.
func TestRfqPriceDirectionAmbiguous(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "price-ambig")
	rig := newPriceRig(t, db)
	fields := importFields()
	fields["origin_locode"] = "CNSHA"
	fields["dest_locode"] = "DEHAM"
	rfq := seedPricedRFQ(t, db, org, extractionDoc(fields), "0.9000")

	require.NoError(t, rig.worker.Work(context.Background(), priceJob(org, uuid.New(), rfq)))
	status, review := rfqState(t, db, org, rfq)
	assert.Equal(t, "NEEDS_REVIEW", status)
	assert.Equal(t, "DIRECTION_AMBIGUOUS", review)
}

// TestRfqPriceUndecodableCancels: PRICING with a payload that is not the
// extraction document is S2-contract drift — JobCancel, never a retry.
func TestRfqPriceUndecodableCancels(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "price-drift")
	rig := newPriceRig(t, db)
	rfq := seedPricedRFQ(t, db, org, `{"not_fields": true}`, "0.9000")

	err := rig.worker.Work(context.Background(), priceJob(org, uuid.New(), rfq))
	assertJobCancel(t, err)
	status, _ := rfqState(t, db, org, rfq)
	assert.Equal(t, "PRICING", status, "a cancelled job leaves the checkpoint untouched")
}

// TestRfqPriceNullConfidenceCancels: a PRICING row with NULL
// extraction_confidence is S2-contract drift — the gate writes
// confidence and status in one commit — so the job cancels via
// JobCancel, never burns retries.
func TestRfqPriceNullConfidenceCancels(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "price-noconf")
	rig := newPriceRig(t, db)
	rfq := seedPricedRFQ(t, db, org, extractionDoc(importFields()), "")
	seedPriceRule(t, db, org, "ALL", "", "", "0.1800", "")
	seedTariff(t, db, org, customsTariff("2026-12-31"))

	err := rig.worker.Work(context.Background(), priceJob(org, uuid.New(), rfq))
	assertJobCancel(t, err)
	status, _ := rfqState(t, db, org, rfq)
	assert.Equal(t, "PRICING", status, "a cancelled job leaves the checkpoint untouched")
	assert.Empty(t, loadQuotes(t, db, org, rfq))
}

// TestRfqPriceConfigErrorCancels: a pricing rule whose fixed_fees_json
// fails the engine's validation is a config error — JobCancel (spec S3b:
// transient errors are DB errors only).
func TestRfqPriceConfigErrorCancels(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "price-config")
	rig := newPriceRig(t, db)
	rfq := seedPricedRFQ(t, db, org, extractionDoc(importFields()), "0.9000")
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.pricing_rules
				(organization_id, modal, default_target_margin, fixed_fees_json)
			VALUES ($1, 'ALL', 0.1800,
			        '[{"charge_code":"X","category":"NOT_A_CATEGORY","amount":1,"currency":"BRL"}]')`, org)
		return err
	}))
	seedTariff(t, db, org, customsTariff("2026-12-31"))

	err := rig.worker.Work(context.Background(), priceJob(org, uuid.New(), rfq))
	assertJobCancel(t, err)
}

// errRunner fails InTenantTx with an injected error — the DB-error path
// must propagate for River's retry, never cancel.
type errRunner struct{ err error }

func (r errRunner) InTenantTx(context.Context, uuid.UUID, func(context.Context, pgx.Tx) error) error {
	return r.err
}

func (r errRunner) InTenantReadTx(context.Context, uuid.UUID, func(context.Context, pgx.Tx) error) error {
	return r.err
}

// TestRfqPriceTransientPropagates: a DB failure inside the tenant tx is a
// transient error — it propagates as a retryable Work error, not a
// JobCancel.
func TestRfqPriceTransientPropagates(t *testing.T) {
	worker := jobs.NewRfqPriceWorker(errRunner{err: errors.New("conn reset")}, func() time.Time { return pricedNow })
	err := worker.Work(context.Background(), priceJob(uuid.New(), uuid.New(), uuid.New()))
	require.Error(t, err)
	var jce *rivertype.JobCancelError
	assert.False(t, errors.As(err, &jce), "a transient DB error must retry, not cancel")
}

// TestRfqPriceTerminalNoOp: a job for an already-READY RFQ is a replay —
// no quote, no park, no render enqueue.
func TestRfqPriceTerminalNoOp(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "price-noop")
	rig := newPriceRig(t, db)
	rfq := seedPricedRFQ(t, db, org, extractionDoc(importFields()), "0.9000")
	setRFQStatus(t, db, org, rfq, "READY")

	require.NoError(t, rig.worker.Work(context.Background(), priceJob(org, uuid.New(), rfq)))
	assert.Empty(t, loadQuotes(t, db, org, rfq))
	n := countRows(t, db, org,
		`SELECT count(*) FROM river.river_job WHERE kind = 'rfq_render'
		 AND args ->> 'organization_id' = $1`, org.String())
	assert.Equal(t, 0, n)
}

// TestRfqPriceOrgIsolation: org B's job names org A's RFQ — RLS makes it
// invisible, so the job no-ops and org B writes nothing.
func TestRfqPriceOrgIsolation(t *testing.T) {
	db := testenv.Open(t)
	orgA := uuid.New()
	orgB := uuid.New()
	testenv.SeedOrganization(t, orgA, "price-orga")
	testenv.SeedOrganization(t, orgB, "price-orgb")
	rig := newPriceRig(t, db)
	rfqA := seedPricedRFQ(t, db, orgA, extractionDoc(importFields()), "0.9000")
	seedPriceRule(t, db, orgA, "ALL", "", "", "0.1800", "")
	seedTariff(t, db, orgA, customsTariff("2026-12-31"))
	seedPriceRule(t, db, orgB, "ALL", "", "", "0.1800", "")
	seedTariff(t, db, orgB, customsTariff("2026-12-31"))

	require.NoError(t, rig.worker.Work(context.Background(), priceJob(orgB, uuid.New(), rfqA)))

	status, _ := rfqState(t, db, orgA, rfqA)
	assert.Equal(t, "PRICING", status, "org A's RFQ is untouched by org B's job")
	assert.Empty(t, loadQuotes(t, db, orgB, rfqA), "org B wrote no quote for org A's RFQ")
	assert.Equal(t, 0, auditCountFor(t, db, orgB, rfqA, "rfq.priced"))
}

// TestRfqPriceQuoteNumbersIndependent: each org's sequence starts at its
// own 0001 — org_quote_counters are per-company (spec 5.2.6).
func TestRfqPriceQuoteNumbersIndependent(t *testing.T) {
	db := testenv.Open(t)
	orgA := uuid.New()
	orgB := uuid.New()
	testenv.SeedOrganization(t, orgA, "seq-a")
	testenv.SeedOrganization(t, orgB, "seq-b")
	rig := newPriceRig(t, db)
	for _, org := range []uuid.UUID{orgA, orgB} {
		seedPriceRule(t, db, org, "ALL", "", "", "0.1800", "")
		seedTariff(t, db, org, customsTariff("2026-12-31"))
	}
	rfqA := seedPricedRFQ(t, db, orgA, extractionDoc(importFields()), "0.9000")
	rfqB := seedPricedRFQ(t, db, orgB, extractionDoc(importFields()), "0.9000")

	require.NoError(t, rig.worker.Work(context.Background(), priceJob(orgA, uuid.New(), rfqA)))
	require.NoError(t, rig.worker.Work(context.Background(), priceJob(orgB, uuid.New(), rfqB)))

	qA := loadQuotes(t, db, orgA, rfqA)
	qB := loadQuotes(t, db, orgB, rfqB)
	require.Len(t, qA, 1)
	require.Len(t, qB, 1)
	assert.Contains(t, qA[0].Number, "-2026-0001")
	assert.Contains(t, qB[0].Number, "-2026-0001")
	assert.NotEqual(t, qA[0].Number, qB[0].Number, "slugs differ")
}

// TestRfqPriceRulePrecedence: the route-specific rule's
// route_target_margin wins over the modal and ALL defaults (spec §2
// precedence: route > modal > ALL).
func TestRfqPriceRulePrecedence(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "price-prec")
	rig := newPriceRig(t, db)
	rfq := seedPricedRFQ(t, db, org, extractionDoc(importFields()), "0.9000")
	seedPriceRule(t, db, org, "ALL", "", "", "0.1000", "")
	seedPriceRule(t, db, org, "FCL", "", "", "0.1800", "")
	seedPriceRule(t, db, org, "FCL", "CNSHA", "BRSSZ", "0.1800", "0.2500")
	seedTariff(t, db, org, customsTariff("2026-12-31"))

	require.NoError(t, rig.worker.Work(context.Background(), priceJob(org, uuid.New(), rfq)))
	quotes := loadQuotes(t, db, org, rfq)
	require.Len(t, quotes, 1)
	assert.Equal(t, "0.2500", quotes[0].TargetMargin,
		"route_target_margin on the route row beats modal and ALL defaults")
}

// TestRfqPriceOfferPinned: an OFFER_FOUND checkpoint on the RFQ is
// pinned onto the quote row for reproducibility — even when the priced
// groups do not consume offer lines (spec S3b audit).
func TestRfqPriceOfferPinned(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "price-pin")
	rig := newPriceRig(t, db)
	rfq := seedPricedRFQ(t, db, org, extractionDoc(importFields()), "0.9000")
	seedPriceRule(t, db, org, "ALL", "", "", "0.1800", "")
	seedTariff(t, db, org, customsTariff("2026-12-31"))
	offerID := seedOffer(t, db, org, rfq, `{
		"carrier": "HAPAG",
		"charges": [{"category":"OCEAN_FREIGHT","code":"OCEAN","amount":900,"currency":"USD","equipment_or_unit":"40HC"}],
		"expires_at": "2026-04-01T00:00:00Z"}`)

	require.NoError(t, rig.worker.Work(context.Background(), priceJob(org, uuid.New(), rfq)))
	quotes := loadQuotes(t, db, org, rfq)
	require.Len(t, quotes, 1)
	require.NotNil(t, quotes[0].CarrierOffer)
	assert.Equal(t, offerID, *quotes[0].CarrierOffer,
		"the quote pins the OFFER_FOUND row even though DAP prices no freight")
}

// TestRfqPriceNoPricingRuleParks: no rule row at all is a config gap the
// analyst fixes — PRICING_INCOMPLETE, not a crash.
func TestRfqPriceNoPricingRuleParks(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "price-norule")
	rig := newPriceRig(t, db)
	rfq := seedPricedRFQ(t, db, org, extractionDoc(importFields()), "0.9000")
	seedTariff(t, db, org, customsTariff("2026-12-31"))

	require.NoError(t, rig.worker.Work(context.Background(), priceJob(org, uuid.New(), rfq)))
	status, review := rfqState(t, db, org, rfq)
	assert.Equal(t, "NEEDS_REVIEW", status)
	assert.Equal(t, "PRICING_INCOMPLETE", review)
}

// TestRfqPriceTimeoutAndAttempts pin the spec-§3 stage table on the args
// and the worker: five attempts, five seconds.
func TestRfqPriceTimeoutAndAttempts(t *testing.T) {
	db := testenv.Open(t)
	worker := jobs.NewRfqPriceWorker(db, func() time.Time { return pricedNow })
	assert.Equal(t, 5*time.Second, worker.Timeout(nil))
	assert.Equal(t, 5, jobs.PriceArgs{}.InsertOpts().MaxAttempts)
	assert.Equal(t, "price", jobs.PriceArgs{}.InsertOpts().Queue)
	assert.Equal(t, "rfq_price", jobs.PriceArgs{}.Kind())
}
