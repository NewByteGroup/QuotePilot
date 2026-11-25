package jobs_test

// Real-Postgres tests for S3b's FX wiring (migration 0011): the phase-A
// currency peek, the phase-B PTAX warm-up against httptest, the phase-C
// LoadRates pin and quotes.cargo_cif_brl. The BCB feed is served by a
// local server — no live network in tests (AGENTS.md).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/fx"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// workerPTAXFixture is the PTAX page the local server serves: one
// closing bulletin per business day inside the worker's [pricedNow-14d,
// pricedNow] fetch window AND at-or-before prevBizDay(ready_date
// 2026-03-15) = Friday 2026-03-13 — the dates the engine's resolver can
// pick. Values are fixed so repeat runs pin identical (source,
// currency, rate_date) keys and stay deterministic on the shared table.
const workerPTAXFixture = `{"value":[
  {"cotacaoVenda":5.100000,"dataHoraCotacao":"2026-03-02 13:10:30.100"},
  {"cotacaoVenda":5.200000,"dataHoraCotacao":"2026-03-05 13:10:30.200"},
  {"cotacaoVenda":5.300000,"dataHoraCotacao":"2026-03-09 13:10:30.300"},
  {"cotacaoVenda":5.400000,"dataHoraCotacao":"2026-03-10 13:10:30.400"}
]}`

// ptaxHTTPServer serves the fixture for any currency request.
func ptaxHTTPServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, workerPTAXFixture)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ptaxSyncer is the production-shaped phase-B dependency pointed at the
// local server: real Fetcher, real pool upserts.
func ptaxSyncer(t *testing.T, db *tenantdb.DB) *fx.Syncer {
	t.Helper()
	srv := ptaxHTTPServer(t)
	return fx.NewSyncer(db.Pool(),
		fx.NewFetcher(fx.WithHTTPClient(srv.Client()), fx.WithBaseURL(srv.URL)))
}

// stubSyncer fakes phase B: records every requested currency set and
// returns the injected error — the honest-degradation path.
type stubSyncer struct {
	mu    sync.Mutex
	calls [][]string
	err   error
}

func (s *stubSyncer) EnsurePTAX(_ context.Context, currencies []string, _, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, append([]string{}, currencies...))
	return s.err
}

func (s *stubSyncer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *stubSyncer) lastCurrencies() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		return nil
	}
	return s.calls[len(s.calls)-1]
}

// seedFXRate pins a row directly — the CUSTOMS/CARRIER sources have no
// worker-side feed yet, so tests write them as qp_app (INSERT is the
// append-only grant, ON CONFLICT keeps re-runs idempotent).
func seedFXRate(t *testing.T, db *tenantdb.DB, source, ccy, rate, day string) {
	t.Helper()
	_, err := db.Pool().Exec(context.Background(), `
		INSERT INTO app.fx_rates (source, currency, rate, rate_date)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (source, currency, rate_date) DO NOTHING`,
		source, ccy, rate, day)
	require.NoError(t, err)
}

// usdCustomsTariff prices the DAP customs-dispatch group in USD — the
// line converts at PTAX (non-carrier path), so the quote is INCOMPLETE
// until a USD rate is pinned.
func usdCustomsTariff() tariffSeed {
	return tariffSeed{
		Modal: "FCL", Origin: "CNSHA", Dest: "BRSSZ", Equipment: "40HC",
		Category: "CUSTOMS", Code: "CUSTOMS_FEE", Currency: "USD", Amount: "70.00",
		ValidFrom: "2026-01-01", ValidUntil: "2026-12-31",
	}
}

// seedPriceRuleWithFees inserts the ALL-modal rule plus a fixed_fees
// payload — the GBP fee on the open customs group is the honest-failure
// scenario: its PTAX rate is missing, it drops from the pool and only
// MISSING_FX:GBP remains (a failed REQUIRED group's tariff would also
// raise MISSING_TARIFF_GROUP and park as STALE_TARIFF by the existing
// mapping — that is a different, louder review).
func seedPriceRuleWithFees(t *testing.T, db *tenantdb.DB, org uuid.UUID, feesJSON string) {
	t.Helper()
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.pricing_rules
				(organization_id, modal, default_target_margin, fx_safety_spread, fixed_fees_json)
			VALUES ($1, 'ALL', 0.1800, 0, $2::jsonb)`, org, feesJSON)
		return err
	}))
}

// declaredCIFFields adds the goods CIF trio to the import extraction:
// cargo value, declared freight and declared insurance, all USD.
func declaredCIFFields() map[string]string {
	fields := importFields()
	fields["cargo_value_amount"] = "10000.00"
	fields["cargo_value_currency"] = "USD"
	fields["declared_freight_amount"] = "1200.00"
	fields["declared_freight_currency"] = "USD"
	fields["declared_insurance_amount"] = "300.00"
	fields["declared_insurance_currency"] = "USD"
	return fields
}

// TestRfqPriceUSDCompletesWithPTAX is the unlock this change exists
// for: a USD-priced group that parked MISSING_FX now reaches COMPLETE —
// phase B pinned the PTAX and phase C loaded it into Input.FXRates.
func TestRfqPriceUSDCompletesWithPTAX(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "price-fx-usd")
	rig := newPriceRig(t, db)
	rig.worker.FX = ptaxSyncer(t, db)
	rfq := seedPricedRFQ(t, db, org, extractionDoc(importFields()), "0.9000")
	seedPriceRule(t, db, org, "ALL", "", "", "0.1800", "")
	seedTariff(t, db, org, usdCustomsTariff())

	require.NoError(t, rig.worker.Work(context.Background(), priceJob(org, uuid.New(), rfq)))

	status, _ := rfqState(t, db, org, rfq)
	assert.Equal(t, "READY", status, "USD PTAX resolved — no MISSING_FX park")
	quotes := loadQuotes(t, db, org, rfq)
	require.Len(t, quotes, 1)
	lines := loadLines(t, db, org, quotes[0].ID)
	require.Len(t, lines, 1)
	// USD 70.00 at the pinned PTAX 5.4 (newest at-or-before prevBizDay
	// 2026-03-13) = BRL 378.00; pass-through sells at cost.
	assert.Equal(t, "USD", lines[0].Currency)
	assert.Equal(t, "70.0000", lines[0].Amount)
	assert.Equal(t, "5.400000", lines[0].FXRate)
	assert.Equal(t, "378.00", lines[0].CostBRL)
	assert.Contains(t, string(quotes[0].PricingResult), `"source": "PTAX"`,
		"the applied rate is recorded on the quote for the audit view")
}

// TestRfqPriceFetchFailureParksHonestly: a dead BCB warns and the job
// continues — the engine parks MISSING_FX (PRICING_INCOMPLETE) exactly
// as before fx_rates existed; five attempts are not burned on an outage.
func TestRfqPriceFetchFailureParksHonestly(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "price-fx-down")
	rig := newPriceRig(t, db)
	stub := &stubSyncer{err: errors.New("bcb unreachable")}
	rig.worker.FX = stub
	rfq := seedPricedRFQ(t, db, org, extractionDoc(importFields()), "0.9000")
	seedPriceRuleWithFees(t, db, org, `[{
		"charge_code": "DOC_FEE", "description": "Docs",
		"category": "CUSTOMS_DISPATCH", "amount": 50, "currency": "GBP"}]`)
	seedTariff(t, db, org, customsTariff("2026-12-31"))

	require.NoError(t, rig.worker.Work(context.Background(), priceJob(org, uuid.New(), rfq)))

	status, review := rfqState(t, db, org, rfq)
	assert.Equal(t, "NEEDS_REVIEW", status)
	assert.Equal(t, "PRICING_INCOMPLETE", review)
	assert.Equal(t, 1, stub.callCount(), "one bounded fetch attempt, then continue")
	assert.Equal(t, []string{"EUR", "USD"}, stub.lastCurrencies())
	var meta string
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT coalesce(metadata_json::text,'') FROM app.tenant_audit_logs
			 WHERE action = 'rfq.parked' AND entity_id = $1`, rfq).Scan(&meta)
	}))
	assert.Contains(t, meta, "MISSING_FX:GBP", "the engine reason survives verbatim")
}

// TestRfqPriceNoFetchWhenNotPricing: a terminal RFQ short-circuits in
// phase A — no currencies, no BCB call.
func TestRfqPriceNoFetchWhenNotPricing(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "price-fx-ready")
	rig := newPriceRig(t, db)
	stub := &stubSyncer{}
	rig.worker.FX = stub
	rfq := seedPricedRFQ(t, db, org, extractionDoc(importFields()), "0.9000")
	setRFQStatus(t, db, org, rfq, "READY")

	require.NoError(t, rig.worker.Work(context.Background(), priceJob(org, uuid.New(), rfq)))
	assert.Equal(t, 0, stub.callCount(), "phase A skips a non-PRICING RFQ entirely")
}

// TestRfqPriceNoFetchWhenUndecodable: the peek cannot derive currencies
// from a payload that will not decode — no call; the main path cancels.
func TestRfqPriceNoFetchWhenUndecodable(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "price-fx-drift")
	rig := newPriceRig(t, db)
	stub := &stubSyncer{}
	rig.worker.FX = stub
	rfq := seedPricedRFQ(t, db, org, `{"not_fields": true}`, "0.9000")

	err := rig.worker.Work(context.Background(), priceJob(org, uuid.New(), rfq))
	assertJobCancel(t, err)
	assert.Equal(t, 0, stub.callCount())
}

// TestRfqPriceFetchSetFromExtraction: the requested set is the
// extraction's non-BRL currencies union the USD/EUR constants — a JPY
// declared insurance joins them, BRL is skipped.
func TestRfqPriceFetchSetFromExtraction(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "price-fx-set")
	rig := newPriceRig(t, db)
	stub := &stubSyncer{err: errors.New("stop after record")}
	rig.worker.FX = stub
	fields := declaredCIFFields()
	fields["cargo_value_currency"] = "BRL"
	fields["declared_insurance_currency"] = "JPY"
	rfq := seedPricedRFQ(t, db, org, extractionDoc(fields), "0.9000")

	require.NoError(t, rig.worker.Work(context.Background(), priceJob(org, uuid.New(), rfq)))
	require.Equal(t, 1, stub.callCount())
	assert.Equal(t, []string{"EUR", "JPY", "USD"}, stub.lastCurrencies())
}

// TestRfqPriceCargoCIFPopulated: the declared trio converts at the
// pinned CUSTOMS rate — (10000 + 1200 + 300) USD × 5.0 = 57500.00.
func TestRfqPriceCargoCIFPopulated(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "price-cif")
	rig := newPriceRig(t, db)
	rig.worker.FX = ptaxSyncer(t, db)
	rfq := seedPricedRFQ(t, db, org, extractionDoc(declaredCIFFields()), "0.9000")
	seedPriceRule(t, db, org, "ALL", "", "", "0.1800", "")
	seedTariff(t, db, org, customsTariff("2026-12-31"))
	// The CUSTOMS pin for the CIF base — at-or-before QuoteDate 2026-03-15.
	seedFXRate(t, db, "CUSTOMS", "USD", "5.000000", "2026-03-12")

	require.NoError(t, rig.worker.Work(context.Background(), priceJob(org, uuid.New(), rfq)))

	quotes := loadQuotes(t, db, org, rfq)
	require.Len(t, quotes, 1)
	require.NotNil(t, quotes[0].CargoCIF, "all three components resolvable — the column carries the CIF")
	assert.Equal(t, "57500.00", *quotes[0].CargoCIF)
}

// TestRfqPriceCargoCIFNullMissingComponent: declared insurance absent →
// NULL — a partial sum is never fabricated.
func TestRfqPriceCargoCIFNullMissingComponent(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "price-cif-null")
	rig := newPriceRig(t, db)
	rig.worker.FX = ptaxSyncer(t, db)
	fields := declaredCIFFields()
	delete(fields, "declared_insurance_amount")
	delete(fields, "declared_insurance_currency")
	rfq := seedPricedRFQ(t, db, org, extractionDoc(fields), "0.9000")
	seedPriceRule(t, db, org, "ALL", "", "", "0.1800", "")
	seedTariff(t, db, org, customsTariff("2026-12-31"))
	seedFXRate(t, db, "CUSTOMS", "USD", "5.000000", "2026-03-12")

	require.NoError(t, rig.worker.Work(context.Background(), priceJob(org, uuid.New(), rfq)))

	quotes := loadQuotes(t, db, org, rfq)
	require.Len(t, quotes, 1)
	assert.Nil(t, quotes[0].CargoCIF, "absent declared insurance — the column stays NULL")
}

// TestRfqPriceCargoCIFNullMissingRate: the trio is declared but no
// CUSTOMS rate exists → NULL (the quote still prices — customs rates
// feed the band path only, and this route has no terminal request).
func TestRfqPriceCargoCIFNullMissingRate(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "price-cif-norate")
	rig := newPriceRig(t, db)
	rig.worker.FX = ptaxSyncer(t, db)
	// JPY trio: no CUSTOMS JPY row exists anywhere, and the PTAX feed
	// cannot mint one — the rate is honestly unresolvable.
	fields := declaredCIFFields()
	fields["cargo_value_currency"] = "JPY"
	fields["declared_freight_currency"] = "JPY"
	fields["declared_insurance_currency"] = "JPY"
	rfq := seedPricedRFQ(t, db, org, extractionDoc(fields), "0.9000")
	seedPriceRule(t, db, org, "ALL", "", "", "0.1800", "")
	seedTariff(t, db, org, customsTariff("2026-12-31"))

	require.NoError(t, rig.worker.Work(context.Background(), priceJob(org, uuid.New(), rfq)))

	quotes := loadQuotes(t, db, org, rfq)
	require.Len(t, quotes, 1)
	assert.Nil(t, quotes[0].CargoCIF, "no CUSTOMS JPY — NULL, never a PTAX substitute")
}
