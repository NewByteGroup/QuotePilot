package jobs_test

// Real-Postgres integration tests for the S1 rfq_ingest worker (spec S1):
// every tenant read/write happens inside InTenantTx, so only the qp_app
// pool and the real role model can prove it — no mocked database.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/jobs"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/resend"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// fakeReceiver answers GetInboundEmail/DownloadAttachment from fixtures.
type fakeReceiver struct {
	email     *resend.InboundEmail
	err       error
	downloads map[string][]byte
	getCalls  int
	dlCalls   int
}

func (f *fakeReceiver) GetInboundEmail(context.Context, string) (*resend.InboundEmail, error) {
	f.getCalls++
	if f.err != nil {
		return nil, f.err
	}
	return f.email, nil
}

func (f *fakeReceiver) DownloadAttachment(_ context.Context, url string) ([]byte, error) {
	f.dlCalls++
	if b, ok := f.downloads[url]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("fakeReceiver: no fixture for %q", url)
}

// ingestRig bundles a worker with its fakes.
type ingestRig struct {
	worker   *jobs.RfqIngestWorker
	receiver *fakeReceiver
	storage  *platform.FakeStorage
}

// newIngestRig builds the worker over the real tenant runner plus an
// insert-only river client (Workers nil ⇒ no kind check; InsertTx works
// without Start — the enqueue still lands in river_job inside the tx).
func newIngestRig(t *testing.T, db *tenantdb.DB, recv *fakeReceiver, store *platform.FakeStorage) *ingestRig {
	t.Helper()
	worker := jobs.NewRfqIngestWorker(db, recv, store)
	client, err := river.NewClient(riverpgxv5.New(db.Pool()), &river.Config{
		Schema:            "river",
		ReindexerSchedule: river.NeverSchedule(),
	})
	require.NoError(t, err)
	worker.Inserter = client
	return &ingestRig{worker: worker, receiver: recv, storage: store}
}

// ingestJob wraps args in the job shape Work consumes; JobRow stays nil,
// which is exactly what the dead-letter guard must tolerate.
func ingestJob(org, corr, receipt uuid.UUID) *river.Job[jobs.IngestArgs] {
	return &river.Job[jobs.IngestArgs]{Args: jobs.IngestArgs{
		OrganizationID: org, CorrelationID: corr, InboundMessageID: receipt,
	}}
}

// seedReceipt inserts one inbound_messages row inside the tenant tx and
// returns its id — the same row the verified webhook writes.
func seedReceipt(t *testing.T, db *tenantdb.DB, org uuid.UUID, resendID, subject string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO app.inbound_messages (organization_id, resend_message_id, subject, received_at)
			VALUES ($1, $2, $3, now()) RETURNING id`, org, resendID, subject).Scan(&id)
	}))
	return id
}

// rfqRow mirrors the columns the worker asserts on.
type rfqRow struct {
	ID            uuid.UUID
	Status        string
	ResendMessage string
	RfcMessageID  string
	CustomerEmail string
	CustomerName  string
	Subject       string
	RawText       string
	RawHTML       string
	ThreadHeaders []byte
	FailureReason string
}

// loadRfq reads the single rfq_requests row for a resend id inside the
// tenant tx; found=false when absent.
func loadRfq(t *testing.T, db *tenantdb.DB, org uuid.UUID, resendID string) (rfqRow, bool) {
	t.Helper()
	var r rfqRow
	var ok bool
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT id, status::text, resend_message_id,
			       coalesce(rfc_message_id,''), customer_email, coalesce(customer_name,''),
			       subject, coalesce(raw_text_body,''), coalesce(raw_html_body,''),
			       coalesce(thread_headers_json::text,''), coalesce(failure_reason,'')
			FROM app.rfq_requests WHERE resend_message_id = $1`, resendID).
			Scan(&r.ID, &r.Status, &r.ResendMessage, &r.RfcMessageID, &r.CustomerEmail,
				&r.CustomerName, &r.Subject, &r.RawText, &r.RawHTML, &r.ThreadHeaders, &r.FailureReason)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		ok = true
		return nil
	}))
	return r, ok
}

// countRows counts rows of a tenant table inside the tenant tx.
func countRows(t *testing.T, db *tenantdb.DB, org uuid.UUID, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	}))
	return n
}

// fixtureEmail is the happy-path message the receiver returns.
func fixtureEmail() *resend.InboundEmail {
	return &resend.InboundEmail{
		ID:        "resend-msg-1",
		From:      "Ada Buyer <ada@buyer.test>",
		Subject:   "RFQ: 2x40HQ",
		Text:      "please quote",
		HTML:      "<p>please quote</p>",
		MessageID: "<m1@buyer.test>",
		Headers: map[string]string{
			"message-id":  "<m1@buyer.test>",
			"in-reply-to": "<m0@fwd.test>",
			"references":  "<m0@fwd.test>",
			"return-path": "ada@buyer.test",
		},
		Authentication: &resend.AuthenticationResult{SPF: "pass", DKIM: "pass", DMARC: "pass"},
		Attachments: []resend.InboundAttachment{
			{ID: "a1", Filename: "rates.pdf", ContentType: "application/pdf", Size: 4, DownloadURL: "https://cdn.test/a1"},
			{ID: "a2", Filename: "schedule.csv", ContentType: "text/csv", Size: 3, DownloadURL: "https://cdn.test/a2"},
		},
	}
}

// TestRfqIngestHappyPath: receipt → rfq row + attachments + storage puts +
// PARSING + rfq_extract enqueued — all committed atomically by one tx.
func TestRfqIngestHappyPath(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "ingest-happy")
	receipt := seedReceipt(t, db, org, "resend-msg-1", "RFQ: 2x40HQ")

	recv := &fakeReceiver{
		email: fixtureEmail(),
		downloads: map[string][]byte{
			"https://cdn.test/a1": []byte("PDF1"),
			"https://cdn.test/a2": []byte("CSV"),
		},
	}
	rig := newIngestRig(t, db, recv, platform.NewFakeStorage())
	require.NoError(t, rig.worker.Work(context.Background(), ingestJob(org, uuid.New(), receipt)))

	r, ok := loadRfq(t, db, org, "resend-msg-1")
	require.True(t, ok, "rfq_requests row must exist")
	assert.Equal(t, "PARSING", r.Status)
	assert.Equal(t, "ada@buyer.test", r.CustomerEmail)
	assert.Equal(t, "Ada Buyer", r.CustomerName)
	assert.Equal(t, "<m1@buyer.test>", r.RfcMessageID)
	assert.Equal(t, "please quote", r.RawText)
	assert.Equal(t, "<p>please quote</p>", r.RawHTML)
	assert.Empty(t, r.FailureReason)
	assert.Contains(t, string(r.ThreadHeaders), "<m0@fwd.test>", "thread headers keep In-Reply-To/References")
	assert.Contains(t, string(r.ThreadHeaders), `"spf": "pass"`, "thread headers keep the SPF/DKIM verdict")

	// Attachments: rows + storage puts under the tenant key.
	n := countRows(t, db, org,
		`SELECT count(*) FROM app.rfq_attachments WHERE rfq_id = $1`, r.ID)
	assert.Equal(t, 2, n)
	keys := rig.storage.Keys()
	require.Len(t, keys, 2)
	for _, k := range keys {
		assert.Contains(t, k, "tenants/"+org.String()+"/rfqs/"+r.ID.String()+"/")
	}
	got, err := rig.storage.Get(context.Background(), keys[0])
	require.NoError(t, err)
	assert.NotEmpty(t, got)
	assert.Equal(t, 1, recv.getCalls)
	assert.Equal(t, 2, recv.dlCalls)

	// rfq_extract was enqueued inside the same tx and committed with the row.
	// river_job has no RLS — scope assertions to this org via the args JSON.
	n = countRows(t, db, org,
		`SELECT count(*) FROM river.river_job WHERE kind = 'rfq_extract' AND queue = 'extract'
		 AND args ->> 'organization_id' = $1`, org.String())
	assert.Equal(t, 1, n)
	var args map[string]string
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT args FROM river.river_job WHERE kind = 'rfq_extract'
			 AND args ->> 'organization_id' = $1`, org.String()).Scan(&args)
	}))
	assert.Equal(t, r.ID.String(), args["rfq_id"])
	assert.Equal(t, org.String(), args["organization_id"])
}

// TestRfqIngestReplayIsNoOp: a second job for an already-promoted receipt
// does nothing — UNIQUE(org, resend_message_id) collapses replays (spec S1).
func TestRfqIngestReplayIsNoOp(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "ingest-replay")
	receipt := seedReceipt(t, db, org, "resend-msg-2", "RFQ replay")

	recv := &fakeReceiver{email: fixtureEmail(), downloads: map[string][]byte{
		"https://cdn.test/a1": []byte("PDF1"), "https://cdn.test/a2": []byte("CSV"),
	}}
	rig := newIngestRig(t, db, recv, platform.NewFakeStorage())
	require.NoError(t, rig.worker.Work(context.Background(), ingestJob(org, uuid.New(), receipt)))
	require.NoError(t, rig.worker.Work(context.Background(), ingestJob(org, uuid.New(), receipt)))

	assert.Equal(t, 1, recv.getCalls, "replay must not re-fetch the message")
	n := countRows(t, db, org,
		`SELECT count(*) FROM app.rfq_requests WHERE resend_message_id = 'resend-msg-2'`)
	assert.Equal(t, 1, n)
	n = countRows(t, db, org,
		`SELECT count(*) FROM river.river_job WHERE kind = 'rfq_extract'
		 AND args ->> 'organization_id' = $1`, org.String())
	assert.Equal(t, 1, n, "replay must not enqueue a second extract job")
}

// TestRfqIngestPermanentFetchFailure: a 4xx still creates the RFQ shell
// with failure_reason so nothing is invisible (spec S1).
func TestRfqIngestPermanentFetchFailure(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "ingest-perm")
	receipt := seedReceipt(t, db, org, "resend-msg-404", "gone")

	recv := &fakeReceiver{err: &resend.FetchError{Op: "get_inbound_email", StatusCode: 404, Detail: "404 Not Found"}}
	rig := newIngestRig(t, db, recv, platform.NewFakeStorage())
	require.NoError(t, rig.worker.Work(context.Background(), ingestJob(org, uuid.New(), receipt)))

	r, ok := loadRfq(t, db, org, "resend-msg-404")
	require.True(t, ok, "the shell row must be written")
	assert.Equal(t, "FAILED", r.Status)
	assert.Contains(t, r.FailureReason, "404")
	assert.Empty(t, rig.storage.Keys(), "no attachment bytes on a failed fetch")
	n := countRows(t, db, org,
		`SELECT count(*) FROM river.river_job WHERE kind = 'rfq_extract'
		 AND args ->> 'organization_id' = $1`, org.String())
	assert.Equal(t, 0, n, "a failed shell never releases extraction")
}

// TestRfqIngestTransientRollsBack: a transient fetch error propagates for
// River retry and leaves the tenant tables untouched — the checkpoint row
// and everything behind it commit together or not at all.
func TestRfqIngestTransientRollsBack(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "ingest-trans")
	receipt := seedReceipt(t, db, org, "resend-msg-500", "flaky")

	recv := &fakeReceiver{err: errors.New("resend: upstream timeout")}
	rig := newIngestRig(t, db, recv, platform.NewFakeStorage())
	err := rig.worker.Work(context.Background(), ingestJob(org, uuid.New(), receipt))
	require.Error(t, err, "transient errors must propagate for retry")

	_, ok := loadRfq(t, db, org, "resend-msg-500")
	assert.False(t, ok, "a rolled-back tx leaves no partial checkpoint")
	assert.Empty(t, rig.storage.Keys())
}

// TestRfqIngestAttachmentFailureRollsBack proves mid-pipeline atomicity:
// the second download fails → the whole tx (rfq row + first attachment)
// rolls back, so a retry sees a clean slate.
func TestRfqIngestAttachmentFailureRollsBack(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "ingest-att")
	receipt := seedReceipt(t, db, org, "resend-msg-att", "partials")

	recv := &fakeReceiver{email: fixtureEmail(), downloads: map[string][]byte{
		"https://cdn.test/a1": []byte("PDF1"),
		// a2 deliberately missing → download error
	}}
	rig := newIngestRig(t, db, recv, platform.NewFakeStorage())
	err := rig.worker.Work(context.Background(), ingestJob(org, uuid.New(), receipt))
	require.Error(t, err)

	_, ok := loadRfq(t, db, org, "resend-msg-att")
	assert.False(t, ok, "no partial rfq row may survive a rolled-back tx")
	// Storage is NOT transactional: the first attachment's bytes stay as an
	// orphan — each attempt mints a fresh rfq id, so the retried put lands
	// under a new key. A bounded object-store artifact, not a data issue.
	assert.Len(t, rig.storage.Keys(), 1)
	n := countRows(t, db, org,
		`SELECT count(*) FROM river.river_job WHERE kind = 'rfq_extract'
		 AND args ->> 'organization_id' = $1`, org.String())
	assert.Equal(t, 0, n)
}

// TestRfqIngestAttachmentKeysAreUnique pins the storage-key contract:
// duplicate filenames and missing attachment ids are normal inputs, and
// every attachment still lands under its own key — the id segment (or the
// slice index without one) guarantees it, with the readable filename kept.
func TestRfqIngestAttachmentKeysAreUnique(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "ingest-keys")
	receipt := seedReceipt(t, db, org, "resend-msg-keys", "dup names")

	em := fixtureEmail()
	em.Attachments = []resend.InboundAttachment{
		{ID: "a1", Filename: "rates.pdf", ContentType: "application/pdf", DownloadURL: "https://cdn.test/k1"},
		{ID: "a2", Filename: "rates.pdf", ContentType: "application/pdf", DownloadURL: "https://cdn.test/k2"},
		{Filename: "", ContentType: "application/octet-stream", DownloadURL: "https://cdn.test/k3"},
		{Filename: "", ContentType: "application/octet-stream", DownloadURL: "https://cdn.test/k4"},
	}
	recv := &fakeReceiver{email: em, downloads: map[string][]byte{
		"https://cdn.test/k1": []byte("1"), "https://cdn.test/k2": []byte("2"),
		"https://cdn.test/k3": []byte("3"), "https://cdn.test/k4": []byte("4"),
	}}
	rig := newIngestRig(t, db, recv, platform.NewFakeStorage())
	require.NoError(t, rig.worker.Work(context.Background(), ingestJob(org, uuid.New(), receipt)))

	r, ok := loadRfq(t, db, org, "resend-msg-keys")
	require.True(t, ok)
	prefix := "tenants/" + org.String() + "/rfqs/" + r.ID.String() + "/"
	keys := rig.storage.Keys()
	assert.ElementsMatch(t, []string{
		prefix + "a1-rates.pdf",
		prefix + "a2-rates.pdf",
		prefix + "att-2-attachment",
		prefix + "att-3-attachment",
	}, keys, "every attachment needs a collision-free tenant key")
	n := countRows(t, db, org,
		`SELECT count(*) FROM app.rfq_attachments WHERE rfq_id = $1`, r.ID)
	assert.Equal(t, 4, n)
}

// TestRfqIngestDeadLetterShell is the spec-S1 dead-letter visibility rule:
// on the job's final attempt a transient failure still leaves a FAILED
// shell — written through a fresh tenant tx because the failed one rolled
// back — while the error itself propagates so River discards the job.
func TestRfqIngestDeadLetterShell(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "ingest-dl")
	receipt := seedReceipt(t, db, org, "resend-msg-dl", "always broken")

	recv := &fakeReceiver{err: errors.New("resend: upstream timeout")}
	rig := newIngestRig(t, db, recv, platform.NewFakeStorage())

	// An earlier attempt must NOT write the shell: the retry still owns it.
	job := ingestJob(org, uuid.New(), receipt)
	job.JobRow = &rivertype.JobRow{Attempt: 1, MaxAttempts: 4}
	require.Error(t, rig.worker.Work(context.Background(), job))
	_, ok := loadRfq(t, db, org, "resend-msg-dl")
	assert.False(t, ok, "a retryable attempt leaves no shell behind")

	job = ingestJob(org, uuid.New(), receipt)
	job.JobRow = &rivertype.JobRow{Attempt: 4, MaxAttempts: 4}
	err := rig.worker.Work(context.Background(), job)
	require.Error(t, err, "the transient error still propagates for the discard")

	r, ok := loadRfq(t, db, org, "resend-msg-dl")
	require.True(t, ok, "final-attempt failure must leave a FAILED shell")
	assert.Equal(t, "FAILED", r.Status)
	assert.Contains(t, r.FailureReason, "exhausted 4 attempts")
	assert.Contains(t, r.FailureReason, "upstream timeout")
	assert.Empty(t, rig.storage.Keys(), "no attachment bytes on a dead-lettered fetch")
}
