package jobs

// Claim-row machinery for the S5 redesign (issue #65): everything the
// claim phase (tx 1) does — the quote lock and refusal gate, the decision
// loads, and the quote_send_claims insert/arbitrate/steal lifecycle that
// carries the mutual exclusion the provider phase runs under. Split out
// of quotesend.go so both files stay under the 500-line limit (AGENTS.md).

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

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
			return fmt.Errorf("%w: quote %s under river_job %d",
				errSendInFlight, args.QuoteID, holder)
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
