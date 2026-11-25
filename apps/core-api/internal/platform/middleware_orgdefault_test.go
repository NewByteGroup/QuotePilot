package platform_test

// ResolveTenantDefault's first-membership fallback (the fresh-session
// fix): a safe-method request carrying NO org hint resolves the caller's
// oldest membership instead of 400ing — the web inbox's first load.
// An unsafe method and a present-but-malformed header still die as the
// same missing_organization they always got; the panicQuerier store
// proves both failure paths answer before any SQL runs.

import (
	"context"
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

// seedMembershipAt links user and org with a pinned created_at — the
// default is the OLDEST membership, so the tests must not race two now()
// defaults on the shared dev database.
func seedMembershipAt(t *testing.T, orgID, userID uuid.UUID, role string, createdAt time.Time) {
	t.Helper()
	_, err := testenv.MigratorPool(t).Exec(context.Background(),
		`INSERT INTO app.organization_memberships (organization_id, user_id, role, created_at)
		 VALUES ($1, $2, $3::app.membership_role, $4)`,
		orgID, userID, role, createdAt)
	require.NoError(t, err)
}

// TestResolveTenantDefaultFirstMembership: a hintless GET — no header, no
// qp_org — resolves the caller's OLDEST membership, the same pick
// GET /v1/session reports as active.
func TestResolveTenantDefaultFirstMembership(t *testing.T) {
	store := newStore(t)
	cfg := jwtConfig()

	orgOld, orgNew := uuid.New(), uuid.New()
	testenv.SeedOrganization(t, orgOld, "def-old")
	testenv.SeedOrganization(t, orgNew, "def-new")
	userID := seedUser(t, uniqueEmail("def-member"), "Def Member")
	// seedMembership uses now() defaults; the explicit created_at variant
	// pins the order rather than racing two timestamps.
	seedMembershipAt(t, orgNew, userID, platform.RoleAdmin, time.Now().Add(-time.Hour))
	seedMembershipAt(t, orgOld, userID, platform.RolePricingAnalyst, time.Now().Add(-48*time.Hour))

	p := &probe{}
	chain := platform.Authenticate(cfg)(platform.ResolveTenantDefault(store)(p.handler()))
	req := authedRequest(t, cfg, userID, http.MethodGet, "/v1/inbox/quotes")
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "the hintless GET must reach the handler")
	require.True(t, p.called)
	gotOrg, ok := platform.OrgID(p.saw.Context())
	require.True(t, ok)
	assert.Equal(t, orgOld, gotOrg, "the oldest membership is the deterministic default")
	gotRole, ok := platform.Role(p.saw.Context())
	require.True(t, ok)
	assert.Equal(t, platform.RolePricingAnalyst, gotRole)
}

// TestResolveTenantDefaultHintsStillApply: the default only fires when
// NO hint exists — a qp_org cookie still resolves its own org, and a
// header still wins over it outright.
func TestResolveTenantDefaultHintsStillApply(t *testing.T) {
	store := newStore(t)
	cfg := jwtConfig()

	orgA, orgB := uuid.New(), uuid.New()
	testenv.SeedOrganization(t, orgA, "defhint-a")
	testenv.SeedOrganization(t, orgB, "defhint-b")
	userID := seedUser(t, uniqueEmail("defhint"), "Def Hint")
	seedMembershipAt(t, orgA, userID, platform.RoleAdmin, time.Now().Add(-48*time.Hour))
	seedMembershipAt(t, orgB, userID, platform.RolePricingAnalyst, time.Now().Add(-time.Hour))

	// Cookie present → the cookie's org, not the oldest membership.
	p := &probe{}
	chain := platform.Authenticate(cfg)(platform.ResolveTenantDefault(store)(p.handler()))
	req := authedRequest(t, cfg, userID, http.MethodGet, "/tenant/route")
	req.AddCookie(&http.Cookie{Name: "qp_org", Value: orgB.String()})
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	gotOrg, ok := platform.OrgID(p.saw.Context())
	require.True(t, ok)
	assert.Equal(t, orgB, gotOrg, "the cookie hint outranks the default")

	// Header present → the header's org, period.
	p = &probe{}
	chain = platform.Authenticate(cfg)(platform.ResolveTenantDefault(store)(p.handler()))
	req = authedRequest(t, cfg, userID, http.MethodGet, "/tenant/route")
	req.Header.Set("X-Organization-Id", orgA.String())
	req.AddCookie(&http.Cookie{Name: "qp_org", Value: orgB.String()})
	rec = httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	gotOrg, ok = platform.OrgID(p.saw.Context())
	require.True(t, ok)
	assert.Equal(t, orgA, gotOrg, "the header still wins outright")
}

// TestResolveTenantDefaultUnsafeMethodAndBadHint: the fallback is a
// safe-method nicety — a hintless POST and a malformed explicit header
// (memberships or not) keep the same missing_organization 400 they
// always got, answered before any SQL (panicQuerier proves it).
func TestResolveTenantDefaultUnsafeMethodAndBadHint(t *testing.T) {
	cfg := jwtConfig()
	userID := uuid.New()

	for _, tc := range []struct {
		name   string
		method string
		header string
		set    bool
	}{
		{"hintless POST", http.MethodPost, "", false},
		{"hintless PUT", http.MethodPut, "", false},
		{"malformed header on GET", http.MethodGet, "org-slug", true},
		{"malformed header on POST", http.MethodPost, "not-a-uuid", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &probe{}
			store := platform.NewStore(panicQuerier{t})
			req := authedRequest(t, cfg, userID, tc.method, "/tenant/route")
			if tc.set {
				req.Header.Set("X-Organization-Id", tc.header)
			}
			rec := httptest.NewRecorder()
			platform.Authenticate(cfg)(platform.ResolveTenantDefault(store)(p.handler())).ServeHTTP(rec, req)

			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Equal(t, `{"error":"missing_organization","message":"an organization hint is required — pass the X-Organization-Id header or the qp_org cookie"}`, rec.Body.String())
			assert.False(t, p.called)
		})
	}
}

// TestResolveTenantDefaultNoMemberships: a hintless GET from a user with
// zero memberships has no default to fall back to — the same uniform 400
// a hintless request on plain ResolveTenant gets (the session bootstrap
// itself 404s for this user, so there is no org to pretend at).
func TestResolveTenantDefaultNoMemberships(t *testing.T) {
	store := newStore(t)
	cfg := jwtConfig()
	userID := seedUser(t, uniqueEmail("def-none"), "No Orgs")

	p := &probe{}
	chain := platform.Authenticate(cfg)(platform.ResolveTenantDefault(store)(p.handler()))
	req := authedRequest(t, cfg, userID, http.MethodGet, "/tenant/route")
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, `{"error":"missing_organization","message":"an organization hint is required — pass the X-Organization-Id header or the qp_org cookie"}`, rec.Body.String())
	assert.False(t, p.called)
}
