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
	"fmt"
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
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/carrier"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/rfqs"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// rigNow is the frozen clock every rig reads — the interval gate needs
// determinism. The S3a request-hash recompute no longer reads it: its
// ready_date fallback is the RFQ's received_at (Devin Review round 3),
// which the seeds pin explicitly. The carrier fixture's ready_date sits
// past it.
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

// extractionOwedNoReady is extractionOwed minus the ready_date field —
// the shape whose S3a run pins request_hash with the received_at-day
// fallback (Devin Review round 3: the fallback used to be the run-day
// clock, which is exactly the day-drift a next-day reprocess must never
// recompute).
var extractionOwedNoReady = []byte(`{"fields":{
	"modal":             {"value":"FCL"},
	"incoterm":          {"value":"FOB"},
	"origin_locode":     {"value":"DEHAM"},
	"dest_locode":       {"value":"BRSSZ"},
	"equipment_or_unit": {"value":"40HC"},
	"quantity":          {"value":"2"}
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

// owedHashOn is owedHash for a fallback-day ready_date — the hash S3a
// pins for extractionOwedNoReady when the RFQ's received_at falls on
// `day` (quoteDateField truncates the fallback to the day).
func owedHashOn(day time.Time) string {
	return carrier.RequestHash(carrier.Request{
		OriginLOCODE:    "DEHAM",
		DestLOCODE:      "BRSSZ",
		EquipmentOrUnit: "40HC",
		Quantity:        2,
		ReadyDate:       time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC),
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
	handler  http.Handler
	cfg      *platform.Config
	db       *tenantdb.DB
	inserter *river.Client[pgx.Tx]
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
	return &reprocessRig{handler: server, cfg: cfg, db: db, inserter: inserter}
}

// postTimeout bounds one reprocess request — the busy tests' hang
// guard. Under a plain-FOR-UPDATE regression the endpoint's quote probe
// would wait on a held row lock forever, so the request ctx deadline
// turns the hang into a failed response the assert can report.
const postTimeout = 5 * time.Second

// postReprocess fires the request the review queue sends: cookie +
// tenant header, no body.
func (r *reprocessRig) postReprocess(t *testing.T, userID, orgID, rfqID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	rec, err := r.tryReprocess(userID, orgID, rfqID)
	require.NoError(t, err)
	return rec
}

// tryReprocess is postReprocess minus the test-boundary require — the
// form a require.Eventually poll calls (its condition runs off the test
// goroutine, where require's FailNow is illegal). The request ctx
// carries postTimeout so a lock-waiting regression fails instead of
// hanging the suite.
func (r *reprocessRig) tryReprocess(userID, orgID, rfqID uuid.UUID) (*httptest.ResponseRecorder, error) {
	raw, err := r.cfg.SignAccess(userID, uuid.New(), time.Now())
	if err != nil {
		return nil, fmt.Errorf("rfqs_test: sign access token: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), postTimeout)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/rfqs/"+rfqID.String()+"/reprocess", nil).
		WithContext(ctx)
	req.AddCookie(&http.Cookie{Name: "qp_access", Value: raw})
	req.Header.Set("X-Organization-Id", orgID.String())
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	return rec, nil
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
// kind, the reason pair, and the S2 checkpoint columns. resendID pins the
// receipt link the ingest-shell tests need; textBody/htmlBody stand in
// for the raw bodies a live row always carries (nil keeps them NULL —
// the dead-letter-shell signature).
type rfqSeed struct {
	status        string
	kind          string
	reviewReason  *string
	failureReason *string
	resendID      string
	textBody      *string
	htmlBody      *string
	payload       []byte
	confidence    *string
	// receivedAt pins received_at — the S3a hash fallback's source — so
	// the day-drift tests control which day "the RFQ arrived"; zero
	// falls back to the column default now().
	receivedAt time.Time
}

// seedRfq inserts the RFQ inside the tenant tx; kind defaults to RFQ.
func seedRfq(t *testing.T, db *tenantdb.DB, org uuid.UUID, s rfqSeed) uuid.UUID {
	t.Helper()
	if s.kind == "" {
		s.kind = "RFQ"
	}
	if s.resendID == "" {
		s.resendID = "resend-" + uuid.NewString()
	}
	var receivedAt any
	if !s.receivedAt.IsZero() {
		receivedAt = s.receivedAt
	}
	var id uuid.UUID
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO app.rfq_requests
				(organization_id, rfq_number, resend_message_id, customer_email, subject,
				 kind, status, review_reason, failure_reason,
				 raw_text_body, raw_html_body,
				 extracted_payload_json, extraction_confidence, received_at)
			VALUES ($1, (SELECT coalesce(max(rfq_number),0)+1 FROM app.rfq_requests WHERE organization_id=$1),
			        $2, 'buyer@test.dev', 'RFQ: 2x40HQ',
			        $3::app.rfq_kind, $4::app.rfq_status, $5, $6, $7, $8, $9::jsonb, $10,
			        coalesce($11::timestamptz, now()))
			RETURNING id`,
			org, s.resendID, s.kind, s.status,
			s.reviewReason, s.failureReason, s.textBody, s.htmlBody,
			s.payload, s.confidence, receivedAt).Scan(&id)
	}))
	return id
}

// seedReceipt inserts one inbound_messages row inside the tenant tx — the
// receipt an ingest shell's resend_message_id resolves to.
func seedReceipt(t *testing.T, db *tenantdb.DB, org uuid.UUID, resendID string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO app.inbound_messages (organization_id, resend_message_id, subject, received_at)
			VALUES ($1, $2, 'RFQ: 2x40HQ', now()) RETURNING id`, org, resendID).Scan(&id)
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

// seedAttachment writes one rfq_attachments row — the marker that keeps an
// attachment-only ingest out of the dead-letter-shell signature (the shell
// never reaches storeAttachments).
func seedAttachment(t *testing.T, db *tenantdb.DB, org, rfq uuid.UUID) {
	t.Helper()
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.rfq_attachments (organization_id, rfq_id, storage_key, filename, mime_type, size_bytes)
			VALUES ($1, $2, $3, 'rates.pdf', 'application/pdf', 4)`,
			org, rfq, "tenants/"+org.String()+"/rfqs/"+rfq.String()+"/a1-rates.pdf")
		return err
	}))
}

// seedCarrierCheckpoint writes the S3a outcome row — NO_OFFER with NULL
// payload and carrier is a legal completed-fetch row (the CHECK only
// forces payload on OFFER_FOUND).
func seedCarrierCheckpoint(t *testing.T, db *tenantdb.DB, org, rfq uuid.UUID, hash string) {
	t.Helper()
	seedCarrierOutcome(t, db, org, rfq, "NO_OFFER", nil, hash)
}

// seedCarrierOutcome generalizes the checkpoint seed to any outcome —
// OFFER_FOUND must carry offer_payload_json (the CHECK's biconditional),
// every other outcome must not.
func seedCarrierOutcome(t *testing.T, db *tenantdb.DB, org, rfq uuid.UUID, outcome string, payload []byte, hash string) {
	t.Helper()
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.carrier_offers
				(organization_id, rfq_id, carrier, outcome, source, request_hash, offer_payload_json, fetched_at)
			VALUES ($1, $2, 'CMA_CGM', $3, 'LIVE', $4, $5::jsonb, now())`,
			org, rfq, outcome, hash, payload)
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
	return stagedJobsBy(t, db, "rfq_id", rfq.String())
}

// stagedJobsBy is stagedJobs generalized to any args member — ingest jobs
// are receipt-keyed (inbound_message_id), not rfq-keyed.
func stagedJobsBy(t *testing.T, db *tenantdb.DB, argKey, argVal string) []string {
	t.Helper()
	rows, err := db.Pool().Query(context.Background(),
		`SELECT kind FROM river.river_job WHERE args->>$1 = $2 ORDER BY kind, id`, argKey, argVal)
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
		textBody:     strptr("please quote"),
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
		textBody:     strptr("please quote"),
		payload:      extractionNotOwed,
		confidence:   strptr("0.6200"),
	})
	seedReprocessedAudit(t, db, org, rfq, user, rigNow.Add(-61*time.Second))

	rec := rig.postReprocess(t, user, org, rfq)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	n, _, _ := reprocessedAudits(t, db, org, rfq)
	assert.Equal(t, 2, n, "the accepted reprocess writes its own audit row")
}

// TestReprocessRateLimitedBeatsStatusGate pins the Devin Review round-3
// gate order: the interval gate runs BEFORE the status gate. A previous
// reprocess left its audit row 30 s ago; meanwhile the resumed render
// completed, so the RFQ sits at READY with a rendered DRAFT — a shape
// the status gate would 409. Inside the interval the answer is still
// 429 + Retry-After, unconditionally: the spec's "a second reprocess
// inside the interval is rate-limited" never depends on status (and the
// refusal never waits on the render's quote lock to find out).
func TestReprocessRateLimitedBeatsStatusGate(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "reproc-429-first")
	user := seedUser(t)
	seedMembership(t, org, user, platform.RolePricingAnalyst)
	rig := newReprocessRig(t, db, 60*time.Second, func() time.Time { return rigNow })
	rfq := seedRfq(t, db, org, rfqSeed{
		status:     "READY",
		textBody:   strptr("please quote"),
		payload:    extractionNotOwed,
		confidence: strptr("0.9100"),
	})
	pdf := "tenants/" + org.String() + "/quotes/x_v1.pdf"
	seedDraftQuote(t, db, org, rfq, &pdf)
	seedReprocessedAudit(t, db, org, rfq, user, rigNow.Add(-30*time.Second))

	rec := rig.postReprocess(t, user, org, rfq)
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Equal(t, "30", rec.Header().Get("Retry-After"))
	assert.JSONEq(t, `{"error":"rate_limited"}`, rec.Body.String())
	assert.Empty(t, stagedJobs(t, db, rfq), "a 429 enqueues nothing")
	n, _, _ := reprocessedAudits(t, db, org, rfq)
	assert.Equal(t, 1, n, "only the seeded audit row — the refusal never writes")
}
