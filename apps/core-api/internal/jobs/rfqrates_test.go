package jobs_test

// Real-Postgres integration tests for the S3a rfq_fetch_rates worker
// (spec S3a): the owed-freight gate, the four terminal outcomes, the
// (rfq_id, carrier, request_hash) idempotency key, the MIRROR source and
// tenant isolation. The carrier connector seam is a named fake — the
// tenant DB, RLS and the in-transaction PriceArgs enqueue are real:
// they are what these tests exist to prove.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/aiengine"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/carrier"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/jobs"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// fakeRateFetcher is the named fake for the carrier.Fetcher seam:
// answers from a configured result/error and records every call — the
// replay tests count calls to prove the paid fetch is never repeated.
type fakeRateFetcher struct {
	carrierName string
	source      string
	result      *carrier.Result
	err         error

	calls int
	last  carrier.Request
}

func (f *fakeRateFetcher) Carrier() string { return f.carrierName }
func (f *fakeRateFetcher) Source() string  { return f.source }

func (f *fakeRateFetcher) FetchOffer(_ context.Context, req carrier.Request) (*carrier.Result, error) {
	f.calls++
	f.last = req
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

// liveFetcher returns a fake binding shaped like the CMA live one.
func liveFetcher(result *carrier.Result, err error) *fakeRateFetcher {
	return &fakeRateFetcher{carrierName: "CMA_CGM", source: carrier.SourceLive, result: result, err: err}
}

// offerResult is one valid OFFER_FOUND answer (payload passes
// ValidateOffer) with a carrier-side reference.
func offerResult() *carrier.Result {
	ref := "CMA-QUOTE-42"
	return &carrier.Result{
		Offer:          []byte(`{"carrier":"CMA_CGM","charges":[{"category":"OCEAN_FREIGHT","code":"OCEAN","amount":900.00,"currency":"USD","equipment_or_unit":"40HC"}],"expires_at":"2026-04-01T00:00:00Z"}`),
		OfferReference: &ref,
	}
}

// ratesRig bundles the worker with its fake fetcher and the insert-only
// River client the other worker rigs use.
type ratesRig struct {
	worker  *jobs.RfqRatesWorker
	fetcher *fakeRateFetcher
}

func newRatesRig(t *testing.T, db *tenantdb.DB, fetcher carrier.Fetcher) *ratesRig {
	t.Helper()
	worker := jobs.NewRfqRatesWorker(db, fetcher, func() time.Time { return pricedNow })
	client, err := river.NewClient(riverpgxv5.New(db.Pool()), &river.Config{
		Schema:            "river",
		ReindexerSchedule: river.NeverSchedule(),
	})
	require.NoError(t, err)
	worker.Inserter = client
	fake, _ := fetcher.(*fakeRateFetcher)
	return &ratesRig{worker: worker, fetcher: fake}
}

// rateJob wraps args in the job shape; attempt >0 pins JobRow's attempt
// and MaxAttempts (3, the stage budget).
func rateJob(org, corr, rfqID uuid.UUID, attempt int) *river.Job[jobs.RateArgs] {
	j := &river.Job[jobs.RateArgs]{Args: jobs.RateArgs{
		OrganizationID: org, CorrelationID: corr, RfqID: rfqID,
	}}
	if attempt > 0 {
		j.JobRow = &rivertype.JobRow{Attempt: attempt, MaxAttempts: 3}
	}
	return j
}

// owedFields is the fetch path's extraction: FCL + FOB + import — the
// requesting customer owes ocean freight (importMatrix freight column).
func owedFields() map[string]string {
	return map[string]string{
		"incoterm":          "FOB",
		"modal":             "FCL",
		"origin_locode":     "CNSHA",
		"dest_locode":       "BRSSZ",
		"equipment_or_unit": "40HC",
		"quantity":          "2",
		"ready_date":        "2026-03-15",
	}
}

// offerRow mirrors the checkpoint columns the tests assert on.
type offerRow struct {
	Outcome     string
	Carrier     *string
	Source      string
	RequestHash string
	Payload     *string
	Reference   *string
	ExpiresAt   *time.Time
	FetchedAt   time.Time
}

func loadOffers(t *testing.T, db *tenantdb.DB, org, rfq uuid.UUID) []offerRow {
	t.Helper()
	var out []offerRow
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT outcome, carrier, source, request_hash, offer_payload_json::text,
			       offer_reference, expires_at, fetched_at
			FROM app.carrier_offers WHERE rfq_id = $1 ORDER BY created_at`, rfq)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r offerRow
			if err := rows.Scan(&r.Outcome, &r.Carrier, &r.Source, &r.RequestHash,
				&r.Payload, &r.Reference, &r.ExpiresAt, &r.FetchedAt); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	}))
	return out
}

// priceJobCount counts the rfq_price jobs staged for one RFQ — every
// terminal outcome must enqueue exactly one (deduped on live states).
func priceJobCount(t *testing.T, db *tenantdb.DB, org, rfq uuid.UUID) int {
	t.Helper()
	return countRows(t, db, org,
		`SELECT count(*) FROM river.river_job WHERE kind = 'rfq_price' AND queue = 'price'
		 AND args ->> 'rfq_id' = $1`, rfq.String())
}

func auditMetaFor(t *testing.T, db *tenantdb.DB, org, rfq uuid.UUID, action string) string {
	t.Helper()
	var meta string
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT coalesce(metadata_json::text,'') FROM app.tenant_audit_logs
			 WHERE action = $1 AND entity_id = $2 ORDER BY created_at DESC LIMIT 1`,
			action, rfq).Scan(&meta)
	}))
	return meta
}

// transient503 is a retryable engine failure (the shared taxonomy).
func transient503() error {
	return &aiengine.EngineError{Op: "carrier_offer_search", StatusCode: 503, Detail: "upstream down"}
}

// ---------------------------------------------------------------------------
// Terminal outcomes — every one checkpoints AND enqueues rfq_price
// ---------------------------------------------------------------------------

// TestRfqRatesOfferFound: the owed path (FCL+FOB+import) calls the
// carrier once, validates the payload and pins the OFFER_FOUND row —
// carrier, source, hash, verbatim payload, reference and expiry — then
// releases pricing in the same tx.
func TestRfqRatesOfferFound(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "rates-found")
	rig := newRatesRig(t, db, liveFetcher(offerResult(), nil))
	rfq := seedPricedRFQ(t, db, org, extractionDoc(owedFields()), "0.9000")

	require.NoError(t, rig.worker.Work(context.Background(), rateJob(org, uuid.New(), rfq, 1)))

	rows := loadOffers(t, db, org, rfq)
	require.Len(t, rows, 1)
	r := rows[0]
	assert.Equal(t, "OFFER_FOUND", r.Outcome)
	require.NotNil(t, r.Carrier)
	assert.Equal(t, "CMA_CGM", *r.Carrier)
	assert.Equal(t, "LIVE", r.Source)
	assert.Len(t, r.RequestHash, 64)
	require.NotNil(t, r.Payload, "OFFER_FOUND pins the validated payload")
	// jsonb normalises key order and spacing, so compare semantically.
	assert.JSONEq(t, `{"carrier":"CMA_CGM","charges":[{"category":"OCEAN_FREIGHT","code":"OCEAN","amount":900.00,"currency":"USD","equipment_or_unit":"40HC"}],"expires_at":"2026-04-01T00:00:00Z"}`, *r.Payload)
	require.NotNil(t, r.Reference)
	assert.Equal(t, "CMA-QUOTE-42", *r.Reference)
	require.NotNil(t, r.ExpiresAt)
	assert.Equal(t, time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), r.ExpiresAt.UTC())
	assert.Equal(t, pricedNow, r.FetchedAt.UTC(), "fetched_at is the worker clock")

	assert.Equal(t, 1, rig.fetcher.calls, "one fetch, one checkpoint")
	got := rig.fetcher.last
	assert.Equal(t, "CNSHA", got.OriginLOCODE)
	assert.Equal(t, "BRSSZ", got.DestLOCODE)
	assert.Equal(t, "40HC", got.EquipmentOrUnit)
	assert.Equal(t, 2, got.Quantity)
	assert.Equal(t, time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC), got.ReadyDate.UTC())

	assert.Equal(t, 1, priceJobCount(t, db, org, rfq), "exactly one rfq_price enqueued")
	status, review := rfqState(t, db, org, rfq)
	assert.Equal(t, "PRICING", status, "S3a does not move the status — S3b owns it")
	assert.Empty(t, review)
	assert.Equal(t, 1, auditCountFor(t, db, org, rfq, "rfq.rates_fetched"))
	meta := auditMetaFor(t, db, org, rfq, "rfq.rates_fetched")
	assert.Contains(t, meta, `"outcome": "OFFER_FOUND"`)
	assert.Contains(t, meta, r.RequestHash)
}

// TestRfqRatesNoOffer: the carrier answered with nothing usable —
// NO_OFFER carries carrier + hash and no content, and releases pricing.
func TestRfqRatesNoOffer(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "rates-none")
	rig := newRatesRig(t, db, liveFetcher(&carrier.Result{Offer: []byte("null")}, nil))
	rfq := seedPricedRFQ(t, db, org, extractionDoc(owedFields()), "0.9000")

	require.NoError(t, rig.worker.Work(context.Background(), rateJob(org, uuid.New(), rfq, 1)))

	rows := loadOffers(t, db, org, rfq)
	require.Len(t, rows, 1)
	assert.Equal(t, "NO_OFFER", rows[0].Outcome)
	require.NotNil(t, rows[0].Carrier)
	assert.Equal(t, "CMA_CGM", *rows[0].Carrier)
	assert.Nil(t, rows[0].Payload)
	assert.Equal(t, 1, priceJobCount(t, db, org, rfq))
	assert.Equal(t, 1, rig.fetcher.calls)
}

// TestRfqRatesFetchFailedFinal: the transient failure exhausts the
// budget on attempt 3 — FETCH_FAILED commits the checkpoint and releases
// pricing (a carrier outage never strands an RFQ; no FAILED mark).
func TestRfqRatesFetchFailedFinal(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "rates-fail")
	rig := newRatesRig(t, db, liveFetcher(nil, transient503()))
	rfq := seedPricedRFQ(t, db, org, extractionDoc(owedFields()), "0.9000")

	require.NoError(t, rig.worker.Work(context.Background(), rateJob(org, uuid.New(), rfq, 3)))

	rows := loadOffers(t, db, org, rfq)
	require.Len(t, rows, 1)
	assert.Equal(t, "FETCH_FAILED", rows[0].Outcome)
	require.NotNil(t, rows[0].Carrier)
	assert.Nil(t, rows[0].Payload)
	assert.Equal(t, 1, priceJobCount(t, db, org, rfq))
	status, _ := rfqState(t, db, org, rfq)
	assert.Equal(t, "PRICING", status, "an outage is recoverable info, never a FAILED mark")
	meta := auditMetaFor(t, db, org, rfq, "rfq.rates_fetched")
	assert.Contains(t, meta, `"reason": "fetch_error"`)
	assert.Contains(t, meta, `"outcome": "FETCH_FAILED"`)
}

// TestRfqRatesTransientRetries: before the budget is spent a fetch error
// propagates for River's retry — nothing commits (no row, no enqueue).
func TestRfqRatesTransientRetries(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "rates-retry")
	rig := newRatesRig(t, db, liveFetcher(nil, transient503()))
	rfq := seedPricedRFQ(t, db, org, extractionDoc(owedFields()), "0.9000")

	err := rig.worker.Work(context.Background(), rateJob(org, uuid.New(), rfq, 1))
	require.Error(t, err)
	var jce *rivertype.JobCancelError
	assert.False(t, errors.As(err, &jce), "a fetch error retries, never cancels")
	assert.Empty(t, loadOffers(t, db, org, rfq), "a retried attempt commits nothing")
	assert.Equal(t, 0, priceJobCount(t, db, org, rfq))
}

// TestRfqRatesInvalidPayloadBudget: a sanity-bounds violation retries
// inside the budget like any fetch error, and the last attempt commits
// FETCH_FAILED with the contract_violation class.
func TestRfqRatesInvalidPayloadBudget(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "rates-bad")
	invalid := &carrier.Result{Offer: []byte(`{"carrier":"CMA_CGM","charges":[],"expires_at":"2026-04-01T00:00:00Z"}`)}
	rig := newRatesRig(t, db, liveFetcher(invalid, nil))
	rfq := seedPricedRFQ(t, db, org, extractionDoc(owedFields()), "0.9000")

	err := rig.worker.Work(context.Background(), rateJob(org, uuid.New(), rfq, 1))
	require.Error(t, err, "a violation retries inside the budget")
	assert.Empty(t, loadOffers(t, db, org, rfq))

	require.NoError(t, rig.worker.Work(context.Background(), rateJob(org, uuid.New(), rfq, 3)))
	rows := loadOffers(t, db, org, rfq)
	require.Len(t, rows, 1)
	assert.Equal(t, "FETCH_FAILED", rows[0].Outcome)
	meta := auditMetaFor(t, db, org, rfq, "rfq.rates_fetched")
	assert.Contains(t, meta, `"reason": "contract_violation"`)
	assert.Equal(t, 1, priceJobCount(t, db, org, rfq))
	assert.Equal(t, 2, rig.fetcher.calls, "both attempts paid the call")
}

// ---------------------------------------------------------------------------
// The owed-freight gate (NOT_APPLICABLE)
// ---------------------------------------------------------------------------

// notApplicableCase bundles one gate-case run's assertions.
func assertNotApplicable(t *testing.T, db *tenantdb.DB, rig *ratesRig, org, rfq uuid.UUID, reason string) {
	t.Helper()
	rows := loadOffers(t, db, org, rfq)
	require.Len(t, rows, 1)
	r := rows[0]
	assert.Equal(t, "NOT_APPLICABLE", r.Outcome)
	assert.Nil(t, r.Carrier, "NOT_APPLICABLE means no carrier call was owed — carrier is NULL")
	assert.Nil(t, r.Payload)
	assert.Len(t, r.RequestHash, 64, "even NOT_APPLICABLE carries the request hash")
	assert.Equal(t, 0, rig.fetcher.calls, "not-owed never reaches the connector")
	assert.Equal(t, 1, priceJobCount(t, db, org, rfq), "NOT_APPLICABLE still releases pricing")
	meta := auditMetaFor(t, db, org, rfq, "rfq.rates_fetched")
	assert.Contains(t, meta, fmt.Sprintf(`"reason": "%s"`, reason))
}

// TestRfqRatesGateMatrix covers the spec's never-fetch cases in one
// table: AIR, LCL, EXPORT, and seller-paid import Incoterms.
func TestRfqRatesGateMatrix(t *testing.T) {
	cases := map[string]map[string]string{
		"air":            {"incoterm": "CPT", "modal": "AIR", "origin_locode": "CNSHA", "dest_locode": "BRSSZ", "equipment_or_unit": "ULD", "ready_date": "2026-03-15"},
		"lcl":            {"incoterm": "FOB", "modal": "LCL", "origin_locode": "CNSHA", "dest_locode": "BRSSZ", "ready_date": "2026-03-15"},
		"export cfr":     {"incoterm": "CFR", "modal": "FCL", "origin_locode": "BRSSZ", "dest_locode": "CNSHA", "equipment_or_unit": "40HC", "quantity": "2", "ready_date": "2026-03-15"},
		"export fob":     {"incoterm": "FOB", "modal": "FCL", "origin_locode": "BRSSZ", "dest_locode": "CNSHA", "equipment_or_unit": "40HC", "quantity": "2", "ready_date": "2026-03-15"},
		"import cif":     {"incoterm": "CIF", "modal": "FCL", "origin_locode": "CNSHA", "dest_locode": "BRSSZ", "equipment_or_unit": "40HC", "quantity": "2", "ready_date": "2026-03-15"},
		"import dap":     {"incoterm": "DAP", "modal": "FCL", "origin_locode": "CNSHA", "dest_locode": "BRSSZ", "equipment_or_unit": "40HC", "quantity": "2", "ready_date": "2026-03-15"},
		"direction none": {"incoterm": "FOB", "modal": "FCL", "origin_locode": "CNSHA", "dest_locode": "DEHAM", "equipment_or_unit": "40HC", "quantity": "2", "ready_date": "2026-03-15"},
	}
	for name, fields := range cases {
		t.Run(name, func(t *testing.T) {
			db := testenv.Open(t)
			org := uuid.New()
			testenv.SeedOrganization(t, org, "rates-gate")
			rig := newRatesRig(t, db, liveFetcher(offerResult(), nil))
			rfq := seedPricedRFQ(t, db, org, extractionDoc(fields), "0.9000")

			require.NoError(t, rig.worker.Work(context.Background(), rateJob(org, uuid.New(), rfq, 1)))
			assertNotApplicable(t, db, rig, org, rfq, "freight_not_owed")
		})
	}
}

// TestRfqRatesExportOwedStillNotApplicable pins the flagged decision:
// EXPORT in the freight-owed Incoterm family still checkpoints
// NOT_APPLICABLE — the export matrix is open (RFC-001), so no offer
// could be used even if fetched.
func TestRfqRatesExportOwedStillNotApplicable(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "rates-exw")
	rig := newRatesRig(t, db, liveFetcher(offerResult(), nil))
	fields := owedFields()
	fields["incoterm"] = "EXW"
	fields["origin_locode"] = "BRSSZ"
	fields["dest_locode"] = "CNSHA"
	rfq := seedPricedRFQ(t, db, org, extractionDoc(fields), "0.9000")

	require.NoError(t, rig.worker.Work(context.Background(), rateJob(org, uuid.New(), rfq, 1)))
	assertNotApplicable(t, db, rig, org, rfq, "freight_not_owed")
}

// TestRfqRatesIncompleteRequestNotApplicable pins the second flagged
// decision: owed is established (FCL+FOB+import) but the request cannot
// be normalized (no quantity) — no call ran, so the row is
// NOT_APPLICABLE with the incomplete_request sub-reason; S3b parks the
// extraction gap itself.
func TestRfqRatesIncompleteRequestNotApplicable(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "rates-inc")
	rig := newRatesRig(t, db, liveFetcher(offerResult(), nil))
	fields := owedFields()
	delete(fields, "quantity")
	rfq := seedPricedRFQ(t, db, org, extractionDoc(fields), "0.9000")

	require.NoError(t, rig.worker.Work(context.Background(), rateJob(org, uuid.New(), rfq, 1)))
	assertNotApplicable(t, db, rig, org, rfq, "incomplete_request")
}

// ---------------------------------------------------------------------------
// Idempotency, provenance and isolation
// ---------------------------------------------------------------------------

// TestRfqRatesReplayNeverRefetches: a second S3a run for the same
// request collapses onto the checkpoint — the paid fetch is not
// repeated, and the rfq_price enqueue is still ensured (deduped on live
// states, so exactly one job exists).
func TestRfqRatesReplayNeverRefetches(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "rates-replay")
	rig := newRatesRig(t, db, liveFetcher(offerResult(), nil))
	rfq := seedPricedRFQ(t, db, org, extractionDoc(owedFields()), "0.9000")

	require.NoError(t, rig.worker.Work(context.Background(), rateJob(org, uuid.New(), rfq, 1)))
	require.NoError(t, rig.worker.Work(context.Background(), rateJob(org, uuid.New(), rfq, 2)))

	assert.Equal(t, 1, rig.fetcher.calls, "the outcome row makes the replay a no-op")
	assert.Len(t, loadOffers(t, db, org, rfq), 1)
	assert.Equal(t, 1, priceJobCount(t, db, org, rfq),
		"the ensured re-enqueue dedupes onto the live job")
}

// TestRfqRatesReplayNotApplicableAlsoDedupes: the NULL-carrier
// NOT_APPLICABLE row dedupes too (NULLS NOT DISTINCT) — a replay
// enqueues pricing without writing a second row.
func TestRfqRatesReplayNotApplicableAlsoDedupes(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "rates-narep")
	rig := newRatesRig(t, db, liveFetcher(offerResult(), nil))
	fields := owedFields()
	fields["incoterm"] = "CIF"
	rfq := seedPricedRFQ(t, db, org, extractionDoc(fields), "0.9000")

	require.NoError(t, rig.worker.Work(context.Background(), rateJob(org, uuid.New(), rfq, 1)))
	require.NoError(t, rig.worker.Work(context.Background(), rateJob(org, uuid.New(), rfq, 2)))

	assert.Len(t, loadOffers(t, db, org, rfq), 1)
	assert.Equal(t, 1, priceJobCount(t, db, org, rfq))
}

// TestRfqRatesRequestHashAcrossRfqs: the digest keys on request content
// — two RFQs with identical normalized requests share one hash; a route
// or equipment change yields a different one.
func TestRfqRatesRequestHashAcrossRfqs(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "rates-hash")
	rig := newRatesRig(t, db, liveFetcher(offerResult(), nil))
	rfqA := seedPricedRFQ(t, db, org, extractionDoc(owedFields()), "0.9000")
	rfqB := seedPricedRFQ(t, db, org, extractionDoc(owedFields()), "0.9100")
	changed := owedFields()
	changed["equipment_or_unit"] = "20GP"
	rfqC := seedPricedRFQ(t, db, org, extractionDoc(changed), "0.9200")

	for _, rfq := range []uuid.UUID{rfqA, rfqB, rfqC} {
		require.NoError(t, rig.worker.Work(context.Background(), rateJob(org, uuid.New(), rfq, 1)))
	}
	hashA := loadOffers(t, db, org, rfqA)[0].RequestHash
	hashB := loadOffers(t, db, org, rfqB)[0].RequestHash
	hashC := loadOffers(t, db, org, rfqC)[0].RequestHash
	assert.Equal(t, hashA, hashB, "identical normalized requests share the digest")
	assert.NotEqual(t, hashA, hashC, "an equipment change re-keys the request")
	assert.Equal(t, 3, rig.fetcher.calls,
		"each RFQ fetches once — the unique key is (rfq_id, carrier, request_hash)")
}

// TestRfqRatesMirrorSource: the real mirror binding writes source=MIRROR
// with its synthetic carrier — the row the UI labels simulated (RFC-003).
func TestRfqRatesMirrorSource(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "rates-mirror")
	rig := newRatesRig(t, db, carrier.MirrorFetcher{})
	rfq := seedPricedRFQ(t, db, org, extractionDoc(owedFields()), "0.9000")

	require.NoError(t, rig.worker.Work(context.Background(), rateJob(org, uuid.New(), rfq, 1)))

	rows := loadOffers(t, db, org, rfq)
	require.Len(t, rows, 1)
	r := rows[0]
	assert.Equal(t, "OFFER_FOUND", r.Outcome)
	assert.Equal(t, "MIRROR", r.Source)
	require.NotNil(t, r.Carrier)
	assert.Equal(t, "MIRROR", *r.Carrier)
	require.NotNil(t, r.Payload)
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(*r.Payload), &payload))
	assert.Equal(t, "MIRROR", payload["carrier"])
	require.NotNil(t, r.Reference)
	assert.Contains(t, *r.Reference, "MIR-")
	assert.Equal(t, 1, priceJobCount(t, db, org, rfq))
}

// TestRfqRatesOrgIsolation: org B's job names org A's RFQ — RLS makes it
// invisible, so nothing writes; and org B cannot read org A's checkpoint
// row at all.
func TestRfqRatesOrgIsolation(t *testing.T) {
	db := testenv.Open(t)
	orgA := uuid.New()
	orgB := uuid.New()
	testenv.SeedOrganization(t, orgA, "rates-orga")
	testenv.SeedOrganization(t, orgB, "rates-orgb")
	rig := newRatesRig(t, db, liveFetcher(offerResult(), nil))
	rfqA := seedPricedRFQ(t, db, orgA, extractionDoc(owedFields()), "0.9000")

	require.NoError(t, rig.worker.Work(context.Background(), rateJob(orgA, uuid.New(), rfqA, 1)))
	require.Len(t, loadOffers(t, db, orgA, rfqA), 1, "org A's checkpoint exists")
	assert.Equal(t, 0, countRows(t, db, orgB,
		`SELECT count(*) FROM app.carrier_offers WHERE rfq_id = $1`, rfqA.String()),
		"org B cannot read org A's checkpoint row")

	calls := rig.fetcher.calls
	require.NoError(t, rig.worker.Work(context.Background(), rateJob(orgB, uuid.New(), rfqA, 1)))
	assert.Equal(t, calls, rig.fetcher.calls, "org B's job on A's RFQ is a no-op")
	status, _ := rfqState(t, db, orgA, rfqA)
	assert.Equal(t, "PRICING", status)
	// river_job is shared-schema (no RLS): the count still shows org A's
	// legitimately staged job — the point is B's no-op added none.
	assert.Equal(t, 1, priceJobCount(t, db, orgB, rfqA), "org B's no-op staged no second price job")
}

// ---------------------------------------------------------------------------
// Drift, no-op and stage-table pins
// ---------------------------------------------------------------------------

// TestRfqRatesUndecodableCancels: PRICING without the extraction
// document is S2-contract drift — JobCancel, never a checkpoint.
func TestRfqRatesUndecodableCancels(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "rates-drift")
	rig := newRatesRig(t, db, liveFetcher(offerResult(), nil))
	rfq := seedPricedRFQ(t, db, org, `{"not_fields": true}`, "0.9000")

	err := rig.worker.Work(context.Background(), rateJob(org, uuid.New(), rfq, 1))
	assertJobCancel(t, err)
	assert.Empty(t, loadOffers(t, db, org, rfq))
	assert.Equal(t, 0, rig.fetcher.calls)
}

// TestRfqRatesTerminalNoOp: a job for an already-READY RFQ is a replay —
// no fetch, no row, no enqueue.
func TestRfqRatesTerminalNoOp(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "rates-noop")
	rig := newRatesRig(t, db, liveFetcher(offerResult(), nil))
	rfq := seedPricedRFQ(t, db, org, extractionDoc(owedFields()), "0.9000")
	setRFQStatus(t, db, org, rfq, "READY")

	require.NoError(t, rig.worker.Work(context.Background(), rateJob(org, uuid.New(), rfq, 1)))
	assert.Empty(t, loadOffers(t, db, org, rfq))
	assert.Equal(t, 0, rig.fetcher.calls)
	assert.Equal(t, 0, priceJobCount(t, db, org, rfq))
}

// TestRfqRatesTimeoutAndAttempts pins the spec-§3 stage table on the
// args and the worker: three attempts, ~15 seconds, the price queue.
func TestRfqRatesTimeoutAndAttempts(t *testing.T) {
	db := testenv.Open(t)
	worker := jobs.NewRfqRatesWorker(db, liveFetcher(nil, nil), func() time.Time { return pricedNow })
	assert.Equal(t, 15*time.Second, worker.Timeout(nil))
	assert.Equal(t, 3, jobs.RateArgs{}.InsertOpts().MaxAttempts)
	assert.Equal(t, "price", jobs.RateArgs{}.InsertOpts().Queue)
	assert.Equal(t, "rfq_fetch_rates", jobs.RateArgs{}.Kind())
}
