package quotes_test

// Real-Postgres integration tests for POST /quotes/{id}/approve (spec
// 2026-11-11 section 4 "Approval"): the endpoint runs inside InTenantTx
// under the real role model, so only the qp_app pool proves the RLS scope,
// the FOR UPDATE stamp, the audit row and the in-transaction send enqueue.
// Mounted exactly as cmd/server mounts it — Authenticate + ResolveTenant +
// RequireRole(OWNER, ADMIN, PRICING_ANALYST) around the strict surface.

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
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/quotes"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// approveConfig is the rig's request-edge config — one JWT key at the
// 32-byte floor, the same shape sessionConfig gives the auth rigs.
func approveConfig() *platform.Config {
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

// approveRig mounts the strict surface behind the production chain —
// Correlation sits outside it in cmd/server and is irrelevant here.
type approveRig struct {
	handler http.Handler
	cfg     *platform.Config
	db      *tenantdb.DB
}

// newApproveRig builds the whole chain: auth.NewServer (which routes the
// approve path through the QuoteApprover delegate) behind the three
// middlewares. The River client is insert-only — the staged quote_send job
// lands in river_job inside the tenant tx and just sits there.
func newApproveRig(t *testing.T, db *tenantdb.DB) *approveRig {
	t.Helper()
	cfg := approveConfig()
	store := platform.NewStore(db.Pool())
	inserter, err := river.NewClient(riverpgxv5.New(db.Pool()), &river.Config{Schema: "river"})
	require.NoError(t, err)
	endpoints := auth.NewEndpoints(cfg, store, store.InStorePgxTx, db,
		func(context.Context, pgx.Tx, uuid.UUID) error {
			t.Fatal("approve never stages mail")
			return nil
		},
		platform.NewLimiter(platform.LoginIPCapacity, platform.LimitWindow, time.Now),
		platform.NewLimiter(platform.LoginEmailCapacity, platform.LimitWindow, time.Now),
		time.Now)
	endpoints.Approver = quotes.NewEndpoints(db, inserter)
	server := platform.Authenticate(cfg)(
		platform.ResolveTenant(store)(
			platform.RequireRole(platform.RoleOwner, platform.RoleAdmin, platform.RolePricingAnalyst)(
				auth.NewServer(endpoints))))
	return &approveRig{handler: server, cfg: cfg, db: db}
}

// postApprove fires the request the web app sends: cookie + tenant header.
func (r *approveRig) postApprove(t *testing.T, userID, orgID, quoteID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := r.cfg.SignAccess(userID, uuid.New(), time.Now())
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/quotes/"+quoteID.String()+"/approve", nil)
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
		`INSERT INTO app.users (email, full_name) VALUES ($1, 'Approve User') RETURNING id`,
		"approve-"+uuid.NewString()+"@test.dev").Scan(&id))
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

// seedRfq inserts a minimal rfq_requests row inside the tenant tx — the
// FK parent a quote needs.
func seedRfq(t *testing.T, db *tenantdb.DB, org uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO app.rfq_requests
				(organization_id, rfq_number, resend_message_id, customer_email, subject, status)
			VALUES ($1, (SELECT coalesce(max(rfq_number),0)+1 FROM app.rfq_requests WHERE organization_id=$1),
			        $2, 'buyer@test.dev', 'RFQ: 2x40HQ', 'READY')
			RETURNING id`, org, "resend-"+uuid.NewString()).Scan(&id)
	}))
	return id
}

// pricingJSON marshals the engine output the worker persisted — cost and
// sell steer NegativeMargin().
func pricingJSON(t *testing.T, costBRL, sellBRL int64) []byte {
	t.Helper()
	raw, err := json.Marshal(pricing.Output{
		Status:        pricing.StatusComplete,
		TotalCostBRL:  decimal.NewFromInt(costBRL),
		FinalPriceBRL: decimal.NewFromInt(sellBRL),
	})
	require.NoError(t, err)
	return raw
}

// seedQuote inserts one DRAFT quote version inside the tenant tx with a
// unique quote_number; status and the pricing blob are the levers each
// refusal path pulls.
func seedQuote(t *testing.T, db *tenantdb.DB, org, rfq uuid.UUID, status string, pricing []byte) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO app.quotes
				(organization_id, rfq_id, quote_number, version, status,
				 total_cost_brl, target_margin, final_price_brl,
				 overall_confidence, pricing_result_json)
			VALUES ($1, $2, $3, 1, $4::app.quote_status, 100.00, 0.1500, 115.00, 0.9000, $5)
			RETURNING id`, org, rfq, "Q-"+uuid.NewString()[:8], status, pricing).Scan(&id)
	}))
	return id
}

// quoteColumns reads back the approval stamp inside the tenant tx.
type quoteColumns struct {
	Status      string
	ApprovedAt  *time.Time
	ApprovedBy  *uuid.UUID
	ApprovedVia *string
}

func loadQuote(t *testing.T, db *tenantdb.DB, org, quote uuid.UUID) quoteColumns {
	t.Helper()
	var q quoteColumns
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT status::text, approved_at, approved_by_user_id, approved_via
			FROM app.quotes WHERE id = $1`, quote).
			Scan(&q.Status, &q.ApprovedAt, &q.ApprovedBy, &q.ApprovedVia)
	}))
	return q
}

// auditRows counts tenant_audit_logs rows for an action inside the tenant
// tx; metadataJSON returns the first row's metadata when fetch is true.
func auditRows(t *testing.T, db *tenantdb.DB, org uuid.UUID, action string) (int, map[string]any) {
	t.Helper()
	var n int
	var meta map[string]any
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM app.tenant_audit_logs WHERE action = $1`, action).Scan(&n); err != nil {
			return err
		}
		var raw []byte
		err := tx.QueryRow(ctx,
			`SELECT metadata_json::text FROM app.tenant_audit_logs WHERE action = $1 LIMIT 1`, action).Scan(&raw)
		if err != nil && err != pgx.ErrNoRows {
			return err
		}
		if len(raw) > 0 {
			return json.Unmarshal(raw, &meta)
		}
		return nil
	}))
	return n, meta
}

// sendJobs counts staged quote_send jobs for one quote — river_job is
// qp_app-readable and carries no RLS, so the count is scoped by args.
func sendJobs(t *testing.T, db *tenantdb.DB, quote uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, db.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM river.river_job WHERE kind = 'quote_send' AND args->>'quote_id' = $1`,
		quote.String()).Scan(&n))
	return n
}

// TestApproveHappyPath is the spec-Approval sequence end to end: one tx
// writes the stamp, the audit row and the quote_send job together.
func TestApproveHappyPath(t *testing.T) {
	db := testenv.Open(t)
	rig := newApproveRig(t, db)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "approve-happy")
	user := seedUser(t)
	seedMembership(t, org, user, platform.RolePricingAnalyst)
	quote := seedQuote(t, db, org, seedRfq(t, db, org), "DRAFT", pricingJSON(t, 100, 115))

	rec := rig.postApprove(t, user, org, quote)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body authapi.ApproveQuoteResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, quote.String(), body.ID)
	assert.Equal(t, authapi.DRAFT, body.Status, "approval leaves the version DRAFT — the send worker flips it")
	assert.Equal(t, user.String(), body.ApprovedByUserID)
	assert.False(t, body.ApprovedAt.IsZero())

	q := loadQuote(t, db, org, quote)
	assert.Equal(t, "DRAFT", q.Status)
	require.NotNil(t, q.ApprovedAt)
	require.NotNil(t, q.ApprovedBy)
	assert.Equal(t, user, *q.ApprovedBy)
	require.NotNil(t, q.ApprovedVia)
	assert.Equal(t, "api", *q.ApprovedVia)

	n, meta := auditRows(t, db, org, "quote.approved")
	assert.Equal(t, 1, n, "one quote.approved audit row")
	assert.NotEmpty(t, meta["quote_number"], "audit metadata carries the quote number")
	assert.EqualValues(t, 1, meta["version"])

	assert.Equal(t, 1, sendJobs(t, db, quote), "the S5 job was enqueued inside the same tx")
	var args map[string]string
	require.NoError(t, db.Pool().QueryRow(context.Background(),
		`SELECT args FROM river.river_job WHERE kind = 'quote_send' AND args->>'quote_id' = $1`,
		quote.String()).Scan(&args))
	assert.Equal(t, org.String(), args["organization_id"])
}

// TestApproveIdempotentNoOp: a re-approve of an approved version returns
// the same body, never re-stamps, and the (kind, quote_id) uniqueness
// collapses the second enqueue — one send job per version (spec Approval).
func TestApproveIdempotentNoOp(t *testing.T) {
	db := testenv.Open(t)
	rig := newApproveRig(t, db)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "approve-idem")
	user := seedUser(t)
	seedMembership(t, org, user, platform.RoleOwner)
	quote := seedQuote(t, db, org, seedRfq(t, db, org), "DRAFT", pricingJSON(t, 100, 115))

	first := rig.postApprove(t, user, org, quote)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	second := rig.postApprove(t, user, org, quote)
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	assert.Equal(t, first.Body.String(), second.Body.String(),
		"re-approve returns the stored stamp — idempotent")

	n, _ := auditRows(t, db, org, "quote.approved")
	assert.Equal(t, 1, n, "the no-op writes no second audit row")
	assert.Equal(t, 1, sendJobs(t, db, quote), "the unique job insert collapses the second enqueue")
}

// TestApproveSupersededRefused: a retired version is a hard 409 —
// recalculation approves only the newest version (rule 9).
func TestApproveSupersededRefused(t *testing.T) {
	db := testenv.Open(t)
	rig := newApproveRig(t, db)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "approve-sup")
	user := seedUser(t)
	seedMembership(t, org, user, platform.RoleAdmin)
	quote := seedQuote(t, db, org, seedRfq(t, db, org), "SUPERSEDED", pricingJSON(t, 100, 115))

	rec := rig.postApprove(t, user, org, quote)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"error":"quote_superseded"}`, rec.Body.String())

	q := loadQuote(t, db, org, quote)
	assert.Nil(t, q.ApprovedAt, "a refused approve writes nothing")
	assert.Equal(t, 0, sendJobs(t, db, quote))
}

// TestApproveNegativeMarginRefused is the pilot-meeting hard block: a
// below-cost price earns 409 + the quote.approve_refused audit row — the
// approval side of the defense-in-depth check S5 repeats.
func TestApproveNegativeMarginRefused(t *testing.T) {
	db := testenv.Open(t)
	rig := newApproveRig(t, db)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "approve-neg")
	user := seedUser(t)
	seedMembership(t, org, user, platform.RolePricingAnalyst)
	quote := seedQuote(t, db, org, seedRfq(t, db, org), "DRAFT", pricingJSON(t, 100, 90))

	rec := rig.postApprove(t, user, org, quote)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"error":"negative_margin"}`, rec.Body.String())

	q := loadQuote(t, db, org, quote)
	assert.Nil(t, q.ApprovedAt)
	n, meta := auditRows(t, db, org, "quote.approve_refused")
	assert.Equal(t, 1, n, "the refusal is audited — approve_refused")
	assert.Equal(t, "negative_margin", meta["reason"])
	assert.Equal(t, 0, sendJobs(t, db, quote))
}

// TestApproveUnknownAndCrossTenant404 proves the two 404 cases: an id no
// tenant owns and an id another tenant owns both answer the uniform
// not_found — RLS makes the cross-tenant row invisible to the FOR UPDATE.
func TestApproveUnknownAndCrossTenant404(t *testing.T) {
	db := testenv.Open(t)
	rig := newApproveRig(t, db)
	orgA, orgB := uuid.New(), uuid.New()
	testenv.SeedOrganization(t, orgA, "approve-404a")
	testenv.SeedOrganization(t, orgB, "approve-404b")
	user := seedUser(t)
	seedMembership(t, orgB, user, platform.RoleOwner)
	foreignQuote := seedQuote(t, db, orgA, seedRfq(t, db, orgA), "DRAFT", pricingJSON(t, 100, 115))

	rec := rig.postApprove(t, user, orgB, uuid.New())
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"error":"not_found"}`, rec.Body.String())

	rec = rig.postApprove(t, user, orgB, foreignQuote)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String(),
		"cross-tenant quote ids are invisible — the same uniform 404")

	q := loadQuote(t, db, orgA, foreignQuote)
	assert.Nil(t, q.ApprovedAt, "a 404 never stamps")
}

// TestApproveRoles covers the approval matrix (spec Approval): every
// membership role in the enum may approve — membership_role only carries
// the three — so 403 is declared but unreachable today. Each role gets its
// own org/user pair so the asserts stay independent.
func TestApproveRoles(t *testing.T) {
	db := testenv.Open(t)
	rig := newApproveRig(t, db)
	for _, role := range []string{platform.RoleOwner, platform.RoleAdmin, platform.RolePricingAnalyst} {
		t.Run(role, func(t *testing.T) {
			org := uuid.New()
			testenv.SeedOrganization(t, org, "approve-role-"+strings.ToLower(role))
			user := seedUser(t)
			seedMembership(t, org, user, role)
			quote := seedQuote(t, db, org, seedRfq(t, db, org), "DRAFT", pricingJSON(t, 100, 115))
			rec := rig.postApprove(t, user, org, quote)
			assert.Equal(t, http.StatusOK, rec.Code, "%s may approve: %s", role, rec.Body.String())
		})
	}
}

// TestApproveUnauthenticated: no qp_access cookie → the middleware's 401
// before any tenant work; a memberless user (membership revoked) → 404.
func TestApproveUnauthenticated(t *testing.T) {
	db := testenv.Open(t)
	rig := newApproveRig(t, db)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "approve-unauth")
	user := seedUser(t)
	seedMembership(t, org, user, platform.RoleOwner)
	quote := seedQuote(t, db, org, seedRfq(t, db, org), "DRAFT", pricingJSON(t, 100, 115))

	req := httptest.NewRequest(http.MethodPost, "/quotes/"+quote.String()+"/approve", nil)
	req.Header.Set("X-Organization-Id", org.String())
	rec := httptest.NewRecorder()
	rig.handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	// Valid JWT but no membership in this org → ResolveTenant's 404.
	stranger := seedUser(t)
	rec = rig.postApprove(t, stranger, org, quote)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	q := loadQuote(t, db, org, quote)
	assert.Nil(t, q.ApprovedAt)
}
