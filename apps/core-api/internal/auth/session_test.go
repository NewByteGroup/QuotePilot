package auth_test

// POST /auth/session proofs (spec 2026-11-10-tenant-resolution-api sections
// 4, 7 and 8): a live magic-link token earns the cookie pair (qp_access
// Path=/ SameSite=Lax, qp_refresh Path=/auth SameSite=Strict, both
// HttpOnly+Secure) around the profile+memberships body the org switcher
// renders. Used, expired and unknown tokens answer the same 401
// byte-for-byte, malformed bodies the uniform 400, driver failures the
// uniform 500 — and only the 200 path may ever write a Set-Cookie header.
// Everything runs through auth.NewServer, the handler production mounts;
// the doubles live in session_fake_test.go.

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// TestSessionIssuesCookiePair is the happy path: the 200 carries the profile
// and switcher rows, both cookies land with the exact spec section 4
// attributes, the access value verifies as a real JWT for the created
// session, and the refresh value is a fresh opaque token whose hash — not
// the token — is what create_session was asked to store.
func TestSessionIssuesCookiePair(t *testing.T) {
	r := newSessionRig(t, sessionConfig())
	userID := uuid.New()
	orgID := uuid.New()
	token := r.plantToken(t, userID, r.clock.now.Add(r.cfg.LoginTTL))
	r.q.profiles[userID] = profileRec{email: "ada@test.dev", fullName: "Ada Lovelace"}
	r.q.members["ada@test.dev"] = []platform.Membership{
		{OrgID: orgID, Name: "Acme Freight", Slug: "acme-freight", Role: platform.RoleOwner},
	}

	rec := r.postSession(t, `{"token":"`+token+`"}`)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var body authapi.SessionResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, userID.String(), body.Profile.ID)
	assert.Equal(t, "ada@test.dev", body.Profile.Email)
	assert.Equal(t, "Ada Lovelace", body.Profile.FullName)
	require.Len(t, body.Memberships, 1)
	assert.Equal(t, orgID.String(), body.Memberships[0].OrganizationID)
	assert.Equal(t, "Acme Freight", body.Memberships[0].Name)
	assert.Equal(t, "acme-freight", body.Memberships[0].Slug)
	assert.Equal(t, authapi.OWNER, body.Memberships[0].Role)

	// The store saw the SHA-256 hash of the token — never the token itself.
	require.Len(t, r.q.consumes, 1)
	assert.Equal(t, platform.HashToken(token), r.q.consumes[0],
		"consume_login_token looks up by hash, not by the raw token")
	require.Equal(t, []string{"ada@test.dev"}, r.q.listed,
		"the switcher list is keyed on the profile's email, not the request's")

	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 2, "exactly the spec section 4 pair is written")

	access := findCookie(t, rec, "qp_access")
	require.NotNil(t, access)
	assert.Equal(t, "/", access.Path)
	assert.Equal(t, int(r.cfg.AccessTTL.Seconds()), access.MaxAge)
	assert.True(t, access.HttpOnly)
	assert.True(t, access.Secure)
	assert.Equal(t, http.SameSiteLaxMode, access.SameSite)

	refresh := findCookie(t, rec, "qp_refresh")
	require.NotNil(t, refresh)
	assert.Equal(t, "/auth", refresh.Path)
	assert.Equal(t, int(r.cfg.RefreshTTL.Seconds()), refresh.MaxAge)
	assert.True(t, refresh.HttpOnly)
	assert.True(t, refresh.Secure)
	assert.Equal(t, http.SameSiteStrictMode, refresh.SameSite)

	// The access cookie is a verifiable JWT bound to the stored session.
	require.Len(t, r.q.creates, 1)
	gotUser, gotSession, err := r.cfg.VerifyAccess(access.Value)
	require.NoError(t, err)
	assert.Equal(t, userID, gotUser)
	assert.Equal(t, r.q.creates[0].sessionID, gotSession,
		"the JWT's sid is the session row create_session returned")

	// The refresh cookie is a 32-byte base64url opaque token; Postgres keeps
	// only its SHA-256 hex, at now+RefreshTTL.
	raw, err := base64.RawURLEncoding.DecodeString(refresh.Value)
	require.NoError(t, err)
	assert.Len(t, raw, 32, "the refresh cookie carries a 32-byte opaque token")
	assert.Equal(t, platform.HashToken(refresh.Value), r.q.creates[0].hash,
		"create_session stored the cookie value's hash, not the value")
	assert.NotEqual(t, refresh.Value, r.q.creates[0].hash,
		"the hash must never cross the wire")
	assert.Equal(t, r.clock.now.Add(r.cfg.RefreshTTL), r.q.creates[0].expiresAt)
}

// TestSessionUniform401 is the no-enumeration proof: a used, an expired and
// an unknown token get byte-identical status, headers and body — the
// single-use guarantee itself is proven by the second consume of the live
// token dying the same way — and none of them writes a cookie.
func TestSessionUniform401(t *testing.T) {
	r := newSessionRig(t, sessionConfig())
	userID := uuid.New()
	r.q.profiles[userID] = profileRec{email: "ada@test.dev", fullName: "Ada"}
	live := r.plantToken(t, userID, r.clock.now.Add(r.cfg.LoginTTL))
	expired := r.plantToken(t, userID, r.clock.now.Add(-time.Second))

	require.Equal(t, http.StatusOK, r.postSession(t, `{"token":"`+live+`"}`).Code,
		"the first exchange succeeds; the second is the used-token case")

	recs := []*httptest.ResponseRecorder{
		r.postSession(t, `{"token":"`+live+`"}`),                    // used
		r.postSession(t, `{"token":"`+expired+`"}`),                 // expired
		r.postSession(t, `{"token":"`+strings.Repeat("x", 43)+`"}`), // unknown
	}
	for _, rec := range recs {
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
		assert.Equal(t, `{"error":"unauthenticated"}`+"\n", rec.Body.String(),
			"used, expired and unknown must answer byte-identically")
		assert.Empty(t, rec.Header().Values("Set-Cookie"),
			"a rejected token must never write a cookie")
	}
	assert.Len(t, r.q.consumes, 4, "every attempt paid the same consume lookup")
	assert.Len(t, r.q.creates, 1, "only the live exchange created a session")
}

// TestSessionBadRequests covers every request-shape failure: bodies the JSON
// binder rejects (routed by RequestErrorHandlerFunc) and bodies that bind
// but carry an absent token (the handler's own 400). All share the one
// {"error":"bad_request"} body and none reach the store.
func TestSessionBadRequests(t *testing.T) {
	r := newSessionRig(t, sessionConfig())

	cases := []struct {
		name string
		body string
	}{
		{"malformed json", `{"token":`},
		{"empty body", ``},
		{"token wrong type", `{"token":42}`},
		{"missing token", `{}`},
		{"empty token", `{"token":""}`},
		{"null token", `{"token":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := r.postSession(t, tc.body)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			assert.Equal(t, `{"error":"bad_request"}`+"\n", rec.Body.String())
			assert.Empty(t, rec.Header().Values("Set-Cookie"))
		})
	}
	assert.Empty(t, r.q.consumes, "no rejected request may reach the store")
	assert.Empty(t, r.q.creates)
}

// TestSessionFailuresAreUniform500 proves a driver failure is the same 500
// {"error":"internal"} wherever it strikes on the path — consume, the two
// payload reads, the session insert — and that no Set-Cookie header ever
// leaks onto an error response. A profile row missing after a successful
// consume joins the table: it is contract drift, answered the same way.
func TestSessionFailuresAreUniform500(t *testing.T) {
	cases := []struct {
		name    string
		failAt  string
		prepare func(r *sessionRig, userID uuid.UUID)
	}{
		{"consume driver error", "consume_login_token", nil},
		{"profile driver error", "get_user_profile", nil},
		{"memberships driver error", "list_memberships_for_email", nil},
		{"session insert driver error", "create_session", nil},
		{"profile row missing after consume", "", func(r *sessionRig, userID uuid.UUID) {
			delete(r.q.profiles, userID)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newSessionRig(t, sessionConfig())
			userID := uuid.New()
			r.q.profiles[userID] = profileRec{email: "ada@test.dev", fullName: "Ada"}
			token := r.plantToken(t, userID, r.clock.now.Add(r.cfg.LoginTTL))
			if tc.failAt != "" {
				r.q.fail[tc.failAt] = errBoom
			}
			if tc.prepare != nil {
				tc.prepare(r, userID)
			}
			rec := r.postSession(t, `{"token":"`+token+`"}`)
			assert.Equal(t, http.StatusInternalServerError, rec.Code)
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			assert.Equal(t, `{"error":"internal"}`+"\n", rec.Body.String())
			assert.Empty(t, rec.Header().Values("Set-Cookie"),
				"the error path must never leak a Set-Cookie header")
		})
	}

	// A signing key that cannot sign (active kid absent from JWTKeys) is the
	// last error on the path — same 500, same clean headers. The fake's inTx
	// is a direct call, so it records the attempted insert; the rollback a
	// real transaction performs is pinned in session_integration_test.go.
	cfg := sessionConfig()
	cfg.ActiveKID = "ghost"
	r := newSessionRig(t, cfg)
	userID := uuid.New()
	r.q.profiles[userID] = profileRec{email: "ada@test.dev"}
	token := r.plantToken(t, userID, r.clock.now.Add(r.cfg.LoginTTL))
	rec := r.postSession(t, `{"token":"`+token+`"}`)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, `{"error":"internal"}`+"\n", rec.Body.String())
	assert.Empty(t, rec.Header().Values("Set-Cookie"))
	assert.Len(t, r.q.creates, 1,
		"the fake has no tx to roll back — the row's fate under a real one is pinned by TestSessionExchangeRollsBackConsume")
}

// TestSessionEmptyMembershipsIsArray pins the contract for a user with no
// orgs: the switcher must render an empty picker, so memberships serializes
// as [] — a null would deserialize as a missing list on the client.
func TestSessionEmptyMembershipsIsArray(t *testing.T) {
	r := newSessionRig(t, sessionConfig())
	userID := uuid.New()
	r.q.profiles[userID] = profileRec{email: "ada@test.dev"}
	token := r.plantToken(t, userID, r.clock.now.Add(r.cfg.LoginTTL))

	rec := r.postSession(t, `{"token":"`+token+`"}`)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"memberships":[]`,
		"an empty switcher list serializes as [] — never null")
}

// TestSessionCookiesOmitSecureWhenConfiguredOff covers the local-http
// escape: AUTH_COOKIE_SECURE=false drops the Secure attribute from both
// cookies while every other spec section 4 attribute stands.
func TestSessionCookiesOmitSecureWhenConfiguredOff(t *testing.T) {
	cfg := sessionConfig()
	cfg.CookieSecure = false
	r := newSessionRig(t, cfg)
	userID := uuid.New()
	r.q.profiles[userID] = profileRec{email: "ada@test.dev"}
	token := r.plantToken(t, userID, r.clock.now.Add(r.cfg.LoginTTL))

	rec := r.postSession(t, `{"token":"`+token+`"}`)

	require.Equal(t, http.StatusOK, rec.Code)
	for _, raw := range rec.Header().Values("Set-Cookie") {
		assert.NotContains(t, raw, "Secure",
			"CookieSecure=false must omit the attribute, got %q", raw)
	}
	access := findCookie(t, rec, "qp_access")
	require.NotNil(t, access)
	assert.False(t, access.Secure)
	assert.True(t, access.HttpOnly, "HttpOnly is not negotiable with the Secure flag")
}
