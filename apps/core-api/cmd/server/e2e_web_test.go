package main

// Real-Postgres end-to-end proofs of the exact HTTP surface the web app
// consumes (the inbox-to-approval browser flow): the production wiring —
// buildEndpoints, the post-build delegate assignments run() performs and
// buildMux's full middleware chain — served on a real listener and driven
// by a jar'd http.Client so qp_access/qp_org transport exactly as the
// browser carries them. Seeding uses qp_migrator for the Layer A rows and
// InTenantTx as qp_app for the domain rows — the same split every
// integration suite keeps (qp_app must never write Layer A). Skipped
// under -short like the rest of the real-Postgres suite.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/quotes"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/rfqs"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/webapi"
)

// e2eConfig is testConfig plus the fields the login path reads: RefreshTTL
// sizes the auth_sessions row and the qp_refresh MaxAge, LoginTTL the
// staged magic link, CookieSecure the production flag — the jar returns
// Secure cookies to a loopback http URL (RFC 6265bis's trustworthy-origin
// rule), so production flags stay honest here.
func e2eConfig() *platform.Config {
	return &platform.Config{
		WebURL:       "https://app.example.test",
		JWTKeys:      map[string]string{"dev": strings.Repeat("k", 40)},
		ActiveKID:    "dev",
		AccessTTL:    15 * time.Minute,
		RefreshTTL:   30 * 24 * time.Hour,
		LoginTTL:     15 * time.Minute,
		CookieSecure: true,
	}
}

// e2eRig is the served production mux plus the qp_app pool seeds run
// against — a real listener, not ServeHTTP shortcuts, so Transport,
// RemoteAddr and cookie semantics all behave like the deployment.
type e2eRig struct {
	url string
	db  *tenantdb.DB
	cfg *platform.Config
}

// newE2ERig wires the surface exactly as run() does: the store over the
// qp_app pool, buildEndpoints with the production enqueueAuthMail adapter,
// the Approver/Web/Reprocessor post-build assignments, auth.NewServer and
// buildMux. The insert-only River client stands in for the started one —
// jobs land in river.river_job inside the request's tenant tx and sit
// there for the assertions instead of being drained mid-test.
func newE2ERig(t *testing.T) *e2eRig {
	t.Helper()
	db := testenv.Open(t)
	store := platform.NewStore(db.Pool())
	cfg := e2eConfig()
	inserter := insertOnlyRiverClient(t, db.Pool())
	endpoints := buildEndpoints(cfg, db, store, enqueueAuthMail(inserter))
	endpoints.Approver = quotes.NewEndpoints(db, inserter)
	endpoints.Web = webapi.NewEndpoints(db, store, cfg, time.Now)
	endpoints.Reprocessor = rfqs.NewEndpoints(db, inserter, reprocessMinInterval(), time.Now)
	srv := httptest.NewServer(buildMux(cfg, store, auth.NewServer(endpoints), nil))
	t.Cleanup(srv.Close)
	return &e2eRig{url: srv.URL, db: db, cfg: cfg}
}

// newE2EClient is one browser session: a jar'd client whose cookies
// persist across requests — one per signed-in user so a second tenant
// never shares the first's transports.
func newE2EClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	return &http.Client{Jar: jar}
}

// do fires one request through the caller's client. Every POST carries
// the trusted WEB_URL Origin — what the web app emits — so CheckOrigin
// judges the browser path rather than the non-browser pass.
func (r *e2eRig) do(t *testing.T, c *http.Client, method, path, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, r.url+path, rdr)
	require.NoError(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if method == http.MethodPost {
		req.Header.Set("Origin", r.cfg.WebURL)
	}
	resp, err := c.Do(req)
	require.NoError(t, err)
	return resp
}

// doBare fires a headerless request — the non-browser caller CheckOrigin
// explicitly lets through (its same-origin POSTs carry no Origin).
func (r *e2eRig) doBare(t *testing.T, c *http.Client, method, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, r.url+path, nil)
	require.NoError(t, err)
	resp, err := c.Do(req)
	require.NoError(t, err)
	return resp
}

// jarHas reports whether the client's jar holds a named cookie for the rig
// origin — the proof a Set-Cookie survived onto the follow-up requests.
func (r *e2eRig) jarHas(t *testing.T, c *http.Client, name string) bool {
	t.Helper()
	u, err := url.Parse(r.url)
	require.NoError(t, err)
	return cookieNamed(c.Jar.Cookies(u), name) != nil
}

// readJSON asserts the status — the raw body rides the failure message so
// a wrong code shows what the server actually said — then decodes the
// shape the web client consumes.
func readJSON[T any](t *testing.T, resp *http.Response, want int) T {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, want, resp.StatusCode, "body: %s", raw)
	var out T
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

// cookieNamed pulls one cookie off a response's Set-Cookie set; nil means
// the route never wrote it.
func cookieNamed(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// seedE2EUser inserts the Layer A triple — user row, password credential,
// membership — through qp_migrator, the only role that may write Layer A.
// The hash lands via the same UPDATE the password suite uses.
func seedE2EUser(t *testing.T, pool *pgxpool.Pool, org uuid.UUID, role, prefix, password string) (uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()
	email := prefix + "-" + uuid.NewString()[:8] + "@test.dev"
	var id uuid.UUID
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO app.users (email, full_name) VALUES ($1, $2) RETURNING id`,
		email, "E2E "+prefix).Scan(&id))
	_, err := pool.Exec(ctx,
		`UPDATE app.users SET password_hash = $1 WHERE id = $2`,
		testenv.PasswordHashPHC(t, password), id)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO app.organization_memberships (organization_id, user_id, role)
		VALUES ($1, $2, $3::app.membership_role)`, org, id, role)
	require.NoError(t, err)
	return id, email
}

// seedE2ERfq inserts one READY rfq_requests row as qp_app inside the
// tenant tx — the status the inbox rolls up as ready_to_send — and
// returns the RFQ id, which doubles as the /v1/quotes/{id} path value.
func seedE2ERfq(t *testing.T, db *tenantdb.DB, org uuid.UUID, number int) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO app.rfq_requests
				(organization_id, rfq_number, resend_message_id, customer_email, customer_name,
				 subject, status, kind, received_at, raw_text_body)
			VALUES ($1, $2, $3, $4, 'E2E Customer', 'E2E freight', 'READY', 'RFQ', $5, $6)
			RETURNING id`,
			org, number, "resend-"+uuid.NewString(), "e2e-cust@test.dev",
			time.Now().Add(-time.Hour), "Please quote 2x40HC Santos to Hamburg.").Scan(&id)
	}))
	return id
}

// seedE2EQuote inserts one DRAFT quote version and returns app.quotes.id —
// the id /quotes/{id}/approve locks, NOT the RFQ id the detail route
// takes. The pricing blob sells above cost so the margin gate passes.
func seedE2EQuote(t *testing.T, db *tenantdb.DB, org, rfq uuid.UUID) uuid.UUID {
	t.Helper()
	out, err := json.Marshal(pricing.Output{
		Status:        pricing.StatusComplete,
		TotalCostBRL:  decimal.NewFromInt(100),
		FinalPriceBRL: decimal.NewFromInt(115),
	})
	require.NoError(t, err)
	var id uuid.UUID
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO app.quotes
				(organization_id, rfq_id, quote_number, version, status,
				 total_cost_brl, target_margin, final_price_brl,
				 overall_confidence, pricing_result_json)
			VALUES ($1, $2, $3, 1, 'DRAFT', 100.00, 0.1500, 115.00, 0.9000, $4)
			RETURNING id`, org, rfq, "Q-"+uuid.NewString()[:8], out).Scan(&id)
	}))
	return id
}

// e2eLogin posts the credential login: asserts the switcher body's
// identity, the cookie pair's pinned attributes and the jar carrying
// qp_access onward, then returns the body for membership asserts.
func (r *e2eRig) e2eLogin(t *testing.T, c *http.Client, email, password string) authapi.SessionResponse {
	t.Helper()
	resp := r.do(t, c, http.MethodPost, "/auth/login",
		`{"email":"`+email+`","password":"`+password+`"}`)
	body := readJSON[authapi.SessionResponse](t, resp, http.StatusOK)
	access := cookieNamed(resp.Cookies(), "qp_access")
	require.NotNil(t, access, "login must set qp_access")
	assert.True(t, access.HttpOnly)
	assert.Equal(t, "/", access.Path)
	assert.Equal(t, http.SameSiteLaxMode, access.SameSite)
	assert.True(t, access.Secure)
	refresh := cookieNamed(resp.Cookies(), "qp_refresh")
	require.NotNil(t, refresh, "login must set qp_refresh")
	assert.Equal(t, "/auth", refresh.Path)
	assert.Equal(t, http.SameSiteStrictMode, refresh.SameSite)
	require.True(t, r.jarHas(t, c, "qp_access"), "the jar must carry qp_access onward")
	return body
}

// e2eAssertBootstrap runs the two bootstrap reads the web app fires after
// login — /me (flattened profile + switcher list) and /v1/session (active
// org + user block) — asserting the fields the client renders.
func (r *e2eRig) e2eAssertBootstrap(t *testing.T, c *http.Client, userID uuid.UUID, email string, org uuid.UUID) {
	t.Helper()
	me := readJSON[authapi.MeResponse](t,
		r.do(t, c, http.MethodGet, "/me", ""), http.StatusOK)
	assert.Equal(t, userID.String(), me.ID)
	assert.Equal(t, email, me.Email)
	require.Len(t, me.Memberships, 1)
	assert.Equal(t, org.String(), me.Memberships[0].OrganizationID)
	assert.Equal(t, authapi.PRICINGANALYST, me.Memberships[0].Role)
	sess := readJSON[authapi.Session](t,
		r.do(t, c, http.MethodGet, "/v1/session", ""), http.StatusOK)
	assert.Equal(t, org, uuid.UUID(sess.Organization.ID))
	require.Len(t, sess.Organizations, 1)
	assert.Equal(t, authapi.Operator, sess.User.Role,
		"PRICING_ANALYST rolls onto the web's two-value role as operator")
	assert.Equal(t, 1, sess.User.OrganizationCount)
}

// e2eStampOrgCookie runs the session bootstrap so the jar receives qp_org
// — the bootstrap's own org-stamp — for callers that only need the tenant
// transport, not the payload asserts.
func (r *e2eRig) e2eStampOrgCookie(t *testing.T, c *http.Client) {
	t.Helper()
	readJSON[authapi.Session](t,
		r.do(t, c, http.MethodGet, "/v1/session", ""), http.StatusOK)
	require.True(t, r.jarHas(t, c, "qp_org"), "the bootstrap must stamp qp_org")
}

// e2eSwitchOrg posts the org switch and asserts the Session answer plus
// the qp_org write — the transport the org-scoped reads then resolve on.
func (r *e2eRig) e2eSwitchOrg(t *testing.T, c *http.Client, org uuid.UUID) {
	t.Helper()
	resp := r.do(t, c, http.MethodPost, "/v1/session/organization",
		`{"organizationId":"`+org.String()+`"}`)
	switched := readJSON[authapi.Session](t, resp, http.StatusOK)
	assert.Equal(t, org, uuid.UUID(switched.Organization.ID))
	orgCookie := cookieNamed(resp.Cookies(), "qp_org")
	require.NotNil(t, orgCookie, "the switch must write qp_org")
	assert.Equal(t, org.String(), orgCookie.Value)
	assert.True(t, orgCookie.HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, orgCookie.SameSite)
	require.True(t, r.jarHas(t, c, "qp_org"), "the jar must carry qp_org onward")
}

// e2eAssertInbox reads the inbox and asserts the seeded RFQ is the single
// ready_to_send row — the fields the web client's list renders.
func (r *e2eRig) e2eAssertInbox(t *testing.T, c *http.Client, rfq uuid.UUID, number int) {
	t.Helper()
	page := readJSON[authapi.InboxQuotePage](t,
		r.do(t, c, http.MethodGet, "/v1/inbox/quotes", ""), http.StatusOK)
	require.Len(t, page.Items, 1)
	assert.Equal(t, rfq, uuid.UUID(page.Items[0].ID))
	assert.Equal(t, number, page.Items[0].RfqNumber)
	assert.Equal(t, authapi.ReadyToSend, page.Items[0].Status)
	assert.Equal(t, "E2E Customer", page.Items[0].CustomerName)
	assert.Equal(t, 1, page.StatusCounts.ReadyToSend)
}

// e2eAssertInboxEmpty reads the inbox and asserts nothing surfaced — the
// cross-tenant leak check for a tenant with no rows of its own.
func (r *e2eRig) e2eAssertInboxEmpty(t *testing.T, c *http.Client) {
	t.Helper()
	page := readJSON[authapi.InboxQuotePage](t,
		r.do(t, c, http.MethodGet, "/v1/inbox/quotes", ""), http.StatusOK)
	assert.Empty(t, page.Items, "another tenant's RFQ must not leak into the inbox")
	assert.Zero(t, page.StatusCounts.All)
}

// e2eAssertDetail reads the audit page and asserts the quote member and
// the pricing block the web client's split-screen decodes.
func (r *e2eRig) e2eAssertDetail(t *testing.T, c *http.Client, rfq uuid.UUID) {
	t.Helper()
	detail := readJSON[authapi.QuoteDetail](t,
		r.do(t, c, http.MethodGet, "/v1/quotes/"+rfq.String(), ""), http.StatusOK)
	assert.Equal(t, rfq, uuid.UUID(detail.Quote.ID))
	assert.Equal(t, "E2E Customer", detail.Quote.CustomerName)
	assert.Equal(t, "e2e-cust@test.dev", string(detail.Email.From))
	require.NotNil(t, detail.Pricing, "a priced quote must carry its composition")
	assert.Equal(t, authapi.Money{Amount: "100", Currency: "BRL"}, detail.Pricing.TotalCost)
	assert.Equal(t, "15.00", detail.Pricing.MarginPercent)
}

// e2eAssertDetail404 asserts the audit route answers a foreign RFQ id
// with the uniform not_found — indistinguishable from an unknown one.
func (r *e2eRig) e2eAssertDetail404(t *testing.T, c *http.Client, rfq uuid.UUID) {
	t.Helper()
	resp := r.do(t, c, http.MethodGet, "/v1/quotes/"+rfq.String(), "")
	body := readJSON[authapi.Error](t, resp, http.StatusNotFound)
	assert.Equal(t, authapi.ErrorCodeNotFound, body.Error,
		"a cross-tenant RFQ id is indistinguishable from an unknown one")
}

// e2eApproveQuote posts the approval and asserts the stored-stamp body
// plus the S5 job the one-tx enqueue leaves queued in river_job — then
// registers the cleanup that removes that row, the convention
// enqueue_test.go's insert proof keeps (the queue ends as it began).
func (r *e2eRig) e2eApproveQuote(t *testing.T, c *http.Client, quote, user uuid.UUID) {
	t.Helper()
	resp := r.do(t, c, http.MethodPost, "/quotes/"+quote.String()+"/approve", "")
	approval := readJSON[authapi.ApproveQuoteResponse](t, resp, http.StatusOK)
	assert.Equal(t, quote.String(), approval.ID)
	assert.Equal(t, authapi.DRAFT, approval.Status,
		"approval leaves the version DRAFT — the send worker owns the flip")
	assert.Equal(t, user.String(), approval.ApprovedByUserID)
	assert.False(t, approval.ApprovedAt.IsZero())
	var staged int
	require.NoError(t, r.db.Pool().QueryRow(context.Background(), `
		SELECT count(*) FROM river.river_job
		WHERE kind = 'quote_send' AND args->>'quote_id' = $1`, quote.String()).Scan(&staged))
	assert.Equal(t, 1, staged, "the approve tx must land the S5 job in river_job")
	t.Cleanup(func() {
		_, err := r.db.Pool().Exec(context.Background(),
			`DELETE FROM river.river_job WHERE kind = 'quote_send' AND args->>'quote_id' = $1`,
			quote.String())
		assert.NoError(t, err, "the staged river_job row must be removable as qp_app")
	})
}

// e2eAssertUnauthenticated fires one cookieless request and asserts the
// middleware's uniform {"error":"unauthenticated"} — the fail-closed
// contract the browser's login redirect depends on.
func (r *e2eRig) e2eAssertUnauthenticated(t *testing.T, c *http.Client, method, path string) {
	t.Helper()
	resp := r.do(t, c, method, path, "")
	body := readJSON[authapi.Error](t, resp, http.StatusUnauthorized)
	assert.Equal(t, authapi.ErrorCodeUnauthenticated, body.Error, "%s %s", method, path)
}

// TestE2EWebHappyPath walks the web app's bootstrap-to-approval sequence
// against the real wiring, in order: password login → /me → session
// bootstrap → org switch → inbox → audit detail → approve. Each helper
// asserts the status plus the fields the web client consumes — the point
// is the wire contract the browser decodes, not handler-level shortcuts.
// The jar carries qp_access after login and qp_org after the switch, so
// the org-scoped steps run on cookie transport alone, exactly as the
// browser does.
func TestE2EWebHappyPath(t *testing.T) {
	r := newE2ERig(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "e2e-happy")
	userID, email := seedE2EUser(t, testenv.MigratorPool(t), org,
		platform.RolePricingAnalyst, "e2e-happy", "s3cret-pilot")
	rfqID := seedE2ERfq(t, r.db, org, 7001)
	quoteID := seedE2EQuote(t, r.db, org, rfqID)
	client := newE2EClient(t)

	login := r.e2eLogin(t, client, email, "s3cret-pilot")
	assert.Equal(t, userID.String(), login.Profile.ID)
	assert.Equal(t, email, login.Profile.Email)
	require.Len(t, login.Memberships, 1)
	assert.Equal(t, org.String(), login.Memberships[0].OrganizationID)

	r.e2eAssertBootstrap(t, client, userID, email, org)
	r.e2eSwitchOrg(t, client, org)
	r.e2eAssertInbox(t, client, rfqID, 7001)
	r.e2eAssertDetail(t, client, rfqID)
	r.e2eApproveQuote(t, client, quoteID, userID)
}

// TestE2EWebTenantIsolation drives the same browser flow for a second
// tenant: after B's own login and bootstrap, org A's seeded RFQ is absent
// from B's inbox and B's detail read of A's id answers the uniform 404 —
// RLS makes the cross-tenant row invisible, never a different error.
func TestE2EWebTenantIsolation(t *testing.T) {
	r := newE2ERig(t)
	orgA, orgB := uuid.New(), uuid.New()
	testenv.SeedOrganization(t, orgA, "e2e-iso-a")
	testenv.SeedOrganization(t, orgB, "e2e-iso-b")
	migrator := testenv.MigratorPool(t)
	seedE2EUser(t, migrator, orgA, platform.RoleAdmin, "e2e-iso-a", "pw-a")
	_, emailB := seedE2EUser(t, migrator, orgB, platform.RoleAdmin, "e2e-iso-b", "pw-b")
	rfqA := seedE2ERfq(t, r.db, orgA, 8001)
	seedE2EQuote(t, r.db, orgA, rfqA)

	client := newE2EClient(t)
	r.e2eLogin(t, client, emailB, "pw-b")
	r.e2eStampOrgCookie(t, client)
	r.e2eAssertInboxEmpty(t, client)
	r.e2eAssertDetail404(t, client, rfqA)
}

// TestE2EWebUnauthenticated pins the uniform 401 on every protected route:
// a cookieless client gets the middleware's {"error":"unauthenticated"}
// before any handler runs — reads and the approve write alike.
func TestE2EWebUnauthenticated(t *testing.T) {
	r := newE2ERig(t)
	bare := &http.Client{} // no jar: nothing can be carried
	for _, path := range []string{
		"/me",
		"/v1/session",
		"/v1/inbox/quotes",
		"/v1/quotes/" + uuid.NewString(),
	} {
		r.e2eAssertUnauthenticated(t, bare, http.MethodGet, path)
	}
	// CheckOrigin lets the Origin-less non-browser POST through —
	// Authenticate stops it at the same uniform 401 as the reads.
	resp := r.doBare(t, bare, http.MethodPost, "/quotes/"+uuid.NewString()+"/approve")
	body := readJSON[authapi.Error](t, resp, http.StatusUnauthorized)
	assert.Equal(t, authapi.ErrorCodeUnauthenticated, body.Error)
}
