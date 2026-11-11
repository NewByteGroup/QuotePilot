package auth_test

// GET /me/organization proofs (spec 2026-11-10-tenant-resolution-api
// sections 7, 8 and 9) — the mission's end-to-end seam: a verified qp_access
// cookie plus a live membership earns the org registry row read through
// get_current_organization() INSIDE the tenant read transaction. The unit
// proofs drive the production mount — Authenticate → ResolveTenant →
// auth.NewServer — with the doubles in me_organization_fake_test.go; the
// final test runs the whole seam against real Postgres: request → JWT →
// X-Organization-Id → get_membership → role → InTenantReadTx → org row.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// TestMeOrganizationReturnsOrgRow is the happy path: a member's request
// carries the header through ResolveTenant's live get_membership check, the
// handler enters the read tx on the resolved org, and the answer is the org
// row the function returns for that tenant context — branding decoded into
// the contract's required object.
func TestMeOrganizationReturnsOrgRow(t *testing.T) {
	r := newMeOrgRig(t, sessionConfig())
	userID, orgID := uuid.New(), uuid.New()
	r.q.roles[memberKey{user: userID, org: orgID}] = platform.RoleAdmin
	r.q.orgs[orgID] = orgRec{
		name:     "Acme Freight",
		slug:     "acme-freight",
		alias:    "acme@in.quotepilot.app",
		branding: json.RawMessage(`{"color":"#0af","footer":"Acme"}`),
	}

	rec := r.getMeOrg(t, r.authed, r.meOrgAccessCookie(t, userID), orgID.String())

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var body authapi.OrganizationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, orgID.String(), body.ID)
	assert.Equal(t, "Acme Freight", body.Name)
	assert.Equal(t, "acme-freight", body.Slug)
	assert.Equal(t, "acme@in.quotepilot.app", body.InboundEmailAlias)
	assert.Equal(t, map[string]interface{}{"color": "#0af", "footer": "Acme"}, body.BrandingJSON)

	require.Equal(t, []memberKey{{user: userID, org: orgID}}, r.q.membershipCalls,
		"membership is re-checked on the request, never cached")
	require.True(t, r.runner.called, "the handler must enter the tenant transaction")
	assert.True(t, r.runner.readTx, "the read tx — not InTenantTx — is the org lookup's transport")
	assert.Equal(t, orgID, r.runner.org, "the tx scopes to the org the middleware resolved")
	assert.Equal(t, 1, r.q.orgReads, "exactly one get_current_organization read")
}

// TestMeOrganizationUniform401 proves the composed chain keeps spec section
// 7's contract: absent, forged and expired cookies die inside Authenticate
// with byte-identical bodies — before get_membership, before the tenant tx.
func TestMeOrganizationUniform401(t *testing.T) {
	r := newMeOrgRig(t, sessionConfig())
	orgID := uuid.New()

	expired, err := r.cfg.SignAccess(uuid.New(), uuid.New(), r.clock.now.Add(-time.Hour))
	require.NoError(t, err)
	forger := &platform.Config{
		JWTKeys: map[string]string{"k1": strings.Repeat("x", 32)}, ActiveKID: "k1", AccessTTL: r.cfg.AccessTTL}
	forgedRaw, err := forger.SignAccess(uuid.New(), uuid.New(), r.clock.now)
	require.NoError(t, err)

	var reference []byte
	for _, tc := range []struct {
		name   string
		cookie *http.Cookie
	}{
		{"absent cookie", nil},
		{"forged signature", accessCookieOf(forgedRaw)},
		{"expired", accessCookieOf(expired)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := r.getMeOrg(t, r.authed, tc.cookie, orgID.String())
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.Equal(t, `{"error":"unauthenticated"}`, rec.Body.String())
			if reference == nil {
				reference = rec.Body.Bytes()
			}
			assert.Equal(t, reference, rec.Body.Bytes(),
				"%s must emit the same body bytes as every other failure mode", tc.name)
		})
	}
	assert.Empty(t, r.q.membershipCalls, "a rejected request must not reach get_membership")
	assert.False(t, r.runner.called, "a rejected request must not enter the tenant tx")
}

// TestMeOrganizationHeaderRequired proves ResolveTenant's ordering: an
// absent or non-UUID X-Organization-Id on an authenticated request is the
// 400 missing_organization — before the membership lookup and before any
// tenant transaction.
func TestMeOrganizationHeaderRequired(t *testing.T) {
	r := newMeOrgRig(t, sessionConfig())
	userID := uuid.New()

	for _, tc := range []struct {
		name   string
		header string
	}{
		{"header absent", ""},
		{"header not a uuid", "acme-freight"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := r.getMeOrg(t, r.authed, r.meOrgAccessCookie(t, userID), tc.header)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			assert.Equal(t, `{"error":"missing_organization"}`, rec.Body.String())
		})
	}
	assert.Empty(t, r.q.membershipCalls, "the 400 must precede the membership lookup")
	assert.False(t, r.runner.called)
}

// TestMeOrganizationNonMemberIs404 proves spec section 7's uniform 404
// through the composed chain: a non-member of an org that exists and a
// request naming an org that does not get byte-identical answers — the same
// get_membership NULL collapses both — and neither reaches the tenant tx.
func TestMeOrganizationNonMemberIs404(t *testing.T) {
	r := newMeOrgRig(t, sessionConfig())
	userID, orgID := uuid.New(), uuid.New()
	r.q.orgs[orgID] = orgRec{name: "Acme", slug: "acme", alias: "acme@in.test"}
	// No roles entry: userID is a non-member of an existing organization.

	var reference []byte
	for _, tc := range []struct {
		name  string
		orgID uuid.UUID
	}{
		{"non-member of existing org", orgID},
		{"nonexistent org", uuid.New()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := r.getMeOrg(t, r.authed, r.meOrgAccessCookie(t, userID), tc.orgID.String())
			assert.Equal(t, http.StatusNotFound, rec.Code)
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			if reference == nil {
				reference = rec.Body.Bytes()
			}
			assert.Equal(t, reference, rec.Body.Bytes(),
				"%s must be byte-identical to every other tenant miss", tc.name)
		})
	}
	assert.Equal(t, `{"error":"not_found"}`, string(reference))
	assert.Len(t, r.q.membershipCalls, 2, "both requests pay the same single lookup")
	assert.False(t, r.runner.called, "a rejected request must not enter the tenant tx")
}

// TestMeOrganizationRequireRoleGates is the spec section 9 authorization
// proof on the endpoint's own mount: RequireRole(OWNER) refuses a
// PRICING_ANALYST with 403 while admitting an OWNER, and the ungated mount
// admits the analyst — all roles reach /me/organization (spec section 8).
// The denied request never enters the tenant transaction.
func TestMeOrganizationRequireRoleGates(t *testing.T) {
	r := newMeOrgRig(t, sessionConfig())
	orgID := uuid.New()
	owner, analyst := uuid.New(), uuid.New()
	r.q.roles[memberKey{user: owner, org: orgID}] = platform.RoleOwner
	r.q.roles[memberKey{user: analyst, org: orgID}] = platform.RolePricingAnalyst
	r.q.orgs[orgID] = orgRec{name: "Acme", slug: "acme", alias: "acme@in.test"}

	rec := r.getMeOrg(t, r.owner, r.meOrgAccessCookie(t, analyst), orgID.String())
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, `{"error":"forbidden"}`, rec.Body.String())
	assert.False(t, r.runner.called, "a role-denied request must not enter the tenant tx")

	rec = r.getMeOrg(t, r.owner, r.meOrgAccessCookie(t, owner), orgID.String())
	require.Equal(t, http.StatusOK, rec.Code, "the OWNER member must pass the owner gate")

	rec = r.getMeOrg(t, r.authed, r.meOrgAccessCookie(t, analyst), orgID.String())
	require.Equal(t, http.StatusOK, rec.Code, "the ungated /me/organization mount admits every role")
	assert.True(t, r.runner.called)
	assert.Equal(t, 2, r.q.orgReads)
}

// TestMeOrganizationFailuresAreUniform500 proves every post-chain failure —
// the runner's driver error, the function call's driver error, the empty row
// that membership made impossible, and a branding blob that is not a JSON
// object — collapses to the one {"error":"internal"} body.
func TestMeOrganizationFailuresAreUniform500(t *testing.T) {
	orgID := uuid.New()
	for _, tc := range []struct {
		name  string
		plant func(r *meOrgRig)
	}{
		{"runner driver error", func(r *meOrgRig) { r.runner.fail = errBoom }},
		{"function driver error", func(r *meOrgRig) {
			r.q.fail["get_current_organization"] = errBoom
		}},
		{"empty row inside tx", func(*meOrgRig) {
			// membership exists but the org row does not resolve — drift.
		}},
		{"branding not an object", func(r *meOrgRig) {
			r.q.orgs[orgID] = orgRec{branding: json.RawMessage(`[1,2]`)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newMeOrgRig(t, sessionConfig())
			userID := uuid.New()
			r.q.roles[memberKey{user: userID, org: orgID}] = platform.RoleOwner
			tc.plant(r)

			rec := r.getMeOrg(t, r.authed, r.meOrgAccessCookie(t, userID), orgID.String())

			assert.Equal(t, http.StatusInternalServerError, rec.Code)
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			assert.Equal(t, `{"error":"internal"}`+"\n", rec.Body.String())
		})
	}
}

// TestMeOrganizationWithoutChainDenies is the fail-closed proof: mounts that
// skip the tenant leg — the bare generated mux and the half-mounted
// Authenticate-only chain — answer the same {"error":"unauthenticated"} the
// middleware emits, without a membership lookup or a tenant transaction.
func TestMeOrganizationWithoutChainDenies(t *testing.T) {
	r := newMeOrgRig(t, sessionConfig())
	userID, orgID := uuid.New(), uuid.New()
	r.q.roles[memberKey{user: userID, org: orgID}] = platform.RoleOwner
	r.q.orgs[orgID] = orgRec{name: "Acme", slug: "acme", alias: "acme@in.test"}

	for _, tc := range []struct {
		name  string
		mount http.Handler
	}{
		{"bare mux — no middleware", r.bare},
		{"authenticate only — no ResolveTenant", r.authOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := r.getMeOrg(t, tc.mount, r.meOrgAccessCookie(t, userID), orgID.String())
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			assert.Equal(t, `{"error":"unauthenticated"}`+"\n", rec.Body.String())
		})
	}
	assert.Empty(t, r.q.membershipCalls)
	assert.False(t, r.runner.called, "a mis-mounted /me/organization must not enter the tenant tx")
	assert.Equal(t, 0, r.q.orgReads)
}

// TestMeOrganizationBrandingEdges pins the required branding_json field's
// serialization: a populated blob passes through, the jsonb default '{}'
// stays the empty object, and the NULL shapes (the defensive decode path —
// the real column is NOT NULL) map to the same {} rather than a null body
// member.
func TestMeOrganizationBrandingEdges(t *testing.T) {
	for _, tc := range []struct {
		name     string
		branding json.RawMessage
		want     map[string]interface{}
	}{
		{"populated", json.RawMessage(`{"color":"#f00"}`), map[string]interface{}{"color": "#f00"}},
		{"empty object", json.RawMessage(`{}`), map[string]interface{}{}},
		{"nil blob", nil, map[string]interface{}{}},
		{"json null", json.RawMessage(`null`), map[string]interface{}{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newMeOrgRig(t, sessionConfig())
			userID, orgID := uuid.New(), uuid.New()
			r.q.roles[memberKey{user: userID, org: orgID}] = platform.RoleOwner
			r.q.orgs[orgID] = orgRec{name: "Acme", slug: "acme", alias: "a@in.test", branding: tc.branding}

			rec := r.getMeOrg(t, r.authed, r.meOrgAccessCookie(t, userID), orgID.String())

			require.Equal(t, http.StatusOK, rec.Code)
			var body authapi.OrganizationResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, tc.want, body.BrandingJSON)
			assert.Contains(t, rec.Body.String(), `"branding_json":{`,
				"a required object field must never serialize as null")
		})
	}
}

// seedOrgUser inserts an app.users row as qp_migrator — Layer A is closed to
// qp_app — and returns its id; the auth_test twin of platform_test.seedUser.
func seedOrgUser(t *testing.T, email string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, testenv.MigratorPool(t).QueryRow(context.Background(),
		`INSERT INTO app.users (email, full_name) VALUES ($1, 'MeOrg User') RETURNING id`,
		email).Scan(&id))
	return id
}

// seedOrgMembership links user and org with the given role, as qp_migrator.
func seedOrgMembership(t *testing.T, orgID, userID uuid.UUID, role string) {
	t.Helper()
	_, err := testenv.MigratorPool(t).Exec(context.Background(),
		`INSERT INTO app.organization_memberships (organization_id, user_id, role) VALUES ($1, $2, $3::app.membership_role)`,
		orgID, userID, role)
	require.NoError(t, err)
}

// TestMeOrganizationEndToEnd is the spec section 9 proof this whole mission
// exists for, run against real Postgres: seed org A with an OWNER member,
// mint the access JWT, and the request travels cookie → Authenticate →
// X-Organization-Id → get_membership → role → InTenantReadTx →
// get_current_organization()'s row — the same org id the GUC carries. The
// negative proves membership-only-in-A gets the uniform 404 for org B.
func TestMeOrganizationEndToEnd(t *testing.T) {
	db := testenv.Open(t)
	migrator := testenv.MigratorPool(t)
	ctx := context.Background()
	cfg := sessionConfig()

	orgA, orgB := uuid.New(), uuid.New()
	slugA := testenv.SeedOrganization(t, orgA, "meorg-e2e-a")
	testenv.SeedOrganization(t, orgB, "meorg-e2e-b")
	userID := seedOrgUser(t, "meorg-e2e-"+uuid.NewString()+"@test.dev")
	seedOrgMembership(t, orgA, userID, platform.RoleOwner)
	_, err := migrator.Exec(ctx,
		`UPDATE app.organizations SET branding_json = $2 WHERE id = $1`,
		orgA, `{"primary":"#0a84ff","footer":"Acme"}`)
	require.NoError(t, err)

	clock := &fakeClock{now: time.Now()}
	store := platform.NewStore(db.Pool())
	endpoints := auth.NewEndpoints(cfg, store, store.InStoreTx, db, &platform.FakeMailSender{},
		platform.NewLimiter(platform.LoginIPCapacity, platform.LimitWindow, clock.Now),
		platform.NewLimiter(platform.LoginEmailCapacity, platform.LimitWindow, clock.Now),
		clock.Now)
	server := platform.Authenticate(cfg)(platform.ResolveTenant(store)(auth.NewServer(endpoints)))

	raw, err := cfg.SignAccess(userID, uuid.New(), time.Now())
	require.NoError(t, err)
	cookie := &http.Cookie{Name: "qp_access", Value: raw}

	// request → JWT → tenant header → get_membership → role → InTenantReadTx →
	// the org row get_current_organization resolves from the RLS GUC.
	req := httptest.NewRequest(http.MethodGet, "/me/organization", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-Organization-Id", orgA.String())
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var body authapi.OrganizationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, orgA.String(), body.ID)
	assert.Equal(t, "Test "+slugA, body.Name)
	assert.Equal(t, slugA, body.Slug)
	assert.Equal(t, slugA+"@in.test", body.InboundEmailAlias)
	assert.Equal(t, map[string]interface{}{"primary": "#0a84ff", "footer": "Acme"}, body.BrandingJSON,
		"branding_json rode the tenant tx out of the real organizations row")

	// Cross-tenant miss: org B exists but the caller holds no membership — the
	// uniform 404, answered before any tenant transaction.
	req = httptest.NewRequest(http.MethodGet, "/me/organization", nil)
	req.AddCookie(cookie)
	req.Header.Set("X-Organization-Id", orgB.String())
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, `{"error":"not_found"}`, rec.Body.String())
}
