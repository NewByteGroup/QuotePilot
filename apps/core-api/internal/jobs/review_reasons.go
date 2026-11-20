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
)

// ReviewReasons is the closed list the stored-value test checks against:
// any review_reason found in rfq_requests must be one of these.
var ReviewReasons = []ReviewReason{
	ReviewReasonExportMatrixOpen,
	ReviewReasonMissingTariff,
	ReviewReasonStaleTariff,
}
