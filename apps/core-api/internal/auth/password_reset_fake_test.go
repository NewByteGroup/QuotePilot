package auth_test

// The loginQuerier halves that belong to the forgot/set-password pair
// (migration 0007): the reset-token create/consume twins, the credential
// write and the two revokes, the per-user advisory lock every issuer and
// the reset share, and the rig helpers the reset proofs use. The doubles
// also append to loginQuerier.order — the sequence the lock-ordering
// assertions read (consume → lock → write → revokes, and lock →
// create_session on the issue paths).

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// resetCreateRow answers SELECT app.create_password_reset_token($1,$2,$3):
// createRow's reset twin — it lands a purpose='password_reset' row in the
// shared token table, exactly what migration 0007's function inserts.
func (q *loginQuerier) resetCreateRow(args []any) pgx.Row {
	return stubRow{scan: func(dest ...any) error {
		if err := q.fail["create_password_reset_token"]; err != nil {
			return err
		}
		call := createCall{
			userID:    args[0].(uuid.UUID),
			hash:      args[1].(string),
			expiresAt: args[2].(time.Time),
			id:        uuid.New(),
		}
		q.resetCreates = append(q.resetCreates, call)
		q.tokens[call.hash] = &tokenRec{
			userID:    call.userID,
			expiresAt: call.expiresAt,
			purpose:   platform.MailPurposePasswordReset,
		}
		*(dest[0].(*uuid.UUID)) = call.id
		return nil
	}}
}

// resetConsumeRow answers SELECT app.consume_password_reset_token($1) with
// the function's atomic semantics: the purpose filter means a login token
// scans NULL alongside the consumed/expired/unknown cases — the
// (Nil, false, nil) miss the Store maps.
func (q *loginQuerier) resetConsumeRow(args []any) pgx.Row {
	return stubRow{scan: func(dest ...any) error {
		if err := q.fail["consume_password_reset_token"]; err != nil {
			return err
		}
		hash := args[0].(string)
		q.resetConsumes = append(q.resetConsumes, hash)
		q.order = append(q.order, "consume_reset")
		out := dest[0].(**uuid.UUID)
		tok, ok := q.tokens[hash]
		if !ok || tok.purpose != platform.MailPurposePasswordReset || tok.consumed || !q.now().Before(tok.expiresAt) {
			*out = nil
			return nil
		}
		tok.consumed = true
		*out = &tok.userID
		return nil
	}}
}

// userLockRow answers SELECT 1 FROM pg_catalog.pg_advisory_xact_lock(...)
// — migration 0007's issue-vs-revoke barrier. The fake cannot serialize
// anything, so it does the next best thing: it RECORDS the acquisition
// (user id and position in the order log) so the tests assert the lock
// was taken before the writes it guards on every path that must hold it.
func (q *loginQuerier) userLockRow(args []any) pgx.Row {
	return stubRow{scan: func(dest ...any) error {
		if err := q.fail["pg_advisory_xact_lock"]; err != nil {
			return err
		}
		q.userLocks = append(q.userLocks, args[0].(uuid.UUID))
		q.order = append(q.order, "lock")
		*(dest[0].(*int)) = 1
		return nil
	}}
}

// passwordSetRow answers SELECT 1 FROM app.set_user_password($1, $2): like
// the plpgsql function it refuses a blank hash — committing an empty
// credential would lock the account — and records the (user, PHC) write.
func (q *loginQuerier) passwordSetRow(args []any) pgx.Row {
	return stubRow{scan: func(dest ...any) error {
		if err := q.fail["set_user_password"]; err != nil {
			return err
		}
		hash := args[1].(string)
		if strings.TrimSpace(hash) == "" {
			return errors.New("set_user_password: password_hash must be a non-empty string")
		}
		q.passwordSets = append(q.passwordSets, passwordSet{userID: args[0].(uuid.UUID), hash: hash})
		q.order = append(q.order, "set_pw")
		*(dest[0].(*int)) = 1
		return nil
	}}
}

// sessionRevokeRow answers SELECT 1 FROM app.revoke_user_sessions($1):
// records the user whose live sessions the reset killed — the OWASP leg of
// the set-password transaction. The real function also takes the user
// advisory lock itself; the Go path already holds it, so the fake models
// the write, and userLockRow models the barrier.
func (q *loginQuerier) sessionRevokeRow(args []any) pgx.Row {
	return stubRow{scan: func(dest ...any) error {
		if err := q.fail["revoke_user_sessions"]; err != nil {
			return err
		}
		userID := args[0].(uuid.UUID)
		q.sessionRevokes = append(q.sessionRevokes, userID)
		q.order = append(q.order, "revoke_sessions")
		*(dest[0].(*int)) = 1
		return nil
	}}
}

// pendingRevokeRow answers SELECT 1 FROM app.revoke_user_pending_tokens($1)
// — the reset's token kill: every unconsumed token of the user, whatever
// its purpose, is marked consumed, which is exactly what the function's
// UPDATE does to the shared table. Outstanding magic links and reset
// links die with the credential.
func (q *loginQuerier) pendingRevokeRow(args []any) pgx.Row {
	return stubRow{scan: func(dest ...any) error {
		if err := q.fail["revoke_user_pending_tokens"]; err != nil {
			return err
		}
		userID := args[0].(uuid.UUID)
		q.pendingRevokes = append(q.pendingRevokes, userID)
		q.order = append(q.order, "revoke_tokens")
		for _, tok := range q.tokens {
			if tok.userID == userID && !tok.consumed {
				tok.consumed = true
			}
		}
		*(dest[0].(*int)) = 1
		return nil
	}}
}

// postPath is postLogin generalized to the whole mounted surface — the
// reset pair and the cross-purpose proofs aim at different routes.
func (r *rig) postPath(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	return rec
}

// plantTokenPurpose records a token of any credential class in the rig's
// token table — session_fake_test's twin on the login rig — and returns the
// raw value the request body carries. plantResetToken is the common case.
func (r *rig) plantTokenPurpose(t *testing.T, userID uuid.UUID, expiresAt time.Time, purpose string) string {
	t.Helper()
	token, err := platform.NewOpaqueToken()
	require.NoError(t, err)
	r.q.tokens[platform.HashToken(token)] = &tokenRec{userID: userID, expiresAt: expiresAt, purpose: purpose}
	return token
}

// plantResetToken records a purpose='password_reset' token — the shape
// create_password_reset_token leaves at rest.
func (r *rig) plantResetToken(t *testing.T, userID uuid.UUID, expiresAt time.Time) string {
	t.Helper()
	return r.plantTokenPurpose(t, userID, expiresAt, platform.MailPurposePasswordReset)
}
