-- rfq_requests.review_reason (spec 2026-11-11-staged-quote-pipeline S3b):
-- why a human must look at a parked RFQ. The column lives on rfq_requests
-- because a parked RFQ has no quote row to carry the reason. failure_reason
-- is deliberately NOT reused: it means something broke — a technical failure
-- after retries — while review_reason names a business gap (unsupported
-- scope, missing data, low confidence) that is always NEEDS_REVIEW.
--
-- Plain text, no CHECK: the codes are a Go-side controlled vocabulary
-- (internal/jobs ReviewReason + the ReviewReasons list), so adding a code is
-- a code change, not a migration. Writes go through the constants only and a
-- real-Postgres test fails on any stored value outside the list.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE app.rfq_requests ADD COLUMN review_reason text;

COMMENT ON COLUMN app.rfq_requests.review_reason IS
    'Why a human must look at this NEEDS_REVIEW RFQ (spec 2026-11-11 S3b). Controlled vocabulary: internal/jobs ReviewReason — never failure_reason, which is reserved for technical failures after retries.';
-- +goose StatementEnd

-- =========================================================================
-- +goose Down
-- =========================================================================
-- +goose StatementBegin
ALTER TABLE app.rfq_requests DROP COLUMN review_reason;
-- +goose StatementEnd
