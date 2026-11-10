package tenantdb_test

// Serialization proofs for the session-family advisory lock (spec
// 2026-11-10-tenant-resolution-api section 5): rotate_session and
// revoke_session_family_by_hash must serialize on the same
// pg_advisory_xact_lock key, so a family kill can never run alongside a
// successor's birth inside its statement window. Split from
// schema_enforcement_auth_test.go to keep files under the 500-line style
// limit (AGENTS.md).

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// familyAdvisoryKey resolves the pg_advisory_xact_lock key the session-family
// mutators serialize on — the same hashtextextended expression the functions
// use, so a test can hold or observe the real lock.
func familyAdvisoryKey(t *testing.T, family uuid.UUID) int64 {
	t.Helper()
	var key int64
	require.NoError(t, testenv.MigratorPool(t).QueryRow(context.Background(),
		`SELECT hashtextextended($1::text, 0)`, family.String()).Scan(&key))
	return key
}

// seedSessionFamily creates a user and a gen0 session as qp_app — proving the
// public create path — and resolves the family's advisory key.
func seedSessionFamily(t *testing.T, db *tenantdb.DB, tag string) (uuid.UUID, string, int64) {
	t.Helper()
	userID := seedAuthUser(t, tag+uuid.NewString()+"@test.dev", "Race User")
	gen0Hash := "rh-" + uuid.NewString()
	var gen0 uuid.UUID
	require.NoError(t, db.Pool().QueryRow(context.Background(),
		`SELECT app.create_session($1, $2, now() + interval '30 days')`,
		userID.String(), gen0Hash).Scan(&gen0))
	return gen0, gen0Hash, familyAdvisoryKey(t, gen0)
}

// holdFamilyLock takes the session-level advisory lock on familyKey — the
// same lock space the functions' pg_advisory_xact_lock uses, so it simulates
// a family mutation mid-flight. Cleanup always unlocks and releases, so a
// failed assertion can never leave a blocked waiter holding a pooled
// connection behind the test.
func holdFamilyLock(t *testing.T, pool *pgxpool.Pool, familyKey int64) *pgxpool.Conn {
	t.Helper()
	conn, err := pool.Acquire(context.Background())
	require.NoError(t, err)
	_, err = conn.Exec(context.Background(), `SELECT pg_advisory_lock($1)`, familyKey)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, familyKey)
		conn.Release()
	})
	return conn
}

// releaseFamilyLock explicitly lets go of the advisory at the moment the test
// chooses — the point where the blocked subject may finally proceed.
func releaseFamilyLock(t *testing.T, conn *pgxpool.Conn, familyKey int64) {
	t.Helper()
	_, err := conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, familyKey)
	require.NoError(t, err)
}

type rotateOutcome struct {
	rows int
	err  error
}

// spawnRotator runs rotate_session on its own pooled connection and reports
// the outcome on a buffered channel — the subject under observation.
func spawnRotator(pool *pgxpool.Pool, gen0Hash, succHash string) <-chan rotateOutcome {
	done := make(chan rotateOutcome, 1)
	go func() {
		rotator, err := pool.Acquire(context.Background())
		if err != nil {
			done <- rotateOutcome{err: err}
			return
		}
		defer rotator.Release()
		var rows int
		err = rotator.QueryRow(context.Background(),
			`SELECT count(*) FROM app.rotate_session($1, $2, now() + interval '30 days')`,
			gen0Hash, succHash).Scan(&rows)
		done <- rotateOutcome{rows: rows, err: err}
	}()
	return done
}

type revokeOutcome struct {
	killed bool
	err    error
}

// spawnRevoker runs revoke_session_family_by_hash on its own pooled
// connection and reports the outcome on a buffered channel.
func spawnRevoker(pool *pgxpool.Pool, gen0Hash string) <-chan revokeOutcome {
	done := make(chan revokeOutcome, 1)
	go func() {
		revoker, err := pool.Acquire(context.Background())
		if err != nil {
			done <- revokeOutcome{err: err}
			return
		}
		defer revoker.Release()
		var killed bool
		err = revoker.QueryRow(context.Background(),
			`SELECT app.revoke_session_family_by_hash($1)`, gen0Hash).Scan(&killed)
		done <- revokeOutcome{killed: killed, err: err}
	}()
	return done
}

// requireFamilyWaiter fails unless pg_locks observes a session WAITING on the
// family advisory key — proof the subject reached the shared lock instead of
// bypassing it. classid/objid are the int64 key's two 32-bit halves; they are
// split in Go so SQL bitwise type inference stays out of the comparison. One
// pool is passed in — polling never opens a new pool per retry. A subject
// that finishes instead of blocking fails fast with its outcome.
func requireFamilyWaiter[T any](t *testing.T, pool *pgxpool.Pool, familyKey int64, done <-chan T, subject string) {
	t.Helper()
	require.Eventually(t, func() bool {
		select {
		case out := <-done:
			t.Fatalf("%s finished without blocking on the advisory: %+v", subject, out)
		default:
		}
		var waiters int
		err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_locks
			 WHERE locktype = 'advisory' AND NOT granted
			   AND classid::bigint = $1 AND objid::bigint = $2`,
			int64(uint64(familyKey)>>32), int64(uint32(familyKey))).Scan(&waiters)
		return err == nil && waiters > 0
	}, 5*time.Second, 10*time.Millisecond, "%s must wait on the family advisory lock", subject)
}

// requireStillBlocked proves the subject is still parked on the advisory —
// it must not have completed while the holder still owns the lock.
func requireStillBlocked[T any](t *testing.T, done <-chan T, subject string) {
	t.Helper()
	select {
	case out := <-done:
		t.Fatalf("%s must block on the family advisory, not finish early: %+v", subject, out)
	default:
	}
}

// awaitOutcome collects the subject's result once the holder has released
// the advisory; a subject that still cannot finish within 5s is a deadlock.
func awaitOutcome[T any](t *testing.T, done <-chan T, subject string) T {
	t.Helper()
	select {
	case out := <-done:
		return out
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not finish after the advisory was released", subject)
		var zero T
		return zero
	}
}

// TestSchemaEnforcementFamilyRevokeSerializes proves the original race is
// closed: a rotate observed blocked on the family advisory cannot see a
// family kill that lands while it waits — after release it returns the empty
// set and no successor row is ever born.
func TestSchemaEnforcementFamilyRevokeSerializes(t *testing.T) {
	db := testenv.Open(t)
	migrator := testenv.MigratorPool(t)
	ctx := context.Background()

	gen0, gen0Hash, familyKey := seedSessionFamily(t, db, "auth-race-")
	holder := holdFamilyLock(t, db.Pool(), familyKey)
	succHash := "rh-" + uuid.NewString()
	done := spawnRotator(db.Pool(), gen0Hash, succHash)

	requireFamilyWaiter(t, migrator, familyKey, done, "rotate_session")
	requireStillBlocked(t, done, "rotate_session")

	// The kill lands inside the wait — the window the advisory exists to
	// close. As migrator so the write is not gated by qp_app's grants.
	_, err := migrator.Exec(ctx,
		`UPDATE app.auth_sessions SET revoked_at = now() WHERE family_id = $1`, gen0.String())
	require.NoError(t, err)
	requireStillBlocked(t, done, "rotate_session")

	releaseFamilyLock(t, holder, familyKey)
	out := awaitOutcome(t, done, "rotate_session")
	require.NoError(t, out.err)
	assert.Zero(t, out.rows, "rotate after the serialized family kill must return the empty set")

	// The original race let the successor commit inside the revoker's
	// statement window and survive; under the advisory it is never born.
	var succRows int
	require.NoError(t, migrator.QueryRow(ctx,
		`SELECT count(*) FROM app.auth_sessions WHERE refresh_token_hash = $1`, succHash).Scan(&succRows))
	assert.Zero(t, succRows, "no successor may be born inside the kill window")
}

// TestSchemaEnforcementRevokerSerializes is the revoker-side half of the
// shared-lock contract (Devin Review): revoke_session_family_by_hash must
// wait on the same family advisory the rotator takes — if its lock
// acquisition were dropped, a kill could run alongside a successor's birth
// and this suite would stay green without this pin.
func TestSchemaEnforcementRevokerSerializes(t *testing.T) {
	db := testenv.Open(t)
	migrator := testenv.MigratorPool(t)
	ctx := context.Background()

	gen0, gen0Hash, familyKey := seedSessionFamily(t, db, "auth-rvkr-")
	holder := holdFamilyLock(t, db.Pool(), familyKey)
	done := spawnRevoker(db.Pool(), gen0Hash)

	// The revoker must be observed WAITING on the family key — the whole
	// point of the shared lock; a version that lost its PERFORM would sail
	// through and kill the family while holding nothing.
	requireFamilyWaiter(t, migrator, familyKey, done, "revoke_session_family_by_hash")
	requireStillBlocked(t, done, "revoke_session_family_by_hash")

	releaseFamilyLock(t, holder, familyKey)
	out := awaitOutcome(t, done, "revoke_session_family_by_hash")
	require.NoError(t, out.err)
	assert.True(t, out.killed, "a family with a live generation must report true")

	var liveRows int
	require.NoError(t, migrator.QueryRow(ctx,
		`SELECT count(*) FROM app.auth_sessions WHERE family_id = $1 AND revoked_at IS NULL`,
		gen0.String()).Scan(&liveRows))
	assert.Zero(t, liveRows, "every generation of the family must be revoked")
}
