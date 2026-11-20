package auth_test

// Forgot/set-password proofs through the mounted server (ADR-005 amendment
// 2026-11-16, migration 0007): POST /auth/password-reset keeps the
// magic-link request's uniform-200 anti-enumeration verbatim while staging
// a purpose='password_reset' token and the set-password link; POST
// /auth/password consumes that token in one transaction that writes the
// argon2id PHC and revokes the user's sessions — and answers no session.
// The token classes are not interchangeable in either direction: the
// cross-purpose proofs close the file. The doubles live in
// login_fake_test.go — the real platform.Store runs over them.

import (
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

// TestPasswordResetUniform200 is the anti-enumeration proof — the login
// magic-link contract verbatim: a provisioned email and an unknown one get
// the byte-identical 200 {"status":"ok"}, and only the known side stages a
// reset token, an outbox row and the queued send.
func TestPasswordResetUniform200(t *testing.T) {
	r := newRig(t, platform.LoginIPCapacity, platform.LoginEmailCapacity)
	userID := uuid.New()
	r.q.users["ada@test.dev"] = userID

	known := r.postPath(t, "/auth/password-reset", `{"email":" ADA@Test.dev "}`)
	unknown := r.postPath(t, "/auth/password-reset", `{"email":"stranger@test.dev"}`)

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"known email":   known,
		"unknown email": unknown,
	} {
		assert.Equal(t, http.StatusOK, rec.Code, name)
		assert.Equal(t, `{"status":"ok"}`+"\n", rec.Body.String(), name)
		assert.Empty(t, rec.Header().Values("Set-Cookie"), name+" must never set a cookie")
	}

	// Only the provisioned side staged a send — a password_reset token,
	// never a login token, with the reset link and the enqueued job.
	require.Len(t, r.q.resetCreates, 1, "one reset token staged for the known email")
	assert.Equal(t, userID, r.q.resetCreates[0].userID)
	assert.Empty(t, r.q.creates, "the reset path must never mint a login token")
	require.Len(t, r.q.outboxCreates, 1)
	rec := r.q.outboxCreates[0]
	assert.Equal(t, r.q.resetCreates[0].id, rec.tokenID)
	assert.True(t, strings.HasPrefix(rec.link, "https://web.test/auth/set-password?token="),
		"the outbox link targets the set-password page, got %q", rec.link)
	assert.Len(t, r.enq.ids, 1, "the staged send queued one auth_mail_send job")
	assert.Equal(t, r.q.outboxCreates[0].outboxID, r.enq.ids[0],
		"the job carries the outbox id — never the link")
}

// TestPasswordResetSpendsBothBuckets is login's limiter contract on the
// reset route: the email bucket denies the sixth request to one recipient
// and the IP bucket denies the thirty-first request from one caller.
func TestPasswordResetSpendsBothBuckets(t *testing.T) {
	r := newRig(t, platform.LoginIPCapacity, 1)
	r.q.users["ada@test.dev"] = uuid.New()

	require.Equal(t, http.StatusOK,
		r.postPath(t, "/auth/password-reset", `{"email":"ada@test.dev"}`).Code)
	rec := r.postPath(t, "/auth/password-reset", `{"email":"ada@test.dev"}`)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, `{"error":"rate_limited"}`+"\n", rec.Body.String())
	assert.Len(t, r.q.resetCreates, 1, "a denied request stages nothing")

	r2 := newRig(t, 1, platform.LoginEmailCapacity)
	require.Equal(t, http.StatusOK,
		r2.postPath(t, "/auth/password-reset", `{"email":"ada@test.dev"}`).Code)
	assert.Equal(t, http.StatusTooManyRequests,
		r2.postPath(t, "/auth/password-reset", `{"email":"grace@test.dev"}`).Code,
		"the IP bucket denies the second request from the same caller")
}

// TestPasswordSetSuccess is the amendment's end state: a live reset token
// sets the argon2id hash and kills the user's sessions in one commit —
// then the 200 carries no session, no cookie, nothing to authenticate
// with. The user logs in through the password path next.
func TestPasswordSetSuccess(t *testing.T) {
	r := newRig(t, platform.LoginIPCapacity, platform.LoginEmailCapacity)
	userID := uuid.New()
	r.q.creds["ada@test.dev"] = credRec{userID: userID, hash: ""} // no password set yet
	token := r.plantResetToken(t, userID, r.clock.Now().Add(15*time.Minute))

	rec := r.postPath(t, "/auth/password",
		`{"token":"`+token+`","password":"s3cret-pilot"}`)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, `{"status":"ok"}`+"\n", rec.Body.String())
	assert.Empty(t, rec.Result().Cookies(), "the set-password page does not start a session")
	assert.Empty(t, r.q.sessions, "no session row is created")

	require.Len(t, r.q.passwordSets, 1, "set_user_password ran exactly once")
	assert.Equal(t, userID, r.q.passwordSets[0].userID)
	assert.True(t, strings.HasPrefix(r.q.passwordSets[0].hash, "$argon2id$v=19$"),
		"only the argon2id PHC reaches the store, got %q", r.q.passwordSets[0].hash)
	assert.NotContains(t, r.q.passwordSets[0].hash, "s3cret-pilot",
		"the raw password never reaches the store")
	assert.Equal(t, []uuid.UUID{userID}, r.q.sessionRevokes,
		"the reset revokes every live session of the user")
	assert.True(t, r.q.tokens[platform.HashToken(token)].consumed,
		"the token was spent inside the same commit")
}

// TestPasswordSetUniform401 is the dead-token proof: unknown, already
// consumed and expired tokens all answer the session endpoint's
// byte-identical 401, and none writes a credential or revokes a session.
func TestPasswordSetUniform401(t *testing.T) {
	r := newRig(t, platform.LoginIPCapacity, platform.LoginEmailCapacity)
	userID := uuid.New()
	token := r.plantResetToken(t, userID, r.clock.Now().Add(15*time.Minute))
	expired := r.plantResetToken(t, userID, r.clock.Now().Add(-time.Minute))
	unknown := "definitely-not-a-real-token"

	// Consume the live one first, so its second use is the reuse case.
	require.Equal(t, http.StatusOK, r.postPath(t, "/auth/password",
		`{"token":"`+token+`","password":"s3cret-pilot"}`).Code)

	for name, tok := range map[string]string{
		"unknown":  unknown,
		"consumed": token,
		"expired":  expired,
	} {
		rec := r.postPath(t, "/auth/password",
			`{"token":"`+tok+`","password":"s3cret-pilot"}`)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, name)
		assert.Equal(t, `{"error":"unauthenticated"}`+"\n", rec.Body.String(), name)
		assert.Empty(t, rec.Header().Values("Set-Cookie"), name)
	}
	assert.Len(t, r.q.passwordSets, 1, "only the first, live consume wrote a credential")
	assert.Len(t, r.q.sessionRevokes, 1)
	assert.Empty(t, r.q.sessions)
}

// TestPasswordSetRequestShapes pins the 400s: a missing or empty token and
// the 8..128 password policy (7 and 129 both die before any argon2id run —
// the binder does not enforce the schema bounds).
func TestPasswordSetRequestShapes(t *testing.T) {
	r := newRig(t, platform.LoginIPCapacity, platform.LoginEmailCapacity)
	for name, body := range map[string]string{
		"missing token":      `{"password":"s3cret-pilot"}`,
		"empty token":        `{"token":"","password":"s3cret-pilot"}`,
		"password too short": `{"token":"x","password":"1234567"}`,
		"password too long":  `{"token":"x","password":"` + strings.Repeat("a", 129) + `"}`,
	} {
		rec := r.postPath(t, "/auth/password", body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, name)
		assert.Equal(t, `{"error":"bad_request"}`+"\n", rec.Body.String(), name)
	}
	assert.Empty(t, r.q.resetConsumes, "a rejected request never reaches the consume")
	assert.Empty(t, r.q.passwordSets)
}

// TestPasswordSetSpendsIPBucket: the set-password route has no email key,
// so only the IP bucket meters it — second request from the same caller at
// capacity 1 is the uniform 429 and pays no consume.
func TestPasswordSetSpendsIPBucket(t *testing.T) {
	r := newRig(t, 1, platform.LoginEmailCapacity)
	userID := uuid.New()
	token := r.plantResetToken(t, userID, r.clock.Now().Add(15*time.Minute))

	require.Equal(t, http.StatusOK, r.postPath(t, "/auth/password",
		`{"token":"`+token+`","password":"s3cret-pilot"}`).Code)
	rec := r.postPath(t, "/auth/password",
		`{"token":"`+token+`","password":"s3cret-pilot"}`)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, `{"error":"rate_limited"}`+"\n", rec.Body.String())
	assert.Len(t, r.q.resetConsumes, 1, "the denied retry never reached the consume")
}

// TestTokenPurposesDoNotExchange is the migration-0007 invariant in both
// directions: a reset token presented to POST /auth/session misses the
// login-purpose consume, and a login token presented to POST /auth/password
// misses the reset-purpose consume — both are the uniform 401, and neither
// mints a session nor writes a credential.
func TestTokenPurposesDoNotExchange(t *testing.T) {
	r := newRig(t, platform.LoginIPCapacity, platform.LoginEmailCapacity)
	userID := uuid.New()
	resetToken := r.plantResetToken(t, userID, r.clock.Now().Add(15*time.Minute))
	loginToken := r.plantTokenPurpose(t, userID, r.clock.Now().Add(15*time.Minute), platform.MailPurposeLogin)

	// Reset token at the session exchange: consume_login_token's purpose
	// filter makes it the same miss as a dead link — 401, no cookies.
	rec := r.postPath(t, "/auth/session", `{"token":"`+resetToken+`"}`)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, `{"error":"unauthenticated"}`+"\n", rec.Body.String())
	assert.Empty(t, rec.Header().Values("Set-Cookie"))
	assert.Empty(t, r.q.sessions, "a reset token must never mint a session")

	// Login token at the set-password endpoint: consume_password_reset_token's
	// filter is the symmetric miss — 401, no credential write, no revoke.
	rec = r.postPath(t, "/auth/password",
		`{"token":"`+loginToken+`","password":"s3cret-pilot"}`)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, `{"error":"unauthenticated"}`+"\n", rec.Body.String())
	assert.Empty(t, r.q.passwordSets, "a magic link must never set a password")
	assert.Empty(t, r.q.sessionRevokes)

	// Neither token was consumed by the wrong consume.
	assert.False(t, r.q.tokens[platform.HashToken(resetToken)].consumed)
	assert.False(t, r.q.tokens[platform.HashToken(loginToken)].consumed)
}
