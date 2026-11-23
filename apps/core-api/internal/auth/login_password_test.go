package auth_test

// POST /auth/login password-path proofs (ADR-005 amendment 2026-11-16):
// `password` present — even empty — verifies against the stored argon2id
// hash and, on success, answers exactly like POST /auth/session: the
// qp_access/qp_refresh cookie pair around the profile+memberships switcher
// body. Unknown email, NULL hash and wrong password share one 401 body and
// every attempt spends both limiter buckets. `password` absent keeps the
// magic-link flow untouched — that regression is already pinned by
// login_test.go's proofs (uniform 200, staged send, bad-request table).
// Everything runs through auth.NewServer; the doubles live in
// login_fake_test.go (loginQuerier) and session_fake_test.go (profile/session
// records).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// TestLoginPasswordIssuesSession is the password path's happy path: the
// verify succeeds and the answer is the session endpoint's exact shape —
// switcher body, both cookies with their pinned attributes, a real JWT for
// the created session row — and none of the magic-link staging runs (no
// token, no outbox, no queued send).
func TestLoginPasswordIssuesSession(t *testing.T) {
	r := newRig(t, platform.LoginIPCapacity, platform.LoginEmailCapacity)
	userID := uuid.New()
	orgID := uuid.New()
	r.q.creds["ada@test.dev"] = credRec{userID: userID, hash: testenv.PasswordHashPHC(t, "s3cret-pilot")}
	r.q.profiles[userID] = profileRec{email: "ada@test.dev", fullName: "Ada Lovelace"}
	r.q.members["ada@test.dev"] = []platform.Membership{
		{OrgID: orgID, Name: "Acme Freight", Slug: "acme-freight", Role: platform.RoleOwner},
	}

	rec := r.postLogin(t, `{"email":" ADA@Test.dev ","password":"s3cret-pilot"}`)

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
	assert.Equal(t, authapi.OWNER, body.Memberships[0].Role)

	// The credential lookup saw the normalized email TWICE — the outside
	// read that fed the verify and the in-tx re-check under the user lock
	// (the stale-credential guard); the magic-link staging never ran — the
	// password path writes a session, not a token.
	require.Equal(t, []string{"ada@test.dev", "ada@test.dev"}, r.q.authFinds,
		"outside lookup plus the in-tx stale-credential re-read")
	assert.Empty(t, r.q.finds, "the password path must not run find_user_by_email")
	assert.Empty(t, r.q.creates)
	assert.Empty(t, r.q.outboxCreates)
	assert.Empty(t, r.enq.ids, "password login stages no mail send")

	// The issue-vs-revoke barrier (migration 0007): the user advisory lock
	// is taken before the session row exists — once by passwordLogin ahead
	// of the recheck and again, idempotently, by issueSession ahead of the
	// mint; both land before create_session in the order log.
	require.Equal(t, []uuid.UUID{userID, userID}, r.q.userLocks)
	require.Equal(t, []string{"lock", "lock", "create_session"}, r.q.order,
		"the user lock must be held before the session write")

	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 2, "exactly the spec section 4 pair is written")
	access := findCookie(t, rec, "qp_access")
	require.NotNil(t, access)
	assert.Equal(t, "/", access.Path)
	assert.True(t, access.HttpOnly)
	assert.True(t, access.Secure)
	assert.Equal(t, http.SameSiteLaxMode, access.SameSite)
	refresh := findCookie(t, rec, "qp_refresh")
	require.NotNil(t, refresh)
	assert.Equal(t, "/auth", refresh.Path)
	assert.True(t, refresh.HttpOnly)
	assert.True(t, refresh.Secure)
	assert.Equal(t, http.SameSiteStrictMode, refresh.SameSite)

	// The access cookie is a verifiable JWT bound to the stored session row;
	// Postgres kept the refresh token's SHA-256 hash, never the token.
	require.Len(t, r.q.sessions, 1)
	gotUser, gotSession, err := r.cfg.VerifyAccess(access.Value)
	require.NoError(t, err)
	assert.Equal(t, userID, gotUser)
	assert.Equal(t, r.q.sessions[0].sessionID, gotSession)
	assert.Equal(t, platform.HashToken(refresh.Value), r.q.sessions[0].hash,
		"create_session stored the cookie value's hash, not the value")
}

// TestLoginPasswordStaleCredentialAborts is the Devin finding's login
// side: a reset committing between the outside credential read and the
// in-tx issue must not let this login mint a session on the dead
// password. The fake's credsShift seam replays exactly that — first
// lookup serves the old PHC (the verify passes), the in-tx re-read under
// the user lock serves the reset's new one — and the mismatch answers
// the uniform 401 with no session and no cookie.
func TestLoginPasswordStaleCredentialAborts(t *testing.T) {
	r := newRig(t, platform.LoginIPCapacity, platform.LoginEmailCapacity)
	userID := uuid.New()
	r.q.creds["ada@test.dev"] = credRec{userID: userID, hash: testenv.PasswordHashPHC(t, "s3cret-pilot")}
	r.q.profiles[userID] = profileRec{email: "ada@test.dev", fullName: "Ada Lovelace"}
	r.q.credsShift["ada@test.dev"] = credRec{userID: userID, hash: testenv.PasswordHashPHC(t, "n3w-pilot-pass")}

	rec := r.postLogin(t, `{"email":"ada@test.dev","password":"s3cret-pilot"}`)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, `{"error":"unauthenticated"}`+"\n", rec.Body.String(),
		"a mid-flight credential change is indistinguishable from a wrong password")
	assert.Empty(t, rec.Header().Values("Set-Cookie"))
	assert.Empty(t, r.q.sessions, "the dead credential mints nothing")
	require.Equal(t, []uuid.UUID{userID}, r.q.userLocks,
		"the lock was taken — the abort happened inside the critical section")
	assert.Equal(t, []string{"lock"}, r.q.order, "nothing runs after the stale abort")
	assert.Len(t, r.q.authFinds, 2, "the outside read and the in-tx recheck")
}

// TestLoginPasswordUniform401 is the anti-enumeration proof for the
// credential path: an unknown email, a known email with a wrong password
// and a known user with no password set (NULL hash) all answer the session
// endpoint's byte-identical 401 — and none writes a cookie or a session.
func TestLoginPasswordUniform401(t *testing.T) {
	r := newRig(t, platform.LoginIPCapacity, platform.LoginEmailCapacity)
	userID := uuid.New()
	r.q.creds["ada@test.dev"] = credRec{userID: userID, hash: testenv.PasswordHashPHC(t, "s3cret-pilot")}
	r.q.creds["nopw@test.dev"] = credRec{userID: uuid.New()} // NULL hash

	unknown := r.postLogin(t, `{"email":"stranger@test.dev","password":"x"}`)
	wrong := r.postLogin(t, `{"email":"ada@test.dev","password":"wrong-horse"}`)
	nullHash := r.postLogin(t, `{"email":"nopw@test.dev","password":"s3cret-pilot"}`)

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"unknown email":  unknown,
		"wrong password": wrong,
		"null hash":      nullHash,
	} {
		assert.Equal(t, http.StatusUnauthorized, rec.Code, name)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"), name)
		assert.Equal(t, `{"error":"unauthenticated"}`+"\n", rec.Body.String(), name)
		assert.Empty(t, rec.Header().Values("Set-Cookie"), name+" must never write a cookie")
	}
	assert.Len(t, r.q.authFinds, 3, "every attempt paid the same credential lookup")
	assert.Empty(t, r.q.sessions, "a rejected attempt creates no session")
	assert.Empty(t, r.q.creates, "a rejected attempt stores no login token")
	assert.Empty(t, r.enq.ids)
}

// TestLoginPasswordFieldShapes pins the presence contract the raw-message
// binding exists for (Devin Review): the six wire shapes of the `password`
// member each get exactly one outcome — absent is the magic-link 200, a JSON
// string is a password attempt ("" just a wrong one), and `null` plus every
// non-string is a 400, never a link. Each case runs its own rig: six
// requests to one recipient would outspend the 5-per-window email bucket.
func TestLoginPasswordFieldShapes(t *testing.T) {
	seed := func(t *testing.T, r *rig, userID uuid.UUID) {
		r.q.users["ada@test.dev"] = userID
		r.q.creds["ada@test.dev"] = credRec{userID: userID, hash: testenv.PasswordHashPHC(t, "s3cret-pilot")}
		r.q.profiles[userID] = profileRec{email: "ada@test.dev", fullName: "Ada Lovelace"}
		r.q.members["ada@test.dev"] = []platform.Membership{
			{OrgID: uuid.New(), Name: "Acme Freight", Slug: "acme-freight", Role: platform.RoleOwner},
		}
	}

	t.Run("absent requests a magic link", func(t *testing.T) {
		r := newRig(t, platform.LoginIPCapacity, platform.LoginEmailCapacity)
		seed(t, r, uuid.New())
		rec := r.postLogin(t, `{"email":"ada@test.dev"}`)
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, `{"status":"ok"}`+"\n", rec.Body.String())
		assert.Len(t, r.q.outboxCreates, 1, "the magic-link staging ran")
		assert.Empty(t, r.q.authFinds, "the credential lookup did not")
	})

	t.Run("null is a bad request", func(t *testing.T) {
		r := newRig(t, platform.LoginIPCapacity, platform.LoginEmailCapacity)
		seed(t, r, uuid.New())
		rec := r.postLogin(t, `{"email":"ada@test.dev","password":null}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, `{"error":"bad_request"}`+"\n", rec.Body.String())
		assert.Empty(t, r.q.outboxCreates, "null must not become a magic link")
		assert.Empty(t, r.q.authFinds, "null is not a password attempt")
	})

	t.Run("empty string is a password attempt", func(t *testing.T) {
		r := newRig(t, platform.LoginIPCapacity, platform.LoginEmailCapacity)
		seed(t, r, uuid.New())
		rec := r.postLogin(t, `{"email":"ada@test.dev","password":""}`)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Equal(t, `{"error":"unauthenticated"}`+"\n", rec.Body.String())
		assert.Len(t, r.q.authFinds, 1, "the credential lookup ran")
		assert.Empty(t, r.q.outboxCreates)
	})

	t.Run("a correct password answers the session payload", func(t *testing.T) {
		r := newRig(t, platform.LoginIPCapacity, platform.LoginEmailCapacity)
		userID := uuid.New()
		seed(t, r, userID)
		rec := r.postLogin(t, `{"email":"ada@test.dev","password":"s3cret-pilot"}`)
		require.Equal(t, http.StatusOK, rec.Code)
		var body authapi.SessionResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, userID.String(), body.Profile.ID)
		assert.Len(t, rec.Result().Cookies(), 2)
		assert.Empty(t, r.q.outboxCreates)
	})

	t.Run("a number is a bad request", func(t *testing.T) {
		r := newRig(t, platform.LoginIPCapacity, platform.LoginEmailCapacity)
		seed(t, r, uuid.New())
		rec := r.postLogin(t, `{"email":"ada@test.dev","password":123}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, `{"error":"bad_request"}`+"\n", rec.Body.String())
		assert.Empty(t, r.q.outboxCreates)
		assert.Empty(t, r.q.authFinds)
	})

	t.Run("an object is a bad request", func(t *testing.T) {
		r := newRig(t, platform.LoginIPCapacity, platform.LoginEmailCapacity)
		seed(t, r, uuid.New())
		rec := r.postLogin(t, `{"email":"ada@test.dev","password":{}}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, `{"error":"bad_request"}`+"\n", rec.Body.String())
		assert.Empty(t, r.q.outboxCreates)
		assert.Empty(t, r.q.authFinds)
	})
}

// TestLoginPasswordEmptyStringRoutesToPasswordPath pins the presence
// contract: "password":"" is a password attempt (just a wrong one), not a
// magic-link request — so it answers 401 and stages no send.
func TestLoginPasswordEmptyStringRoutesToPasswordPath(t *testing.T) {
	r := newRig(t, platform.LoginIPCapacity, platform.LoginEmailCapacity)
	r.q.creds["ada@test.dev"] = credRec{userID: uuid.New(), hash: testenv.PasswordHashPHC(t, "s3cret-pilot")}

	rec := r.postLogin(t, `{"email":"ada@test.dev","password":""}`)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, `{"error":"unauthenticated"}`+"\n", rec.Body.String())
	assert.Empty(t, r.q.outboxCreates, "a password attempt never stages a magic-link send")
	assert.Empty(t, r.enq.ids)
}

// TestLoginPasswordAttemptsSpendBothLimiters proves a password attempt is a
// login attempt for the buckets: the per-email bucket (drained first here)
// denies the retry with the uniform 429 before the credential lookup runs —
// the same order the magic-link path pays.
func TestLoginPasswordAttemptsSpendBothLimiters(t *testing.T) {
	r := newRig(t, 100, 1)
	r.q.creds["ada@test.dev"] = credRec{userID: uuid.New(), hash: testenv.PasswordHashPHC(t, "s3cret-pilot")}

	require.Equal(t, http.StatusUnauthorized,
		r.postLogin(t, `{"email":"ada@test.dev","password":"wrong"}`).Code,
		"the first wrong-password attempt spends the email bucket")

	rec := r.postLogin(t, `{"email":"ada@test.dev","password":"s3cret-pilot"}`)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, `{"error":"rate_limited"}`+"\n", rec.Body.String())
	assert.Empty(t, rec.Header().Values("Set-Cookie"))
	assert.Len(t, r.q.authFinds, 1, "a denied attempt never reaches the credential lookup")

	// The IP bucket is spent the same way — at capacity 1 the second caller
	// from the same address is denied even for a different recipient.
	r2 := newRig(t, 1, 100)
	r2.q.creds["grace@test.dev"] = credRec{userID: uuid.New(), hash: testenv.PasswordHashPHC(t, "pw")}
	require.Equal(t, http.StatusUnauthorized,
		r2.postLogin(t, `{"email":"grace@test.dev","password":"wrong"}`).Code)
	assert.Equal(t, http.StatusTooManyRequests,
		r2.postLogin(t, `{"email":"ada@test.dev","password":"pw"}`).Code,
		"the IP bucket denies the second attempt from the same caller")
	assert.Len(t, r2.q.authFinds, 1)
}

// TestLoginPasswordFailuresAreUniform500 pins the credential path's driver
// failures to the same {"error":"internal"} every other route emits — a
// server fault is not an enumeration signal, and no cookie may leak.
func TestLoginPasswordFailuresAreUniform500(t *testing.T) {
	r := newRig(t, platform.LoginIPCapacity, platform.LoginEmailCapacity)
	r.q.creds["ada@test.dev"] = credRec{userID: uuid.New(), hash: testenv.PasswordHashPHC(t, "s3cret-pilot")}
	r.q.fail["get_user_auth_by_email"] = errBoom

	rec := r.postLogin(t, `{"email":"ada@test.dev","password":"s3cret-pilot"}`)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, `{"error":"internal"}`+"\n", rec.Body.String())
	assert.Empty(t, rec.Header().Values("Set-Cookie"))
}
