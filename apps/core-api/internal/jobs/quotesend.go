package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/resend"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
)

// Quote status strings as the app.quote_status enum stores them — the
// worker's transitions (DRAFT→APPROVED_SENT) and no-op check read these.
const (
	quoteDraft        = "DRAFT"
	quoteSuperseded   = "SUPERSEDED"
	quoteApprovedSent = "APPROVED_SENT"
)

// send_refused metadata reasons — the vocabulary of why a staged send died
// without calling the provider. Every refusal also writes the audit row;
// the reason is what the audit view groups on.
const (
	refuseUnapproved     = "unapproved"
	refuseNegativeMargin = "negative_margin"
	refuseMissingRender  = "missing_render"
	refuseSuperseded     = "superseded"
	refuseNoRecipient    = "no_recipient"
	refuseQuoteMissing   = "quote_missing"
	// refuseProviderPermanent marks a provider's deterministic rejection
	// (4xx other than 408/429, or an unconfigured client) — audited in a
	// fresh tx because the send's own tx already rolled back.
	refuseProviderPermanent = "provider_permanent"
)

// QuoteSendWorker is S5 quote_send (spec section 4): the last pipeline
// stage and the only path that can put a quote in a customer's inbox.
// Issue #65 split the single tenant tx into three phases — claim (tx 1),
// provider call (no tx), record (tx 2) — so no database lock spans the
// seconds-long Resend call. Mutual exclusion between concurrent
// quote_send jobs moved to app.quote_send_claims (migration 0015): one
// row per quote version whose claimed_by names the owning river_job, so
// liveness is checkable without timers — a live rival is a retryable
// in-flight error, a dead one is a stale claim the next job steals. A
// crash between provider accept and the record commit still collapses on
// the Resend idempotency key ({quote_id}:{version}).
type QuoteSendWorker struct {
	river.WorkerDefaults[SendArgs]
	// Runner is the only DB path a worker may use (ADR-002).
	Runner tenantdb.TenantRunner
	// Sender is the Resend sending seam — FakeSender in tests,
	// UnconfiguredSender when credentials are absent.
	Sender resend.Sender
	// Storage fetches the rendered PDF by pdf_storage_key.
	Storage platform.ObjectStorage
	// OutboundFrom is the verified platform mailbox Resend accepts as the
	// From: domain (QP_OUTBOUND_FROM) — the tenant's outbound_reply_to
	// rides as Reply-To instead, so customer answers still land on the
	// tenant while the send domain stays platform-verified.
	OutboundFrom string
}

// NewQuoteSendWorker builds the worker.
//
//	w := jobs.NewQuoteSendWorker(db, sender, storage, cfg.OutboundFrom)
func NewQuoteSendWorker(runner tenantdb.TenantRunner, sender resend.Sender, store platform.ObjectStorage, outboundFrom string) *QuoteSendWorker {
	return &QuoteSendWorker{Runner: runner, Sender: sender, Storage: store, OutboundFrom: outboundFrom}
}

// Work runs S5 for one approved quote version. Deterministic failures —
// the refusal set plus permanent provider errors — cancel via JobCancel;
// transient errors propagate for River's retry; a transient error on the
// final attempt additionally parks the RFQ FAILED (spec section 3:
// "exhausted ⇒ FAILED, no automatic resend").
func (w *QuoteSendWorker) Work(ctx context.Context, job *river.Job[SendArgs]) error {
	args := job.Args
	jobID := claimJobID(job)
	cancelCause, err := w.run(ctx, args, jobID)
	if err != nil {
		if resend.IsPermanent(err) {
			// A deterministic provider rejection cancels — but not
			// invisibly: the tenant audit row lands in a fresh tx (the
			// send's own tx rolled back). The audit write failing must
			// not mask the original error — join, like writeSendFailed.
			if aerr := w.writeProviderRefusal(ctx, args, jobID); aerr != nil {
				return errors.Join(err, aerr)
			}
			return river.JobCancel(err)
		}
		if job.JobRow != nil && job.MaxAttempts > 0 && job.Attempt >= job.MaxAttempts {
			if ferr := w.writeSendFailed(ctx, args, jobID, job.MaxAttempts, err); ferr != nil {
				return errors.Join(err, ferr)
			}
		}
		return err
	}
	if cancelCause != nil {
		return river.JobCancel(cancelCause)
	}
	return nil
}

// claimJobID is the river_job.id a claim row records in claimed_by — the
// liveness key the steal path checks. JobRow is nil only on direct Work
// calls outside the River executor (worker unit tests); those callers all
// share id 0, which is own-claim-consistent between their calls and dead
// to every real job (no river_job row 0 exists to be alive).
func claimJobID(job *river.Job[SendArgs]) int64 {
	if job.JobRow == nil {
		return 0
	}
	return job.ID
}

// run is the three-phase shape that replaced one long InTenantTx (issue
// #65): tx 1 locks the version briefly, runs every refusal check the
// single-tx send ran, and leaves a quote_send_claims row as the mutual
// exclusion; the provider phase then runs uncommitted; tx 2 revalidates
// and commits the sent bookkeeping together with the claim's delete. The
// returned cancelCause becomes the JobCancel Work wraps afterwards; a
// real error rolls back whichever tx raised it — and keeps the claim, so
// the retry continues as the claim's owner instead of re-racing for it.
func (w *QuoteSendWorker) run(ctx context.Context, args SendArgs, jobID int64) (cancelCause error, err error) {
	d, cancelCause, err := w.claim(ctx, args, jobID)
	if err != nil || cancelCause != nil || d == nil {
		return cancelCause, err
	}
	messageID, cancelCause, err := w.deliver(ctx, args, d)
	if err != nil || cancelCause != nil {
		return cancelCause, err
	}
	return w.record(ctx, args, messageID)
}

// claim is tx 1's boundary — the tenant tx commits the refusal audit (or
// nothing on the success path) and hands the provider phase the decision
// it proved and loaded.
func (w *QuoteSendWorker) claim(ctx context.Context, args SendArgs, jobID int64) (*sendDecision, error, error) {
	var d *sendDecision
	var cancelCause error
	err := w.Runner.InTenantTx(ctx, args.OrganizationID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, cancelCause, err = w.claimTx(ctx, tx, args, jobID)
		return err
	})
	return d, cancelCause, err
}

// claimTx is the claim phase inside tx 1: the same lock and refusal gate
// the single-tx send ran — missing/superseded/unapproved/already-sent —
// then margin, render key, recipient and the rfq/branding loads, ending
// in the claim insert itself. Every early exit releases the claim row:
// refusals drop it unconditionally (each refusal is data-deterministic —
// the same answer holds for any claimant) and the already-sent replay
// sweeps whatever a crashed run left.
func (w *QuoteSendWorker) claimTx(ctx context.Context, tx pgx.Tx, args SendArgs, jobID int64) (*sendDecision, error, error) {
	q, found, err := lockSendQuote(ctx, tx, args.QuoteID)
	if err != nil {
		return nil, nil, fmt.Errorf("jobs: quote_send: lock %s: %w", args.QuoteID, err)
	}
	if !found {
		// entity_id carries no FK, so the refusal is still auditable —
		// a send job for a deleted quote is drift, not a silent skip.
		return refused(w.refuse(ctx, tx, args, refuseQuoteMissing,
			fmt.Errorf("jobs: quote_send: quote %s does not exist", args.QuoteID)))
	}
	if q.Status == quoteApprovedSent {
		return nil, nil, dropClaim(ctx, tx, args.QuoteID) // replay: no second send
	}
	if q.Status == quoteSuperseded {
		return refused(w.refuse(ctx, tx, args, refuseSuperseded,
			fmt.Errorf("jobs: quote_send: quote %s is superseded", q.ID)))
	}
	if q.ApprovedAt == nil {
		return refused(w.refuse(ctx, tx, args, refuseUnapproved,
			fmt.Errorf("jobs: quote_send: quote %s was never approved", q.ID)))
	}
	return w.prepareClaim(ctx, tx, args, q, jobID)
}

// refused adapts refuse's (cancelCause, err) pair for the decision-
// returning claim paths — a refusal never carries a sendDecision.
func refused(cancelCause, err error) (*sendDecision, error, error) {
	return nil, cancelCause, err
}

// prepareClaim is the claim phase's second half: the margin and render-key
// refusals, the rfq/branding/recipient loads, then the claim insert —
// last, so a losing rival reads our committed row rather than racing an
// in-flight one.
func (w *QuoteSendWorker) prepareClaim(ctx context.Context, tx pgx.Tx, args SendArgs, q sendQuote, jobID int64) (*sendDecision, error, error) {
	if cancelCause, err := w.checkSendable(ctx, tx, args, q); cancelCause != nil || err != nil {
		return nil, cancelCause, err
	}
	d, cancelCause, err := w.loadDecision(ctx, tx, args, q)
	if cancelCause != nil || err != nil {
		return nil, cancelCause, err
	}
	if err := w.acquireClaim(ctx, tx, args, jobID); err != nil {
		return nil, nil, err
	}
	return d, nil, nil
}

// checkSendable is the data-deterministic refusal pair — margin flip and
// missing render key — unchanged from the single-tx send (spec S5: the
// margin check fires at approval AND at send).
func (w *QuoteSendWorker) checkSendable(ctx context.Context, tx pgx.Tx, args SendArgs, q sendQuote) (cancelCause error, err error) {
	negative, err := recheckMargin(q.PricingJSON)
	if err != nil {
		return nil, fmt.Errorf("jobs: quote_send: pricing_result_json on %s: %w", q.ID, err)
	}
	if negative {
		return w.refuseMargin(ctx, tx, args, q)
	}
	if q.PdfKey == nil || *q.PdfKey == "" {
		return w.refuse(ctx, tx, args, refuseMissingRender,
			fmt.Errorf("jobs: quote_send: quote %s has no pdf_storage_key", q.ID))
	}
	return nil, nil
}

// loadDecision reads the RFQ, validates the recipient and fetches
// branding — every row the provider phase needs, loaded while tx 1 is
// still open so no storage or network call runs under a lock.
func (w *QuoteSendWorker) loadDecision(ctx context.Context, tx pgx.Tx, args SendArgs, q sendQuote) (*sendDecision, error, error) {
	rfq, err := loadSendRfq(ctx, tx, q.RfqID)
	if err != nil {
		return nil, nil, fmt.Errorf("jobs: quote_send: load rfq %s: %w", q.RfqID, err)
	}
	branding, err := loadBranding(ctx, tx)
	if err != nil {
		return nil, nil, fmt.Errorf("jobs: quote_send: load org branding: %w", err)
	}
	to, err := recipient(rfq.CustomerEmail)
	if err != nil {
		return refused(w.refuse(ctx, tx, args, refuseNoRecipient, err))
	}
	return &sendDecision{quote: q, rfq: rfq, branding: branding, to: to}, nil, nil
}

// acquireClaim resolves the INSERT ... ON CONFLICT race inside tx 1. A
// fresh insert or our own row proceeds — a job's own claim is always
// valid for it, which is what makes every retry a single writer. A
// rival's row forks on river_job liveness: a live state means the send
// is in flight and this job returns a retryable error that converges
// once the holder finishes, while a terminal or missing job row is a
// stale claim the steal path replaces.
func (w *QuoteSendWorker) acquireClaim(ctx context.Context, tx pgx.Tx, args SendArgs, jobID int64) error {
	ok, err := insertClaim(ctx, tx, args, jobID)
	if err != nil || ok {
		return err
	}
	holder, found, err := claimHolder(ctx, tx, args.QuoteID)
	if err != nil {
		return err
	}
	if found && holder == jobID {
		return nil
	}
	if found {
		live, err := jobAlive(ctx, tx, holder)
		if err != nil {
			return err
		}
		if live {
			return fmt.Errorf("jobs: quote_send: quote %s send already in flight under river_job %d",
				args.QuoteID, holder)
		}
	}
	return stealClaim(ctx, tx, args, jobID)
}

// stealClaim replaces a stale row — the previous claimant's river_job is
// terminal or gone, so its provider call can never commit bookkeeping —
// or a row that vanished between the conflict and the read. One
// re-insert; a second conflict means another job won the same steal,
// which the caller's retry resolves.
func stealClaim(ctx context.Context, tx pgx.Tx, args SendArgs, jobID int64) error {
	if err := dropClaim(ctx, tx, args.QuoteID); err != nil {
		return err
	}
	ok, err := insertClaim(ctx, tx, args, jobID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("jobs: quote_send: quote %s claim lost a steal race", args.QuoteID)
	}
	return nil
}

// jobAlive reports whether river_job id sits in a live state — the
// dead-or-alive check the steal path runs. river_job is shared-schema
// (no RLS), so the lookup sees the platform's whole queue. The live set
// is envelope.go's liveJobStates — the same non-terminal vocabulary the
// enqueue dedupe window uses.
func jobAlive(ctx context.Context, tx pgx.Tx, jobID int64) (bool, error) {
	var live bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM river.river_job
			WHERE id = $1 AND state::text = ANY($2::text[])
		)`, jobID, liveJobStateNames()).Scan(&live)
	if err != nil {
		return false, fmt.Errorf("jobs: quote_send: river_job %d liveness: %w", jobID, err)
	}
	return live, nil
}

// liveJobStateNames renders envelope.go's liveJobStates as the text[]
// jobAlive compares river_job.state::text against — one source for the
// non-terminal vocabulary.
func liveJobStateNames() []string {
	states := liveJobStates()
	names := make([]string, len(states))
	for i, s := range states {
		names[i] = string(s)
	}
	return names
}

// insertClaim attempts the one-row claim; false means ON CONFLICT fired —
// a committed rival row exists and acquireClaim arbitrates it.
func insertClaim(ctx context.Context, tx pgx.Tx, args SendArgs, jobID int64) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO app.quote_send_claims (quote_id, organization_id, claimed_by)
		VALUES ($1, $2, $3)
		ON CONFLICT (quote_id) DO NOTHING`,
		args.QuoteID, args.OrganizationID, jobID)
	if err != nil {
		return false, fmt.Errorf("jobs: quote_send: claim %s: %w", args.QuoteID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// claimHolder reads the claim row for one quote — RLS-scoped, so a rival
// tenant's claim on the same quote_id is invisible (and unreachable: the
// quote lookup itself missed first).
func claimHolder(ctx context.Context, tx pgx.Tx, quoteID uuid.UUID) (int64, bool, error) {
	var holder int64
	err := tx.QueryRow(ctx,
		`SELECT claimed_by FROM app.quote_send_claims WHERE quote_id = $1`, quoteID).
		Scan(&holder)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("jobs: quote_send: read claim %s: %w", quoteID, err)
	}
	return holder, true, nil
}

// dropClaim removes the claim row unconditionally — the terminal paths
// (every refusal, the already-sent replay, the record commit) use it
// because their answer is data-deterministic for any claimant.
func dropClaim(ctx context.Context, tx pgx.Tx, quoteID uuid.UUID) error {
	_, err := tx.Exec(ctx,
		`DELETE FROM app.quote_send_claims WHERE quote_id = $1`, quoteID)
	if err != nil {
		return fmt.Errorf("jobs: quote_send: drop claim %s: %w", quoteID, err)
	}
	return nil
}

// dropOwnClaim removes the claim only when this job holds it — the
// dead-letter path can be reached off the in-flight error, where the row
// legitimately belongs to a live rival.
func dropOwnClaim(ctx context.Context, tx pgx.Tx, quoteID uuid.UUID, jobID int64) error {
	_, err := tx.Exec(ctx,
		`DELETE FROM app.quote_send_claims WHERE quote_id = $1 AND claimed_by = $2`,
		quoteID, jobID)
	if err != nil {
		return fmt.Errorf("jobs: quote_send: drop own claim %s: %w", quoteID, err)
	}
	return nil
}

// sendDecision is everything tx 1 proved and loaded for the provider
// phase: the locked quote row, the recipient RFQ, the org branding and
// the validated To address. None of it requires a transaction — that is
// the point of the split.
type sendDecision struct {
	quote    sendQuote
	rfq      sendRfq
	branding sendBranding
	to       string
}

// deliver is the provider phase, deliberately outside any transaction:
// Storage.Get and Sender.Send are the network calls the claim row exists
// to keep lock-free. Every input was already proved inside tx 1 — a
// render object gone since maps onto the same missing_render refusal,
// committed by refuseOutside in a fresh tx. Sender/configuration faults
// are not recipient refusals: they propagate as job errors — loud,
// retried, then dead-lettered — and the claim rides through the retry.
func (w *QuoteSendWorker) deliver(ctx context.Context, args SendArgs, d *sendDecision) (messageID string, cancelCause error, err error) {
	pdf, err := w.Storage.Get(ctx, *d.quote.PdfKey)
	if errors.Is(err, platform.ErrObjectNotFound) {
		c, e := w.refuseOutside(ctx, args, refuseMissingRender,
			fmt.Errorf("jobs: quote_send: render artifact %q is gone", *d.quote.PdfKey))
		return "", c, e
	}
	if err != nil {
		return "", nil, fmt.Errorf("jobs: quote_send: storage get %q: %w", *d.quote.PdfKey, err)
	}
	req, err := w.buildSendRequest(d.quote, d.rfq, d.branding, pdf, args.QuoteID, d.to)
	if err != nil {
		return "", nil, err
	}
	if messageID, err = w.Sender.Send(ctx, *req); err != nil {
		return "", nil, err // classified by Work: permanent cancels, transient retries
	}
	return messageID, nil, nil
}

// refuseOutside runs the refusal audit + claim release in a fresh tenant
// tx — the provider phase's only refusal path (render object gone) runs
// with no transaction open, so its decision needs its own commit
// boundary, like writeProviderRefusal.
func (w *QuoteSendWorker) refuseOutside(ctx context.Context, args SendArgs, reason string, cause error) (error, error) {
	var cancelCause error
	err := w.Runner.InTenantTx(ctx, args.OrganizationID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		cancelCause, err = w.refuse(ctx, tx, args, reason, cause)
		return err
	})
	return cancelCause, err
}

// record is tx 2's boundary — revalidate, bookkeeping, claim delete, one
// commit.
func (w *QuoteSendWorker) record(ctx context.Context, args SendArgs, messageID string) (cancelCause error, err error) {
	err = w.Runner.InTenantTx(ctx, args.OrganizationID, func(ctx context.Context, tx pgx.Tx) error {
		cancelCause, err = w.recordTx(ctx, tx, args, messageID)
		return err
	})
	return cancelCause, err
}

// recordTx re-locks the version and confirms the send is still current
// before booking it: a row already flipped APPROVED_SENT is the
// idempotent replay — the provider deduped on the idempotency key or a
// predecessor attempt committed — so the claim is swept and the job
// succeeds; SUPERSEDED or a vanished row runs the normal refusal. The
// claim dies in the same commit as recordSent, so a sent quote can never
// hold a live claim. Lock order is the invariant recordSent documents:
// quote first, then rfq_requests — never the inverse.
func (w *QuoteSendWorker) recordTx(ctx context.Context, tx pgx.Tx, args SendArgs, messageID string) (cancelCause error, err error) {
	q, found, err := lockSendQuote(ctx, tx, args.QuoteID)
	if err != nil {
		return nil, fmt.Errorf("jobs: quote_send: relock %s: %w", args.QuoteID, err)
	}
	if !found {
		return w.refuse(ctx, tx, args, refuseQuoteMissing,
			fmt.Errorf("jobs: quote_send: quote %s does not exist", args.QuoteID))
	}
	if q.Status == quoteApprovedSent {
		return nil, dropClaim(ctx, tx, args.QuoteID)
	}
	if q.Status == quoteSuperseded {
		return w.refuse(ctx, tx, args, refuseSuperseded,
			fmt.Errorf("jobs: quote_send: quote %s is superseded", q.ID))
	}
	if err := w.recordSent(ctx, tx, args, q, messageID); err != nil {
		return nil, err
	}
	return nil, dropClaim(ctx, tx, args.QuoteID)
}

// refuse writes the quote.send_refused audit row, releases the claim and
// returns the cause the tx commits — Work turns it into the JobCancel
// afterwards. The claim drop is unconditional on purpose: every refusal
// reason is data-deterministic, so the same answer holds for any claimant
// — a rival mid-send still self-cleans in its own record tx.
func (w *QuoteSendWorker) refuse(ctx context.Context, tx pgx.Tx, args SendArgs, reason string, cause error) (error, error) {
	if err := auditSend(ctx, tx, args.OrganizationID, "quote.send_refused", args.QuoteID,
		map[string]any{"reason": reason}); err != nil {
		return nil, err
	}
	if err := dropClaim(ctx, tx, args.QuoteID); err != nil {
		return nil, err
	}
	return cause, nil
}

// refuseMargin is the defense-in-depth refusal (spec: the check fires at
// approval AND at send): the RFQ parks NEEDS_REVIEW with
// NEGATIVE_MARGIN alongside the send_refused audit row.
func (w *QuoteSendWorker) refuseMargin(ctx context.Context, tx pgx.Tx, args SendArgs, q sendQuote) (error, error) {
	if _, err := tx.Exec(ctx, `
		UPDATE app.rfq_requests
		SET status = 'NEEDS_REVIEW', review_reason = $2, updated_at = now()
		WHERE id = $1`, q.RfqID, string(ReviewReasonNegativeMargin)); err != nil {
		return nil, fmt.Errorf("jobs: quote_send: park rfq %s: %w", q.RfqID, err)
	}
	return w.refuse(ctx, tx, args, refuseNegativeMargin,
		fmt.Errorf("jobs: quote_send: quote %s is priced below cost", q.ID))
}

// recordSent is the success bookkeeping (spec S5): quote and RFQ flip to
// APPROVED_SENT, the provider message id lands on
// sent_outbound_message_id (migration 0008's partial unique index is the
// final double-send guard) and quote.sent audits the delivery.
//
// Lock order is an invariant, not a detail: this tx took the quote row
// lock BEFORE this rfq_requests update (quote → RFQ). No path may hold an
// rfq_requests row lock while acquiring app.quotes — the reprocess gate's
// FOR UPDATE NOWAIT probe (draftRenderState) is the only RFQ → quote
// acquisition, and it stays non-blocking precisely so the two orders can
// never meet and deadlock.
func (w *QuoteSendWorker) recordSent(ctx context.Context, tx pgx.Tx, args SendArgs, q sendQuote, messageID string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE app.quotes
		SET status = 'APPROVED_SENT', sent_outbound_message_id = $2, updated_at = now()
		WHERE id = $1`, q.ID, messageID); err != nil {
		return fmt.Errorf("jobs: quote_send: flip quote %s: %w", q.ID, err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE app.rfq_requests
		SET status = 'APPROVED_SENT', updated_at = now()
		WHERE id = $1`, q.RfqID); err != nil {
		return fmt.Errorf("jobs: quote_send: flip rfq %s: %w", q.RfqID, err)
	}
	return auditSend(ctx, tx, args.OrganizationID, "quote.sent", q.ID,
		map[string]any{"message_id": messageID})
}

// writeProviderRefusal audits a permanent provider rejection in a fresh
// tenant transaction — the send's own tx rolled back, so without this row
// the cancelled delivery would be invisible to the tenant. Terminal
// refusal, so our claim goes with it (ownership-filtered like
// writeSendFailed's).
func (w *QuoteSendWorker) writeProviderRefusal(ctx context.Context, args SendArgs, jobID int64) error {
	return w.Runner.InTenantTx(ctx, args.OrganizationID, func(ctx context.Context, tx pgx.Tx) error {
		if err := auditSend(ctx, tx, args.OrganizationID, "quote.send_refused", args.QuoteID,
			map[string]any{"reason": refuseProviderPermanent}); err != nil {
			return err
		}
		return dropOwnClaim(ctx, tx, args.QuoteID, jobID)
	})
}

// writeSendFailed is the dead-letter half of S5: retries exhausted on a
// transient error parks the RFQ FAILED with the reason — a fresh tenant tx
// because the failed one already rolled back (mirrors writeFailedShell).
// The claim release is ownership-filtered: this path is also reachable off
// the in-flight error, where the row belongs to a live rival and must be
// left alone.
func (w *QuoteSendWorker) writeSendFailed(ctx context.Context, args SendArgs, jobID int64, attempts int, cause error) error {
	return w.Runner.InTenantTx(ctx, args.OrganizationID, func(ctx context.Context, tx pgx.Tx) error {
		reason := truncate(fmt.Sprintf("quote_send: exhausted %d attempts, last error: %s", attempts, cause), maxFailureReason)
		if _, err := tx.Exec(ctx, `
			UPDATE app.rfq_requests AS r
			SET status = 'FAILED', failure_reason = $2, updated_at = now()
			FROM app.quotes AS q
			WHERE q.id = $1 AND r.id = q.rfq_id`,
			args.QuoteID, reason); err != nil {
			return err
		}
		return dropOwnClaim(ctx, tx, args.QuoteID, jobID)
	})
}

// sendQuote is the locked slice of app.quotes the decision reads.
type sendQuote struct {
	ID          uuid.UUID
	RfqID       uuid.UUID
	Status      string
	QuoteNumber string
	Version     int
	ApprovedAt  *time.Time
	PdfKey      *string
	PricingJSON []byte
}

// lockSendQuote SELECTs the version FOR UPDATE. The lock is now
// deliberately short-lived (issue #65): it serializes the claim tx's
// check-then-insert and the record tx's revalidate-then-book — the
// quote_send_claims row, not this lock, is the mutual exclusion across
// the provider call.
func lockSendQuote(ctx context.Context, tx pgx.Tx, quoteID uuid.UUID) (sendQuote, bool, error) {
	var q sendQuote
	err := tx.QueryRow(ctx, `
		SELECT id, rfq_id, status::text, quote_number, version,
		       approved_at, pdf_storage_key, pricing_result_json
		FROM app.quotes WHERE id = $1 FOR UPDATE`, quoteID).
		Scan(&q.ID, &q.RfqID, &q.Status, &q.QuoteNumber, &q.Version,
			&q.ApprovedAt, &q.PdfKey, &q.PricingJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return sendQuote{}, false, nil
	}
	return q, err == nil, err
}

// sendRfq is the recipient + thread data the RFQ row carries.
type sendRfq struct {
	CustomerEmail string
	Subject       string
	ThreadHeaders []byte
}

// loadSendRfq reads the RFQ the quote hangs off — always present (FK), so a
// miss is contract drift and an error, never a quiet skip.
func loadSendRfq(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID) (sendRfq, error) {
	var r sendRfq
	var subject *string
	err := tx.QueryRow(ctx, `
		SELECT customer_email, subject, thread_headers_json::text
		FROM app.rfq_requests WHERE id = $1`, rfqID).
		Scan(&r.CustomerEmail, &subject, &r.ThreadHeaders)
	if err != nil {
		return sendRfq{}, err
	}
	if subject != nil {
		r.Subject = *subject
	}
	return r, nil
}

// sendBranding is the org's outbound pair (spec S5): the display name on
// From and the reply-to address customer answers land on.
type sendBranding struct {
	FromName string
	ReplyTo  string
}

// loadBranding reads outbound_from_name/outbound_reply_to through
// app.get_current_organization() — qp_app holds no direct grant on
// organizations, so the SECURITY DEFINER accessor is the only read path;
// inside the tenant tx it scopes to the resolved org by construction.
func loadBranding(ctx context.Context, tx pgx.Tx) (sendBranding, error) {
	var b sendBranding
	err := tx.QueryRow(ctx, `
		SELECT outbound_from_name, outbound_reply_to
		FROM app.get_current_organization()`).
		Scan(&b.FromName, &b.ReplyTo)
	return b, err
}

// recheckMargin decodes the pinned engine output — the same guard the
// approve endpoint runs, repeated here because data may have changed
// between approval and send (defense in depth, spec S5).
func recheckMargin(raw []byte) (bool, error) {
	if len(raw) == 0 {
		return false, errors.New("pricing_result_json is NULL")
	}
	var out pricing.Output
	if err := json.Unmarshal(raw, &out); err != nil {
		return false, fmt.Errorf("decode: %w", err)
	}
	return out.NegativeMargin(), nil
}

// buildSendRequest composes the minimal send (spec S5: the PDF, the quote
// number in the text, thread headers so the reply lands in the customer's
// existing conversation — S4's render owns the real HTML later).
func (w *QuoteSendWorker) buildSendRequest(q sendQuote, rfq sendRfq, b sendBranding, pdf []byte, quoteID uuid.UUID, to string) (*resend.SendRequest, error) {
	from, err := fromAddress(b, w.OutboundFrom)
	if err != nil {
		return nil, err
	}
	return &resend.SendRequest{
		To:       []string{to},
		From:     from,
		ReplyTo:  b.ReplyTo,
		Subject:  replySubject(rfq.Subject, q.QuoteNumber),
		TextBody: textBody(q.QuoteNumber, b.FromName),
		Attachments: []resend.SendAttachment{{
			Filename: q.QuoteNumber + ".pdf",
			Content:  pdf,
		}},
		Headers:        replyHeaders(rfq.ThreadHeaders),
		IdempotencyKey: fmt.Sprintf("%s:%d", quoteID, q.Version),
	}, nil
}

// recipient validates the RFQ's customer address — an empty or malformed
// address is deterministic drift (the row was written from a verified
// inbound From), so it refuses rather than retrying.
func recipient(email string) (string, error) {
	addr, err := mail.ParseAddress(strings.TrimSpace(email))
	if err != nil {
		return "", fmt.Errorf("jobs: quote_send: rfq customer_email %q is not a usable address", email)
	}
	return addr.Address, nil
}

// fromAddress builds "Name <platform-addr>" — From must be a Resend-
// verified domain, and the tenant's reply_to is not one (sending From an
// unverified domain is a permanent 4xx). The tenant's name still fronts
// the message and its outbound_reply_to rides as Reply-To, so the customer
// sees and answers the tenant's address either way. An empty outboundFrom
// is a wiring bug — a plain error, not a refusal, so it surfaces as a
// retryable job failure instead of a silent cancel.
func fromAddress(b sendBranding, outboundFrom string) (string, error) {
	addr := strings.TrimSpace(outboundFrom)
	if addr == "" {
		return "", errors.New("jobs: quote_send: outbound From address not configured (QP_OUTBOUND_FROM)")
	}
	name := strings.TrimSpace(b.FromName)
	if name == "" {
		return addr, nil
	}
	return fmt.Sprintf("%s <%s>", name, addr), nil
}

// replyHeaders turns thread_headers_json into the outbound threading pair:
// In-Reply-To anchors on the RFQ's own Message-ID (the message the quote
// answers), References chains the inbound References plus that Message-ID
// per RFC 5322. With no Message-ID the inbound pair passes through so the
// mail still threads onto whatever the customer replied to.
func replyHeaders(raw []byte) map[string]string {
	var t map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &t) != nil {
		return nil
	}
	str := func(k string) string {
		if v, ok := t[k].(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	messageID := str("message_id")
	references := strings.TrimSpace(str("references") + " " + messageID)
	inReplyTo := messageID
	if inReplyTo == "" {
		inReplyTo = str("in-reply-to")
	}
	out := map[string]string{}
	if inReplyTo != "" {
		out["In-Reply-To"] = inReplyTo
	}
	if references != "" {
		out["References"] = references
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// replySubject prefixes the RFQ subject with Re: once; an empty subject
// falls back to the quote number so the mail is never subjectless.
func replySubject(subject, quoteNumber string) string {
	s := strings.TrimSpace(subject)
	if s == "" {
		return "Quote " + quoteNumber
	}
	if len(s) >= 3 && strings.EqualFold(s[:3], "re:") {
		return s
	}
	return "Re: " + s
}

// textBody is the minimal body spec S5 asks for — the quote number is the
// reference every support conversation starts from.
func textBody(quoteNumber, fromName string) string {
	return fmt.Sprintf("Hello,\n\nPlease find attached your quotation %s.\n\nReply to this email with any questions.\n\n%s", quoteNumber, fromName)
}

// auditSend appends the tenant_audit_logs row — worker actions carry no
// actor (ActorUserID nil; a machine did this, not a user).
func auditSend(ctx context.Context, tx pgx.Tx, org uuid.UUID, action string, quoteID uuid.UUID, meta map[string]any) error {
	if _, err := platform.NewStore(tx).InsertAuditLog(ctx, platform.AuditEntry{
		OrganizationID: org,
		Action:         action,
		EntityType:     "quote",
		EntityID:       quoteID,
		Metadata:       meta,
	}); err != nil {
		return fmt.Errorf("jobs: quote_send: audit %q on %s: %w", action, quoteID, err)
	}
	return nil
}
