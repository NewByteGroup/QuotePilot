package jobs

// rfqrender.go — S4 rfq_render (spec §4): the draft-quote document stage.
// One InTenantTx holds the whole sequence — quote lock, request assembly,
// the engine render call, the storage writes and the pdf_storage_key
// commit — so a crash anywhere rolls the checkpoint back and a retry
// redoes it harmlessly (a same-key Put simply overwrites).
//
// Retry-exhaustion rule (spec §3, adapted from S2): quotes has no FAILED
// column, so a transient error on the final attempt does NOT propagate to
// a discard. The worker audits quote.render_failed in a fresh tenant tx —
// the failed one already rolled back — and returns nil. The audit row is
// the honest dead-letter mark a discard would otherwise lose; reprocess
// re-enqueues rfq_render by quote id (spec §4 "Reprocess").
//
// The pipeline stops here for a human (spec S4): S3b already parked the
// RFQ READY when the quote committed, so this stage writes no RFQ status —
// only the artifacts and pdf_storage_key the send stage later consumes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/shopspring/decimal"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/aiengine"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
)

// renderJobTimeout bounds one attempt (spec §3 leaves S4's per-attempt
// budget blank — 60 s covers the render call plus the upload inside the
// stage's three attempts).
const renderJobTimeout = 60 * time.Second

// render failure classes — the audit metadata vocabulary for
// quote.render_failed. contract_violation is the engine's deterministic
// refusal (non-transient status or an invalid payload); stored_drift is
// core-side data the render cannot assemble from (undecodable extraction,
// missing line items); transient_exhausted marks retries spent on
// transient errors.
const (
	renderErrContractViolation = "contract_violation"
	renderErrStoredDrift       = "stored_drift"
	renderErrTransient         = "transient_exhausted"
)

// RfqRenderWorker is S4 (spec §4): it renders one DRAFT quote version to
// PDF + email HTML via ai-engine, stores both under the tenant's quotes
// prefix and pins pdf_storage_key — the artifact the S5 send later
// attaches (its missing_render refusal is this stage's contract).
type RfqRenderWorker struct {
	river.WorkerDefaults[RenderArgs]
	// Runner is the only DB path a worker may use (ADR-002).
	Runner tenantdb.TenantRunner
	// Renderer is the ai-engine seam (HTTPClient in prod, named fake in
	// tests, UnconfiguredClient without credentials).
	Renderer aiengine.Client
	// Storage receives the rendered artifacts inside the tenant tx, like
	// rfq_ingest's attachments — the key write is atomic with the commit
	// and a retry's same-key Put overwrites harmlessly.
	Storage platform.ObjectStorage
}

// NewRfqRenderWorker builds the worker.
//
//	w := jobs.NewRfqRenderWorker(db, engine, storage)
func NewRfqRenderWorker(runner tenantdb.TenantRunner, renderer aiengine.Client, store platform.ObjectStorage) *RfqRenderWorker {
	return &RfqRenderWorker{Runner: runner, Renderer: renderer, Storage: store}
}

// Timeout bounds one attempt (spec §3: blank for S4 — the bounded 60 s
// render+upload choice documented on the constant).
func (w *RfqRenderWorker) Timeout(*river.Job[RenderArgs]) time.Duration { return renderJobTimeout }

// Work runs S4 for one quote version. Transient errors propagate for
// River's retry; on the final attempt the audit row is written in a fresh
// tenant tx and the job returns nil (file header: the audit-only dead
// letter, since quotes has no FAILED column). Contract violations and
// stored-payload drift are deterministic — audited inside the stage tx,
// then JobCancel.
func (w *RfqRenderWorker) Work(ctx context.Context, job *river.Job[RenderArgs]) error {
	args := job.Args
	cancelCause, err := w.run(ctx, args)
	if err != nil {
		if job.JobRow != nil && job.MaxAttempts > 0 && job.Attempt >= job.MaxAttempts {
			if aerr := w.writeRenderFailed(ctx, args, job.MaxAttempts, err); aerr != nil {
				return errors.Join(err, aerr)
			}
			return nil
		}
		return err
	}
	if cancelCause != nil {
		return river.JobCancel(cancelCause)
	}
	return nil
}

// run wraps the whole stage in the quote's tenant transaction — the same
// run→cancelCause shape QuoteSendWorker uses: the tx commits whatever the
// stage decided (the artifacts or the refusal audit) and the returned
// cancelCause becomes the JobCancel Work wraps afterwards.
func (w *RfqRenderWorker) run(ctx context.Context, args RenderArgs) (cancelCause error, err error) {
	err = w.Runner.InTenantTx(ctx, args.OrganizationID, func(ctx context.Context, tx pgx.Tx) error {
		cancelCause, err = w.stage(ctx, tx, args)
		return err
	})
	return cancelCause, err
}

// stage is the full S4 sequence inside the tenant transaction:
// lock → assemble → render → store → commit.
func (w *RfqRenderWorker) stage(ctx context.Context, tx pgx.Tx, args RenderArgs) (cancelCause error, err error) {
	if w.Renderer == nil {
		return errors.New("jobs: rfq_render: Renderer not configured"), nil
	}
	q, found, err := lockRenderQuote(ctx, tx, args.QuoteID)
	if err != nil {
		return nil, fmt.Errorf("jobs: rfq_render: lock quote %s: %w", args.QuoteID, err)
	}
	if !found || q.Status != quoteDraft || (q.PdfKey != nil && *q.PdfKey != "") {
		// Missing/cross-tenant, no longer a draft, or already rendered —
		// all idempotent no-ops under the FOR UPDATE read (spec S4).
		return nil, nil
	}
	req, err := w.assemble(ctx, tx, args, q)
	if err != nil {
		var de *renderDriftError
		if errors.As(err, &de) {
			return w.refuse(ctx, tx, args, renderErrStoredDrift, de.err)
		}
		return nil, err
	}
	res, err := w.Renderer.RenderQuote(ctx, *req)
	if err != nil {
		if aiengine.IsTransient(err) {
			return nil, fmt.Errorf("jobs: rfq_render: render on %s: %w", q.ID, err)
		}
		// Non-transient engine faults — a deterministic 4xx or a
		// contract-violating payload — never retry (spec §3 taxonomy).
		return w.refuse(ctx, tx, args, renderErrContractViolation,
			fmt.Errorf("jobs: rfq_render: engine refused quote %s: %w", q.ID, err))
	}
	if res == nil || len(res.PDF) == 0 {
		return w.refuse(ctx, tx, args, renderErrContractViolation,
			fmt.Errorf("jobs: rfq_render: engine returned an empty render for quote %s", q.ID))
	}
	return nil, w.commitRender(ctx, tx, args, q, res)
}

// renderDriftError marks deterministic stored-data faults found while
// assembling the request — a DRAFT quote implies S3b already validated
// the same inputs, so drift here cancels rather than retrying.
type renderDriftError struct{ err error }

func (e *renderDriftError) Error() string { return e.err.Error() }

// drift wraps a stored-data fault for classification in stage.
func drift(format string, a ...any) error {
	return &renderDriftError{err: fmt.Errorf(format, a...)}
}

// assemble builds the RenderRequest from the quote row, its line items,
// the RFQ's extraction (shipment fields only) and the org's branding.
// Query failures are plain errors (transient); missing or undecodable
// stored content is drift.
func (w *RfqRenderWorker) assemble(ctx context.Context, tx pgx.Tx, args RenderArgs, q renderQuote) (*aiengine.RenderRequest, error) {
	lines, err := loadRenderLines(ctx, tx, q.ID)
	if err != nil {
		return nil, fmt.Errorf("jobs: rfq_render: line items on %s: %w", q.ID, err)
	}
	if len(lines) == 0 {
		// A COMPLETE quote always committed its lines with the row (S3b).
		return nil, drift("quote %s has no line items", q.ID)
	}
	warnings, assumptions, err := decodeRenderOutput(q)
	if err != nil {
		return nil, drift("pricing_result_json on %s: %s", q.ID, err)
	}
	ship, err := w.loadShipment(ctx, tx, q.RfqID)
	if err != nil {
		return nil, err // already classified (drift or transient)
	}
	org, branding, err := loadRenderOrg(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("jobs: rfq_render: org branding: %w", err)
	}
	return &aiengine.RenderRequest{
		CorrelationID: args.CorrelationID,
		Quote:         requestQuote(q, lines, ship, warnings, assumptions),
		Branding:      branding,
		Org:           aiengine.RenderOrg{Name: org.name, FromName: org.fromName, ReplyTo: org.replyTo},
	}, nil
}

// requestQuote maps the assembled pieces onto the wire shape. The arrays
// are normalized non-nil so the contract sends [] — never null — for
// empty warnings/assumptions/flags.
func requestQuote(q renderQuote, lines []aiengine.RenderLine, ship renderShipment, warnings, assumptions []string) aiengine.RenderQuoteData {
	rq := aiengine.RenderQuoteData{
		QuoteNumber:       q.QuoteNumber,
		Version:           q.Version,
		Modal:             ship.modal,
		Direction:         ship.direction,
		Incoterm:          ship.incoterm,
		OriginLOCODE:      ship.origin,
		DestLOCODE:        ship.dest,
		EquipmentOrUnit:   ship.equipment,
		Lines:             lines,
		TotalBRL:          q.FinalPriceBRL,
		OverallConfidence: q.OverallConfidence,
		Warnings:          warnings,
		Assumptions:       assumptions,
	}
	if q.QuoteDate != nil {
		rq.QuoteDate = q.QuoteDate.Format(isoDate)
	}
	// valid_until stays empty: quotes carries no validity column yet —
	// the wire slot is reserved for when it lands (aiengine.RenderQuoteData).
	if rq.Warnings == nil {
		rq.Warnings = []string{}
	}
	if rq.Assumptions == nil {
		rq.Assumptions = []string{}
	}
	return rq
}

// decodeRenderOutput reads the pinned engine output for the document's
// warning/assumption blocks — the same pricing_result_json decode
// quotesend runs, minus internals the template never sees.
func decodeRenderOutput(q renderQuote) (warnings, assumptions []string, err error) {
	if len(q.PricingJSON) == 0 {
		return nil, nil, errors.New("pricing_result_json is NULL")
	}
	var out pricing.Output
	if err := json.Unmarshal(q.PricingJSON, &out); err != nil {
		return nil, nil, fmt.Errorf("decode: %w", err)
	}
	assumptions = make([]string, 0, len(out.Assumptions))
	for _, a := range out.Assumptions {
		assumptions = append(assumptions, string(a))
	}
	return out.Warnings, assumptions, nil
}

// renderShipment is the extraction-derived header block the template
// prints — re-parsed from the RFQ's stored payload rather than trusted
// from args (rule 6 + rule 7: job args carry ids only).
type renderShipment struct {
	modal, direction, incoterm string
	origin, dest, equipment    string
}

// loadShipment reads the RFQ's extracted_payload_json and derives the
// shipment fields with the same parsers S3b used — every key it needs
// was required to reach COMPLETE, so an absent one is drift.
func (w *RfqRenderWorker) loadShipment(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID) (renderShipment, error) {
	var payload []byte
	err := tx.QueryRow(ctx, `
		SELECT coalesce(extracted_payload_json::text,'')
		FROM app.rfq_requests WHERE id = $1`, rfqID).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		// The quote's FK guarantees the RFQ row under the same org — a
		// miss is drift, not a retryable lookup.
		return renderShipment{}, drift("rfq %s backing quote is missing", rfqID)
	}
	if err != nil {
		return renderShipment{}, fmt.Errorf("jobs: rfq_render: load rfq %s: %w", rfqID, err)
	}
	fields, err := decodePriceExtraction(payload)
	if err != nil {
		return renderShipment{}, drift("extraction on %s: %s", rfqID, err)
	}
	return shipmentFromFields(fields)
}

// shipmentFromFields is the required-field half of the assembly: modal,
// incoterm, both locodes and the derived direction — all mandatory at
// S3b, so absence can only be stored drift. equipment_or_unit is
// optional: LCL quotes legitimately carry none.
func shipmentFromFields(fields map[string]extractedField) (renderShipment, error) {
	var s renderShipment
	modal, ok := parseModalField(fields)
	if !ok {
		return s, drift("modal is missing or unparseable")
	}
	inc, ok := parseIncotermField(fields)
	if !ok {
		return s, drift("incoterm is missing or unparseable")
	}
	origin, ok := requiredLocode(fields, "origin_locode")
	if !ok {
		return s, drift("origin_locode is missing or not a locode")
	}
	dest, ok := requiredLocode(fields, "dest_locode")
	if !ok {
		return s, drift("dest_locode is missing or not a locode")
	}
	dir, ok := deriveDirection(origin, dest)
	if !ok {
		return s, drift("direction is ambiguous for %s→%s", origin, dest)
	}
	s = renderShipment{modal: string(modal), incoterm: string(inc),
		origin: origin, dest: dest, direction: string(dir)}
	if equip, ok := fieldString(fields, "equipment_or_unit"); ok {
		s.equipment = strings.ToUpper(equip)
	}
	return s, nil
}

// renderOrg is the org registry slice get_current_organization exposes —
// the document's display identity plus the verbatim branding blob.
type renderOrg struct {
	name, fromName, replyTo string
}

// loadRenderOrg reads the org row through the SECURITY DEFINER accessor —
// qp_app holds no direct grant on organizations, and inside the tenant tx
// it scopes to the resolved org by construction (same read as quotesend's
// loadBranding).
func loadRenderOrg(ctx context.Context, tx pgx.Tx) (renderOrg, json.RawMessage, error) {
	var o renderOrg
	var branding json.RawMessage
	err := tx.QueryRow(ctx, `
		SELECT name, outbound_from_name, outbound_reply_to, branding_json::text
		FROM app.get_current_organization()`).
		Scan(&o.name, &o.fromName, &o.replyTo, &branding)
	if len(branding) == 0 {
		branding = json.RawMessage(`{}`)
	}
	return o, branding, err
}

// commitRender stores the artifacts and pins the key: the PDF always, the
// email HTML when the engine returned one (S5's body is its own for now —
// the artifact is stored for it anyway), then pdf_storage_key and the
// quote.rendered audit, all inside this tx.
func (w *RfqRenderWorker) commitRender(ctx context.Context, tx pgx.Tx, args RenderArgs, q renderQuote, res *aiengine.RenderResult) error {
	pdfKey := renderKey(args.OrganizationID, q, ".pdf")
	if err := w.Storage.Put(ctx, pdfKey, res.PDF); err != nil {
		return fmt.Errorf("jobs: rfq_render: storage put %q: %w", pdfKey, err)
	}
	var htmlKey string
	if res.EmailHTML != "" {
		htmlKey = renderKey(args.OrganizationID, q, ".html")
		if err := w.Storage.Put(ctx, htmlKey, []byte(res.EmailHTML)); err != nil {
			return fmt.Errorf("jobs: rfq_render: storage put %q: %w", htmlKey, err)
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE app.quotes SET pdf_storage_key = $2, updated_at = now() WHERE id = $1`,
		q.ID, pdfKey); err != nil {
		return fmt.Errorf("jobs: rfq_render: pin pdf_storage_key on %s: %w", q.ID, err)
	}
	meta := map[string]any{
		"pdf_storage_key": pdfKey,
		"quote_number":    q.QuoteNumber,
		"version":         q.Version,
	}
	if htmlKey != "" {
		meta["html_storage_key"] = htmlKey
	}
	return auditRender(ctx, tx, args.OrganizationID, "quote.rendered", q.ID, meta)
}

// renderKey builds tenants/{org}/quotes/{quote_id}_v{version}{ext} — the
// spec-S4 key family; version in the key keeps a reprocess's new version
// from overwriting the prior version's artifact (rule 9).
func renderKey(org uuid.UUID, q renderQuote, ext string) string {
	return fmt.Sprintf("tenants/%s/quotes/%s_v%d%s", org, q.ID, q.Version, ext)
}

// refuse audits a deterministic render failure inside the tx and returns
// the cause for Work to wrap in JobCancel — a committed, visible refusal
// instead of a silent skip (the send_refused shape on the render side).
func (w *RfqRenderWorker) refuse(ctx context.Context, tx pgx.Tx, args RenderArgs, errClass string, cause error) (error, error) {
	if err := auditRender(ctx, tx, args.OrganizationID, "quote.render_failed", args.QuoteID,
		map[string]any{"error_class": errClass, "error": truncate(cause.Error(), maxFailureReason)}); err != nil {
		return nil, err
	}
	return cause, nil
}

// writeRenderFailed is the dead-letter half of S4 (file header): the
// exhausted transient lands as quote.render_failed in a fresh tenant tx —
// the failed one rolled back, and quotes carries no FAILED column to
// park anyway.
func (w *RfqRenderWorker) writeRenderFailed(ctx context.Context, args RenderArgs, attempts int, cause error) error {
	return w.Runner.InTenantTx(ctx, args.OrganizationID, func(ctx context.Context, tx pgx.Tx) error {
		return auditRender(ctx, tx, args.OrganizationID, "quote.render_failed", args.QuoteID, map[string]any{
			"error_class": renderErrTransient,
			"attempts":    attempts,
			"error": truncate(fmt.Sprintf("rfq_render: exhausted %d attempts, last error: %s",
				attempts, cause), maxFailureReason),
		})
	})
}

// renderQuote is the locked slice of app.quotes the stage reads.
type renderQuote struct {
	ID                uuid.UUID
	RfqID             uuid.UUID
	Status            string
	QuoteNumber       string
	Version           int
	QuoteDate         *time.Time
	FinalPriceBRL     decimal.Decimal
	OverallConfidence decimal.Decimal
	PricingJSON       []byte
	PdfKey            *string
}

// lockRenderQuote SELECTs the version FOR UPDATE — the lock is what makes
// "pdf_storage_key already set ⇒ no second render" airtight against a
// racing attempt (same guard as lockSendQuote).
func lockRenderQuote(ctx context.Context, tx pgx.Tx, quoteID uuid.UUID) (renderQuote, bool, error) {
	var q renderQuote
	err := tx.QueryRow(ctx, `
		SELECT id, rfq_id, status::text, quote_number, version, quote_date,
		       final_price_brl, overall_confidence,
		       coalesce(pricing_result_json::text,''), pdf_storage_key
		FROM app.quotes WHERE id = $1 FOR UPDATE`, quoteID).
		Scan(&q.ID, &q.RfqID, &q.Status, &q.QuoteNumber, &q.Version, &q.QuoteDate,
			&q.FinalPriceBRL, &q.OverallConfidence, &q.PricingJSON, &q.PdfKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return renderQuote{}, false, nil
	}
	return q, err == nil, err
}

// loadRenderLines reads the persisted line items in display order — the
// rows S3b committed with the quote, not a re-derivation of the engine
// output.
func loadRenderLines(ctx context.Context, tx pgx.Tx, quoteID uuid.UUID) ([]aiengine.RenderLine, error) {
	rows, err := tx.Query(ctx, `
		SELECT category::text, description, original_currency, original_amount,
		       fx_rate_used, sell_amount_brl, flags::text
		FROM app.quote_line_items WHERE quote_id = $1 ORDER BY sort_order`, quoteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lines []aiengine.RenderLine
	for rows.Next() {
		var l aiengine.RenderLine
		var flags []byte
		if err := rows.Scan(&l.Category, &l.Description, &l.OriginalCurrency,
			&l.OriginalAmount, &l.FXRateUsed, &l.SellAmountBRL, &flags); err != nil {
			return nil, err
		}
		l.Flags = []string{}
		if err := json.Unmarshal(flags, &l.Flags); err != nil {
			return nil, fmt.Errorf("line %q flags: %w", l.Category, err)
		}
		lines = append(lines, l)
	}
	return lines, rows.Err()
}

// auditRender appends the tenant_audit_logs row — worker actions carry no
// actor (a machine did this, not a user). Entity is the quote, matching
// the quote.* action family.
func auditRender(ctx context.Context, tx pgx.Tx, org uuid.UUID, action string, quoteID uuid.UUID, meta map[string]any) error {
	if _, err := platform.NewStore(tx).InsertAuditLog(ctx, platform.AuditEntry{
		OrganizationID: org,
		Action:         action,
		EntityType:     "quote",
		EntityID:       quoteID,
		Metadata:       meta,
	}); err != nil {
		return fmt.Errorf("jobs: rfq_render: audit %q on %s: %w", action, quoteID, err)
	}
	return nil
}
