package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/aiengine"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
)

// extractJobTimeout is the per-attempt budget from the spec's stage table
// (S2: ~120 s — the LLM calls are the slow part).
const extractJobTimeout = 120 * time.Second

// classifyBodyLimit bounds the body_text sent to the classifier (ADR-006
// fixes the classify input at the first 16 KB of body — enough signal for
// routing, bounded enough to keep the call cheap and predictable).
const classifyBodyLimit = 16 * 1024

// extractBodyLimit bounds the body_text sent to the extractor. The engine
// owner asked extraction to see the fuller body than the 16 KB routing
// cap — 64 KiB keeps the paid call bounded while surviving long thread
// tails (engine-side limits are finalized on the engine; see
// packages/contracts/openapi/ai-engine.yaml).
const extractBodyLimit = 64 * 1024

// contractMaxAttempts bounds contract-violating output to the spec's "at
// most one retry": attempt 1 errors for the retry, attempt 2 parks
// INVALID_EXTRACTION — each attempt costs money (spec section 3).
const contractMaxAttempts = 2

// extractionConfidenceGate is the minimum auditor confidence to release an
// RFQ to pricing. RFC-004 open question — provisional global gate; the
// code is threshold-agnostic, and the structural guarantee (no code path
// sends a quote without human approval) means a wrong threshold costs
// reviewer attention, never a wrong quote on the street.
const extractionConfidenceGate = 0.70

// RfqExtractWorker is S2 rfq_extract (spec section 4): parse attachments →
// classify → extract → contract-validate → confidence gate → chain S3a.
// Everything happens inside one InTenantTx so the checkpoint row, the
// parked/ignored decision and the next stage's job commit or roll back
// together. ai-engine output is untrusted: nothing it returns reaches
// extracted_payload_json without ValidateExtraction (rule 6).
type RfqExtractWorker struct {
	river.WorkerDefaults[ExtractArgs]
	// Runner is the only DB path a worker may use (ADR-002).
	Runner tenantdb.TenantRunner
	// AIEngine is the classify/extract/parse seam (HTTPClient in prod,
	// named fake in tests, UnconfiguredClient without credentials).
	AIEngine aiengine.Client
	// Storage fetches attachment bytes by rfq_attachments.storage_key.
	Storage platform.ObjectStorage
	// Inserter is the River client; assigned post-build (see RfqIngestWorker).
	Inserter JobInserter
}

// NewRfqExtractWorker builds the worker.
//
//	w := jobs.NewRfqExtractWorker(db, engine, storage)
//	... client := river.NewClient(...); w.Inserter = client
func NewRfqExtractWorker(runner tenantdb.TenantRunner, engine aiengine.Client, store platform.ObjectStorage) *RfqExtractWorker {
	return &RfqExtractWorker{Runner: runner, AIEngine: engine, Storage: store}
}

// Timeout bounds one attempt (spec section 3: 120 s for S2).
func (w *RfqExtractWorker) Timeout(*river.Job[ExtractArgs]) time.Duration {
	return extractJobTimeout
}

// Work runs the S2 pipeline for one RFQ inside its tenant transaction.
// Transient errors propagate for River's retry — exhausted attempts end
// FAILED (ADR-004); a contract violation retries at most once before
// parking NEEDS_REVIEW (spec section 3).
func (w *RfqExtractWorker) Work(ctx context.Context, job *river.Job[ExtractArgs]) error {
	args := job.Args
	return w.Runner.InTenantTx(ctx, args.OrganizationID, func(ctx context.Context, tx pgx.Tx) error {
		return w.extract(ctx, tx, args, jobAttempt(job), jobMaxAttempts(job))
	})
}

// jobAttempt reads the attempt number; a nil JobRow (direct Work calls in
// tests) counts as the first attempt.
func jobAttempt(job *river.Job[ExtractArgs]) int {
	if job.JobRow == nil {
		return 1
	}
	return job.Attempt
}

// jobMaxAttempts reads the attempt budget; 0 (nil JobRow) means "unknown",
// which disables the final-attempt transient FAILED write.
func jobMaxAttempts(job *river.Job[ExtractArgs]) int {
	if job.JobRow == nil {
		return 0
	}
	return job.MaxAttempts
}

// extract is the full S2 sequence inside the tenant transaction:
// load → parse attachments → classify → extract → gate → chain.
func (w *RfqExtractWorker) extract(ctx context.Context, tx pgx.Tx, args ExtractArgs, attempt, maxAttempts int) error {
	rfq, found, err := loadExtractRfq(ctx, tx, args.RfqID)
	if err != nil {
		return fmt.Errorf("jobs: rfq_extract: load rfq %s: %w", args.RfqID, err)
	}
	if !found || rfq.done() {
		return nil // replay or terminal status — idempotent no-op
	}
	atts, err := loadExtractAttachments(ctx, tx, rfq.ID)
	if err != nil {
		return fmt.Errorf("jobs: rfq_extract: load attachments %s: %w", rfq.ID, err)
	}
	markdowns, err := w.parseAttachments(ctx, tx, args, rfq, atts)
	if err != nil {
		return err
	}
	// The body resolves once: the text part, or the HTML part reduced to
	// text — the guard below must judge what the engine will actually see,
	// not whether a markup-only column was non-empty.
	body := requestBody(rfq)
	if !hasUsableText(body, markdowns) {
		return w.park(ctx, tx, args, rfq, ReviewReasonNoUsableText, nil)
	}
	res, err := w.AIEngine.Classify(ctx, classifyReq(args, rfq, atts, body))
	if err != nil {
		return w.engineError(ctx, tx, args, rfq, attempt, maxAttempts, err)
	}
	if err := w.auditClassify(ctx, tx, args, rfq, res); err != nil {
		return err
	}
	switch res.Label {
	case "OTHER":
		return w.ignore(ctx, tx, args, rfq)
	case "TARIFF_UPDATE":
		return w.park(ctx, tx, args, rfq, ReviewReasonTariffUpdate,
			map[string]any{"probabilities": res.Probabilities, "model": res.Model})
	case "RFQ":
		return w.extractFields(ctx, tx, args, rfq, body, markdowns, res, attempt, maxAttempts)
	default:
		// An unrecognized label is untrusted output, not a routing
		// decision — the contract-violation budget applies, and it must
		// never fall through into the paid extraction call.
		return w.contractViolation(ctx, tx, args, rfq, attempt,
			fmt.Errorf("classify label %q is not one of RFQ/TARIFF_UPDATE/OTHER", res.Label))
	}
}

// extractFields is the RFQ half: extract → validate → echo check → gate →
// chain/park.
func (w *RfqExtractWorker) extractFields(ctx context.Context, tx pgx.Tx, args ExtractArgs, rfq extractRfq, body string, markdowns []aiengine.ParsedMarkdown, res *aiengine.ClassifyResult, attempt, maxAttempts int) error {
	raw, err := w.AIEngine.Extract(ctx, extractReq(args, rfq, body, markdowns, res))
	if err != nil {
		return w.engineError(ctx, tx, args, rfq, attempt, maxAttempts, err)
	}
	report, err := aiengine.ValidateExtraction(raw)
	if err != nil {
		return w.contractViolation(ctx, tx, args, rfq, attempt, err)
	}
	// The engine echoes the request's classification — only the label is
	// equality-checked: it is the routing-relevant fact, while
	// probabilities/model may legitimately re-serialize (rounding,
	// normalization) and a spurious diff would park a valid RFQ.
	if report.ClassificationLabel != res.Label {
		return w.contractViolation(ctx, tx, args, rfq, attempt,
			fmt.Errorf("extraction classification.label %q does not echo the sent label %q",
				report.ClassificationLabel, res.Label))
	}
	if report.MinConfidence >= extractionConfidenceGate {
		return w.releaseToPricing(ctx, tx, args, rfq, raw, report)
	}
	return w.parkLowConfidence(ctx, tx, args, rfq, raw, report)
}

// parseAttachments produces {filename, markdown} pairs for every row that
// yields one — the real filename rides to extract so evidence can cite
// ATTACHMENT:<filename>. Per-attachment failures are audited and degrade
// (column stays NULL, nothing is sent for the file) — they never abort
// the pipeline; a body-less RFQ that yields nothing parks NO_USABLE_TEXT
// afterwards.
func (w *RfqExtractWorker) parseAttachments(ctx context.Context, tx pgx.Tx, args ExtractArgs, rfq extractRfq, atts []extractAttachment) ([]aiengine.ParsedMarkdown, error) {
	markdowns := []aiengine.ParsedMarkdown{}
	for i := range atts {
		a := &atts[i]
		if a.ParsedMarkdown != nil {
			markdowns = append(markdowns, aiengine.ParsedMarkdown{Filename: a.Filename, Markdown: *a.ParsedMarkdown})
			continue
		}
		md, err := w.parseAttachment(ctx, tx, args, rfq, a)
		if err != nil {
			return nil, err
		}
		if md != nil {
			markdowns = append(markdowns, aiengine.ParsedMarkdown{Filename: a.Filename, Markdown: *md})
		}
	}
	return markdowns, nil
}

// parseAttachment handles one row: storage → ai-engine parse → write back.
// nil,nil means "audited degrade, continue".
func (w *RfqExtractWorker) parseAttachment(ctx context.Context, tx pgx.Tx, args ExtractArgs, rfq extractRfq, a *extractAttachment) (*string, error) {
	data, err := w.Storage.Get(ctx, a.StorageKey)
	if errors.Is(err, platform.ErrObjectNotFound) {
		return nil, w.attachmentFailed(ctx, tx, args, rfq, a, "storage_miss")
	}
	if err != nil {
		return nil, fmt.Errorf("jobs: rfq_extract: storage get %q: %w", a.StorageKey, err)
	}
	md, err := w.AIEngine.ParseAttachment(ctx, aiengine.ParseAttachmentRequest{
		CorrelationID: args.CorrelationID,
		Filename:      a.Filename,
		ContentType:   a.MimeType,
		Bytes:         data,
	})
	if err != nil {
		return nil, w.attachmentFailed(ctx, tx, args, rfq, a, attachmentFailureCode(err))
	}
	if _, err := tx.Exec(ctx, `
		UPDATE app.rfq_attachments SET parsed_markdown = $2 WHERE id = $1`, a.ID, md); err != nil {
		return nil, fmt.Errorf("jobs: rfq_extract: store markdown %s: %w", a.ID, err)
	}
	return &md, nil
}

// engineError classifies an ai-engine failure: transient propagates for
// River retry — except on the job's final attempt, where returning the
// error would discard the job and leave the RFQ stuck invisible in
// PARSING, so it writes FAILED + failure_reason and commits instead
// (ADR-004: exhausted retries are a technical failure, not a business
// park — failure_reason is the column reserved for them). Everything
// else follows the contract-violation budget.
func (w *RfqExtractWorker) engineError(ctx context.Context, tx pgx.Tx, args ExtractArgs, rfq extractRfq, attempt, maxAttempts int, err error) error {
	if !aiengine.IsTransient(err) {
		return w.contractViolation(ctx, tx, args, rfq, attempt, err)
	}
	if maxAttempts > 0 && attempt >= maxAttempts {
		reason := truncate(fmt.Sprintf(
			"rfq_extract: ai-engine unavailable after %d attempts, last error: %s",
			maxAttempts, err), maxFailureReason)
		return w.fail(ctx, tx, args, rfq, reason)
	}
	return fmt.Errorf("jobs: rfq_extract: engine call on %s: %w", rfq.ID, err)
}

// contractViolation enforces "at most one retry": under the budget the
// error propagates, past it the RFQ parks INVALID_EXTRACTION with the
// failure audited — the invalid payload is never written.
func (w *RfqExtractWorker) contractViolation(ctx context.Context, tx pgx.Tx, args ExtractArgs, rfq extractRfq, attempt int, cause error) error {
	if attempt < contractMaxAttempts {
		return fmt.Errorf("jobs: rfq_extract: invalid engine output on %s (attempt %d): %w", rfq.ID, attempt, cause)
	}
	return w.park(ctx, tx, args, rfq, ReviewReasonInvalidExtraction,
		map[string]any{"validation_error": cause.Error()})
}

// ignore routes OTHER: status IGNORED, kind mirrors the classification.
func (w *RfqExtractWorker) ignore(ctx context.Context, tx pgx.Tx, args ExtractArgs, rfq extractRfq) error {
	if _, err := tx.Exec(ctx, `
		UPDATE app.rfq_requests SET status = 'IGNORED', kind = 'OTHER', updated_at = now()
		WHERE id = $1`, rfq.ID); err != nil {
		return fmt.Errorf("jobs: rfq_extract: ignore rfq %s: %w", rfq.ID, err)
	}
	return w.audit(ctx, tx, args, rfq.ID, "rfq.ignored", nil)
}

// fail is the dead-letter write of S2 (ADR-004: after the final attempt
// the job is dead-lettered and the RFQ moves to FAILED with a reason):
// failure_reason — never review_reason, which is for business gaps — plus
// the audit row that keeps the failure visible in the inbox.
func (w *RfqExtractWorker) fail(ctx context.Context, tx pgx.Tx, args ExtractArgs, rfq extractRfq, reason string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE app.rfq_requests
		SET status = 'FAILED', failure_reason = $2, updated_at = now()
		WHERE id = $1`, rfq.ID, reason); err != nil {
		return fmt.Errorf("jobs: rfq_extract: fail rfq %s: %w", rfq.ID, err)
	}
	return w.audit(ctx, tx, args, rfq.ID, "rfq.failed",
		map[string]any{"failure_reason": reason})
}

// park writes NEEDS_REVIEW + review_reason + the audit row — the triage
// state the spec's human queue and future reclassify action build on.
func (w *RfqExtractWorker) park(ctx context.Context, tx pgx.Tx, args ExtractArgs, rfq extractRfq, reason ReviewReason, meta map[string]any) error {
	kind := rfq.Kind
	if reason == ReviewReasonTariffUpdate {
		kind = "TARIFF_UPDATE"
	}
	if _, err := tx.Exec(ctx, `
		UPDATE app.rfq_requests
		SET status = 'NEEDS_REVIEW', kind = $2, review_reason = $3, updated_at = now()
		WHERE id = $1`, rfq.ID, kind, string(reason)); err != nil {
		return fmt.Errorf("jobs: rfq_extract: park rfq %s: %w", rfq.ID, err)
	}
	if meta == nil {
		meta = map[string]any{}
	}
	meta["review_reason"] = string(reason)
	return w.audit(ctx, tx, args, rfq.ID, "rfq.parked", meta)
}

// releaseToPricing is the gate's green path: payload + confidence
// persisted, status PRICING, rfq_extracted audited and S3a enqueued inside
// the same transaction (checkpoint and job commit together, spec section 3).
func (w *RfqExtractWorker) releaseToPricing(ctx context.Context, tx pgx.Tx, args ExtractArgs, rfq extractRfq, raw json.RawMessage, report *aiengine.ExtractionReport) error {
	if err := w.persistExtraction(ctx, tx, rfq.ID, raw, report, "PRICING"); err != nil {
		return err
	}
	if err := w.auditExtracted(ctx, tx, args, rfq.ID, report); err != nil {
		return err
	}
	return w.enqueueRates(ctx, tx, args)
}

// parkLowConfidence is the gate's amber path: fields stay persisted (the
// human queue is pre-filled — the gate IS the triage) while the RFQ waits.
func (w *RfqExtractWorker) parkLowConfidence(ctx context.Context, tx pgx.Tx, args ExtractArgs, rfq extractRfq, raw json.RawMessage, report *aiengine.ExtractionReport) error {
	if err := w.persistExtraction(ctx, tx, rfq.ID, raw, report, "NEEDS_REVIEW"); err != nil {
		return err
	}
	return w.park(ctx, tx, args, rfq, ReviewReasonLowConfidence,
		map[string]any{"min_confidence": report.MinConfidence, "gate": extractionConfidenceGate})
}

// persistExtraction writes the validated payload + confidence once; the
// only difference between the two gate outcomes is the status.
func (w *RfqExtractWorker) persistExtraction(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID, raw json.RawMessage, report *aiengine.ExtractionReport, status string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE app.rfq_requests
		SET status = $2, kind = 'RFQ', extracted_payload_json = $3,
		    extraction_confidence = $4, review_reason = NULL, updated_at = now()
		WHERE id = $1`, rfqID, status, raw, report.MinConfidence); err != nil {
		return fmt.Errorf("jobs: rfq_extract: persist extraction on %s: %w", rfqID, err)
	}
	return nil
}

// enqueueRates chains S3a rfq_fetch_rates in the same transaction; queue
// and uniqueness come from RateArgs.InsertOpts().
func (w *RfqExtractWorker) enqueueRates(ctx context.Context, tx pgx.Tx, args ExtractArgs) error {
	if w.Inserter == nil {
		return errors.New("jobs: rfq_extract: Inserter not configured")
	}
	// ExtractArgs and RateArgs share the same fields — direct conversion.
	_, err := w.Inserter.InsertTx(ctx, tx, RateArgs(args), nil)
	if err != nil {
		return fmt.Errorf("jobs: rfq_extract: enqueue rfq_fetch_rates %s: %w", args.RfqID, err)
	}
	return nil
}

// attachmentFailed audits a degraded attachment (the column stays NULL and
// the pipeline continues — spec S2). The audit write itself failing is a
// real error, not a degrade.
func (w *RfqExtractWorker) attachmentFailed(ctx context.Context, tx pgx.Tx, args ExtractArgs, rfq extractRfq, a *extractAttachment, code string) error {
	return w.audit(ctx, tx, args, rfq.ID, "rfq.attachment_parse_failed",
		map[string]any{"filename": a.Filename, "reason": code})
}

// auditClassify records every classification — label, calibrated
// probabilities and the model — so the review UI can show WHY an RFQ was
// parked or ignored (Pedro's "visible needs-review with reason" rule).
func (w *RfqExtractWorker) auditClassify(ctx context.Context, tx pgx.Tx, args ExtractArgs, rfq extractRfq, res *aiengine.ClassifyResult) error {
	return w.audit(ctx, tx, args, rfq.ID, "rfq.classified",
		map[string]any{"label": res.Label, "probabilities": res.Probabilities, "model": res.Model})
}

// auditExtracted records the gate's decision data: contract version, field
// count and minimum confidence (the day-one data for answering RFC-004).
func (w *RfqExtractWorker) auditExtracted(ctx context.Context, tx pgx.Tx, args ExtractArgs, rfqID uuid.UUID, report *aiengine.ExtractionReport) error {
	return w.audit(ctx, tx, args, rfqID, "rfq.extracted", map[string]any{
		"contract_version": report.ContractVersion,
		"fields":           report.FieldCount,
		"min_confidence":   report.MinConfidence,
		"gate":             extractionConfidenceGate,
	})
}

// audit appends one tenant_audit_logs row — worker actions carry no actor.
func (w *RfqExtractWorker) audit(ctx context.Context, tx pgx.Tx, args ExtractArgs, rfqID uuid.UUID, action string, meta map[string]any) error {
	if _, err := platform.NewStore(tx).InsertAuditLog(ctx, platform.AuditEntry{
		OrganizationID: args.OrganizationID,
		Action:         action,
		EntityType:     "rfq",
		EntityID:       rfqID,
		Metadata:       meta,
	}); err != nil {
		return fmt.Errorf("jobs: rfq_extract: audit %q on %s: %w", action, rfqID, err)
	}
	return nil
}

// extractRfq is the slice of app.rfq_requests the worker reads/writes.
type extractRfq struct {
	ID            uuid.UUID
	Kind          string
	Status        string
	Subject       string
	CustomerEmail string
	RawText       *string
	RawHTML       *string
}

// done reports whether this RFQ is already past S2 — terminal or
// downstream statuses make the job an idempotent no-op (replay).
func (r extractRfq) done() bool {
	return r.Status != "PARSING" && r.Status != "RECEIVED"
}

// extractAttachment is the rfq_attachments slice S2 needs.
type extractAttachment struct {
	ID             uuid.UUID
	StorageKey     string
	Filename       string
	MimeType       string
	ParsedMarkdown *string
}

// loadExtractRfq reads the RFQ; RLS scopes it — an id belonging to another
// org is simply found=false.
func loadExtractRfq(ctx context.Context, tx pgx.Tx, id uuid.UUID) (extractRfq, bool, error) {
	var r extractRfq
	err := tx.QueryRow(ctx, `
		SELECT id, kind::text, status::text, subject, customer_email, raw_text_body, raw_html_body
		FROM app.rfq_requests WHERE id = $1`, id).
		Scan(&r.ID, &r.Kind, &r.Status, &r.Subject, &r.CustomerEmail, &r.RawText, &r.RawHTML)
	if errors.Is(err, pgx.ErrNoRows) {
		return extractRfq{}, false, nil
	}
	return r, err == nil, err
}

// loadExtractAttachments reads the RFQ's attachment rows (all of them —
// metas go to classify even when the parse never ran).
func loadExtractAttachments(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID) ([]extractAttachment, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, storage_key, filename, coalesce(mime_type,'') AS mime_type, parsed_markdown
		FROM app.rfq_attachments WHERE rfq_id = $1 ORDER BY created_at`, rfqID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[extractAttachment])
}

// attachmentFailureCode maps a parse failure to a short audit reason.
func attachmentFailureCode(err error) string {
	var ee *aiengine.EngineError
	if errors.As(err, &ee) {
		if ee.Contract {
			return "contract_violation"
		}
		return "engine_error"
	}
	return "engine_error"
}
