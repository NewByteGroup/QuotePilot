package platform_test

// RecordInboundQuarantine against the real migration-0016 function: the
// no_match and multi_match reasons land with their recipients and candidate
// org ids, inbound_unmatched_total counts DISTINCT quarantined messages,
// and a Svix redelivery dedupes on resend_message_id to false — no second
// row, no second count. Reads run as qp_migrator: the tables are Layer A,
// so qp_app's only door is the function (asserting through the owner also
// proves the write landed, not merely that the call returned true).

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// unmatchedCounter reads the shared ops counter as qp_migrator — qp_app
// has no SELECT on app.platform_counters at all.
func unmatchedCounter(t *testing.T, conn *pgxpool.Conn) int64 {
	t.Helper()
	var v int64
	require.NoError(t, conn.QueryRow(context.Background(),
		`SELECT coalesce((SELECT value FROM app.platform_counters
		  WHERE key = 'inbound_unmatched_total'), 0)`).Scan(&v))
	return v
}

// lockQuarantineCounter holds the suite-wide advisory lock that serializes
// counter-sensitive assertions against other test binaries (the teardown
// test bumps the same counter under the same key — the auth_mail_sweep
// pair set the precedent). Released on cleanup; the lock is
// session-scoped, so it must ride one acquired connection, not the pool.
// The release is registered before any fallible step: a failed require
// must never strand the connection MigratorPool's cleanup waits on.
func lockQuarantineCounter(t *testing.T) *pgxpool.Conn {
	t.Helper()
	conn, err := testenv.MigratorPool(t).Acquire(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(),
			`SELECT pg_catalog.pg_advisory_unlock(pg_catalog.hashtextextended('inbound_quarantine_counter', 0))`)
		conn.Release()
	})
	_, err = conn.Exec(context.Background(),
		`SELECT pg_catalog.pg_advisory_lock(pg_catalog.hashtextextended('inbound_quarantine_counter', 0))`)
	require.NoError(t, err)
	return conn
}

// quarRow reads one quarantine row's auditable columns as text so the
// assertions can compare values directly (jsonb and uuid[] render to their
// canonical text forms).
func quarRow(t *testing.T, conn *pgxpool.Conn, resendMessageID string) (reason, recipients, matched string, receivedAt time.Time) {
	t.Helper()
	require.NoError(t, conn.QueryRow(context.Background(),
		`SELECT reason, recipients::text, matched_org_ids::text, received_at
		 FROM app.inbound_quarantine WHERE resend_message_id = $1`,
		resendMessageID).Scan(&reason, &recipients, &matched, &receivedAt))
	return reason, recipients, matched, receivedAt
}

// TestStoreRecordInboundQuarantineNoMatch records a no_match drop as qp_app
// and reads the row back as qp_migrator — asserting through the owner proves
// the write landed, not merely that the call returned true.
func TestStoreRecordInboundQuarantineNoMatch(t *testing.T) {
	db := testenv.Open(t)
	conn := lockQuarantineCounter(t)
	store := platform.NewStore(db.Pool())

	drop := platform.InboundQuarantineDrop{
		ResendMessageID: "quar-nomatch-" + uuid.NewString(),
		Reason:          "no_match",
		Recipients:      []string{"ghost@in.test"},
		Subject:         "RFQ: 20 boxes Shenzhen",
		FromAddr:        "customer@example.com",
		ReceivedAt:      time.Now().Add(-time.Minute).UTC(),
	}
	recorded, err := store.RecordInboundQuarantine(context.Background(), drop)
	require.NoError(t, err)
	assert.True(t, recorded, "the first delivery of a drop must record")

	reason, recipients, matched, receivedAt := quarRow(t, conn, drop.ResendMessageID)
	assert.Equal(t, "no_match", reason)
	assert.JSONEq(t, `["ghost@in.test"]`, recipients)
	assert.Equal(t, "{}", matched, "no_match carries no candidates")
	assert.WithinDuration(t, drop.ReceivedAt, receivedAt, time.Second,
		"received_at is Resend's receipt time, not processing time")
}

// TestStoreRecordInboundQuarantineMultiMatch records a multi_match drop and
// proves both candidate orgs land in resolution order.
func TestStoreRecordInboundQuarantineMultiMatch(t *testing.T) {
	db := testenv.Open(t)
	conn := lockQuarantineCounter(t)
	store := platform.NewStore(db.Pool())
	orgA, orgB := uuid.New(), uuid.New()

	drop := platform.InboundQuarantineDrop{
		ResendMessageID: "quar-multi-" + uuid.NewString(),
		Reason:          "multi_match",
		Recipients:      []string{"a@in.test", "b@in.test"},
		MatchedOrgIDs:   []uuid.UUID{orgA, orgB},
		ReceivedAt:      time.Now().Add(-time.Minute).UTC(),
	}
	recorded, err := store.RecordInboundQuarantine(context.Background(), drop)
	require.NoError(t, err)
	assert.True(t, recorded)

	reason, recipients, matched, _ := quarRow(t, conn, drop.ResendMessageID)
	assert.Equal(t, "multi_match", reason)
	assert.JSONEq(t, `["a@in.test","b@in.test"]`, recipients)
	assert.Equal(t, "{"+orgA.String()+","+orgB.String()+"}", matched,
		"multi_match records both candidate orgs in resolution order")
}

// TestStoreRecordInboundQuarantineDedupe proves at-least-once delivery does
// not double the audit trail: the counter counts distinct messages once and
// a Svix redelivery returns recorded=false with no second row.
func TestStoreRecordInboundQuarantineDedupe(t *testing.T) {
	db := testenv.Open(t)
	ctx := context.Background()
	conn := lockQuarantineCounter(t)
	store := platform.NewStore(db.Pool())
	base := unmatchedCounter(t, conn)

	drop := platform.InboundQuarantineDrop{
		ResendMessageID: "quar-dedupe-" + uuid.NewString(),
		Reason:          "no_match",
		Recipients:      []string{"ghost@in.test"},
		ReceivedAt:      time.Now().UTC(),
	}
	recorded, err := store.RecordInboundQuarantine(ctx, drop)
	require.NoError(t, err)
	require.True(t, recorded)
	assert.Equal(t, base+1, unmatchedCounter(t, conn),
		"the counter counts each distinct message once")

	recorded, err = store.RecordInboundQuarantine(ctx, drop)
	require.NoError(t, err)
	assert.False(t, recorded, "a redelivery must dedupe, not re-record")
	var rows int
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT count(*) FROM app.inbound_quarantine WHERE resend_message_id = $1`,
		drop.ResendMessageID).Scan(&rows))
	assert.Equal(t, 1, rows, "a redelivery must not write a second row")
	assert.Equal(t, base+1, unmatchedCounter(t, conn), "must not re-count")
}

// TestStoreRecordInboundQuarantineRejectsEmptyKey proves the function
// refuses an empty dedupe key — every drop would collapse onto one row.
func TestStoreRecordInboundQuarantineRejectsEmptyKey(t *testing.T) {
	store := platform.NewStore(testenv.Open(t).Pool())
	_, err := store.RecordInboundQuarantine(context.Background(),
		platform.InboundQuarantineDrop{
			Reason:     "no_match",
			Recipients: []string{"a@in.test"},
			ReceivedAt: time.Now(),
		})
	require.Error(t, err, "an empty resend_message_id must be refused")
}

// TestStoreRecordInboundQuarantineRejectsUnknownReason proves the reason
// enum guard names the offending value in the error.
func TestStoreRecordInboundQuarantineRejectsUnknownReason(t *testing.T) {
	store := platform.NewStore(testenv.Open(t).Pool())
	_, err := store.RecordInboundQuarantine(context.Background(),
		platform.InboundQuarantineDrop{
			ResendMessageID: "quar-guard-" + uuid.NewString(),
			Reason:          "bogus",
			Recipients:      []string{"a@in.test"},
			ReceivedAt:      time.Now(),
		})
	require.Error(t, err, "an unknown reason must be refused")
	assert.Contains(t, err.Error(), "bogus")
}
