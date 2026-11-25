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
func lockQuarantineCounter(t *testing.T) *pgxpool.Conn {
	t.Helper()
	conn, err := testenv.MigratorPool(t).Acquire(context.Background())
	require.NoError(t, err)
	_, err = conn.Exec(context.Background(),
		`SELECT pg_catalog.pg_advisory_lock(pg_catalog.hashtextextended('inbound_quarantine_counter', 0))`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(),
			`SELECT pg_catalog.pg_advisory_unlock(pg_catalog.hashtextextended('inbound_quarantine_counter', 0))`)
		conn.Release()
	})
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

// TestStoreRecordInboundQuarantine exercises the function end to end as
// qp_app: both drop reasons record, the counter bumps once per distinct
// message, and redelivery dedupes — the webhook's at-least-once reality
// must not double the audit trail.
func TestStoreRecordInboundQuarantine(t *testing.T) {
	db := testenv.Open(t)
	ctx := context.Background()
	conn := lockQuarantineCounter(t)
	store := platform.NewStore(db.Pool())
	base := unmatchedCounter(t, conn)

	noMatch := platform.InboundQuarantineDrop{
		ResendMessageID: "quar-nomatch-" + uuid.NewString(),
		Reason:          "no_match",
		Recipients:      []string{"ghost@in.test"},
		Subject:         "RFQ: 20 boxes Shenzhen",
		FromAddr:        "customer@example.com",
		ReceivedAt:      time.Now().Add(-time.Minute).UTC(),
	}
	recorded, err := store.RecordInboundQuarantine(ctx, noMatch)
	require.NoError(t, err)
	assert.True(t, recorded, "the first delivery of a drop must record")

	orgA, orgB := uuid.New(), uuid.New()
	multiMatch := platform.InboundQuarantineDrop{
		ResendMessageID: "quar-multi-" + uuid.NewString(),
		Reason:          "multi_match",
		Recipients:      []string{"a@in.test", "b@in.test"},
		MatchedOrgIDs:   []uuid.UUID{orgA, orgB},
		FromAddr:        "customer@example.com",
		ReceivedAt:      time.Now().Add(-time.Minute).UTC(),
	}
	recorded, err = store.RecordInboundQuarantine(ctx, multiMatch)
	require.NoError(t, err)
	assert.True(t, recorded)

	assert.Equal(t, base+2, unmatchedCounter(t, conn),
		"the counter counts distinct quarantined messages")

	reason, recipients, matched, receivedAt := quarRow(t, conn, noMatch.ResendMessageID)
	assert.Equal(t, "no_match", reason)
	assert.JSONEq(t, `["ghost@in.test"]`, recipients)
	assert.Equal(t, "{}", matched, "no_match carries no candidates")
	assert.WithinDuration(t, noMatch.ReceivedAt, receivedAt, time.Second,
		"received_at is Resend's receipt time, not processing time")

	reason, recipients, matched, _ = quarRow(t, conn, multiMatch.ResendMessageID)
	assert.Equal(t, "multi_match", reason)
	assert.JSONEq(t, `["a@in.test","b@in.test"]`, recipients)
	assert.Equal(t, "{"+orgA.String()+","+orgB.String()+"}", matched,
		"multi_match records both candidate orgs in resolution order")

	// Svix redelivery of the first drop: recorded=false, still one row,
	// counter unchanged — the dedupe key is the resend_message_id.
	recorded, err = store.RecordInboundQuarantine(ctx, noMatch)
	require.NoError(t, err)
	assert.False(t, recorded, "a redelivery must dedupe, not re-record")
	var rows int
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT count(*) FROM app.inbound_quarantine WHERE resend_message_id = $1`,
		noMatch.ResendMessageID).Scan(&rows))
	assert.Equal(t, 1, rows, "a redelivery must not write a second row")
	assert.Equal(t, base+2, unmatchedCounter(t, conn),
		"a redelivery must not re-count")
}

// TestStoreRecordInboundQuarantineGuards proves the function refuses the
// two shapes that would silently corrupt the audit trail — an empty dedupe
// key (everything would collapse onto one row) and an unknown reason —
// with the offending value in the error.
func TestStoreRecordInboundQuarantineGuards(t *testing.T) {
	db := testenv.Open(t)
	ctx := context.Background()
	store := platform.NewStore(db.Pool())

	_, err := store.RecordInboundQuarantine(ctx, platform.InboundQuarantineDrop{
		ResendMessageID: "",
		Reason:          "no_match",
		Recipients:      []string{"a@in.test"},
		ReceivedAt:      time.Now(),
	})
	require.Error(t, err, "an empty resend_message_id must be refused")

	_, err = store.RecordInboundQuarantine(ctx, platform.InboundQuarantineDrop{
		ResendMessageID: "quar-guard-" + uuid.NewString(),
		Reason:          "bogus",
		Recipients:      []string{"a@in.test"},
		ReceivedAt:      time.Now(),
	})
	require.Error(t, err, "an unknown reason must be refused")
	assert.Contains(t, err.Error(), "bogus")
}
