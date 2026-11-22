package rfqs_test

// Real-Postgres integration tests for POST /v1/rfqs/{id}/reprocess (spec
// 2026-11-11 section 4 "Reprocess"): the endpoint runs inside InTenantTx
// under the real role model, so only the qp_app pool proves the RLS
// scope, the FOR UPDATE lock, the cleared reasons, the audit row and the
// in-transaction resume enqueue. Mounted exactly as cmd/server mounts
// it — Authenticate + ResolveTenant + RequireRole(OWNER, ADMIN,
// PRICING_ANALYST) around the strict surface.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/carrier"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/rfqs"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// rigNow is the frozen clock every rig reads — the interval gate and the
// S3a request-hash recompute both need determinism. The carrier fixture's
// ready_date sits past it.
var rigNow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// extractionOwed is a usable extraction whose S3a gate fires: FCL + FOB
// (OwesImportFreight) + non-BR→BR (derived IMPORT). equipment/quantity/
// ready_date complete the normalized request so the seeded checkpoint
// hash matches the recompute exactly.
var extractionOwed = []byte(`{"fields":{
	"modal":             {"value":"FCL"},
	"incoterm":          {"value":"FOB"},
	"origin_locode":     {"value":"DEHAM"},
	"dest_locode":       {"value":"BRSSZ"},
	"equipment_or_unit": {"value":"40HC"},
	"quantity":          {"value":"2"},
	"ready_date":        {"value":"2026-12-01"}
}}`)

// extractionNotOwed carries the required field set but stays off the S3a
// path (LCL is never owed — the mirror's carrier fetches are FCL-only).
var extractionNotOwed = []byte(`{"fields":{
	"modal":         {"value":"LCL"},
	"incoterm":      {"value":"FOB"},
	"origin_locode": {"value":"DEHAM"},
	"dest_locode":   {"value":"BRSSZ"},
	"ready_date":    {"value":"2026-12-01"}
}}`)

// owedHash is the carrier_offers.request_hash DecideResume recomputes for
// extractionOwed — built through the same carrier.RequestHash the worker
// pins, with the fields rateRequest normalizes (upper-cased equipment,
// integral quantity, the extracted ready_date).
func owedHash() string {
	return carrier.RequestHash(carrier.Request{
		OriginLOCODE:    "DEHAM",
		DestLOCODE:      "BRSSZ",
		EquipmentOrUnit: "40HC",
		Quantity:        2,
		ReadyDate:       time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
	})
}

// reprocessConfig is the rig's request-edge config — one JWT key at the
// 32-byte floor, the same shape approveConfig gives the approve rig.
func reprocessConfig() *platform.Config {
	return &platform.Config{
		WebURL:       "https://web.test",
		JWTKeys:      map[string]string{"k1": strings.Repeat("s", 32)},
		ActiveKID:    "k1",
		AccessTTL:    15 * time.Minute,
		RefreshTTL:   30 * 24 * time.Hour,
		LoginTTL:     15 * time.Minute,
		CookieSecure: true,
	}
}

// reprocessRig mounts the strict surface behind the production chain —
// Correlation and CheckOrigin sit outside it in cmd/server and are
// irrelevant to these assertions.
type reprocessRig struct {
	handler http.Handler
	cfg     *platform.Config
	db      *tenantdb.DB
}

// newReprocessRig builds the whole chain: auth.NewServer (which routes
// the reprocess path through the RfqReprocessor delegate) behind the
// three middlewares. The River client is insert-only — the staged job
// lands in river_job inside the tenant tx and just sits there.
// minInterval and the clock are the gate's two levers.
func newReprocessRig(t *testing.T, db *tenantdb.DB, minInterval time.Duration, now func() time.Time) *reprocessRig {
	t.Helper()
	cfg := reprocessConfig()
	store := platform.NewStore(db.Pool())
	inserter, err := river.NewClient(riverpgxv5.New(db.Pool()), &river.Config{Schema: "river"})
	require.NoError(t, err)
	endpoints := auth.NewEndpoints(cfg, store, store.InStorePgxTx, db,
		func(context.Context, pgx.Tx, uuid.UUID) error {
			t.Fatal("reprocess never stages mail")
			return nil
		},
		platform.NewLimiter(platform.LoginIPCapacity, platform.LimitWindow, time.Now),
		platform.NewLimiter(platform.LoginEmailCapacity, platform.LimitWindow, time.Now),
		time.Now)
	endpoints.Reprocessor = rfqs.NewEndpoints(db, inserter, minInterval, now)
	server := platform.Authenticate(cfg)(
		platform.ResolveTenant(store)(
			platform.RequireRole(platform.RoleOwner, platform.RoleAdmin, platform.RolePricingAnalyst)(
				auth.NewServer(endpoints))))
	return &reprocessRig{handler: server, cfg: cfg, db: db}
}

// postReprocess fires the request the review queue sends: cookie +
// tenant header, no body.
func (r *reprocessRig) postReprocess(t *testing.T, userID, orgID, rfqID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := r.cfg.SignAccess(userID, uuid.New(), time.Now())
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/v1/rfqs/"+rfqID.String()+"/reprocess", nil)
	req.AddCookie(&http.Cookie{Name: "qp_access", Value: raw})
	req.Header.Set("X-Organization-Id", orgID.String())
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	return rec
}

// seedUser inserts an app.users row as qp_migrator (Layer A is closed to
// qp_app) with a per-call unique email.
func seedUser(t *testing.T) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, testenv.MigratorPool(t).QueryRow(context.Background(),
		`INSERT INTO app.users (email, full_name) VALUES ($1, 'Reprocess User') RETURNING id`,
		"reprocess-"+uuid.NewString()+"@test.dev").Scan(&id))
	return id
}

// seedMembership links user and org with the given role, as qp_migrator.
func seedMembership(t *testing.T, orgID, userID uuid.UUID, role string) {
	t.Helper()
	_, err := testenv.MigratorPool(t).Exec(context.Background(),
		`INSERT INTO app.organization_memberships (organization_id, user_id, role) VALUES ($1, $2, $3::app.membership_role)`,
		orgID, userID, role)
	require.NoError(t, err)
}

// rfqSeed is one parked RFQ the way the pipeline leaves it: status +
// kind, the reason pair, and the S2 checkpoint columns.
type rfqSeed struct {
	status        string
	kind          string
	reviewReason  *string
	failureReason *string
	payload       []byte
	confidence    *string
}

// seedRfq inserts the RFQ inside the tenant tx; kind defaults to RFQ.
func seedRfq(t *testing.T, db *tenantdb.DB, org uuid.UUID, s rfqSeed) uuid.UUID {
	t.Helper()
	if s.kind == "" {
		s.kind = "RFQ"
	}
	var id uuid.UUID
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO app.rfq_requests
				(organization_id, rfq_number, resend_message_id, customer_email, subject,
				 kind, status, review_reason, failure_reason,
				 extracted_payload_json, extraction_confidence)
			VALUES ($1, (SELECT coalesce(max(rfq_number),0)+1 FROM app.rfq_requests WHERE organization_id=$1),
			        $2, 'buyer@test.dev', 'RFQ: 2x40HQ',
			        $3::app.rfq_kind, $4::app.rfq_status, $5, $6, $7::jsonb, $8)
			RETURNING id`,
			org, "resend-"+uuid.NewString(), s.kind, s.status,
			s.reviewReason, s.failureReason, s.payload, s.confidence).Scan(&id)
	}))
	return id
}

// seedDraftQuote inserts one DRAFT quote version inside the tenant tx;
// pdfKey nil leaves pdf_storage_key NULL — the render checkpoint.
func seedDraftQuote(t *testing.T, db *tenantdb.DB, org, rfq uuid.UUID, pdfKey *string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO app.quotes
				(organization_id, rfq_id, quote_number, version, status,
				 total_cost_brl, target_margin, final_price_brl,
				 overall_confidence, pdf_storage_key)
			VALUES ($1, $2, $3, 1, 'DRAFT', 100.00, 0.1500, 115.00, 0.9000, $4)
			RETURNING id`, org, rfq, "Q-"+uuid.NewString()[:8], pdfKey).Scan(&id)
	}))
	return id
}

// seedCarrierCheckpoint writes the S3a outcome row — NO_OFFER with NULL
// payload and carrier is a legal completed-fetch row (the CHECK only
// forces payload on OFFER_FOUND).
func seedCarrierCheckpoint(t *testing.T, db *tenantdb.DB, org, rfq uuid.UUID, hash string) {
	t.Helper()
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.carrier_offers
				(organization_id, rfq_id, carrier, outcome, source, request_hash, fetched_at)
			VALUES ($1, $2, 'CMA_CGM', 'NO_OFFER', 'LIVE', $3, now())`, org, rfq, hash)
		return err
	}))
}

// seedReprocessedAudit writes the rfq.reprocessed row the interval gate
// reads back, at a controlled created_at.
func seedReprocessedAudit(t *testing.T, db *tenantdb.DB, org, rfq, actor uuid.UUID, at time.Time) {
	t.Helper()
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.tenant_audit_logs
				(organization_id, actor_user_id, action, entity_type, entity_id, metadata_json, created_at)
			VALUES ($1, $2, 'rfq.reprocessed', 'rfq', $3, '{}'::jsonb, $4)`,
			org, actor, rfq, at)
		return err
	}))
}

// rfqColumns reads back the reprocess stamp inside the tenant tx.
type rfqColumns struct {
	Status        string
	ReviewReason  *string
	FailureReason *string
}

func loadRfq(t *testing.T, db *tenantdb.DB, org, rfq uuid.UUID) rfqColumns {
	t.Helper()
	var r rfqColumns
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT status::text, review_reason, failure_reason
			FROM app.rfq_requests WHERE id = $1`, rfq).
			Scan(&r.Status, &r.ReviewReason, &r.FailureReason)
	}))
	return r
}

// stagedJobs lists the river_job kinds staged for one RFQ — river_job is
// qp_app-readable and carries no RLS, so the read scopes by args.
func stagedJobs(t *testing.T, db *tenantdb.DB, rfq uuid.UUID) []string {
	t.Helper()
	rows, err := db.Pool().Query(context.Background(),
		`SELECT kind FROM river.river_job WHERE args->>'rfq_id' = $1 ORDER BY kind`, rfq.String())
	require.NoError(t, err)
	defer rows.Close()
	kinds := []string{}
	for rows.Next() {
		var k string
		require.NoError(t, rows.Scan(&k))
		kinds = append(kinds, k)
	}
	require.NoError(t, rows.Err())
	return kinds
}

// stagedJobArg reads one args member off the staged job for assertions
// the kind list cannot carry (rfq_render's quote_id).
func stagedJobArg(t *testing.T, db *tenantdb.DB, kind string, rfq uuid.UUID, key string) string {
	t.Helper()
	var v string
	require.NoError(t, db.Pool().QueryRow(context.Background(),
		`SELECT args->>$3 FROM river.river_job WHERE kind = $1 AND args->>'rfq_id' = $2`,
		kind, rfq.String(), key).Scan(&v))
	return v
}

// reprocessedAudits counts rfq.reprocessed rows for an RFQ inside the
// tenant tx; the metadata of the newest row rides along.
func reprocessedAudits(t *testing.T, db *tenantdb.DB, org, rfq uuid.UUID) (int, map[string]any, *uuid.UUID) {
	t.Helper()
	var n int
	var meta map[string]any
	var actor *uuid.UUID
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM app.tenant_audit_logs
			 WHERE action = 'rfq.reprocessed' AND entity_id = $1`, rfq).Scan(&n); err != nil {
			return err
		}
		var raw []byte
		err := tx.QueryRow(ctx,
			`SELECT metadata_json::text, actor_user_id FROM app.tenant_audit_logs
			 WHERE action = 'rfq.reprocessed' AND entity_id = $1
			 ORDER BY created_at DESC LIMIT 1`, rfq).Scan(&raw, &actor)
		if err == pgx.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, &meta)
	}))
	return n, meta, actor
}

// strptr is the literal helper for reason seeds.
func strptr(s string) *string { return &s }

// ---------------------------------------------------------------------------
// Happy paths — one per resumed stage (spec Reprocess checkpoint walk)
// ---------------------------------------------------------------------------

// TestReprocessResumeExtractFromFailed: a dead-lettered FAILED shell has
// no usable extraction — the walk lands on S2, the RFQ goes back to
// PARSING and the rfq_extract job is staged in the same tx. The audit
// row carries the actor, the cleared failure_reason and the stage.
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

// ---------------------------------------------------------------------------
// Min-interval gate (spec Reprocess: REPROCESS_MIN_INTERVAL)
// ---------------------------------------------------------------------------

// TestReprocessRateLimited: a previous rfq.reprocessed younger than the
// interval answers 429 + Retry-After with the remaining seconds — and
// writes nothing: no job, no reason clearing, no second audit row.
func TestReprocessRateLimited(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "reproc-429")
	user := seedUser(t)
	seedMembership(t, org, user, platform.RolePricingAnalyst)
	rig := newReprocessRig(t, db, 60*time.Second, func() time.Time { return rigNow })
	rfq := seedRfq(t, db, org, rfqSeed{
		status:       "NEEDS_REVIEW",
		reviewReason: strptr("LOW_CONFIDENCE"),
		payload:      extractionNotOwed,
		confidence:   strptr("0.6200"),
	})
	seedReprocessedAudit(t, db, org, rfq, user, rigNow.Add(-30*time.Second))

	rec := rig.postReprocess(t, user, org, rfq)
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Equal(t, "30", rec.Header().Get("Retry-After"),
		"the remaining wait off the audit row's created_at")
	assert.JSONEq(t, `{"error":"rate_limited"}`, rec.Body.String())

	r := loadRfq(t, db, org, rfq)
	assert.Equal(t, "NEEDS_REVIEW", r.Status, "a refused reprocess writes nothing")
	require.NotNil(t, r.ReviewReason)
	assert.Equal(t, "LOW_CONFIDENCE", *r.ReviewReason)
	assert.Empty(t, stagedJobs(t, db, rfq), "nothing is enqueued inside the interval")
	n, _, _ := reprocessedAudits(t, db, org, rfq)
	assert.Equal(t, 1, n, "only the seeded audit row — the refusal never writes")
}

// TestReprocessIntervalExpired: the audit row older than the floor lets
// the reprocess through — the gate is a per-RFQ rate limit, not a lock.
func TestReprocessIntervalExpired(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "reproc-429-old")
	user := seedUser(t)
	seedMembership(t, org, user, platform.RolePricingAnalyst)
	rig := newReprocessRig(t, db, 60*time.Second, func() time.Time { return rigNow })
	rfq := seedRfq(t, db, org, rfqSeed{
		status:       "NEEDS_REVIEW",
		reviewReason: strptr("LOW_CONFIDENCE"),
		payload:      extractionNotOwed,
		confidence:   strptr("0.6200"),
	})
	seedReprocessedAudit(t, db, org, rfq, user, rigNow.Add(-61*time.Second))

	rec := rig.postReprocess(t, user, org, rfq)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	n, _, _ := reprocessedAudits(t, db, org, rfq)
	assert.Equal(t, 2, n, "the accepted reprocess writes its own audit row")
}

// ---------------------------------------------------------------------------
// Refusals — status gate, tenancy, auth
// ---------------------------------------------------------------------------

// TestReprocessConflictStatuses: every non-parked status is a 409 with
// rfq_not_parked plus the current app.rfq_status in the body — IGNORED is
// intentionally terminal (spec Reprocess).
func TestReprocessConflictStatuses(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "reproc-409")
	user := seedUser(t)
	seedMembership(t, org, user, platform.RolePricingAnalyst)
	rig := newReprocessRig(t, db, 60*time.Second, func() time.Time { return rigNow })
	for _, status := range []string{"RECEIVED", "PARSING", "PRICING", "READY", "APPROVED_SENT", "IGNORED"} {
		t.Run(status, func(t *testing.T) {
			rfq := seedRfq(t, db, org, rfqSeed{status: status})
			rec := rig.postReprocess(t, user, org, rfq)
			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			var body authapi.ReprocessConflict
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, authapi.ErrorCodeRfqNotParked, body.Error)
			assert.Equal(t, status, body.Status, "the current status rides the 409 body")
			assert.Empty(t, stagedJobs(t, db, rfq))
			r := loadRfq(t, db, org, rfq)
			assert.Equal(t, status, r.Status, "a 409 writes nothing")
		})
	}
}

// TestReprocessUnknownAndCrossTenant404 proves the two 404 cases: an id
// no tenant owns and an id another tenant owns both answer the uniform
// not_found — RLS makes the cross-tenant row invisible to the FOR UPDATE.
func TestReprocessUnknownAndCrossTenant404(t *testing.T) {
	db := testenv.Open(t)
	orgA, orgB := uuid.New(), uuid.New()
	testenv.SeedOrganization(t, orgA, "reproc-404a")
	testenv.SeedOrganization(t, orgB, "reproc-404b")
	user := seedUser(t)
	seedMembership(t, orgB, user, platform.RolePricingAnalyst)
	rig := newReprocessRig(t, db, 60*time.Second, func() time.Time { return rigNow })
	foreign := seedRfq(t, db, orgA, rfqSeed{status: "NEEDS_REVIEW"})

	rec := rig.postReprocess(t, user, orgB, uuid.New())
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"error":"not_found"}`, rec.Body.String())

	rec = rig.postReprocess(t, user, orgB, foreign)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String(),
		"cross-tenant RFQ ids are invisible — the same uniform 404")

	r := loadRfq(t, db, orgA, foreign)
	assert.Equal(t, "NEEDS_REVIEW", r.Status, "a 404 never touches the row")
}

// TestReprocessRoles covers the reprocess matrix (spec Reprocess): every
// membership role may reprocess — membership_role only carries the
// three — so 403 is declared but unreachable today.
func TestReprocessRoles(t *testing.T) {
	db := testenv.Open(t)
	for _, role := range []string{platform.RoleOwner, platform.RoleAdmin, platform.RolePricingAnalyst} {
		t.Run(role, func(t *testing.T) {
			org := uuid.New()
			testenv.SeedOrganization(t, org, "reproc-role-"+strings.ToLower(role))
			user := seedUser(t)
			seedMembership(t, org, user, role)
			rig := newReprocessRig(t, db, 60*time.Second, func() time.Time { return rigNow })
			rfq := seedRfq(t, db, org, rfqSeed{status: "FAILED", failureReason: strptr("boom")})
			rec := rig.postReprocess(t, user, org, rfq)
			assert.Equal(t, http.StatusAccepted, rec.Code, "%s may reprocess: %s", role, rec.Body.String())
		})
	}
}

// TestReprocessUnauthenticated: no qp_access cookie → the middleware's
// 401 before any tenant work; a memberless user (membership revoked) →
// 404.
func TestReprocessUnauthenticated(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "reproc-unauth")
	user := seedUser(t)
	seedMembership(t, org, user, platform.RoleOwner)
	rig := newReprocessRig(t, db, 60*time.Second, func() time.Time { return rigNow })
	rfq := seedRfq(t, db, org, rfqSeed{status: "NEEDS_REVIEW"})

	req := httptest.NewRequest(http.MethodPost, "/v1/rfqs/"+rfq.String()+"/reprocess", nil)
	req.Header.Set("X-Organization-Id", org.String())
	rec := httptest.NewRecorder()
	rig.handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	// Valid JWT but no membership in this org → ResolveTenant's 404.
	stranger := seedUser(t)
	rec = rig.postReprocess(t, stranger, org, rfq)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	r := loadRfq(t, db, org, rfq)
	assert.Equal(t, "NEEDS_REVIEW", r.Status)
}
