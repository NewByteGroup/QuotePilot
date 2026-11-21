package jobs

// ReviewReason is the controlled vocabulary written into
// rfq_requests.review_reason (spec 2026-11-11-staged-quote-pipeline S3b):
// why a human must look at a parked (NEEDS_REVIEW) RFQ. It is never
// failure_reason — that is reserved for technical failures after retries.
// Writes go through these constants only, never a string literal at the
// call site, and the column stays plain text with no CHECK: the real-
// Postgres test fails on any stored value outside ReviewReasons, so adding
// a code is a code change here, not a migration. P2 adds its own codes
// (tariff-update parking, low confidence) in its PR — they are deliberately
// absent until that worker exists.
type ReviewReason string

const (
	// ReviewReasonExportMatrixOpen parks EXPORT RFQs: the engine returns
	// INCOMPLETE{EXPORT_MATRIX_OPEN} and the export matrix is still pending
	// (RFC-001 questions 13-16), so the RFQ waits for analyst reprocess —
	// nothing broke, the scope is not priced yet.
	ReviewReasonExportMatrixOpen ReviewReason = "EXPORT_MATRIX_OPEN"
	// ReviewReasonMissingTariff means no private-tariff row exists for a
	// required carrier group — the analyst adds the tariff, then reprocesses.
	ReviewReasonMissingTariff ReviewReason = "MISSING_TARIFF"
	// ReviewReasonStaleTariff means tariff rows exist but none is valid on
	// the quote date — the analyst refreshes validity, then reprocesses.
	ReviewReasonStaleTariff ReviewReason = "STALE_TARIFF"
	// ReviewReasonNegativeMargin parks the RFQ when S5's defense-in-depth
	// re-check finds the approved quote priced below cost (spec S5: a
	// negative-margin quote is never sent; the analyst reprices as a new
	// version — nothing broke, the margin gate did its job).
	ReviewReasonNegativeMargin ReviewReason = "NEGATIVE_MARGIN"
	// ReviewReasonRequiredFieldMissing parks an RFQ whose persisted
	// extraction lacks (or cannot parse) a field pricing cannot run
	// without — the analyst reviews the extraction, then reprocesses.
	// S3b treats the extraction's required keys plus the modal-dependent
	// ones (equipment for FCL/AIR, quantity for FCL) as required.
	ReviewReasonRequiredFieldMissing ReviewReason = "REQUIRED_FIELD_MISSING"
	// ReviewReasonDirectionAmbiguous parks an RFQ whose locodes do not
	// resolve a direction: S3b derives IMPORT from a BR destination and
	// EXPORT from a BR origin; both-BR or neither-BR is ambiguous under
	// RFC-001 Q15 (still open) and needs the analyst, not a guess.
	ReviewReasonDirectionAmbiguous ReviewReason = "DIRECTION_AMBIGUOUS"
	// ReviewReasonPricingIncomplete is the generic park for every engine
	// INCOMPLETE reason without a dedicated code (MISSING_FX:*,
	// NO_USABLE_OFFER, MISSING_CARGO_VALUE, SEA_INCOTERM_ON_AIR,
	// MODAL_OUT_OF_MVP, MISSING_TERMINAL_BAND, MISSING_DECLARED_FREIGHT,
	// MISSING_CUSTOMS_FX, a missing pricing_rules row or a malformed
	// optional extraction field). The engine's own Reasons ride the audit
	// row verbatim, so the generic code loses no detail.
	ReviewReasonPricingIncomplete ReviewReason = "PRICING_INCOMPLETE"
)

// ReviewReasons is the closed list the stored-value test checks against:
// any review_reason found in rfq_requests must be one of these.
var ReviewReasons = []ReviewReason{
	ReviewReasonExportMatrixOpen,
	ReviewReasonMissingTariff,
	ReviewReasonStaleTariff,
	ReviewReasonNegativeMargin,
	ReviewReasonRequiredFieldMissing,
	ReviewReasonDirectionAmbiguous,
	ReviewReasonPricingIncomplete,
}
