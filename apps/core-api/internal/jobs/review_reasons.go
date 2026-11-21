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

	// P2 (rfq_extract) codes — spec S2 parks three ways before pricing.
	// ReviewReasonTariffUpdate parks a classifier-flagged tariff update:
	// a tariff never enters pricing without a reviewed batch (RFC-002),
	// and a misclassified real RFQ stays visible on the review list
	// instead of being priced or dropped.
	ReviewReasonTariffUpdate ReviewReason = "TARIFF_UPDATE"
	// ReviewReasonLowConfidence parks a valid extraction whose minimum
	// field confidence sits below the gate — fields stay persisted so the
	// review queue is pre-filled (the gate is the triage).
	ReviewReasonLowConfidence ReviewReason = "LOW_CONFIDENCE"
	// ReviewReasonNoUsableText parks an RFQ with no usable body text and
	// no successfully parsed attachment — nothing worth paying an LLM
	// call for. The analyst supplies the missing info, then reprocesses.
	ReviewReasonNoUsableText ReviewReason = "NO_USABLE_TEXT"
	// ReviewReasonInvalidExtraction parks after a contract-violating
	// ai-engine output survived its single retry (spec section 3: at
	// most one retry, each attempt costs money). The invalid payload is
	// never written; the validation error is audited.
	ReviewReasonInvalidExtraction ReviewReason = "INVALID_EXTRACTION"
	// ReviewReasonEngineUnavailable parks when transient ai-engine
	// failures exhausted the job's attempts — committed NEEDS_REVIEW
	// instead of letting the row sit invisible in PARSING after River
	// discards the job. The analyst reprocesses once the engine is back.
	ReviewReasonEngineUnavailable ReviewReason = "ENGINE_UNAVAILABLE"
)

// ReviewReasons is the closed list the stored-value test checks against:
// any review_reason found in rfq_requests must be one of these.
var ReviewReasons = []ReviewReason{
	ReviewReasonExportMatrixOpen,
	ReviewReasonMissingTariff,
	ReviewReasonStaleTariff,
	ReviewReasonNegativeMargin,
	ReviewReasonTariffUpdate,
	ReviewReasonLowConfidence,
	ReviewReasonNoUsableText,
	ReviewReasonInvalidExtraction,
	ReviewReasonEngineUnavailable,
}
