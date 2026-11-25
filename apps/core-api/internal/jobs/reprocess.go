package jobs

// reprocess.go — the checkpoint walk behind POST /v1/rfqs/{id}/reprocess
// (spec 2026-11-11-staged-quote-pipeline section 4 "Reprocess"). The
// endpoint locks the RFQ and clears its reasons; this file decides which
// pipeline stage resumes — the earliest MISSING checkpoint wins, so the
// paid extraction and a completed carrier fetch are never repeated. Every
// helper the walk recomputes (the S3a owed gate, request normalization,
// request_hash, the checkpoint lookup) is the worker's own — the decision
// can never drift from what the stage would actually do.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/carrier"
)

// ResumeStage is the job-kind vocabulary of a reprocess resume — the same
// strings river_job.kind carries, derived from the args types so a rename
// cannot split the two.
type ResumeStage string

var (
	ResumeIngest  = ResumeStage(IngestArgs{}.Kind())
	ResumeExtract = ResumeStage(ExtractArgs{}.Kind())
	ResumeRates   = ResumeStage(RateArgs{}.Kind())
	ResumePrice   = ResumeStage(PriceArgs{}.Kind())
	ResumeRender  = ResumeStage(RenderArgs{}.Kind())
)

// ResumeDecision is the earliest missing checkpoint: the stage's job kind
// and the rfq_status the RFQ takes while the stage re-runs. QuoteID is set
// for ResumeRender only — rfq_render is the one stage keyed off a quote
// row (the DRAFT version whose pdf_storage_key is missing).
// InboundMessageID is set for ResumeIngest only — rfq_ingest is keyed off
// the inbound_messages receipt the shell's resend_message_id resolved to.
type ResumeDecision struct {
	Stage            ResumeStage
	Status           string
	QuoteID          uuid.UUID
	InboundMessageID uuid.UUID
}

// ShellFacts is the ingest-shell input the endpoint read off its
// already-locked RFQ row: the resend_message_id the shell stored (the
// receipt lookup key), whether both body columns are NULL, and whether
// zero rfq_attachments rows exist — together the exact signature of the
// S1 dead-letter shell insertFailedShell writes. A successfully-ingested
// attachment-only email also leaves the bodies NULL, but it always
// stores rfq_attachments rows; the shell never does, so the attachment
// count is the distinguisher (Devin Review).
type ShellFacts struct {
	ResendMessageID string
	BodiesMissing   bool
	NoAttachments   bool
}

// ErrQuoteBusy is DecideResume's answer when the DRAFT quote's row lock
// is already held — draftRenderState's NOWAIT refusal (55P03). A live
// worker (rfq_render's lockRenderQuote, quote_send's claim/record txs'
// lockSendQuote) owns downstream state, so the park claim a reprocess
// would act on is wrong for now. The endpoint maps it to the same 409 a
// not-parked RFQ gets: a refusal, never a fault.
var ErrQuoteBusy = errors.New("jobs: reprocess: draft quote is locked by a live worker")

// DecideResume walks the checkpoint chain for one already-locked RFQ row
// inside the caller's tenant transaction:
//
//  0. Ingest shell — both body columns NULL, zero rfq_attachments rows
//     and no persisted extraction is the signature of the S1 dead-letter
//     shell (permanent Resend fetch failure): re-running extraction with
//     nothing to extract is hopeless, so rfq_ingest re-runs for the
//     receipt instead (RECEIVED) and the worker promotes the shell in
//     place. The attachment and payload guards keep a successfully-
//     ingested attachment-only email — bodies NULL, attachments stored,
//     possibly an extraction committed — out of step 0: insertFailedShell
//     never reaches storeAttachments, so zero attachment rows is the
//     distinguisher (Devin Review). A missing inbound_messages receipt
//     can't resurrect the message — the walk falls through to
//     rfq_extract, which parks NO_USABLE_TEXT and stays visible.
//  1. Usable extraction — extracted_payload_json and
//     extraction_confidence are the pair S2 commits together; either
//     absent ⇒ rfq_extract (PARSING). NO_USABLE_TEXT, INVALID_EXTRACTION,
//     TARIFF_UPDATE parks and dead-lettered FAILED shells land here:
//     with nothing usable downstream, re-running S2 is the only honest
//     resume. (Spec's "re-extraction is a separate explicit request"
//     means a future dedicated endpoint, not a refusal here — flagged on
//     the PR.)
//  2. DRAFT quote — its existence PROVES the S3a checkpoint committed
//     (S3a always writes its outcome row before enqueueing S3b — every
//     outcome releases pricing), so a priced RFQ never even reaches the
//     request_hash recompute. A row lock already held ⇒ ErrQuoteBusy:
//     a live worker owns the quote and the park claim is wrong while it
//     runs (the endpoint answers the not-parked 409). NULL pdf_storage_key ⇒
//     rfq_render (READY — the RFQ is already past pricing; render
//     re-runs for that version). A rendered DRAFT means every
//     checkpoint exists ⇒ rfq_price (PRICING): the park questions the
//     quote itself (NEGATIVE_MARGIN at send, a dead-lettered delivery),
//     so S3b supersedes the stale DRAFT into a new version (rule 9)
//     instead of re-rendering the questioned one.
//  3. No DRAFT — only now is the carrier checkpoint recomputed: the S3a
//     owed gate off the stored extraction (freightOwed: FCL + derived
//     IMPORT + OwesImportFreight); no carrier_offers row for the
//     recomputed request_hash ⇒ rfq_fetch_rates (PRICING), else
//     rfq_price. The hash's ready_date fallback is the RFQ's received_at
//     — stable per entity, so a next-day reprocess recomputes the SAME
//     digest S3a pinned and the completed (paid) fetch is never
//     mistaken for missing (Devin Review round 3). Any recorded outcome
//     — OFFER_FOUND, NO_OFFER,
//     FETCH_FAILED, NOT_APPLICABLE — counts as completed: the fetch is
//     never repeated (spec Reprocess). LOW_CONFIDENCE, MISSING_TARIFF,
//     REQUIRED_FIELD_MISSING and the other business parks land here —
//     the human reprocess IS the gate override; fields stay persisted
//     and the quote keeps the pinned confidence.
//
// receivedAt is the locked RFQ's received_at — the stable ready_date
// fallback the hash recompute shares with the S3a worker.
func DecideResume(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID, shell ShellFacts, payload []byte, confidenceSet bool, receivedAt time.Time) (ResumeDecision, error) {
	if shell.BodiesMissing && shell.NoAttachments && len(payload) == 0 {
		id, found, err := inboundMessageID(ctx, tx, shell.ResendMessageID)
		if err != nil {
			return ResumeDecision{}, err
		}
		if found {
			return ResumeDecision{Stage: ResumeIngest, Status: "RECEIVED", InboundMessageID: id}, nil
		}
	}
	if len(payload) == 0 || !confidenceSet {
		return ResumeDecision{Stage: ResumeExtract, Status: "PARSING"}, nil
	}
	quoteID, rendered, found, busy, err := draftRenderState(ctx, tx, rfqID)
	if err != nil {
		return ResumeDecision{}, err
	}
	if busy {
		return ResumeDecision{}, ErrQuoteBusy
	}
	if found {
		if !rendered {
			return ResumeDecision{Stage: ResumeRender, Status: "READY", QuoteID: quoteID}, nil
		}
		return ResumeDecision{Stage: ResumePrice, Status: rfqPricing}, nil
	}
	fields, err := decodePriceExtraction(payload)
	if err != nil {
		// Non-NULL columns that no longer decode are stored drift — S2
		// contract-validated this document before committing it. Loud,
		// not a resume anywhere downstream that would decode the same
		// bytes and cancel.
		return ResumeDecision{}, fmt.Errorf("jobs: reprocess: extraction on %s: %w", rfqID, err)
	}
	missing, err := carrierCheckpointMissing(ctx, tx, rfqID, fields, receivedAt)
	if err != nil {
		return ResumeDecision{}, err
	}
	if missing {
		return ResumeDecision{Stage: ResumeRates, Status: rfqPricing}, nil
	}
	return ResumeDecision{Stage: ResumePrice, Status: rfqPricing}, nil
}

// carrierCheckpointMissing recomputes the S3a owed gate off the stored
// extraction fields — the same parse helpers and freightOwed call the
// rfq_fetch_rates worker runs — and reports whether no outcome row exists
// for the request hash the fetch would carry. owed=false
// short-circuits: no fetch was ever owed, so nothing is missing
// (NOT_APPLICABLE rows are an S3a write, not a checkpoint the reprocess
// owes).
func carrierCheckpointMissing(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID, fields map[string]extractedField, receivedAt time.Time) (bool, error) {
	modal, _ := parseModalField(fields)
	incoterm, _ := parseIncotermField(fields)
	origin, _ := requiredLocode(fields, "origin_locode")
	dest, _ := requiredLocode(fields, "dest_locode")
	if !freightOwed(modal, incoterm, origin, dest) {
		return false, nil
	}
	// The hash must match what S3a computes for this extraction — the
	// worker's own rateRequest normalizes it (locodes, equipment,
	// quantity and the ready_date fallback off the RFQ's received_at,
	// the same value the worker read off its own locked row).
	req, _ := rateRequest(fields, receivedAt)
	exists, err := checkpointExists(ctx, tx, rfqID, carrier.RequestHash(req))
	if err != nil {
		return false, fmt.Errorf("jobs: reprocess: carrier checkpoint on %s: %w", rfqID, err)
	}
	return !exists, nil
}

// inboundMessageID resolves the receipt an ingest shell points at. RLS
// scopes the lookup to the tenant org and (organization_id,
// resend_message_id) is UNIQUE, so at most one row answers.
func inboundMessageID(ctx context.Context, tx pgx.Tx, resendID string) (uuid.UUID, bool, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT id FROM app.inbound_messages WHERE resend_message_id = $1`, resendID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("jobs: reprocess: receipt for %q: %w", resendID, err)
	}
	return id, true, nil
}

// DraftAwaitingRender reports whether the RFQ's DRAFT quote still waits on
// its first render — the READY-status carveout of the reprocess gate (S4's
// terminal failure leaves the RFQ READY with an unrendered DRAFT; every
// other READY shape stays a 409). busy reports draftRenderState's NOWAIT
// refusal: a live worker's tx owns the quote row (the same lock the render
// worker's lockRenderQuote takes), which is honest "not reprocessable"
// signal — a mid-flight render can never be observed as a stale row and
// never has to be waited out (Devin Review: without the lock, a `running`
// rfq_render dedupe-hit at enqueue time was not provably about-to-complete,
// because the worker locks the QUOTE row, not the RFQ the endpoint holds).
// The probe is the only RFQ → quote acquisition anywhere — the lock-order
// invariant recordSent documents is what keeps it safe to run NOWAIT.
func DraftAwaitingRender(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID) (awaiting, busy bool, err error) {
	_, rendered, found, busy, err := draftRenderState(ctx, tx, rfqID)
	if err != nil {
		return false, false, err
	}
	return found && !rendered, busy, nil
}

// draftRenderState reads the RFQ's DRAFT quote FOR UPDATE NOWAIT — at most
// one exists (commitQuote supersedes every prior DRAFT in the same tx); the
// version-DESC pick keeps the lookup honest if data ever drifts. rendered
// means pdf_storage_key carries a key.
//
// NOWAIT is a lock-ordering rule, not impatience: this endpoint already
// holds FOR UPDATE on the RFQ row (RFQ → quote), while the live workers
// acquire the quote row lock first and update the RFQ afterwards
// (quote → RFQ; recordSent documents the invariant — no path may hold an
// rfq_requests row lock while acquiring app.quotes). A blocking wait here
// sits in the inverse order and deadlocks. A contended row is honest
// "not reprocessable" signal instead — a live worker owns downstream
// state — so lock_not_available (55P03) reports busy rather than
// waiting. The lock attempt still matters: acquiring it proves no render
// is mid-flight, which is what makes a `running` dedupe hit at enqueue
// time provably about-to-complete (Devin Review).
//
// The SAVEPOINT is not optional: 55P03 aborts the whole transaction like
// any statement error, so the probe must fail into a savepoint and roll
// back to it — otherwise the refusal path's own commit dies with
// "commands ignored until end of transaction block". A lingering
// savepoint is harmless (released at commit); on the READY-accepted
// path a second call (gate probe, then DecideResume's) redefines the
// same name — also harmless, since our own tx already holds the row
// lock and the probe can never contend against itself.
func draftRenderState(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID) (quoteID uuid.UUID, rendered, found, busy bool, err error) {
	id, pdfKey, busy, err := probeQuote(ctx, tx, rfqID)
	if err != nil || busy || id == uuid.Nil {
		return uuid.Nil, false, false, busy, err
	}
	return id, pdfKey != nil && *pdfKey != "", true, false, nil
}

// probeQuote runs the SAVEPOINT + FOR UPDATE NOWAIT + rollback sequence
// draftRenderState's contract comment documents: busy on the 55P03
// refusal with the tx already rolled back to the savepoint, uuid.Nil
// when no DRAFT row exists, the row's id and pdf_storage_key otherwise.
func probeQuote(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID) (quoteID uuid.UUID, pdfKey *string, busy bool, err error) {
	if _, err = tx.Exec(ctx, `SAVEPOINT reprocess_quote_probe`); err != nil {
		return uuid.Nil, nil, false, fmt.Errorf("jobs: reprocess: quote probe savepoint on %s: %w", rfqID, err)
	}
	err = tx.QueryRow(ctx, `
		SELECT id, pdf_storage_key FROM app.quotes
		WHERE rfq_id = $1 AND status = 'DRAFT'
		ORDER BY version DESC LIMIT 1
		FOR UPDATE NOWAIT`, rfqID).Scan(&quoteID, &pdfKey)
	if err == nil {
		return quoteID, pdfKey, false, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, nil, false, nil
	}
	if isLockNotAvailable(err) {
		return uuid.Nil, nil, true, probeRollback(ctx, tx, rfqID)
	}
	return uuid.Nil, nil, false, fmt.Errorf("jobs: reprocess: draft quote on %s: %w", rfqID, err)
}

// probeRollback frees the tx from its aborted state after the NOWAIT
// refusal — rolling back to the savepoint is what keeps the refusal
// path's own commit alive (a bare statement error would otherwise
// poison every later statement in the tx).
func probeRollback(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT reprocess_quote_probe`); err != nil {
		return fmt.Errorf("jobs: reprocess: quote probe rollback on %s: %w", rfqID, err)
	}
	return nil
}

// sqlstateLockNotAvailable is Postgres's 55P03 — SQLSTATE
// lock_not_available, the NOWAIT refusal a contended row lock raises.
// The literal avoids a direct dependency (github.com/jackc/pgerrcode)
// for a single constant.
const sqlstateLockNotAvailable = "55P03"

// isLockNotAvailable reports whether err is Postgres 55P03 — the NOWAIT
// refusal a contended row lock raises.
func isLockNotAvailable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == sqlstateLockNotAvailable
}
