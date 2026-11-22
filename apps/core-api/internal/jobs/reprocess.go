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
// receipt lookup key) and whether both body columns are NULL — the exact
// signature of the S1 dead-letter shell insertFailedShell writes (every
// later stage failure has at least one body stored, so bodies-NULL alone
// identifies the shell; reprocess also only sees parked/READY rows).
type ShellFacts struct {
	ResendMessageID string
	BodiesMissing   bool
}

// DecideResume walks the checkpoint chain for one already-locked RFQ row
// inside the caller's tenant transaction:
//
//  0. Ingest shell — both body columns NULL is the signature of the S1
//     dead-letter shell (permanent Resend fetch failure): re-running
//     extraction with nothing to extract is hopeless, so rfq_ingest
//     re-runs for the receipt instead (RECEIVED) and the worker promotes
//     the shell in place. A missing inbound_messages receipt can't
//     resurrect the message — the walk falls through to rfq_extract,
//     which parks NO_USABLE_TEXT and stays visible.
//  1. Usable extraction — extracted_payload_json and
//     extraction_confidence are the pair S2 commits together; either
//     absent ⇒ rfq_extract (PARSING). NO_USABLE_TEXT, INVALID_EXTRACTION,
//     TARIFF_UPDATE parks and dead-lettered FAILED shells land here:
//     with nothing usable downstream, re-running S2 is the only honest
//     resume. (Spec's "re-extraction is a separate explicit request"
//     means a future dedicated endpoint, not a refusal here — flagged on
//     the PR.)
//  2. Carrier checkpoint owed-but-missing — the S3a owed gate recomputed
//     off the stored extraction (freightOwed: FCL + derived IMPORT +
//     OwesImportFreight); no carrier_offers row for the recomputed
//     request_hash ⇒ rfq_fetch_rates (PRICING). Any recorded outcome —
//     OFFER_FOUND, NO_OFFER, FETCH_FAILED, NOT_APPLICABLE — counts as
//     completed: the fetch is never repeated (spec Reprocess).
//  3. No DRAFT quote ⇒ rfq_price (PRICING). LOW_CONFIDENCE,
//     MISSING_TARIFF, REQUIRED_FIELD_MISSING and the other business
//     parks land here or at step 2 — the human reprocess IS the gate
//     override; fields stay persisted and the quote keeps the pinned
//     confidence.
//  4. DRAFT quote with NULL pdf_storage_key ⇒ rfq_render (READY — the
//     RFQ is already past pricing; render re-runs for that version).
//
// A DRAFT quote whose pdf_storage_key is set means every checkpoint
// exists: the park then questions the quote itself (NEGATIVE_MARGIN at
// send, a dead-lettered delivery), so the honest resume is rfq_price —
// S3b supersedes the stale DRAFT into a new version (rule 9) instead of
// re-rendering the questioned one.
func DecideResume(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID, shell ShellFacts, payload []byte, confidenceSet bool, now time.Time) (ResumeDecision, error) {
	if shell.BodiesMissing {
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
	quoteID, rendered, found, err := draftRenderState(ctx, tx, rfqID)
	if err != nil {
		return ResumeDecision{}, err
	}
	switch {
	case !found:
		return ResumeDecision{Stage: ResumePrice, Status: rfqPricing}, nil
	case !rendered:
		return ResumeDecision{Stage: ResumeRender, Status: "READY", QuoteID: quoteID}, nil
	default:
		return ResumeDecision{Stage: ResumePrice, Status: rfqPricing}, nil
	}
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
// other READY shape stays a 409).
func DraftAwaitingRender(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID) (bool, error) {
	_, rendered, found, err := draftRenderState(ctx, tx, rfqID)
	if err != nil {
		return false, err
	}
	return found && !rendered, nil
}

// draftRenderState reads the RFQ's DRAFT quote — at most one exists
// (commitQuote supersedes every prior DRAFT in the same tx); the
// version-DESC pick keeps the lookup honest if data ever drifts.
// rendered means pdf_storage_key carries a key.
func draftRenderState(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID) (quoteID uuid.UUID, rendered, found bool, err error) {
	var pdfKey *string
	err = tx.QueryRow(ctx, `
		SELECT id, pdf_storage_key FROM app.quotes
		WHERE rfq_id = $1 AND status = 'DRAFT'
		ORDER BY version DESC LIMIT 1`, rfqID).Scan(&quoteID, &pdfKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, false, nil
	}
	if err != nil {
		return uuid.Nil, false, false, fmt.Errorf("jobs: reprocess: draft quote on %s: %w", rfqID, err)
	}
	return quoteID, pdfKey != nil && *pdfKey != "", true, nil
}
