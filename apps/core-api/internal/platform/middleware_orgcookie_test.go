package platform_test

// The qp_org cookie fallback (migration 0009's web read surface): the
// explicit X-Organization-Id header still wins for API clients, the cookie
// resolves a browser request that cannot set one, and a malformed cookie
// dies as the same missing_organization 400 an absent hint gets —
// membership itself is re-checked unchanged on every request.
// Cookie and header names stay string literals on purpose: they pin the
// wire contract so renaming a constant cannot drift the tests.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// cookieRequest builds an authenticated request that also carries qp_org.
func orgCookieRequest(t *testing.T, cfg *platform.Config, userID uuid.UUID, cookieVal string) *http.Request {
	t.Helper()
	req := authedRequest(t, cfg, userID, http.MethodGet, "/tenant/route")
	req.AddCookie(&http.Cookie{Name: "qp_org", Value: cookieVal})
	return req
}

// TestResolveTenantCookieFallback proves the cookie transport end to end:
// no header at all, the qp_org value resolves through the same live
// get_membership check, and the handler sees the cookie org and role.
func TestResolveTenantCookieFallback(t *testing.T) {
	store := newStore(t)
	cfg := jwtConfig()

	orgID := uuid.New()
	testenv.SeedOrganization(t, orgID, "mw-cookie")
	userID := seedUser(t, uniqueEmail("mw-cookie"), "MW Cookie")
	seedMembership(t, orgID, userID, platform.RoleAdmin)

	p := &probe{}
	chain := platform.Authenticate(cfg)(platform.ResolveTenant(store)(p.handler()))
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, orgCookieRequest(t, cfg, userID, orgID.String()))

	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, p.called, "the cookie-resolved member must reach the handler")
	gotOrg, ok := platform.OrgID(p.saw.Context())
	require.True(t, ok)
	assert.Equal(t, orgID, gotOrg)
	gotRole, ok := platform.Role(p.saw.Context())
	require.True(t, ok)
	assert.Equal(t, platform.RoleAdmin, gotRole)
}

// TestResolveTenantHeaderBeatsCookie proves the precedence rule: a member
// of two orgs sends a header for one and a cookie for the other — the
// explicit header is the tenant, the cookie is only the fallback.
func TestResolveTenantHeaderBeatsCookie(t *testing.T) {
	store := newStore(t)
	cfg := jwtConfig()

	headerOrg, cookieOrg := uuid.New(), uuid.New()
	testenv.SeedOrganization(t, headerOrg, "mw-prec-h")
	testenv.SeedOrganization(t, cookieOrg, "mw-prec-c")
	userID := seedUser(t, uniqueEmail("mw-prec"), "MW Prec")
	seedMembership(t, headerOrg, userID, platform.RoleOwner)
	seedMembership(t, cookieOrg, userID, platform.RoleAdmin)

	p := &probe{}
	chain := platform.Authenticate(cfg)(platform.ResolveTenant(store)(p.handler()))
	req := orgCookieRequest(t, cfg, userID, cookieOrg.String())
	req.Header.Set("X-Organization-Id", headerOrg.String())
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, p.called)
	gotOrg, ok := platform.OrgID(p.saw.Context())
	require.True(t, ok)
	assert.Equal(t, headerOrg, gotOrg, "the header must win over the cookie")
	gotRole, ok := platform.Role(p.saw.Context())
	require.True(t, ok)
	assert.Equal(t, platform.RoleOwner, gotRole)
}

// TestResolveTenantMalformedCookie pins the failure shapes of the cookie
// fallback: garbage and absent hints collapse to the same 400 an absent
// header always got, and a well-formed cookie naming an org the user does
// not belong to is the uniform 404 — all before the handler, and the
// malformed cases before any SQL (panicQuerier proves it).
func TestResolveTenantMalformedCookie(t *testing.T) {
	cfg := jwtConfig()
	userID := uuid.New()

	for _, tc := range []struct {
		name  string
		value string
		set   bool
	}{
		{"cookie absent", "", false},
		{"cookie not a uuid", "acme-corp", true},
		{"cookie empty", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &probe{}
			store := platform.NewStore(panicQuerier{t})
			req := authedRequest(t, cfg, userID, http.MethodGet, "/tenant/route")
			if tc.set {
				req.AddCookie(&http.Cookie{Name: "qp_org", Value: tc.value})
			}
			rec := httptest.NewRecorder()
			platform.Authenticate(cfg)(platform.ResolveTenant(store)(p.handler())).ServeHTTP(rec, req)

			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Equal(t, `{"error":"missing_organization"}`, rec.Body.String())
			assert.False(t, p.called)
		})
	}
}

// TestResolveTenantCookieNonMemberIs404: a well-formed qp_org for an org
// the user does not belong to gets the same uniform 404 as the header
// path — the cookie carries no authority, membership does.
func TestResolveTenantCookieNonMemberIs404(t *testing.T) {
	store := newStore(t)
	cfg := jwtConfig()

	orgID := uuid.New()
	testenv.SeedOrganization(t, orgID, "mw-cookie-404")
	userID := seedUser(t, uniqueEmail("mw-cookie-404"), "MW Cookie 404")
	// No membership row.

	p := &probe{}
	chain := platform.Authenticate(cfg)(platform.ResolveTenant(store)(p.handler()))
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, orgCookieRequest(t, cfg, userID, orgID.String()))

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, `{"error":"not_found"}`, rec.Body.String())
	assert.False(t, p.called)
}

// TestResolveTenantMalformedHeaderBeatsCookie: a present-but-bad header
// must not fall through to a valid cookie — the explicit transport's
// failure is the answer.
func TestResolveTenantMalformedHeaderBeatsCookie(t *testing.T) {
	store := newStore(t)
	cfg := jwtConfig()

	orgID := uuid.New()
	testenv.SeedOrganization(t, orgID, "mw-hbad")
	userID := seedUser(t, uniqueEmail("mw-hbad"), "MW HBad")
	seedMembership(t, orgID, userID, platform.RoleAdmin)

	p := &probe{}
	chain := platform.Authenticate(cfg)(platform.ResolveTenant(store)(p.handler()))
	req := orgCookieRequest(t, cfg, userID, orgID.String())
	req.Header.Set("X-Organization-Id", "not-a-uuid")
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, `{"error":"missing_organization"}`, rec.Body.String())
	assert.False(t, p.called, "a bad header must not fall back to the cookie")
}

// TestSetOrgCookie pins the cookie attributes the contract relies on:
// name, HttpOnly, Lax, root path, Secure off the config flag and the
// refresh-lifetime MaxAge.
func TestSetOrgCookie(t *testing.T) {
	cfg := &platform.Config{CookieSecure: true, RefreshTTL: 30 * 24 * time.Hour}
	orgID := uuid.New()
	rec := httptest.NewRecorder()

	platform.SetOrgCookie(cfg, rec, orgID)

	res := rec.Result()
	cookies := res.Cookies()
	require.Len(t, cookies, 1)
	c := cookies[0]
	assert.Equal(t, "qp_org", c.Name)
	assert.Equal(t, orgID.String(), c.Value)
	assert.True(t, c.HttpOnly)
	assert.True(t, c.Secure)
	assert.Equal(t, http.SameSiteLaxMode, c.SameSite)
	assert.Equal(t, "/", c.Path)
	assert.Equal(t, int(cfg.RefreshTTL.Seconds()), c.MaxAge)
}

// TestSetOrgCookieInsecureLocal: CookieSecure=false (local http dev) drops
// only the Secure flag — the rest of the contract is unchanged.
func TestSetOrgCookieInsecureLocal(t *testing.T) {
	cfg := &platform.Config{CookieSecure: false, RefreshTTL: time.Hour}
	rec := httptest.NewRecorder()

	platform.SetOrgCookie(cfg, rec, uuid.New())

	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)
	assert.False(t, cookies[0].Secure)
	assert.True(t, cookies[0].HttpOnly)
	assert.Equal(t, "qp_org", cookies[0].Name)
	assert.Equal(t, http.SameSiteLaxMode, cookies[0].SameSite)
}
