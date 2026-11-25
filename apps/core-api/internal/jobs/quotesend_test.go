package jobs_test

// Real-Postgres integration tests for the S5 quote_send worker (spec S5):
// every tenant read/write happens inside InTenantTx, so only the qp_app
// pool and the real role model can prove the RLS scope, the FOR UPDATE
// no-double-send guard and the refusal bookkeeping. External I/O is the
// named FakeSender/FakeStorage pair — never the network.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/jobs"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/resend"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// sendRig bundles the worker with its named fakes.
type sendRig struct {
	worker  *jobs.QuoteSendWorker
	sender  *resend.FakeSender
	storage *platform.FakeStorage
}

// rigOutboundFrom is the verified platform mailbox the fixtures send From —
// the value QP_OUTBOUND_FROM carries in production.
const rigOutboundFrom = "quotes@platform.test"

func newSendRig(db *tenantdb.DB, sender *resend.FakeSender, store *platform.FakeStorage) *sendRig {
	return &sendRig{
		worker:  jobs.NewQuoteSendWorker(db, sender, store, rigOutboundFrom),
		sender:  sender,
		storage: store,
	}
}

// sendJob wraps args in the job shape Work consumes.
func sendJob(org, quote uuid.UUID) *river.Job[jobs.SendArgs] {
	return &river.Job[jobs.SendArgs]{Args: jobs.SendArgs{
		OrganizationID: org, CorrelationID: uuid.New(), QuoteID: quote,
	}}
}

// sendOpts lets each fixture steer the fields the send reads.
type sendOpts struct {
	customerEmail string
	subject       string
	threadHeaders map[string]any
	status        string
	approvedQuote bool   // stamp approved_* — the send precondition
	quoteStatus   string // defaults DRAFT
	pdfKey        string // empty leaves pdf_storage_key NULL
	costBRL       int64
	sellBRL       int64
}

// seedSend writes one RFQ + one quote version inside the tenant tx
// and returns both ids plus the storage key the fixture used.
type sendFixture struct {
	org     uuid.UUID
	slug    string
	rfq     uuid.UUID
	quote   uuid.UUID
	pdfKey  string
	number  string
	version int
}

func seedSend(t *testing.T, db *tenantdb.DB, o sendOpts) sendFixture {
	t.Helper()
	f := sendFixture{org: uuid.New(), pdfKey: o.pdfKey, version: 1, number: "Q-" + uuid.NewString()[:8]}
	f.slug = testenv.SeedOrganization(t, f.org, "send-"+uuid.NewString()[:6])
	threadJSON, err := json.Marshal(o.threadHeaders)
	require.NoError(t, err)
	pricingJSON, err := json.Marshal(pricing.Output{
		Status:        pricing.StatusComplete,
		TotalCostBRL:  decimal.NewFromInt(o.costBRL),
		FinalPriceBRL: decimal.NewFromInt(o.sellBRL),
	})
	require.NoError(t, err)
	if o.quoteStatus == "" {
		o.quoteStatus = "DRAFT"
	}
	require.NoError(t, db.InTenantTx(context.Background(), f.org, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO app.rfq_requests
				(organization_id, rfq_number, resend_message_id, customer_email, subject,
				 status, thread_headers_json)
			VALUES ($1, (SELECT coalesce(max(rfq_number),0)+1 FROM app.rfq_requests WHERE organization_id=$1),
			        $2, $3, $4, coalesce($5::app.rfq_status,'READY'), $6)
			RETURNING id`,
			f.org, "resend-"+uuid.NewString(), o.customerEmail, o.subject,
			emptyToNil(o.status), threadJSON).Scan(&f.rfq); err != nil {
			return err
		}
		var pdfArg *string
		if f.pdfKey != "" {
			pdfArg = &f.pdfKey
		}
		var approvedAt, approvedVia any
		if o.approvedQuote {
			approvedAt = time.Now()
			approvedVia = "api"
		}
		return tx.QueryRow(ctx, `
			INSERT INTO app.quotes
				(organization_id, rfq_id, quote_number, version, status,
				 total_cost_brl, target_margin, final_price_brl,
				 overall_confidence, pricing_result_json,
				 pdf_storage_key, approved_at, approved_via)
			VALUES ($1, $2, $3, $4, $5::app.quote_status, $6, 0.1500, $7,
			        0.9000, $8, $9, $10, $11)
			RETURNING id`,
			f.org, f.rfq, f.number, f.version, o.quoteStatus,
			o.costBRL, o.sellBRL, pricingJSON, pdfArg, approvedAt, approvedVia).Scan(&f.quote)
	}))
	return f
}

// emptyToNil maps "" to nil so a COALESCE parameter can fall back.
func emptyToNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// quoteState reads back the columns S5 writes.
type quoteState struct {
	Status   string
	SentMsg  *string
	RfqState string
	Review   *string
	Failure  *string
}

func loadSendState(t *testing.T, db *tenantdb.DB, f sendFixture) quoteState {
	t.Helper()
	var s quoteState
	require.NoError(t, db.InTenantTx(context.Background(), f.org, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT status::text, sent_outbound_message_id FROM app.quotes WHERE id = $1`,
			f.quote).Scan(&s.Status, &s.SentMsg); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT status::text, review_reason, failure_reason FROM app.rfq_requests WHERE id = $1`,
			f.rfq).Scan(&s.RfqState, &s.Review, &s.Failure)
	}))
	return s
}

// auditCount tallies tenant_audit_logs rows for an action; meta returns the
// first row's metadata.
func auditCount(t *testing.T, db *tenantdb.DB, org uuid.UUID, action string) (int, map[string]any) {
	t.Helper()
	var n int
	var meta map[string]any
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM app.tenant_audit_logs WHERE action = $1`, action).Scan(&n); err != nil {
			return err
		}
		var raw []byte
		err := tx.QueryRow(ctx,
			`SELECT metadata_json::text FROM app.tenant_audit_logs WHERE action = $1 LIMIT 1`, action).Scan(&raw)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if len(raw) > 0 {
			return json.Unmarshal(raw, &meta)
		}
		return nil
	}))
	return n, meta
}

// assertJobCancelFails unwraps Work's verdict: the job must cancel, never
// retry, on a deterministic refusal.
func assertJobCancel(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	var jce *rivertype.JobCancelError
	assert.True(t, errors.As(err, &jce), "expected river.JobCancelError, got %v", err)
}

// healthyFixture is the approved, rendered, positive-margin quote every
// happy path starts from.
func healthyFixture() sendOpts {
	return sendOpts{
		customerEmail: "buyer@customer.test",
		subject:       "RFQ: 2x40HQ Santos→Rotterdam",
		status:        "READY",
		approvedQuote: true,
		pdfKey:        "tenants/x/quotes/q.pdf",
		costBRL:       100, sellBRL: 115,
		threadHeaders: map[string]any{
			"message_id":  "<m1@buyer.test>",
			"references":  "<m0@fwd.test>",
			"in-reply-to": "<m0@fwd.test>",
		},
	}
}

// TestQuoteSendHappyPath is the spec-S5 sequence: send with the idempotency
// key, thread headers and branding → quote and RFQ flip APPROVED_SENT →
// message id persisted → quote.sent audited — one commit behind it all.
func TestQuoteSendHappyPath(t *testing.T) {
	db := testenv.Open(t)
	f := seedSend(t, db, healthyFixture())
	storage := platform.NewFakeStorage()
	pdf := []byte("%PDF-quote-bytes")
	require.NoError(t, storage.Put(context.Background(), f.pdfKey, pdf))
	sender := &resend.FakeSender{MessageID: "out-msg-" + uuid.NewString()}
	rig := newSendRig(db, sender, storage)

	require.NoError(t, rig.worker.Work(context.Background(), sendJob(f.org, f.quote)))

	require.Equal(t, 1, sender.Calls())
	req := sender.Requests()[0]
	assert.Equal(t, []string{"buyer@customer.test"}, req.To)
	assert.Equal(t, f.quote.String()+":1", req.IdempotencyKey, "the {quote_id}:{version} key")
	assert.Equal(t, "<m1@buyer.test>", req.Headers["In-Reply-To"], "reply anchors on the RFQ's Message-ID")
	assert.Equal(t, "<m0@fwd.test> <m1@buyer.test>", req.Headers["References"])
	assert.Equal(t, "Re: RFQ: 2x40HQ Santos→Rotterdam", req.Subject)
	require.Len(t, req.Attachments, 1)
	assert.Equal(t, f.number+".pdf", req.Attachments[0].Filename)
	assert.Equal(t, pdf, req.Attachments[0].Content)
	assert.Contains(t, req.TextBody, f.number)
	// Branding (spec S5 + the verified-domain rule): From is the tenant's
	// display name on the platform's verified mailbox; the tenant's
	// outbound_reply_to stays Reply-To so answers land on the tenant.
	assert.Equal(t, "ops@"+f.slug+".test", req.ReplyTo)
	assert.Equal(t, "Test "+f.slug+" <"+rigOutboundFrom+">", req.From)

	s := loadSendState(t, db, f)
	assert.Equal(t, "APPROVED_SENT", s.Status)
	assert.Equal(t, "APPROVED_SENT", s.RfqState)
	require.NotNil(t, s.SentMsg)
	assert.Equal(t, sender.MessageID, *s.SentMsg)

	n, meta := auditCount(t, db, f.org, "quote.sent")
	assert.Equal(t, 1, n)
	assert.Equal(t, sender.MessageID, meta["message_id"])
}

// TestQuoteSendReplayNoSecondSend is the no-double-send guarantee: a second
// worker run on an APPROVED_SENT quote no-ops under the FOR UPDATE lock —
// the provider is never called again (spec S5).
func TestQuoteSendReplayNoSecondSend(t *testing.T) {
	db := testenv.Open(t)
	f := seedSend(t, db, healthyFixture())
	storage := platform.NewFakeStorage()
	require.NoError(t, storage.Put(context.Background(), f.pdfKey, []byte("pdf")))
	// The provider id must be per-run unique: migration 0008's index is
	// GLOBAL (not org-prefixed — provider message ids are), so a fixed
	// literal collides with a previous run's committed row.
	sender := &resend.FakeSender{MessageID: "out-msg-" + uuid.NewString()}
	rig := newSendRig(db, sender, storage)

	require.NoError(t, rig.worker.Work(context.Background(), sendJob(f.org, f.quote)))
	require.NoError(t, rig.worker.Work(context.Background(), sendJob(f.org, f.quote)))
	assert.Equal(t, 1, sender.Calls(), "APPROVED_SENT under FOR UPDATE collapses the replay")

	n, _ := auditCount(t, db, f.org, "quote.sent")
	assert.Equal(t, 1, n, "the replay writes no second audit row")
}

// TestQuoteSendUnapprovedRefuses: the structural guarantee — a quote with
// no approval stamp cancels with the audited refusal, never sends (spec:
// "no code path sends a quote without human approval").
func TestQuoteSendUnapprovedRefuses(t *testing.T) {
	db := testenv.Open(t)
	o := healthyFixture()
	o.approvedQuote = false
	f := seedSend(t, db, o)
	storage := platform.NewFakeStorage()
	require.NoError(t, storage.Put(context.Background(), f.pdfKey, []byte("pdf")))
	sender := &resend.FakeSender{MessageID: "out-msg-" + uuid.NewString()}
	rig := newSendRig(db, sender, storage)

	err := rig.worker.Work(context.Background(), sendJob(f.org, f.quote))
	assertJobCancel(t, err)
	assert.Equal(t, 0, sender.Calls())

	s := loadSendState(t, db, f)
	assert.Equal(t, "DRAFT", s.Status)
	assert.Equal(t, "READY", s.RfqState, "an unapproved refusal does not move the RFQ")
	n, meta := auditCount(t, db, f.org, "quote.send_refused")
	assert.Equal(t, 1, n)
	assert.Equal(t, "unapproved", meta["reason"])
}

// TestQuoteSendEmptySenderIsJobError is the Devin-Review regression pin:
// an empty outbound sender is a wiring fault, not a recipient refusal —
// Work must return a plain error (retried, eventually dead-lettered as a
// technical failure), send nothing, and write no send_refused audit.
func TestQuoteSendEmptySenderIsJobError(t *testing.T) {
	db := testenv.Open(t)
	f := seedSend(t, db, healthyFixture())
	storage := platform.NewFakeStorage()
	require.NoError(t, storage.Put(context.Background(), f.pdfKey, []byte("pdf")))
	sender := &resend.FakeSender{MessageID: "out-msg-" + uuid.NewString()}
	worker := jobs.NewQuoteSendWorker(db, sender, storage, "   ")

	err := worker.Work(context.Background(), sendJob(f.org, f.quote))
	require.Error(t, err)
	var jce *rivertype.JobCancelError
	assert.False(t, errors.As(err, &jce), "a sender wiring fault must retry, not JobCancel")
	assert.Equal(t, 0, sender.Calls())

	s := loadSendState(t, db, f)
	assert.Equal(t, "DRAFT", s.Status)
	assert.Equal(t, "READY", s.RfqState)
	n, _ := auditCount(t, db, f.org, "quote.send_refused")
	assert.Equal(t, 0, n, "no refusal audit — the refusal vocabulary is for recipient/data defects")
}

// TestQuoteSendMarginFlipRefuses is the defense-in-depth check: data
// changed between approval and send → the RFQ parks NEEDS_REVIEW with
// NEGATIVE_MARGIN (the controlled constant), the refusal is audited and
// nothing is sent.
func TestQuoteSendMarginFlipRefuses(t *testing.T) {
	db := testenv.Open(t)
	o := healthyFixture()
	o.sellBRL = 90 // below the 100 cost — negative margin
	f := seedSend(t, db, o)
	storage := platform.NewFakeStorage()
	require.NoError(t, storage.Put(context.Background(), f.pdfKey, []byte("pdf")))
	sender := &resend.FakeSender{MessageID: "out-msg-" + uuid.NewString()}
	rig := newSendRig(db, sender, storage)

	err := rig.worker.Work(context.Background(), sendJob(f.org, f.quote))
	assertJobCancel(t, err)
	assert.Equal(t, 0, sender.Calls())

	s := loadSendState(t, db, f)
	assert.Equal(t, "DRAFT", s.Status)
	assert.Equal(t, "NEEDS_REVIEW", s.RfqState)
	require.NotNil(t, s.Review)
	assert.Equal(t, string(jobs.ReviewReasonNegativeMargin), *s.Review)
	n, meta := auditCount(t, db, f.org, "quote.send_refused")
	assert.Equal(t, 1, n)
	assert.Equal(t, "negative_margin", meta["reason"])
}

// TestQuoteSendMissingRenderRefuses: pdf_storage_key NULL cancels with
// missing_render and leaves the RFQ status untouched (spec S5).
func TestQuoteSendMissingRenderRefuses(t *testing.T) {
	db := testenv.Open(t)
	o := healthyFixture()
	o.pdfKey = ""
	f := seedSend(t, db, o)
	sender := &resend.FakeSender{MessageID: "out-msg-" + uuid.NewString()}
	rig := newSendRig(db, sender, platform.NewFakeStorage())

	err := rig.worker.Work(context.Background(), sendJob(f.org, f.quote))
	assertJobCancel(t, err)
	assert.Equal(t, 0, sender.Calls())

	s := loadSendState(t, db, f)
	assert.Equal(t, "READY", s.RfqState, "a missing render leaves the RFQ unchanged")
	n, meta := auditCount(t, db, f.org, "quote.send_refused")
	assert.Equal(t, 1, n)
	assert.Equal(t, "missing_render", meta["reason"])
}

// TestQuoteSendMissingObjectRefuses: the key exists but the artifact is
// gone — the same missing_render refusal, distinct from a transient
// storage failure which would retry.
func TestQuoteSendMissingObjectRefuses(t *testing.T) {
	db := testenv.Open(t)
	f := seedSend(t, db, healthyFixture())
	sender := &resend.FakeSender{MessageID: "out-msg-" + uuid.NewString()}
	rig := newSendRig(db, sender, platform.NewFakeStorage()) // no bytes under the key

	err := rig.worker.Work(context.Background(), sendJob(f.org, f.quote))
	assertJobCancel(t, err)
	assert.Equal(t, 0, sender.Calls())
	n, meta := auditCount(t, db, f.org, "quote.send_refused")
	assert.Equal(t, 1, n)
	assert.Equal(t, "missing_render", meta["reason"])
}

// TestQuoteSendSupersededRefuses: a retired version cancels — only the
// newest version may ever reach the customer (rule 9).
func TestQuoteSendSupersededRefuses(t *testing.T) {
	db := testenv.Open(t)
	o := healthyFixture()
	o.quoteStatus = "SUPERSEDED"
	f := seedSend(t, db, o)
	storage := platform.NewFakeStorage()
	require.NoError(t, storage.Put(context.Background(), f.pdfKey, []byte("pdf")))
	sender := &resend.FakeSender{MessageID: "out-msg-" + uuid.NewString()}
	rig := newSendRig(db, sender, storage)

	err := rig.worker.Work(context.Background(), sendJob(f.org, f.quote))
	assertJobCancel(t, err)
	assert.Equal(t, 0, sender.Calls())
	n, meta := auditCount(t, db, f.org, "quote.send_refused")
	assert.Equal(t, 1, n)
	assert.Equal(t, "superseded", meta["reason"])
}

// TestQuoteSendTransientErrorRetries: a transport/5xx answer propagates as
// a normal error — River retries it — and the rollback leaves no
// bookkeeping behind.
func TestQuoteSendTransientErrorRetries(t *testing.T) {
	db := testenv.Open(t)
	f := seedSend(t, db, healthyFixture())
	storage := platform.NewFakeStorage()
	require.NoError(t, storage.Put(context.Background(), f.pdfKey, []byte("pdf")))
	sender := &resend.FakeSender{Err: errors.New("resend: upstream 503")}
	rig := newSendRig(db, sender, storage)

	err := rig.worker.Work(context.Background(), sendJob(f.org, f.quote))
	require.Error(t, err)
	var jce *rivertype.JobCancelError
	assert.False(t, errors.As(err, &jce), "transient must propagate for retry, not cancel")

	s := loadSendState(t, db, f)
	assert.Equal(t, "DRAFT", s.Status)
	assert.Equal(t, "READY", s.RfqState)
	n, _ := auditCount(t, db, f.org, "quote.sent")
	assert.Equal(t, 0, n)
}

// TestQuoteSendPermanentErrorCancels: a 4xx answer is deterministic —
// JobCancel so the remaining attempts are not burned on a call that can
// never succeed (spec section 3 classification).
func TestQuoteSendPermanentErrorCancels(t *testing.T) {
	db := testenv.Open(t)
	f := seedSend(t, db, healthyFixture())
	storage := platform.NewFakeStorage()
	require.NoError(t, storage.Put(context.Background(), f.pdfKey, []byte("pdf")))
	sender := &resend.FakeSender{Err: &resend.SendError{Op: "send_email", StatusCode: 422, Detail: "invalid to"}}
	rig := newSendRig(db, sender, storage)

	err := rig.worker.Work(context.Background(), sendJob(f.org, f.quote))
	assertJobCancel(t, err)
	s := loadSendState(t, db, f)
	assert.Equal(t, "DRAFT", s.Status, "a cancelled send leaves the quote approvable-retryable state untouched")
	assert.Equal(t, "READY", s.RfqState)
	// The rejection is audited in a fresh tx — a cancelled delivery must
	// never be invisible to the tenant.
	n, meta := auditCount(t, db, f.org, "quote.send_refused")
	assert.Equal(t, 1, n)
	assert.Equal(t, "provider_permanent", meta["reason"])
}

// TestQuoteSendDeadLetterMarksFailed: the fifth attempt's transient
// failure parks the RFQ FAILED with the reason — spec S5's "exhausted ⇒
// FAILED, no automatic resend" — while the error still propagates for the
// discard.
func TestQuoteSendDeadLetterMarksFailed(t *testing.T) {
	db := testenv.Open(t)
	f := seedSend(t, db, healthyFixture())
	storage := platform.NewFakeStorage()
	require.NoError(t, storage.Put(context.Background(), f.pdfKey, []byte("pdf")))
	sender := &resend.FakeSender{Err: errors.New("resend: upstream 503")}
	rig := newSendRig(db, sender, storage)

	job := sendJob(f.org, f.quote)
	job.JobRow = &rivertype.JobRow{Attempt: 5, MaxAttempts: 5}
	err := rig.worker.Work(context.Background(), job)
	require.Error(t, err)

	s := loadSendState(t, db, f)
	assert.Equal(t, "FAILED", s.RfqState)
	require.NotNil(t, s.Failure)
	assert.Contains(t, *s.Failure, "exhausted 5 attempts")
	assert.Equal(t, "DRAFT", s.Status)
}

// TestQuoteSendCrossTenantInvisible: a job whose organization_id does not
// own the quote sees nothing — RLS makes the FOR UPDATE lookup a miss,
// audited as quote_missing, and no tenant row moves (the same
// indistinguishability the endpoint's 404 proves).
func TestQuoteSendCrossTenantInvisible(t *testing.T) {
	db := testenv.Open(t)
	f := seedSend(t, db, healthyFixture())
	other := uuid.New()
	testenv.SeedOrganization(t, other, "send-other")
	sender := &resend.FakeSender{MessageID: "out-msg-" + uuid.NewString()}
	rig := newSendRig(db, sender, platform.NewFakeStorage())

	err := rig.worker.Work(context.Background(), sendJob(other, f.quote))
	assertJobCancel(t, err)
	assert.Equal(t, 0, sender.Calls())

	s := loadSendState(t, db, f)
	assert.Equal(t, "DRAFT", s.Status, "the real quote is untouched")
	n, meta := auditCount(t, db, other, "quote.send_refused")
	assert.Equal(t, 1, n, "the refusal lands in the JOB's tenant, not the quote's")
	assert.Equal(t, "quote_missing", meta["reason"])
	n, _ = auditCount(t, db, f.org, "quote.send_refused")
	assert.Equal(t, 0, n, "no audit row leaks into the quote's own tenant")
}

// TestQuoteSendArgsStayIDsOnly pins rule 7 on the envelope: job args carry
// identifiers — no bodies, no pricing, no customer data.
func TestQuoteSendArgsStayIDsOnly(t *testing.T) {
	raw, err := json.Marshal(jobs.SendArgs{
		OrganizationID: uuid.New(), CorrelationID: uuid.New(), QuoteID: uuid.New(),
	})
	require.NoError(t, err)
	var args map[string]any
	require.NoError(t, json.Unmarshal(raw, &args))
	assert.Len(t, args, 3, "SendArgs must stay ids-only")
	for key := range args {
		assert.Contains(t, []string{"organization_id", "correlation_id", "quote_id"}, key)
	}
}
