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
//     outcome releases pricing), so the walk never reaches the
//     time-dependent request_hash recompute for a priced RFQ: a next-day
//     reprocess would recompute ready_date off `now`, drift the hash and
//     mistake the completed fetch for a missing one — a paid re-fetch
//     the spec forbids (Devin Review). NULL pdf_storage_key ⇒
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
//     rfq_price. Any recorded outcome — OFFER_FOUND, NO_OFFER,
//     FETCH_FAILED, NOT_APPLICABLE — counts as completed: the fetch is
//     never repeated (spec Reprocess). LOW_CONFIDENCE, MISSING_TARIFF,
//     REQUIRED_FIELD_MISSING and the other business parks land here —
//     the human reprocess IS the gate override; fields stay persisted
//     and the quote keeps the pinned confidence.
func DecideResume(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID, shell ShellFacts, payload []byte, confidenceSet bool, now time.Time) (ResumeDecision, error) {
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
	quoteID, rendered, found, err := draftRenderState(ctx, tx, rfqID)
	if err != nil {
		return ResumeDecision{}, err
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
	missing, err := carrierCheckpointMissing(ctx, tx, rfqID, fields, now)
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
// for the request hash the fetch would carry today. owed=false
// short-circuits: no fetch was ever owed, so nothing is missing
// (NOT_APPLICABLE rows are an S3a write, not a checkpoint the reprocess
// owes).
func carrierCheckpointMissing(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID, fields map[string]extractedField, now time.Time) (bool, error) {
	modal, _ := parseModalField(fields)
	incoterm, _ := parseIncotermField(fields)
	origin, _ := requiredLocode(fields, "origin_locode")
	dest, _ := requiredLocode(fields, "dest_locode")
	if !freightOwed(modal, incoterm, origin, dest) {
		return false, nil
	}
	// The hash must match what S3a computes for this extraction — the
	// worker's own rateRequest normalizes it (locodes, equipment,
	// quantity and the ready_date fallback off the injected clock).
	req, _ := rateRequest(fields, now)
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
// other READY shape stays a 409). The row is locked FOR UPDATE — the same
// lock the render worker's lockRenderQuote takes — so the check can never
// observe a mid-flight render (Devin Review: without it, a `running`
// rfq_render dedupe-hit at enqueue time was not provably about-to-complete,
// because the worker locks the QUOTE row, not the RFQ the endpoint holds).
func DraftAwaitingRender(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID) (bool, error) {
	_, rendered, found, err := draftRenderState(ctx, tx, rfqID)
	if err != nil {
		return false, err
	}
	return found && !rendered, nil
}

// draftRenderState reads the RFQ's DRAFT quote FOR UPDATE — at most one
// exists (commitQuote supersedes every prior DRAFT in the same tx); the
// version-DESC pick keeps the lookup honest if data ever drifts.
// rendered means pdf_storage_key carries a key. FOR UPDATE is the lock
// invariant behind the endpoint's running-dedupe-hit reasoning: the
// rfq_render worker takes this same lock on the quote row inside
// lockRenderQuote, so a mid-flight render blocks this read until its tx
// ends — after which the row either shows the pinned pdf_storage_key or
// no in-flight render can exist. A `running` dedupe hit at enqueue time
// is then provably about-to-complete (Devin Review).
func draftRenderState(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID) (quoteID uuid.UUID, rendered, found bool, err error) {
	var pdfKey *string
	err = tx.QueryRow(ctx, `
		SELECT id, pdf_storage_key FROM app.quotes
		WHERE rfq_id = $1 AND status = 'DRAFT'
		ORDER BY version DESC LIMIT 1
		FOR UPDATE`, rfqID).Scan(&quoteID, &pdfKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, false, nil
	}
	if err != nil {
		return uuid.Nil, false, false, fmt.Errorf("jobs: reprocess: draft quote on %s: %w", rfqID, err)
	}
	return quoteID, pdfKey != nil && *pdfKey != "", true, nil
}
