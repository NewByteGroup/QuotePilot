package auth_test

// GET /me proofs (spec 2026-11-10-tenant-resolution-api sections 4, 7 and
// 8): a valid qp_access cookie earns the flat {id, email, full_name,
// memberships} body the org switcher renders, the list keyed on the
// profile's email — never request data. Authenticate rejects every bad
// cookie before the handler with the byte-identical uniform 401; a valid
// token naming a missing user row is the uniform 500 (FK drift, same rule
// session's payload read follows); driver faults are the same 500; and a
// bare mount — no Authenticate — fails closed at 401 without touching the
// store. Tests drive the production mount, platform.Authenticate over
// auth.NewServer; the doubles live in session_fake_test.go.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// meRig is a mounted /me surface plus the test's windows into it: authed is
// the production mount — Authenticate before the generated mux — and bare
// is the same mux alone, the mis-mount shape the fail-closed test drives.
type meRig struct {
	authed http.Handler
	bare   http.Handler
	clock  *fakeClock
	cfg    *platform.Config
	q      *sessionQuerier
}

// newMeRig wires the endpoints the way cmd/server will for the /me routes:
// real Store over the fake Querier mounted through auth.NewServer behind
// platform.Authenticate — a mux-level wrap, not a strict middleware,
// because strict middlewares return (response, error) and cannot emit the
// platform's exact 401 body shape (authapi/doc.go).
func newMeRig(t *testing.T, cfg *platform.Config) *meRig {
	t.Helper()
	clock := &fakeClock{now: time.Now()}
	q := &sessionQuerier{
		now:      clock.Now,
		tokens:   map[string]*tokenRec{},
		profiles: map[uuid.UUID]profileRec{},
		members:  map[string][]platform.Membership{},
		fail:     map[string]error{},
	}
	store := platform.NewStore(q)
	endpoints := auth.NewEndpoints(cfg,
		store, directInTx(store), &platform.FakeMailSender{},
		platform.NewLimiter(platform.LoginIPCapacity, platform.LimitWindow, clock.Now),
		platform.NewLimiter(platform.LoginEmailCapacity, platform.LimitWindow, clock.Now),
		clock.Now)
	server := auth.NewServer(endpoints)
	return &meRig{authed: platform.Authenticate(cfg)(server), bare: server, clock: clock, cfg: cfg, q: q}
}

// getMe fires one request at the production-mounted /me; a nil cookie means
// no qp_access travels at all — the absent-cookie case.
func (r *meRig) getMe(t *testing.T, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	r.authed.ServeHTTP(rec, req)
	return rec
}

// meAccessCookie mints a real access JWT for userID — the sid is a fresh
// uuid, and /me never re-validates it: Authenticate is stateless, so no
// session row exists to check against (spec section 4).
func (r *meRig) meAccessCookie(t *testing.T, userID uuid.UUID) *http.Cookie {
	t.Helper()
	raw, err := r.cfg.SignAccess(userID, uuid.New(), r.clock.now)
	require.NoError(t, err)
	return &http.Cookie{Name: "qp_access", Value: raw}
}

// accessCookieOf wraps a raw JWT in the qp_access cookie shape.
func accessCookieOf(raw string) *http.Cookie {
	return &http.Cookie{Name: "qp_access", Value: raw}
}

// signNone mints an unsigned alg=none token carrying real claims — forgery
// VerifyAccess must refuse on the signing method alone, before any key
// lookup: with no signature to check, only the method allowlist stops it.
func signNone(t *testing.T, now time.Time, userID uuid.UUID) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"sub": userID.String(),
		"sid": uuid.New().String(),
		"iat": now.Unix(),
		"exp": now.Add(15 * time.Minute).Unix(),
	})
	token.Header["kid"] = "k1"
	raw, err := token.SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)
	return raw
}

// TestMeReturnsFlatProfileAndMemberships is the happy path: a verified
// cookie earns the caller's identity as one flat object — id, email and
// full_name sit beside the membership list, never nested under `profile`
// like the session response — and the switcher rows are keyed on the
// profile's stored email.
func TestMeReturnsFlatProfileAndMemberships(t *testing.T) {
	r := newMeRig(t, sessionConfig())
	userID := uuid.New()
	orgID := uuid.New()
	r.q.profiles[userID] = profileRec{email: "ada@test.dev", fullName: "Ada Lovelace"}
	r.q.members["ada@test.dev"] = []platform.Membership{
		{OrgID: orgID, Name: "Acme Freight", Slug: "acme-freight", Role: platform.RoleOwner},
		{OrgID: uuid.New(), Name: "Globex", Slug: "globex", Role: platform.RolePricingAnalyst},
	}

	rec := r.getMe(t, r.meAccessCookie(t, userID))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var body authapi.MeResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, userID.String(), body.ID)
	assert.Equal(t, "ada@test.dev", body.Email)
	assert.Equal(t, "Ada Lovelace", body.FullName)
	require.Len(t, body.Memberships, 2)
	assert.Equal(t, orgID.String(), body.Memberships[0].OrganizationID)
	assert.Equal(t, "Acme Freight", body.Memberships[0].Name)
	assert.Equal(t, "acme-freight", body.Memberships[0].Slug)
	assert.Equal(t, authapi.OWNER, body.Memberships[0].Role)
	assert.Equal(t, authapi.PRICINGANALYST, body.Memberships[1].Role)
	assert.NotContains(t, rec.Body.String(), `"profile"`,
		"the /me shape is flat — session's `profile` nesting must not appear")

	require.Equal(t, []uuid.UUID{userID}, r.q.profileCalls,
		"the profile read ran once, on the JWT's sub")
	require.Equal(t, []string{"ada@test.dev"}, r.q.listed,
		"the switcher list is keyed on the profile's email, not request data")
}

// TestMeUniform401 is the spec section 7 uniform-failure proof: absent,
// forged, wrong-kid, expired and alg=none cookies all die inside
// Authenticate with byte-identical status and body — before the handler,
// before any store call — so no failure mode is distinguishable. The forged
// and unknown-kid tokens are minted by configs the verifier does not share.
func TestMeUniform401(t *testing.T) {
	r := newMeRig(t, sessionConfig())
	userID := uuid.New()
	r.q.profiles[userID] = profileRec{email: "ada@test.dev"}

	expired, err := r.cfg.SignAccess(userID, uuid.New(), r.clock.now.Add(-time.Hour))
	require.NoError(t, err)
	forger := &platform.Config{
		JWTKeys: map[string]string{"k1": strings.Repeat("x", 32)}, ActiveKID: "k1", AccessTTL: r.cfg.AccessTTL}
	forgedRaw, err := forger.SignAccess(userID, uuid.New(), r.clock.now)
	require.NoError(t, err)
	ghost := &platform.Config{
		JWTKeys: map[string]string{"ghost": strings.Repeat("y", 32)}, ActiveKID: "ghost", AccessTTL: r.cfg.AccessTTL}
	wrongKID, err := ghost.SignAccess(userID, uuid.New(), r.clock.now)
	require.NoError(t, err)

	cases := []struct {
		name   string
		cookie *http.Cookie
	}{
		{"absent cookie", nil},
		{"forged signature", accessCookieOf(forgedRaw)},
		{"unknown kid", accessCookieOf(wrongKID)},
		{"expired", accessCookieOf(expired)},
		{"alg none", accessCookieOf(signNone(t, r.clock.now, userID))},
	}

	var reference []byte
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := r.getMe(t, tc.cookie)
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			assert.Equal(t, `{"error":"unauthenticated"}`, rec.Body.String())
			if reference == nil {
				reference = rec.Body.Bytes()
			}
			assert.Equal(t, reference, rec.Body.Bytes(),
				"%s must emit the same body bytes as every other failure mode", tc.name)
		})
	}
	assert.Empty(t, r.q.profileCalls, "no rejected request may reach the store")
	assert.Empty(t, r.q.listed)
}

// TestMeMissingProfileIsInternalError covers a verified token naming a user
// row that does not exist: drift between the JWT's sub and the users table,
// answered through the uniform 500 — never an empty 200 (the same rule
// session's payload read follows).
func TestMeMissingProfileIsInternalError(t *testing.T) {
	r := newMeRig(t, sessionConfig())
	userID := uuid.New() // no profile row planted

	rec := r.getMe(t, r.meAccessCookie(t, userID))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, `{"error":"internal"}`+"\n", rec.Body.String())
	require.Equal(t, []uuid.UUID{userID}, r.q.profileCalls)
	assert.Empty(t, r.q.listed, "a missing profile stops before the membership read")
}

// TestMeFailuresAreUniform500 proves a driver fault is the same 500
// {"error":"internal"} at each store call on the path — the profile read
// and the membership list alike.
func TestMeFailuresAreUniform500(t *testing.T) {
	for _, failAt := range []string{"get_user_profile", "list_memberships_for_email"} {
		t.Run(failAt, func(t *testing.T) {
			r := newMeRig(t, sessionConfig())
			userID := uuid.New()
			r.q.profiles[userID] = profileRec{email: "ada@test.dev"}
			r.q.fail[failAt] = errBoom

			rec := r.getMe(t, r.meAccessCookie(t, userID))

			assert.Equal(t, http.StatusInternalServerError, rec.Code)
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			assert.Equal(t, `{"error":"internal"}`+"\n", rec.Body.String())
		})
	}
}

// TestMeWithoutAuthenticateDenies is the mis-mount proof: the generated mux
// alone — Authenticate never ran — must answer the uniform 401 even when
// the request carries a perfectly valid cookie, because nothing planted the
// user context. Fail closed, and the store is never touched.
func TestMeWithoutAuthenticateDenies(t *testing.T) {
	r := newMeRig(t, sessionConfig())
	userID := uuid.New()
	r.q.profiles[userID] = profileRec{email: "ada@test.dev"}

	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.AddCookie(r.meAccessCookie(t, userID))
	rec := httptest.NewRecorder()
	r.bare.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, `{"error":"unauthenticated"}`+"\n", rec.Body.String())
	assert.Empty(t, r.q.profileCalls, "a mis-mounted /me must not reach the store")
	assert.Empty(t, r.q.listed)
}

// TestMeEmptyMembershipsIsArray pins the contract for a member of no org:
// memberships serializes as [] — never null — so the switcher renders an
// empty picker rather than a missing list.
func TestMeEmptyMembershipsIsArray(t *testing.T) {
	r := newMeRig(t, sessionConfig())
	userID := uuid.New()
	r.q.profiles[userID] = profileRec{email: "ada@test.dev"}

	rec := r.getMe(t, r.meAccessCookie(t, userID))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"memberships":[]`,
		"an empty switcher list serializes as [] — never null")
}
