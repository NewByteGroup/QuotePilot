package webapi_test

// Shared rig for the web v1 read-surface proofs plus the pure-mapper unit
// tests that need no database. The integration side mirrors the
// cmd/server mounts: one auth strict surface carrying the webapi delegate,
// Authenticate on the session pair, ResolveTenant added for the two
// org-scoped reads — every request travels cookie → JWT → membership →
// InTenantReadTx against real Postgres (ADR-008: no mocked database for
// tenant proofs).

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
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/webapi"
)

// webConfig mirrors cmd/server's request-edge config: one JWT key at the
// 32-byte floor, spec TTLs, Secure cookies on.
func webConfig() *platform.Config {
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

// rig bundles the mounted surface with the test's clock and store.
type rig struct {
	mux   *http.ServeMux
	db    *tenantdb.DB
	store *platform.Store
	cfg   *platform.Config
	now   time.Time
}

// newRig builds the production mount chain: the strict surface with the
// webapi delegate on the same mounts cmd/server wires (CheckOrigin is
// omitted — httptest requests carry no Origin and would pass anyway).
func newRig(t *testing.T, now time.Time) *rig {
	t.Helper()
	db := testenv.Open(t)
	store := platform.NewStore(db.Pool())
	cfg := webConfig()
	endpoints := auth.NewEndpoints(cfg, store, store.InStorePgxTx, db,
		func(context.Context, pgx.Tx, uuid.UUID) error { return nil },
		platform.NewLimiter(30, time.Hour, time.Now),
		platform.NewLimiter(5, time.Hour, time.Now),
		time.Now)
	endpoints.Web = webapi.NewEndpoints(db, store, cfg, func() time.Time { return now })
	srv := auth.NewServer(endpoints)
	mux := http.NewServeMux()
	mux.Handle("GET /v1/session", platform.Authenticate(cfg)(webapi.OrgHints(srv)))
	mux.Handle("POST /v1/session/organization", platform.Authenticate(cfg)(srv))
	mux.Handle("GET /v1/inbox/quotes", platform.Authenticate(cfg)(platform.ResolveTenant(store)(srv)))
	mux.Handle("GET /v1/quotes/{quoteId}", platform.Authenticate(cfg)(platform.ResolveTenant(store)(srv)))
	return &rig{mux: mux, db: db, store: store, cfg: cfg, now: now}
}

// accessCookie mints a real qp_access for the user — the same transport
// Authenticate verifies.
func (r *rig) accessCookie(t *testing.T, userID uuid.UUID) *http.Cookie {
	t.Helper()
	raw, err := r.cfg.SignAccess(userID, uuid.New(), time.Now())
	require.NoError(t, err)
	return &http.Cookie{Name: "qp_access", Value: raw}
}

// get serves an authenticated GET; orgCookie/header add the tenant
// transports on demand.
func (r *rig) get(t *testing.T, userID uuid.UUID, path, orgHeader string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(r.accessCookie(t, userID))
	for _, c := range cookies {
		req.AddCookie(c)
	}
	if orgHeader != "" {
		req.Header.Set("X-Organization-Id", orgHeader)
	}
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec
}

// post serves an authenticated JSON POST.
func (r *rig) post(t *testing.T, userID uuid.UUID, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(r.accessCookie(t, userID))
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec
}

// seedWebUser inserts the Layer A user row as qp_migrator. The email is
// prefix-uuid@test.dev — the dev database is shared, so a literal address
// collides with the previous run's row.
func seedWebUser(t *testing.T, prefix, fullName string) (uuid.UUID, string) {
	t.Helper()
	email := prefix + "-" + uuid.NewString()[:8] + "@test.dev"
	var id uuid.UUID
	require.NoError(t, testenv.MigratorPool(t).QueryRow(context.Background(),
		`INSERT INTO app.users (email, full_name) VALUES ($1, NULLIF($2, '')) RETURNING id`,
		email, fullName).Scan(&id))
	return id, email
}

// seedWebMembership links user and org with an explicit created_at — the
// session's first-membership fallback orders on it, so tests pin distinct
// ages rather than racing two now() defaults.
func seedWebMembership(t *testing.T, orgID, userID uuid.UUID, role string, createdAt time.Time) {
	t.Helper()
	_, err := testenv.MigratorPool(t).Exec(context.Background(),
		`INSERT INTO app.organization_memberships (organization_id, user_id, role, created_at)
		 VALUES ($1, $2, $3::app.membership_role, $4)`,
		orgID, userID, role, createdAt)
	require.NoError(t, err)
}

// rfqSeed is the column set one inbox/detail RFQ carries.
type rfqSeed struct {
	number        int
	customerEmail string
	customerName  *string
	subject       string
	status        string
	kind          string
	receivedAt    time.Time
	rawText       *string
	extraction    []byte
	confidence    *string // numeric text, nil = NULL
}

// seedRfq inserts one rfq_requests row inside the tenant tx with an
// explicit rfq_number — tests pin the number rather than allocate it.
func seedRfq(t *testing.T, db *tenantdb.DB, org uuid.UUID, o rfqSeed) uuid.UUID {
	t.Helper()
	kind, status := o.kind, o.status
	if kind == "" {
		kind = "RFQ"
	}
	if status == "" {
		status = "RECEIVED"
	}
	var id uuid.UUID
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO app.rfq_requests
				(organization_id, rfq_number, resend_message_id, customer_email, customer_name,
				 subject, status, kind, received_at, raw_text_body, extracted_payload_json,
				 extraction_confidence)
			VALUES ($1, $2, $3, $4, $5, $6, $7::app.rfq_status, $8::app.rfq_kind, $9, $10, $11, $12)
			RETURNING id`,
			org, o.number, "resend-"+uuid.NewString(), o.customerEmail, o.customerName,
			o.subject, status, kind, o.receivedAt, o.rawText, o.extraction, o.confidence).Scan(&id)
	}))
	return id
}

// seedQuote inserts one quote version against the RFQ — the detail's
// margin triplet rides the columns while the section/fx payload rides
// pricing_result_json, exactly as the engine persists it.
func seedQuote(t *testing.T, db *tenantdb.DB, org, rfqID uuid.UUID, margin, cost, price, conf string, pricingJSON []byte, approvedAt *time.Time) {
	t.Helper()
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.quotes
				(organization_id, rfq_id, quote_number, version, status,
				 total_cost_brl, target_margin, final_price_brl, overall_confidence,
				 pricing_result_json, approved_at)
			VALUES ($1, $2, $3, 1, 'DRAFT', $4, $5, $6, $7, $8, $9)`,
			org, rfqID, "T-"+uuid.NewString()[:8], cost, margin, price, conf, pricingJSON, approvedAt)
		return err
	}))
}

// pricingFixture is the pinned pricing.Output the detail test audits:
// one USD freight group, one BRL fees group, one applied carrier rate.
func pricingFixture(t *testing.T) []byte {
	t.Helper()
	out := pricing.Output{
		Status: pricing.StatusComplete,
		Groups: []pricing.GroupTotal{
			{Group: pricing.GroupInternationalFreight, Currency: "USD",
				Cost: decimalReq("1800"), Sell: decimalReq("2070")},
			{Group: pricing.GroupLocalFees, Currency: "BRL",
				Cost: decimalReq("500"), Sell: decimalReq("575")},
		},
		Lines: []pricing.Line{
			{Group: pricing.GroupInternationalFreight, ChargeCode: "OFE",
				Description: "Ocean freight", Currency: "USD", SellOriginal: decimalReq("2070")},
			{Group: pricing.GroupLocalFees, ChargeCode: "THC",
				Description: "Terminal handling", Currency: "BRL", SellOriginal: decimalReq("575")},
		},
		TotalCostBRL:  decimalReq("9800.00"),
		FinalPriceBRL: decimalReq("11270.00"),
		FXApplied: []pricing.FXApplied{{
			Source: "CARRIER", Currency: "USD",
			Date: time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC),
			Rate: decimalReq("5.32"), Spread: decimalReq("0.05"),
		}},
	}
	b, err := json.Marshal(out)
	require.NoError(t, err)
	return b
}

// decimalReq parses a fixed fixture literal; a bad string is a test bug,
// so panic is honest.
func decimalReq(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic("bad fixture decimal " + s)
	}
	return d
}
