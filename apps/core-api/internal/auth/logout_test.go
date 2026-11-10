package auth_test

// POST /auth/logout proofs (spec 2026-11-10-tenant-resolution-api sections
// 4, 8 and 9): one presented qp_refresh cookie revokes its whole session
// family — every generation dies, live and already-rotated alike, so the
// next refresh answers the uniform 401 — while every non-fault call
// answers the same 204 with both spec section 4 cookies cleared: absent,
// unknown and already-dead cookies are indistinguishable on the wire (no
// validity oracle — the store's boolean feeds metrics only). The only
// non-204 is a driver fault: the uniform 500 with NO Set-Cookie, because
// clearing on a failed revoke would log the client out of a live family.
// Everything runs through auth.NewServer — the same strict handler,
// middleware and error hooks production mounts; the doubles live in
// refresh_fake_test.go.

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// TestLogoutRevokesWholeFamily is the happy path: the 204 carries no body,
// both spec section 4 cookies are cleared, and the family kill is total —
// the presented generation and its rotated ancestor are stamped together,
// so the successor's own cookie dies at the next refresh (spec section 9's
// logout → refresh → 401). The store saw the SHA-256 hash only.
func TestLogoutRevokesWholeFamily(t *testing.T) {
	r := newRefreshRig(t, sessionConfig())
	userID := uuid.New()
	familyID := uuid.New()
	expiresAt := r.clock.now.Add(r.cfg.RefreshTTL)
	gen0 := r.plantGeneration(t, userID, familyID, expiresAt)
	gen1 := r.plantGeneration(t, userID, familyID, expiresAt)
	now := r.clock.now
	r.q.sessions[platform.HashToken(gen0)].rotatedAt = &now // the ancestor a rotation left

	rec := r.postLogout(t, qpRefreshCookie(gen1))

	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String(), "the contract's 204 carries no body")
	requireClearedPair(t, rec)

	require.Len(t, r.q.revocations, 1)
	assert.Equal(t, platform.HashToken(gen1), r.q.revocations[0],
		"revoke_session_family_by_hash looks up by hash, not the raw cookie")
	assert.NotEqual(t, gen1, r.q.revocations[0], "the raw token must never reach the store")
	for hash, s := range r.q.sessions {
		assert.Equal(t, familyID, s.familyID)
		assert.NotNil(t, s.revokedAt, "family member %s must be revoked", hash)
	}

	dead := r.postRefresh(t, qpRefreshCookie(gen1))
	assert.Equal(t, http.StatusUnauthorized, dead.Code,
		"a revoked family answers the refresh 401 (spec section 9)")
}

// TestLogoutAbsentCookieSkipsStore proves a jar without qp_refresh still
// earns the 204 and the cleared pair — and never pays the store call: the
// middleware plants nothing, so the handler's ok=false branch answers
// before any hash could be computed.
func TestLogoutAbsentCookieSkipsStore(t *testing.T) {
	r := newRefreshRig(t, sessionConfig())

	rec := r.postLogout(t, nil)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String())
	requireClearedPair(t, rec)
	assert.Empty(t, r.q.revocations, "an absent cookie must never reach the store")
	assert.Empty(t, r.q.rotations)
}

// TestLogoutUnknownCookieClearsAnyway is the no-oracle proof: a hash no
// family owns answers the exact response a live revoke gave — status, body
// and the clearing pair byte-identical — while the store still paid the
// lookup and simply found nothing.
func TestLogoutUnknownCookieClearsAnyway(t *testing.T) {
	r := newRefreshRig(t, sessionConfig())
	live := r.plantGeneration(t, uuid.New(), uuid.New(), r.clock.now.Add(r.cfg.RefreshTTL))

	liveRec := r.postLogout(t, qpRefreshCookie(live))
	require.Equal(t, http.StatusNoContent, liveRec.Code)

	unknown := r.postLogout(t, qpRefreshCookie(strings.Repeat("x", 43)))
	assert.Equal(t, http.StatusNoContent, unknown.Code)
	assert.Equal(t, liveRec.Body.String(), unknown.Body.String())
	assert.Equal(t, liveRec.Header().Values("Set-Cookie"), unknown.Header().Values("Set-Cookie"),
		"unknown and live cookies must answer byte-identically")
	requireClearedPair(t, unknown)
	assert.Len(t, r.q.revocations, 2, "the unknown hash still pays the lookup — its false is unobservable")
}

// TestLogoutRevokedFamilyClearsAnyway covers the already-dead family: the
// function resolves the hash but updates nothing, so its boolean is false
// — indistinguishable on the wire from the live kill.
func TestLogoutRevokedFamilyClearsAnyway(t *testing.T) {
	r := newRefreshRig(t, sessionConfig())
	familyID := uuid.New()
	token := r.plantGeneration(t, uuid.New(), familyID, r.clock.now.Add(r.cfg.RefreshTTL))
	now := r.clock.now
	r.q.sessions[platform.HashToken(token)].revokedAt = &now

	rec := r.postLogout(t, qpRefreshCookie(token))

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String())
	requireClearedPair(t, rec)
	assert.Len(t, r.q.revocations, 1, "the dead family still pays the lookup")
}

// TestLogoutExpiredCookieStillRevokesFamily pins the function's
// expiry-blind semantics: revoke_session_family_by_hash resolves the
// family without checking expires_at — unlike rotate_session — so an
// already-dead cookie still condemns its family.
func TestLogoutExpiredCookieStillRevokesFamily(t *testing.T) {
	r := newRefreshRig(t, sessionConfig())
	familyID := uuid.New()
	expired := r.plantGeneration(t, uuid.New(), familyID, r.clock.now.Add(-time.Second))

	rec := r.postLogout(t, qpRefreshCookie(expired))

	assert.Equal(t, http.StatusNoContent, rec.Code)
	requireClearedPair(t, rec)
	require.Len(t, r.q.revocations, 1)
	assert.NotNil(t, r.q.sessions[platform.HashToken(expired)].revokedAt,
		"logout's kill is expiry-blind — an expired cookie still resolves its family")
}

// TestLogoutDriverErrorKeepsCookies proves the only non-204: a store fault
// is the uniform 500 with no Set-Cookie at all — clearing on a failed
// revoke would log the client out of a family that is still live (the same
// rule refresh's 500 follows).
func TestLogoutDriverErrorKeepsCookies(t *testing.T) {
	r := newRefreshRig(t, sessionConfig())
	token := r.plantGeneration(t, uuid.New(), uuid.New(), r.clock.now.Add(r.cfg.RefreshTTL))
	r.q.fail["revoke_session_family_by_hash"] = errBoom

	rec := r.postLogout(t, qpRefreshCookie(token))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	assert.Equal(t, `{"error":"internal"}`+"\n", rec.Body.String())
	assert.Empty(t, rec.Header().Values("Set-Cookie"),
		"a failed revoke must not log the client out of a live family")
	assert.Empty(t, r.q.revocations, "a driver fault registers no revocation")
	assert.Nil(t, r.q.sessions[platform.HashToken(token)].revokedAt,
		"the family survives the fault")
}

// TestLogoutCookiesOmitSecureWhenConfiguredOff covers the local-http
// escape: AUTH_COOKIE_SECURE=false drops the Secure attribute from the
// cleared pair while every other spec section 4 attribute stands.
func TestLogoutCookiesOmitSecureWhenConfiguredOff(t *testing.T) {
	cfg := sessionConfig()
	cfg.CookieSecure = false
	r := newRefreshRig(t, cfg)
	token := r.plantGeneration(t, uuid.New(), uuid.New(), r.clock.now.Add(r.cfg.RefreshTTL))

	rec := r.postLogout(t, qpRefreshCookie(token))

	require.Equal(t, http.StatusNoContent, rec.Code)
	for _, raw := range rec.Header().Values("Set-Cookie") {
		assert.NotContains(t, raw, "Secure",
			"CookieSecure=false must omit the attribute, got %q", raw)
	}
	requireClearedPair(t, rec)
}
