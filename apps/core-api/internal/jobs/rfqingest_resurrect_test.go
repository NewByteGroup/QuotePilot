package jobs_test

// Real-Postgres integration tests for the S1 rfq_ingest resurrection path
// (spec Reprocess step 0): a bodies-NULL rfq_requests row — the dead-letter
// shell insertFailedShell writes, or the same shell after the reprocess
// flips it to RECEIVED — is promoted in place instead of skipped as a
// replay. Same rig and fakes as rfqingest_test.go — same package.

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// seedShell inserts the row insertFailedShell writes — FAILED, no bodies,
// failure_reason set — inside the tenant tx and returns (id, rfq_number)
// so the resurrection asserts the update kept both. status lets a case
// seed the RECEIVED shape the reprocess leaves behind before the job runs.
func seedShell(t *testing.T, db *tenantdb.DB, org uuid.UUID, resendID, status string) (uuid.UUID, int) {
	t.Helper()
	var id uuid.UUID
	var num int
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO app.rfq_requests
				(organization_id, rfq_number, resend_message_id, customer_email, subject,
				 received_at, status, failure_reason)
			VALUES ($1, (SELECT coalesce(max(rfq_number),0)+1 FROM app.rfq_requests WHERE organization_id=$1),
			        $2, '', 'RFQ: 2x40HQ', now(), $3::app.rfq_status, 'rfq_ingest: fetch: 404 Not Found')
			RETURNING id, rfq_number`, org, resendID, status).Scan(&id, &num)
	}))
	return id, num
}

// rfqNumber reads the row's number back inside the tenant tx.
func rfqNumber(t *testing.T, db *tenantdb.DB, org, rfq uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT rfq_number FROM app.rfq_requests WHERE id = $1`, rfq).Scan(&n)
	}))
	return n
}

// TestRfqIngestResurrectsShell: the reprocess resume (or a late replay of
// the original job) finds the bodies-NULL shell — FAILED or already
// RECEIVED — fetches the message and promotes the shell in place: same
// id, same rfq_number, full content, PARSING, extract enqueued.
func TestRfqIngestResurrectsShell(t *testing.T) {
	for _, status := range []string{"FAILED", "RECEIVED"} {
		t.Run(status, func(t *testing.T) {
			db := testenv.Open(t)
			org := uuid.New()
			testenv.SeedOrganization(t, org, "ingest-shell-"+strings.ToLower(status))
			resendID := "resend-shell-" + status
			receipt := seedReceipt(t, db, org, resendID, "RFQ: 2x40HQ")
			shellID, shellNum := seedShell(t, db, org, resendID, status)

			recv := &fakeReceiver{
				email: fixtureEmail(),
				downloads: map[string][]byte{
					"https://cdn.test/a1": []byte("PDF1"),
					"https://cdn.test/a2": []byte("CSV"),
				},
			}
			rig := newIngestRig(t, db, recv, platform.NewFakeStorage())
			require.NoError(t, rig.worker.Work(context.Background(), ingestJob(org, uuid.New(), receipt)))

			r, ok := loadRfq(t, db, org, resendID)
			require.True(t, ok)
			assert.Equal(t, shellID, r.ID, "the shell is promoted in place — id unchanged")
			assert.Equal(t, shellNum, rfqNumber(t, db, org, r.ID), "rfq_number unchanged")
			assert.Equal(t, "PARSING", r.Status)
			assert.Equal(t, "ada@buyer.test", r.CustomerEmail)
			assert.Equal(t, "Ada Buyer", r.CustomerName)
			assert.Equal(t, "<m1@buyer.test>", r.RfcMessageID)
			assert.Equal(t, "please quote", r.RawText)
			assert.Equal(t, "<p>please quote</p>", r.RawHTML)
			assert.Empty(t, r.FailureReason, "the dead-letter reason clears on resurrection")
			assert.Contains(t, string(r.ThreadHeaders), "<m0@fwd.test>")

			n := countRows(t, db, org,
				`SELECT count(*) FROM app.rfq_attachments WHERE rfq_id = $1`, r.ID)
			assert.Equal(t, 2, n, "attachments land under the resurrected id")
			n = countRows(t, db, org,
				`SELECT count(*) FROM river.river_job WHERE kind = 'rfq_extract'
				 AND args ->> 'organization_id' = $1`, org.String())
			assert.Equal(t, 1, n, "resurrection chains rfq_extract")
			assert.Equal(t, 1, countRows(t, db, org,
				`SELECT count(*) FROM app.rfq_requests WHERE resend_message_id = $1`, resendID),
				"still one row — promotion never inserts a second RFQ")
		})
	}
}

// TestRfqIngestAttachmentRowIsNotShell: a successfully-ingested
// attachment-only email leaves both bodies NULL too — but its
// rfq_attachments rows keep the row out of the shell signature, so a
// replayed or resumed rfq_ingest treats it as a live row: no fetch, no
// promoteShell overwrite, no duplicated attachment rows or storage puts
// (Devin Review: bodies-NULL alone misdetected the shape).
func TestRfqIngestAttachmentRowIsNotShell(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "ingest-att-row")
	receipt := seedReceipt(t, db, org, "resend-msg-attonly", "RFQ: attachments only")
	var rfqID uuid.UUID
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO app.rfq_requests
				(organization_id, rfq_number, resend_message_id, customer_email, subject,
				 received_at, status)
			VALUES ($1, 11, 'resend-msg-attonly', 'ada@buyer.test', 'RFQ: attachments only',
			        now(), 'PARSING')
			RETURNING id`, org).Scan(&rfqID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO app.rfq_attachments (organization_id, rfq_id, storage_key, filename, mime_type, size_bytes)
			VALUES ($1, $2, 'tenants/x/a1-rates.pdf', 'rates.pdf', 'application/pdf', 4)`, org, rfqID)
		return err
	}))

	recv := &fakeReceiver{email: fixtureEmail(), downloads: map[string][]byte{
		"https://cdn.test/a1": []byte("PDF1"), "https://cdn.test/a2": []byte("CSV"),
	}}
	rig := newIngestRig(t, db, recv, platform.NewFakeStorage())
	require.NoError(t, rig.worker.Work(context.Background(), ingestJob(org, uuid.New(), receipt)))

	assert.Equal(t, 0, recv.getCalls, "an attachment-carrying row is not a shell — no fetch")
	assert.Equal(t, 0, recv.dlCalls, "attachments are never re-stored")
	assert.Empty(t, rig.storage.Keys())
	r, ok := loadRfq(t, db, org, "resend-msg-attonly")
	require.True(t, ok)
	assert.Equal(t, rfqID, r.ID)
	assert.Equal(t, "PARSING", r.Status, "promoteShell leaves the live row untouched")
	assert.Equal(t, 1, countRows(t, db, org,
		`SELECT count(*) FROM app.rfq_attachments WHERE rfq_id = $1`, rfqID),
		"no duplicate attachment rows")
	assert.Equal(t, 0, countRows(t, db, org,
		`SELECT count(*) FROM river.river_job WHERE kind = 'rfq_extract'
		 AND args ->> 'organization_id' = $1`, org.String()))
}

// TestRfqIngestNonShellRowIsNoOp: an existing row with bodies — any
// status, here a FAILED extract dead-letter — is a replay, not a shell:
// no fetch, no write, no chained job.
func TestRfqIngestNonShellRowIsNoOp(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "ingest-nonshell")
	receipt := seedReceipt(t, db, org, "resend-msg-live", "RFQ live")
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.rfq_requests
				(organization_id, rfq_number, resend_message_id, customer_email, subject,
				 received_at, status, failure_reason, raw_text_body)
			VALUES ($1, 9, 'resend-msg-live', 'ada@buyer.test', 'RFQ live',
			        now(), 'FAILED', 'rfq_extract: contract violation', 'please quote')`, org)
		return err
	}))

	recv := &fakeReceiver{email: fixtureEmail(), downloads: map[string][]byte{}}
	rig := newIngestRig(t, db, recv, platform.NewFakeStorage())
	require.NoError(t, rig.worker.Work(context.Background(), ingestJob(org, uuid.New(), receipt)))

	assert.Equal(t, 0, recv.getCalls, "a body-carrying row is a replay — no fetch")
	r, ok := loadRfq(t, db, org, "resend-msg-live")
	require.True(t, ok)
	assert.Equal(t, "FAILED", r.Status, "the extract dead-letter is untouched")
	assert.Equal(t, "rfq_extract: contract violation", r.FailureReason)
	n := countRows(t, db, org,
		`SELECT count(*) FROM river.river_job WHERE kind = 'rfq_extract'
		 AND args ->> 'organization_id' = $1`, org.String())
	assert.Equal(t, 0, n)
}
