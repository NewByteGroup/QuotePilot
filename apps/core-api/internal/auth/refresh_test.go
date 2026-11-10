package auth_test

// POST /auth/refresh proofs (spec 2026-11-10-tenant-resolution-api sections
// 4, 7 and 8): a live cookie earns the rotated pair — a fresh access JWT on
// the successor's session id plus a new opaque refresh scoped Path=/auth —
// while absent, unknown, expired, revoked and replayed cookies all answer
// the byte-identical 401 with both cookies cleared. Reuse additionally
// kills the whole family server-side (the committed successor dies with the
// replayed ancestor), a driver or signing failure is the uniform 500 with
// no Set-Cookie at all. Everything runs through auth.NewServer — the same
// strict handler, middleware and error hooks production mounts; the doubles
// live in refresh_fake_test.go.

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// qpRefreshCookie builds the inbound Cookie header — the planted token
// travels exactly as a browser would send it.
func qpRefreshCookie(value string) *http.Cookie {
	return &http.Cookie{Name: "qp_refresh", Value: value}
}

// requireClearedPair asserts the response expired both spec section 4
// cookies: same names and paths as the set versions, empty value, MaxAge
// at or below zero — the browser-side delete spec section 8 mandates.
func requireClearedPair(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	access := findCookie(t, rec, "qp_access")
	require.NotNil(t, access, "a rejection must expire the access cookie")
	assert.Equal(t, "", access.Value)
	assert.Equal(t, "/", access.Path)
	assert.LessOrEqual(t, access.MaxAge, 0)
	assert.True(t, access.HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, access.SameSite)
	refresh := findCookie(t, rec, "qp_refresh")
	require.NotNil(t, refresh, "a rejection must expire the refresh cookie")
	assert.Equal(t, "", refresh.Value)
	assert.Equal(t, "/auth", refresh.Path)
	assert.True(t, refresh.HttpOnly)
	assert.Equal(t, http.SameSiteStrictMode, refresh.SameSite)
	assert.LessOrEqual(t, refresh.MaxAge, 0)
}

// TestRefreshRotatesCookiePair is the happy path: the 200 carries no body,
// both cookies land with the exact spec section 4 attributes, the access
// value verifies as a real JWT bound to the successor session, and the
// refresh value is a fresh opaque token whose hash — not the token — is
// what rotate_session stored on a row in the same family at now+RefreshTTL.
func TestRefreshRotatesCookiePair(t *testing.T) {
	r := newRefreshRig(t, sessionConfig())
	userID := uuid.New()
	familyID := uuid.New()
	token := r.plantGeneration(t, userID, familyID, r.clock.now.Add(r.cfg.RefreshTTL))

	rec := r.postRefresh(t, qpRefreshCookie(token))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Body.String(), "the contract's 200 carries no body")

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

	// The store saw hashes only: the presented generation's SHA-256 looked
	// the row up, the fresh successor's SHA-256 is what got stored.
	require.Len(t, r.q.rotations, 1)
	assert.Equal(t, platform.HashToken(token), r.q.rotations[0].presentedHash,
		"rotate_session looks up by hash, not by the raw cookie value")
	assert.Equal(t, platform.HashToken(refresh.Value), r.q.rotations[0].newHash,
		"the successor row holds the new cookie value's hash")
	assert.NotEqual(t, refresh.Value, r.q.rotations[0].newHash,
		"the hash must never cross the wire")
	assert.Equal(t, r.clock.now.Add(r.cfg.RefreshTTL), r.q.rotations[0].newExpiresAt)

	// The presented generation is stamped exchanged; the successor descends
	// in the same family, live and unmarked.
	presented := r.q.sessions[platform.HashToken(token)]
	require.NotNil(t, presented)
	require.NotNil(t, presented.rotatedAt, "the exchanged generation is stamped rotated_at")
	successor := r.q.sessions[platform.HashToken(refresh.Value)]
	require.NotNil(t, successor)
	assert.Equal(t, familyID, successor.familyID, "rotation descends within one family")
	assert.Equal(t, userID, successor.userID)
	assert.Equal(t, r.clock.now.Add(r.cfg.RefreshTTL), successor.expiresAt)
	assert.Nil(t, successor.rotatedAt)
	assert.Nil(t, successor.revokedAt)

	// The refresh cookie is a 32-byte base64url opaque token, and the
	// access cookie a verifiable JWT bound to the successor's session id.
	raw, err := base64.RawURLEncoding.DecodeString(refresh.Value)
	require.NoError(t, err)
	assert.Len(t, raw, 32, "the refresh cookie carries a 32-byte opaque token")
	gotUser, gotSession, err := r.cfg.VerifyAccess(access.Value)
	require.NoError(t, err)
	assert.Equal(t, userID, gotUser)
	assert.Equal(t, successor.id, gotSession,
		"the JWT's sid is the session row rotate_session returned")
}

// TestRefreshReuseRevokesFamily is the reuse proof: presenting an
// already-exchanged generation answers the uniform 401 with the cleared
// pair AND kills the whole family in the same call — the committed
// successor dies with the replayed ancestor, so a stolen token cannot
// outlive its own rotation (spec sections 4 and 9).
func TestRefreshReuseRevokesFamily(t *testing.T) {
	r := newRefreshRig(t, sessionConfig())
	userID := uuid.New()
	familyID := uuid.New()
	gen0 := r.plantGeneration(t, userID, familyID, r.clock.now.Add(r.cfg.RefreshTTL))

	first := r.postRefresh(t, qpRefreshCookie(gen0))
	require.Equal(t, http.StatusOK, first.Code)
	gen1 := findCookie(t, first, "qp_refresh").Value

	replay := r.postRefresh(t, qpRefreshCookie(gen0))
	assert.Equal(t, http.StatusUnauthorized, replay.Code)
	assert.Equal(t, "application/json", replay.Header().Get("Content-Type"))
	assert.Equal(t, `{"error":"unauthenticated"}`+"\n", replay.Body.String())
	requireClearedPair(t, replay)

	// The family kill is total: the replayed generation and its committed
	// successor are both dead — so the successor's own cookie, minted
	// seconds ago, now hits the revoked branch and dies the same way.
	for hash, s := range r.q.sessions {
		assert.Equal(t, familyID, s.familyID)
		assert.NotNil(t, s.revokedAt, "family member %s must be revoked", hash)
	}
	dead := r.postRefresh(t, qpRefreshCookie(gen1))
	assert.Equal(t, http.StatusUnauthorized, dead.Code,
		"the committed successor is dead with its family")
	assert.Equal(t, `{"error":"unauthenticated"}`+"\n", dead.Body.String())
	requireClearedPair(t, dead)
}

// TestRefreshUniform401 is the no-enumeration proof: absent, unknown,
// expired and revoked cookies answer byte-identically — status, body and
// the clearing Set-Cookie pair — so the wire cannot tell a dead token from
// a missing one. Absent alone never reaches the store.
func TestRefreshUniform401(t *testing.T) {
	r := newRefreshRig(t, sessionConfig())
	userID := uuid.New()
	familyID := uuid.New()
	expired := r.plantGeneration(t, userID, familyID, r.clock.now.Add(-time.Second))
	revoked := r.plantGeneration(t, userID, familyID, r.clock.now.Add(r.cfg.RefreshTTL))
	now := r.clock.now
	r.q.sessions[platform.HashToken(revoked)].revokedAt = &now

	recs := []*httptest.ResponseRecorder{
		r.postRefresh(t, nil), // absent
		r.postRefresh(t, qpRefreshCookie(strings.Repeat("x", 43))), // unknown
		r.postRefresh(t, qpRefreshCookie(expired)),                 // expired
		r.postRefresh(t, qpRefreshCookie(revoked)),                 // revoked
	}
	for _, rec := range recs {
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
		assert.Equal(t, `{"error":"unauthenticated"}`+"\n", rec.Body.String(),
			"absent, unknown, expired and revoked must answer byte-identically")
		requireClearedPair(t, rec)
	}
	want := recs[0].Header().Values("Set-Cookie")
	for _, rec := range recs[1:] {
		assert.Equal(t, want, rec.Header().Values("Set-Cookie"),
			"the clearing pair is byte-identical across rejection cases")
	}
	assert.Len(t, r.q.rotations, 3,
		"absent never reaches the store; the other three pay the same lookup")
}

// TestRefreshDriverErrorIsUniform500 proves a store fault answers the same
// 500 {"error":"internal"} as every other handler-side failure — and that
// no Set-Cookie header leaks onto the error path: cookies live only inside
// typed response objects, so a bare error can never write one.
func TestRefreshDriverErrorIsUniform500(t *testing.T) {
	r := newRefreshRig(t, sessionConfig())
	token := r.plantGeneration(t, uuid.New(), uuid.New(), r.clock.now.Add(r.cfg.RefreshTTL))
	r.q.fail["rotate_session"] = errBoom

	rec := r.postRefresh(t, qpRefreshCookie(token))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, `{"error":"internal"}`+"\n", rec.Body.String())
	assert.Empty(t, rec.Header().Values("Set-Cookie"),
		"the error path must never leak a Set-Cookie header")
}

// TestRefreshSignFailureOrphansRotation pins the known stranding (C2
// review): rotate_session commits the successor BEFORE SignAccess runs —
// the successor's session id is the JWT's sid claim — so a signing key that
// cannot sign leaves the client holding an already-exchanged cookie behind
// the uniform 500. The successor is inert (no cookie for it ever left the
// handler); replaying the predecessor afterwards is the reuse case.
func TestRefreshSignFailureOrphansRotation(t *testing.T) {
	cfg := sessionConfig()
	cfg.ActiveKID = "ghost"
	r := newRefreshRig(t, cfg)
	userID := uuid.New()
	token := r.plantGeneration(t, userID, uuid.New(), r.clock.now.Add(r.cfg.RefreshTTL))

	rec := r.postRefresh(t, qpRefreshCookie(token))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, `{"error":"internal"}`+"\n", rec.Body.String())
	assert.Empty(t, rec.Header().Values("Set-Cookie"))

	// The rotation committed anyway: the presented row is stamped and the
	// successor row exists — orphaned, with no cookie ever minted for it.
	require.Len(t, r.q.rotations, 1)
	presented := r.q.sessions[platform.HashToken(token)]
	require.NotNil(t, presented)
	assert.NotNil(t, presented.rotatedAt, "the exchange was stamped before the sign failed")
	successor := r.q.sessions[r.q.rotations[0].newHash]
	require.NotNil(t, successor, "the successor row was committed before the sign failed")
	assert.Equal(t, presented.familyID, successor.familyID)
	assert.Equal(t, userID, successor.userID)
	assert.Nil(t, successor.rotatedAt)
	assert.Nil(t, successor.revokedAt)
}

// TestRefreshCookiesOmitSecureWhenConfiguredOff covers the local-http
// escape: AUTH_COOKIE_SECURE=false drops the Secure attribute from the
// rotated pair — and from the cleared pair on the rejection path — while
// every other spec section 4 attribute stands.
func TestRefreshCookiesOmitSecureWhenConfiguredOff(t *testing.T) {
	cfg := sessionConfig()
	cfg.CookieSecure = false
	r := newRefreshRig(t, cfg)
	token := r.plantGeneration(t, uuid.New(), uuid.New(), r.clock.now.Add(r.cfg.RefreshTTL))

	rec := r.postRefresh(t, qpRefreshCookie(token))
	require.Equal(t, http.StatusOK, rec.Code)
	for _, raw := range rec.Header().Values("Set-Cookie") {
		assert.NotContains(t, raw, "Secure",
			"CookieSecure=false must omit the attribute, got %q", raw)
	}
	access := findCookie(t, rec, "qp_access")
	require.NotNil(t, access)
	assert.False(t, access.Secure)
	assert.True(t, access.HttpOnly, "HttpOnly is not negotiable with the Secure flag")

	rejected := r.postRefresh(t, qpRefreshCookie(token))
	require.Equal(t, http.StatusUnauthorized, rejected.Code)
	for _, raw := range rejected.Header().Values("Set-Cookie") {
		assert.NotContains(t, raw, "Secure",
			"the cleared pair honors the flag too, got %q", raw)
	}
}
