package auth_test

// Test doubles for the POST /auth/refresh proofs in refresh_test.go: an
// in-memory platform.Querier modelling app.auth_sessions and
// rotate_session's exact branch order (migration 0003), plus the
// mounted-server rig. The real platform.Store runs over the fake, so the
// SQL text, the NULL-ids reuse shape and the (rotated, found) mapping are
// production's — only the transport is faked.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// sessionRec is one auth_sessions row as rotate_session sees it: the map
// key stands in for refresh_token_hash — the raw token never reaches the
// store — and the two NULL-able stamps model rotated_at/revoked_at.
type sessionRec struct {
	id        uuid.UUID
	userID    uuid.UUID
	familyID  uuid.UUID
	expiresAt time.Time
	rotatedAt *time.Time
	revokedAt *time.Time
}

// rotateCall records one rotate_session invocation: the presented hash, the
// successor hash and the sliding expiry the handler computed.
type rotateCall struct {
	presentedHash string
	newHash       string
	newExpiresAt  time.Time
}

// refreshQuerier is an in-memory platform.Querier for the refresh path: an
// auth_sessions table keyed by hash, a call record and driver-error
// injection. rotate_session is the only Query traffic on this path;
// QueryRow exists solely to satisfy the interface — nothing may call it.
type refreshQuerier struct {
	now       func() time.Time
	sessions  map[string]*sessionRec
	fail      map[string]error
	rotations []rotateCall
}

var _ platform.Querier = (*refreshQuerier)(nil)

func (q *refreshQuerier) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	return stubRow{scan: func(...any) error {
		return fmt.Errorf("refreshQuerier: unexpected QueryRow sql %q", sql)
	}}
}

func (q *refreshQuerier) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	if !strings.Contains(sql, "rotate_session") {
		return nil, fmt.Errorf("refreshQuerier: unexpected sql %q", sql)
	}
	if err := q.fail["rotate_session"]; err != nil {
		return nil, err
	}
	return q.rotate(args), nil
}

// rotate replays app.rotate_session's branch order (migration 0003):
// unknown, revoked or expired hashes return the empty set; an
// already-rotated generation is reuse — the whole family is revoked in the
// same call and NULL ids come back with rotated=false; a live generation is
// stamped and its successor inserted in the same family — consume-and-
// create in one step, so the fake can never leave a half-rotated state.
func (q *refreshQuerier) rotate(args []any) pgx.Rows {
	call := rotateCall{
		presentedHash: args[0].(string),
		newHash:       args[1].(string),
		newExpiresAt:  args[2].(time.Time),
	}
	q.rotations = append(q.rotations, call)
	rec, ok := q.sessions[call.presentedHash]
	if !ok || rec.revokedAt != nil || !q.now().Before(rec.expiresAt) {
		return &stubRows{}
	}
	if rec.rotatedAt != nil {
		now := q.now()
		for _, s := range q.sessions {
			if s.familyID == rec.familyID {
				s.revokedAt = &now
			}
		}
		var nilID *uuid.UUID
		return &stubRows{rows: [][]any{{nilID, nilID, false}}}
	}
	now := q.now()
	rec.rotatedAt = &now
	successor := &sessionRec{
		id:        uuid.New(),
		userID:    rec.userID,
		familyID:  rec.familyID,
		expiresAt: call.newExpiresAt,
	}
	q.sessions[call.newHash] = successor
	return &stubRows{rows: [][]any{{&successor.id, &successor.userID, true}}}
}

// refreshRig is a mounted auth surface plus the test's windows into it.
type refreshRig struct {
	handler http.Handler
	clock   *fakeClock
	cfg     *platform.Config
	q       *refreshQuerier
}

// newRefreshRig wires the endpoints the way cmd/server will: real Store
// over the fake Querier, real Limiters on the fake clock, mounted through
// auth.NewServer. The clock starts at real now — VerifyAccess checks exp
// against wall time, so a fixed epoch would mint an already-expired JWT.
func newRefreshRig(t *testing.T, cfg *platform.Config) *refreshRig {
	t.Helper()
	clock := &fakeClock{now: time.Now()}
	q := &refreshQuerier{
		now:      clock.Now,
		sessions: map[string]*sessionRec{},
		fail:     map[string]error{},
	}
	store := platform.NewStore(q)
	endpoints := auth.NewEndpoints(cfg,
		store, directInTx(store), &platform.FakeMailSender{},
		platform.NewLimiter(platform.LoginIPCapacity, platform.LimitWindow, clock.Now),
		platform.NewLimiter(platform.LoginEmailCapacity, platform.LimitWindow, clock.Now),
		clock.Now)
	return &refreshRig{handler: auth.NewServer(endpoints), clock: clock, cfg: cfg, q: q}
}

// plantGeneration records a live session row the way create_session or a
// prior rotate left it — the map key is the SHA-256 hash, never the raw
// token — and returns the token itself for the Cookie header.
func (r *refreshRig) plantGeneration(t *testing.T, userID, familyID uuid.UUID, expiresAt time.Time) string {
	t.Helper()
	token, err := platform.NewOpaqueToken()
	require.NoError(t, err)
	r.q.sessions[platform.HashToken(token)] = &sessionRec{
		id:        uuid.New(),
		userID:    userID,
		familyID:  familyID,
		expiresAt: expiresAt,
	}
	return token
}

// postRefresh fires one request at the mounted mux; a nil cookie means no
// qp_refresh travels at all — the absent-cookie case.
func (r *refreshRig) postRefresh(t *testing.T, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	return rec
}
