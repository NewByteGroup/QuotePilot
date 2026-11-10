package platform_test

// Middleware chain proofs (spec 2026-11-10-tenant-resolution-api sections 7
// and 9): the uniform-401 contract across every authentication failure mode,
// the byte-identical 404 for non-member and nonexistent org, the per-request
// membership re-check, the required X-Organization-Id header, RequireRole
// admission, and the accessors carrying the chain's values to the handler.
// The cookie and header names are written as string literals on purpose —
// they pin the wire contract so renaming a constant cannot drift the tests.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// probe is a chain-tail handler: reaching it records the request it saw (for
// context assertions) and answers 200. Rejection tests assert called stays
// false — proving the middleware stopped the request, not that it 200'd by
// accident.
type probe struct {
	called bool
	saw    *http.Request
}

func (p *probe) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.called = true
		p.saw = r
		w.WriteHeader(http.StatusOK)
	})
}

// mintAccessCookie signs a real access token and wraps it in the qp_access
// cookie a browser would send.
func mintAccessCookie(t *testing.T, cfg *platform.Config, userID, sessionID uuid.UUID) *http.Cookie {
	t.Helper()
	raw, err := cfg.SignAccess(userID, sessionID, time.Now())
	require.NoError(t, err)
	return &http.Cookie{Name: "qp_access", Value: raw}
}

// authedRequest builds a request carrying a valid qp_access cookie.
func authedRequest(t *testing.T, cfg *platform.Config, userID uuid.UUID, method, target string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.AddCookie(mintAccessCookie(t, cfg, userID, uuid.New()))
	return req
}

// panicQuerier is a Store Querier that fails the test on any call — backing
// ResolveTenant with it proves a request was answered before any SQL ran.
type panicQuerier struct{ t *testing.T }

var _ platform.Querier = panicQuerier{}

func (q panicQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	q.t.Fatal("unexpected SQL: this request must never reach the database")
	return nil
}

func (q panicQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	q.t.Fatal("unexpected SQL: this request must never reach the database")
	return nil, nil
}

// TestAuthenticateUniform401 proves spec section 7's uniform-failure
// contract: every bad-cookie mode — absent, garbage, forged, wrong-kid,
// expired — gets the same 401 with byte-identical bodies, the wrapped
// handler never runs, and (because the store behind ResolveTenant is a
// panicQuerier) the rejection happens before any tenant lookup.
func TestAuthenticateUniform401(t *testing.T) {
	cfg := jwtConfig()
	userID, sessionID := uuid.New(), uuid.New()
	now := time.Now()
	valid := accessClaims(userID, sessionID, now, now.Add(15*time.Minute))

	expired, err := cfg.SignAccess(userID, sessionID, now.Add(-time.Hour))
	require.NoError(t, err)
	validRaw, err := cfg.SignAccess(userID, sessionID, now)
	require.NoError(t, err)

	cases := []struct {
		name   string
		cookie *http.Cookie
	}{
		{"absent cookie", nil},
		{"empty value", &http.Cookie{Name: "qp_access"}},
		{"garbage value", &http.Cookie{Name: "qp_access", Value: "this.is.not-a-jwt"}},
		{"forged signature", &http.Cookie{Name: "qp_access", Value: signAny(t, jwt.SigningMethodHS256, "k1", []byte("attacker-secret"), valid)}},
		{"unknown kid", &http.Cookie{Name: "qp_access", Value: signAny(t, jwt.SigningMethodHS256, "ghost", []byte("secret-one"), valid)}},
		{"expired", &http.Cookie{Name: "qp_access", Value: expired}},
		{"valid token under wrong cookie name", &http.Cookie{Name: "qp_refresh", Value: validRaw}},
	}

	var reference []byte
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &probe{}
			store := platform.NewStore(panicQuerier{t})
			chain := platform.Authenticate(cfg)(platform.ResolveTenant(store)(p.handler()))
			req := httptest.NewRequest(http.MethodPost, "/tenant/route", nil)
			if tc.cookie != nil {
				req.AddCookie(tc.cookie)
			}
			rec := httptest.NewRecorder()
			chain.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.False(t, p.called, "the handler must not run on a rejected request")
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			assert.Equal(t, `{"error":"unauthenticated"}`, rec.Body.String())
			if reference == nil {
				reference = rec.Body.Bytes()
			}
			assert.Equal(t, reference, rec.Body.Bytes(),
				"%s must emit the same body bytes as every other failure mode", tc.name)
		})
	}
}

// TestAuthenticatePassesContext proves the happy path: sub and sid land on
// the request context for the accessors — and nothing else does, because
// org and role are ResolveTenant's job.
func TestAuthenticatePassesContext(t *testing.T) {
	cfg := jwtConfig()
	userID, sessionID := uuid.New(), uuid.New()
	p := &probe{}
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.AddCookie(mintAccessCookie(t, cfg, userID, sessionID))
	rec := httptest.NewRecorder()
	platform.Authenticate(cfg)(p.handler()).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, p.called)
	gotUser, ok := platform.UserID(p.saw.Context())
	require.True(t, ok, "Authenticate must store the sub claim")
	assert.Equal(t, userID, gotUser)
	gotSession, ok := platform.SessionID(p.saw.Context())
	require.True(t, ok, "Authenticate must store the sid claim")
	assert.Equal(t, sessionID, gotSession)
	_, ok = platform.OrgID(p.saw.Context())
	assert.False(t, ok, "Authenticate must not set an org")
	_, ok = platform.Role(p.saw.Context())
	assert.False(t, ok, "Authenticate must not set a role")
}

// TestResolveTenantUniform404 proves the spec section 7 security property
// end to end: a non-member of an org that exists and a request naming an org
// that does not get the same 404 — byte-identical, same lookup, same writer.
func TestResolveTenantUniform404(t *testing.T) {
	store := newStore(t)
	cfg := jwtConfig()

	orgID := uuid.New()
	testenv.SeedOrganization(t, orgID, "mw-nonmember")
	userID := seedUser(t, uniqueEmail("mw-nonmember"), "MW NonMember")
	// No membership row: userID is a non-member of an existing organization.

	bodies := map[string][]byte{}
	for _, tc := range []struct {
		name  string
		orgID uuid.UUID
	}{
		{"non-member of existing org", orgID},
		{"nonexistent org", uuid.New()},
	} {
		p := &probe{}
		req := authedRequest(t, cfg, userID, http.MethodGet, "/tenant/route")
		req.Header.Set("X-Organization-Id", tc.orgID.String())
		rec := httptest.NewRecorder()
		platform.Authenticate(cfg)(platform.ResolveTenant(store)(p.handler())).ServeHTTP(rec, req)

		assert.Equal(t, http.StatusNotFound, rec.Code, "%s must answer 404", tc.name)
		assert.False(t, p.called, "%s must not reach the handler", tc.name)
		bodies[tc.name] = rec.Body.Bytes()
	}
	assert.Equal(t, `{"error":"not_found"}`, string(bodies["non-member of existing org"]))
	assert.Equal(t, bodies["non-member of existing org"], bodies["nonexistent org"],
		"non-member and nonexistent org responses must be byte-identical")
}

// TestResolveTenantRechecksMembership proves the per-request check (ADR-005
// section 5): a member passes, the membership row is deleted as qp_migrator,
// and the next request with the same credentials gets the 404 — nothing was
// cached from the first request.
func TestResolveTenantRechecksMembership(t *testing.T) {
	store := newStore(t)
	cfg := jwtConfig()

	orgID := uuid.New()
	testenv.SeedOrganization(t, orgID, "mw-removed")
	userID := seedUser(t, uniqueEmail("mw-removed"), "MW Removed")
	seedMembership(t, orgID, userID, "PRICING_ANALYST")

	p := &probe{}
	serve := platform.Authenticate(cfg)(platform.ResolveTenant(store)(p.handler()))
	req := authedRequest(t, cfg, userID, http.MethodGet, "/tenant/route")
	req.Header.Set("X-Organization-Id", orgID.String())

	rec := httptest.NewRecorder()
	serve.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "the member must reach the handler")
	require.True(t, p.called)

	_, err := testenv.MigratorPool(t).Exec(context.Background(),
		`DELETE FROM app.organization_memberships WHERE organization_id = $1 AND user_id = $2`,
		orgID, userID)
	require.NoError(t, err)

	p.called = false
	rec = httptest.NewRecorder()
	serve.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code, "a removed member must fail on the next request")
	assert.Equal(t, `{"error":"not_found"}`, rec.Body.String())
	assert.False(t, p.called)
}

// TestResolveTenantHeaderRequired proves the spec section 7 400: an absent,
// empty or non-UUID X-Organization-Id on a tenant route is the same
// missing_organization body — answered before any lookup, which the
// panicQuerier-backed store guarantees (any SQL call fails the test).
func TestResolveTenantHeaderRequired(t *testing.T) {
	cfg := jwtConfig()
	userID := uuid.New()

	for _, tc := range []struct {
		name   string
		header string
		set    bool
	}{
		{"header absent", "", false},
		{"header empty", "", true},
		{"header not a uuid", "org-slug-not-uuid", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &probe{}
			store := platform.NewStore(panicQuerier{t})
			req := authedRequest(t, cfg, userID, http.MethodPost, "/tenant/route")
			if tc.set {
				req.Header.Set("X-Organization-Id", tc.header)
			}
			rec := httptest.NewRecorder()
			platform.Authenticate(cfg)(platform.ResolveTenant(store)(p.handler())).ServeHTTP(rec, req)

			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Equal(t, `{"error":"missing_organization"}`, rec.Body.String())
			assert.False(t, p.called)
		})
	}
}

// errQuerier is a Store Querier whose every call fails — the driver-error
// shape ResolveTenant must answer 500 to rather than leak into the uniform
// 404 path.
type errQuerier struct{}

var _ platform.Querier = errQuerier{}
var errDriver = errors.New("driver down")

func (errQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	return errRow{}
}

func (errQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errDriver
}

type errRow struct{}

func (errRow) Scan(...any) error { return errDriver }

// TestResolveTenantDriverError proves a get_membership driver failure maps
// to 500 {"error":"internal"} — a server fault, not org-existence information
// — and never reaches the handler.
func TestResolveTenantDriverError(t *testing.T) {
	p := &probe{}
	store := platform.NewStore(errQuerier{})
	cfg := jwtConfig()
	req := authedRequest(t, cfg, uuid.New(), http.MethodGet, "/tenant/route")
	req.Header.Set("X-Organization-Id", uuid.NewString())
	rec := httptest.NewRecorder()
	platform.Authenticate(cfg)(platform.ResolveTenant(store)(p.handler())).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, `{"error":"internal"}`, rec.Body.String())
	assert.False(t, p.called)
}

// ResolveTenant reached with a well-formed header but no authenticated user
// on the context answers the uniform 401 — before any lookup — rather than
// letting a bare request through.
func TestResolveTenantWithoutAuth(t *testing.T) {
	p := &probe{}
	store := platform.NewStore(panicQuerier{t})
	req := httptest.NewRequest(http.MethodPost, "/tenant/route", nil)
	req.Header.Set("X-Organization-Id", uuid.NewString())
	rec := httptest.NewRecorder()
	platform.ResolveTenant(store)(p.handler()).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, `{"error":"unauthenticated"}`, rec.Body.String())
	assert.False(t, p.called)
}

// TestRequireRoleWithoutContext proves RequireRole denies rather than fails
// open when no role exists on the context.
func TestRequireRoleWithoutContext(t *testing.T) {
	p := &probe{}
	rec := httptest.NewRecorder()
	platform.RequireRole(platform.RoleOwner)(p.handler()).ServeHTTP(
		rec, httptest.NewRequest(http.MethodPost, "/owner-only", nil))

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, `{"error":"forbidden"}`, rec.Body.String())
	assert.False(t, p.called)
}

// TestRequireRoleThroughChain covers spec section 9's authorization rows end
// to end: a PRICING_ANALYST on an OWNER-only route gets 403, the same
// analyst reaches the all-roles route (the /me/organization shape, spec
// section 8), and an OWNER reaches the OWNER route.
func TestRequireRoleThroughChain(t *testing.T) {
	store := newStore(t)
	cfg := jwtConfig()

	orgID := uuid.New()
	testenv.SeedOrganization(t, orgID, "mw-roles")
	analyst := seedUser(t, uniqueEmail("mw-analyst"), "MW Analyst")
	owner := seedUser(t, uniqueEmail("mw-owner"), "MW Owner")
	seedMembership(t, orgID, analyst, platform.RolePricingAnalyst)
	seedMembership(t, orgID, owner, platform.RoleOwner)

	okTail := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ownerRoute := platform.Authenticate(cfg)(platform.ResolveTenant(store)(
		platform.RequireRole(platform.RoleOwner)(okTail)))
	allRolesRoute := platform.Authenticate(cfg)(platform.ResolveTenant(store)(
		platform.RequireRole(platform.RoleOwner, platform.RoleAdmin, platform.RolePricingAnalyst)(okTail)))

	tenantReq := func(userID uuid.UUID) *http.Request {
		req := authedRequest(t, cfg, userID, http.MethodPost, "/tenant/route")
		req.Header.Set("X-Organization-Id", orgID.String())
		return req
	}
	cases := []struct {
		name   string
		chain  http.Handler
		req    *http.Request
		status int
	}{
		{"analyst denied on owner-only route", ownerRoute, tenantReq(analyst), http.StatusForbidden},
		{"owner admitted on owner-only route", ownerRoute, tenantReq(owner), http.StatusOK},
		{"analyst admitted on all-roles route", allRolesRoute, tenantReq(analyst), http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.chain.ServeHTTP(rec, tc.req)
			assert.Equal(t, tc.status, rec.Code)
			if tc.status == http.StatusForbidden {
				assert.Equal(t, `{"error":"forbidden"}`, rec.Body.String())
			}
		})
	}
}

// TestChainPopulatesContext proves the accessors surface everything the
// chain recorded — the whole point of the stack: sub/sid from the verified
// token, org and role from the live membership row.
func TestChainPopulatesContext(t *testing.T) {
	store := newStore(t)
	cfg := jwtConfig()

	orgID := uuid.New()
	testenv.SeedOrganization(t, orgID, "mw-ctx")
	userID := seedUser(t, uniqueEmail("mw-ctx"), "MW Ctx")
	seedMembership(t, orgID, userID, "PRICING_ANALYST")
	sessionID := uuid.New()

	p := &probe{}
	chain := platform.Authenticate(cfg)(platform.ResolveTenant(store)(
		platform.RequireRole(platform.RolePricingAnalyst)(p.handler())))
	req := httptest.NewRequest(http.MethodGet, "/me/organization", nil)
	req.AddCookie(mintAccessCookie(t, cfg, userID, sessionID))
	req.Header.Set("X-Organization-Id", orgID.String())
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, p.called)
	require.NotNil(t, p.saw)

	gotUser, ok := platform.UserID(p.saw.Context())
	require.True(t, ok)
	assert.Equal(t, userID, gotUser)
	gotSession, ok := platform.SessionID(p.saw.Context())
	require.True(t, ok)
	assert.Equal(t, sessionID, gotSession)
	gotOrg, ok := platform.OrgID(p.saw.Context())
	require.True(t, ok)
	assert.Equal(t, orgID, gotOrg)
	gotRole, ok := platform.Role(p.saw.Context())
	require.True(t, ok)
	assert.Equal(t, platform.RolePricingAnalyst, gotRole)
}

// CheckOrigin is ADR-005 section 4's Origin check: a cross-site unsafe
// request is denied with the middleware's own 403 body, safe methods and
// non-browser requests pass, and the configured WebURL survives the
// sibling-host comparison the stdlib performs.
func TestCheckOrigin(t *testing.T) {
	cfg := jwtConfig()
	cfg.WebURL = "https://web.quotepilot.test"
	p := &probe{}
	handler := platform.CheckOrigin(cfg)(p.handler())

	tests := []struct {
		name     string
		method   string
		headers  map[string]string
		wantCode int
		wantCall bool
	}{
		{"cross-site fetch on unsafe method is denied",
			http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden, false},
		{"cross-site fetch on safe method passes",
			http.MethodGet, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusOK, true},
		{"same-site without a trusted origin is still cross-origin",
			http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden, false},
		{"same-site fetch with the trusted WebURL origin passes",
			http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://web.quotepilot.test"}, http.StatusOK, true},
		{"trusted WebURL origin passes",
			http.MethodPost, map[string]string{"Origin": "https://web.quotepilot.test"}, http.StatusOK, true},
		{"untrusted origin denied",
			http.MethodPost, map[string]string{"Origin": "https://evil.test"}, http.StatusForbidden, false},
		{"no fetch or origin headers treated as non-browser",
			http.MethodPost, nil, http.StatusOK, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p.called = false
			req := httptest.NewRequest(tc.method, "/tenant/route", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			assert.Equal(t, tc.wantCode, rec.Code)
			assert.Equal(t, tc.wantCall, p.called)
			if tc.wantCode == http.StatusForbidden {
				assert.Equal(t, `{"error":"forbidden"}`, rec.Body.String())
			}
		})
	}
}

// A denied response that cannot reach the wire must not panic — the write
// failure is logged and dropped.
func TestWriteErrorWriteFailure(t *testing.T) {
	cfg := jwtConfig()
	cfg.WebURL = "https://web.quotepilot.test"
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	platform.CheckOrigin(cfg)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler must not run")
	})).ServeHTTP(failWriter{}, req)
}

type failWriter struct{}

func (failWriter) Header() http.Header       { return http.Header{} }
func (failWriter) WriteHeader(int)           {}
func (failWriter) Write([]byte) (int, error) { return 0, errDriver }
