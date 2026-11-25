package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/shopspring/decimal"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/fx"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
)

// rfqPricing is the only status S3b prices from (spec §5): the S2/S3a
// checkpoint. Every other state — READY, NEEDS_REVIEW, FAILED, IGNORED,
// APPROVED_SENT, and the pre-extraction states — already carries its
// checkpoint, so a replay is a no-op; the state machine owns statuses,
// not a stray job.
const rfqPricing = "PRICING"

// priceJobTimeout pins the spec-§3 stage table: S3b is rows-and-CPU plus
// one bounded PTAX fetch — phase B caps at fxEnsureBudget, so five
// seconds still buys the whole DB path plus the warm-up.
const priceJobTimeout = 5 * time.Second

// Fallback provenance vocabulary persisted in pricing_result_json
// (spec S3b "rate provenance"): why a tariff-sourced freight line is not
// an offer line. missing covers every OFFER_FOUND absence — no checkpoint
// row, NO_OFFER, FETCH_FAILED, NOT_APPLICABLE (a deliberate conflation:
// the checkpoint's outcome detail stays on carrier_offers while the quote
// records only that no offer was supplied).
const (
	fallbackMissing    = "missing"
	fallbackExpired    = "expired"
	fallbackIncomplete = "incomplete"
)

// errUndecodable marks a stored row whose JSON contract drifted (fixed
// fees, offer payload): deterministic by definition — retrying cannot
// fix it, so the caller cancels the job instead of propagating.
var errUndecodable = errors.New("undecodable stored json")

// RfqPriceWorker is S3b rfq_price (spec §4): deterministic pricing. One
// InTenantTx holds the whole sequence — RFQ lock, candidate loads, the
// pure engine call, quote rows and the S4 enqueue — so a crash anywhere
// rolls the checkpoint back and a retry redoes it.
type RfqPriceWorker struct {
	river.WorkerDefaults[PriceArgs]
	// Runner is the only DB path a worker may use (ADR-002).
	Runner tenantdb.TenantRunner
	// Inserter is the River client; set before client.Start because the
	// registry must exist before the client can be built.
	Inserter JobInserter
	// FX is the phase-B rate synchronizer (*fx.Syncer in production);
	// nil skips the warm-up — the engine then parks MISSING_FX exactly
	// as it did before the fx_rates table existed.
	FX FXSyncer
	// clock is injected so tests pin QuoteDate/PricedAt (rule 5: the
	// engine never reads time itself, and neither should the worker's
	// output be clock-dependent in tests).
	clock func() time.Time
}

// NewRfqPriceWorker builds the worker. main assigns Inserter once the
// River client exists.
//
//	w := jobs.NewRfqPriceWorker(db, time.Now)
//	... client := river.NewClient(...); w.Inserter = client
func NewRfqPriceWorker(runner tenantdb.TenantRunner, clock func() time.Time) *RfqPriceWorker {
	return &RfqPriceWorker{Runner: runner, clock: clock}
}

// Timeout pins the spec-§3 per-attempt budget for this stage.
func (w *RfqPriceWorker) Timeout(*river.Job[PriceArgs]) time.Duration { return priceJobTimeout }

// Work runs S3b for one RFQ. Transient errors — DB failures only —
// propagate for River's retry; deterministic anomalies cancel via
// JobCancel (spec §3: a pure-function failure never burns five attempts).
// INCOMPLETE and parks are not errors: they commit inside the tx and
// return nil, so nothing retries them.
func (w *RfqPriceWorker) Work(ctx context.Context, job *river.Job[PriceArgs]) error {
	args := job.Args
	cancelCause, err := w.run(ctx, args)
	if err != nil {
		return err
	}
	if cancelCause != nil {
		return river.JobCancel(cancelCause)
	}
	return nil
}

// run sequences the worker's three phases: peek (read tx) → PTAX warm-up
// (no tx) → price (tenant write tx, same shape as QuoteSendWorker.run).
// The tenant tx commits whatever price decided — a park or the whole
// quote — and the returned cancelCause becomes the JobCancel Work wraps.
func (w *RfqPriceWorker) run(ctx context.Context, args PriceArgs) (cancelCause error, err error) {
	fxCurrencies := w.peekFXCurrencies(ctx, args)
	w.warmFXRates(ctx, fxCurrencies)
	err = w.Runner.InTenantTx(ctx, args.OrganizationID, func(ctx context.Context, tx pgx.Tx) error {
		cancelCause, err = w.price(ctx, tx, args, fxCurrencies)
		return err
	})
	return cancelCause, err
}

// price is the full S3b sequence inside the tenant transaction:
// lock → decode → assemble Input → load candidates → pin FX rates →
// engine → park or quote-and-chain.
func (w *RfqPriceWorker) price(ctx context.Context, tx pgx.Tx, args PriceArgs, fxCurrencies []string) (cancelCause error, err error) {
	rfq, found, err := lockPriceRfq(ctx, tx, args.RfqID)
	if err != nil {
		return nil, fmt.Errorf("jobs: rfq_price: lock rfq %s: %w", args.RfqID, err)
	}
	if !found {
		// Cross-tenant or deleted — RLS makes the row invisible; nothing
		// to audit (a job for another tenant's RFQ is envelope drift, and
		// the audit must not leak into this tenant either way).
		return nil, nil
	}
	if rfq.Status != rfqPricing {
		return nil, nil
	}
	fields, err := decodePriceExtraction(rfq.Payload)
	if err != nil {
		// PRICING without a decodable payload is contract drift: S2 writes
		// extracted_payload_json in the same commit as the status.
		return fmt.Errorf("jobs: rfq_price: extraction on %s: %w", rfq.ID, err), nil
	}
	verdict := buildShipment(fields, w.clock())
	if verdict.park != "" {
		return nil, w.park(ctx, tx, args, rfq.ID, verdict.park,
			map[string]any{"field": verdict.field})
	}
	in := verdict.in
	rule, found, err := loadPriceRule(ctx, tx, in)
	switch {
	case errors.Is(err, errUndecodable):
		return fmt.Errorf("jobs: rfq_price: %w", err), nil
	case err != nil:
		return nil, fmt.Errorf("jobs: rfq_price: pricing_rules: %w", err)
	}
	if !found {
		// No rule row is a config gap, not drift — the analyst adds the
		// org's margin rule and reprocesses.
		return nil, w.park(ctx, tx, args, rfq.ID, ReviewReasonPricingIncomplete,
			map[string]any{"engine_reasons": []string{"NO_PRICING_RULE"}})
	}
	in.MarkupRate, in.MinMarkupUSD = rule.markup, rule.minMarkup
	in.FXSafetySpread, in.FixedFees = rule.fxSpread, rule.fixedFees
	in.Tariffs, err = loadTariffCandidates(ctx, tx, in)
	if err != nil {
		return nil, fmt.Errorf("jobs: rfq_price: private_tariffs: %w", err)
	}
	offer, err := loadLatestOffer(ctx, tx, rfq.ID)
	switch {
	case errors.Is(err, errUndecodable):
		// An OFFER_FOUND row the contract cannot decode is drift — the
		// row passed validation at S3a.
		return fmt.Errorf("jobs: rfq_price: carrier offer on %s: %w", rfq.ID, err), nil
	case err != nil:
		return nil, fmt.Errorf("jobs: rfq_price: carrier_offers: %w", err)
	}
	in.CarrierOffer = offer
	// Phase C: pin the window phase B fetched plus whatever other feeds
	// already stored (CARRIER/CUSTOMS land through their own paths).
	// The resolver takes newest-at-or-before, so a window that reaches
	// back only adds staler candidates — never a wrong pick. An empty
	// set means phase A saw nothing to price; FXRates stays empty and
	// the engine parks MISSING_FX as before this table existed.
	if len(fxCurrencies) > 0 {
		in.FXRates, err = fx.LoadRates(ctx, tx, fxCurrencies, loadFXWindow(in))
		if err != nil {
			return nil, fmt.Errorf("jobs: rfq_price: fx_rates: %w", err)
		}
	}
	out, err := pricing.Price(in, w.clock)
	if err != nil {
		// validate() failures are programmer/config errors (spec §2).
		return fmt.Errorf("jobs: rfq_price: engine rejected input on %s: %w", rfq.ID, err), nil
	}
	if out.Status == pricing.StatusIncomplete {
		reason, err := w.parkReasonFor(ctx, tx, in, out)
		if err != nil {
			return nil, err
		}
		return nil, w.parkIncomplete(ctx, tx, args, rfq.ID, reason, out)
	}
	if !rfq.Confidence.Valid {
		// PRICING without a confidence is S2-contract drift — the gate
		// writes extraction_confidence in the same commit as the status.
		// Deterministic: cancel, never retry (spec §3).
		return errors.New("jobs: rfq_price: extraction_confidence is NULL"), nil
	}
	return nil, w.commitQuote(ctx, tx, args, rfq, in, out)
}

// park writes NEEDS_REVIEW + the review_reason and audits it — the
// extraction stays untouched so the analyst works from it (spec S3b:
// parks are never FAILED; nothing broke).
func (w *RfqPriceWorker) park(ctx context.Context, tx pgx.Tx, args PriceArgs, rfqID uuid.UUID, reason ReviewReason, meta map[string]any) error {
	if _, err := tx.Exec(ctx, `
		UPDATE app.rfq_requests
		SET status = 'NEEDS_REVIEW', review_reason = $2, updated_at = now()
		WHERE id = $1`, rfqID, string(reason)); err != nil {
		return fmt.Errorf("jobs: rfq_price: park rfq %s: %w", rfqID, err)
	}
	if meta == nil {
		meta = map[string]any{}
	}
	meta["review_reason"] = string(reason)
	return w.audit(ctx, tx, args, rfqID, "rfq.parked", meta)
}

// parkIncomplete parks an engine INCOMPLETE with the engine's own reasons
// and warnings carried verbatim in the audit — the review_reason code
// loses no detail (spec S3b).
func (w *RfqPriceWorker) parkIncomplete(ctx context.Context, tx pgx.Tx, args PriceArgs, rfqID uuid.UUID, reason ReviewReason, out pricing.Output) error {
	return w.park(ctx, tx, args, rfqID, reason, map[string]any{
		"engine_reasons":  out.Reasons,
		"engine_warnings": out.Warnings,
	})
}

// parkReasonFor maps the engine's INCOMPLETE reasons onto the
// review_reason vocabulary: EXPORT and the missing/stale tariff split
// are dedicated codes; everything else is the generic PRICING_INCOMPLETE.
func (w *RfqPriceWorker) parkReasonFor(ctx context.Context, tx pgx.Tx, in pricing.Input, out pricing.Output) (ReviewReason, error) {
	for _, r := range out.Reasons {
		if r == pricing.ReasonExportMatrixOpen {
			return ReviewReasonExportMatrixOpen, nil
		}
	}
	groups := missingTariffGroups(in.Modal, out.Reasons)
	if len(groups) > 0 {
		missing, err := anyTariffGroupEmpty(ctx, tx, in, groups)
		if err != nil {
			return "", fmt.Errorf("jobs: rfq_price: tariff re-query: %w", err)
		}
		if missing {
			return ReviewReasonMissingTariff, nil
		}
		return ReviewReasonStaleTariff, nil
	}
	return ReviewReasonPricingIncomplete, nil
}

// commitQuote is the COMPLETE half: counter → quote row → line items →
// RFQ READY → audit → S4 enqueue, all in this tx (spec S3b).
func (w *RfqPriceWorker) commitQuote(ctx context.Context, tx pgx.Tx, args PriceArgs, rfq priceRfq, in pricing.Input, out pricing.Output) error {
	version, err := nextQuoteVersion(ctx, tx, rfq.ID)
	if err != nil {
		return fmt.Errorf("jobs: rfq_price: version: %w", err)
	}
	// Reprocess supersedes the previous DRAFT (rule 9): the prior version
	// keeps its row — SUPERSEDED, never overwritten or deleted.
	if _, err := tx.Exec(ctx, `
		UPDATE app.quotes SET status = 'SUPERSEDED', updated_at = now()
		WHERE rfq_id = $1 AND status = 'DRAFT'`, rfq.ID); err != nil {
		return fmt.Errorf("jobs: rfq_price: supersede drafts on %s: %w", rfq.ID, err)
	}
	seq, err := allocQuoteSeq(ctx, tx, args.OrganizationID)
	if err != nil {
		return fmt.Errorf("jobs: rfq_price: org_quote_counters: %w", err)
	}
	slug, err := orgSlug(ctx, tx)
	if err != nil {
		return fmt.Errorf("jobs: rfq_price: org slug: %w", err)
	}
	number := fmt.Sprintf("%s-%d-%04d", strings.ToUpper(slug), in.PricedAt.Year(), seq)
	fallback := composeFallbackReason(in.Modal, in.CarrierOffer != nil, out)
	pricingJSON, err := pricingResultJSON(out, fallback)
	if err != nil {
		return fmt.Errorf("jobs: rfq_price: pricing_result_json: %w", err)
	}
	quoteID, err := w.insertQuote(ctx, tx, args, rfq, in, out, number, version, pricingJSON)
	if err != nil {
		return err
	}
	if err := insertQuoteLines(ctx, tx, args.OrganizationID, quoteID, out.Lines, rfq.Confidence.Decimal); err != nil {
		return fmt.Errorf("jobs: rfq_price: line items: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE app.rfq_requests SET status = 'READY', updated_at = now() WHERE id = $1`, rfq.ID); err != nil {
		return fmt.Errorf("jobs: rfq_price: set READY %s: %w", rfq.ID, err)
	}
	if err := w.audit(ctx, tx, args, rfq.ID, "rfq.priced", map[string]any{
		"quote_id":        quoteID.String(),
		"quote_number":    number,
		"version":         version,
		"fallback_reason": fallback,
		"inputs_hash":     out.InputsHash,
	}); err != nil {
		return err
	}
	return w.enqueueRender(ctx, tx, args, quoteID)
}

// insertQuote writes the DRAFT header and returns its id. cargo_cif_brl
// carries the declared CIF base (goods + declared freight + declared
// insurance at the pinned CUSTOMS rate — cargoCIFBRL); NULL when any
// component or rate is missing, which is the only honest "not
// computable" (never a partial sum).
func (w *RfqPriceWorker) insertQuote(ctx context.Context, tx pgx.Tx, args PriceArgs, rfq priceRfq, in pricing.Input, out pricing.Output, number string, version int, pricingJSON []byte) (uuid.UUID, error) {
	fxJSON, err := marshalSlice(out.FXApplied)
	if err != nil {
		return uuid.Nil, fmt.Errorf("jobs: rfq_price: fx_rates_applied_json: %w", err)
	}
	var offerID *uuid.UUID
	if in.CarrierOffer != nil {
		offerID = &in.CarrierOffer.ID
	}
	var cif decimal.NullDecimal
	if v, ok := cargoCIFBRL(in); ok {
		cif = decimal.NullDecimal{Decimal: v, Valid: true}
	}
	var id uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO app.quotes
			(organization_id, rfq_id, quote_number, version, status,
			 quote_date, priced_at, inputs_hash, carrier_offer_id, cargo_cif_brl,
			 total_cost_brl, target_margin, final_price_brl, fx_rates_applied_json,
			 overall_confidence, pricing_result_json)
		VALUES ($1,$2,$3,$4,'DRAFT',$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		RETURNING id`,
		args.OrganizationID, rfq.ID, number, version,
		in.QuoteDate, in.PricedAt, out.InputsHash, offerID, cif,
		out.TotalCostBRL, in.MarkupRate, out.FinalPriceBRL, fxJSON,
		rfq.Confidence.Decimal, pricingJSON).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("jobs: rfq_price: insert quote: %w", err)
	}
	return id, nil
}

// insertQuoteLines writes one row per engine line — never Alternatives,
// which live inside pricing_result_json instead. line_confidence carries
// the quote's overall_confidence value: no per-line calibrated source
// exists yet (flagged).
func insertQuoteLines(ctx context.Context, tx pgx.Tx, org, quoteID uuid.UUID, lines []pricing.Line, confidence decimal.Decimal) error {
	for i, l := range lines {
		flags, err := marshalSlice(l.Flags)
		if err != nil {
			return fmt.Errorf("flags on line %d: %w", i, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO app.quote_line_items
				(organization_id, quote_id, sort_order, category, description,
				 source_type, private_tariff_id, public_snapshot_id, carrier_offer_id,
				 original_currency, original_amount, fx_rate_used,
				 cost_amount_brl, sell_amount_brl, line_confidence, flags)
			VALUES ($1,$2,$3,$4::app.quote_line_category,$5,$6::app.line_source_type,
			        $7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
			org, quoteID, i, string(l.Category), l.Description,
			string(l.SourceType), l.PrivateTariffID, l.PublicSnapshotID, l.CarrierOfferID,
			l.Currency, l.OriginalAmount, l.FXRateUsed,
			l.CostBRL, l.SellBRL, confidence, flags); err != nil {
			return fmt.Errorf("insert line %d (%s): %w", i, l.ChargeCode, err)
		}
	}
	return nil
}

// enqueueRender chains S4 in the same transaction; queue and uniqueness
// come from RenderArgs.InsertOpts() (a re-enqueue dedupes on live
// states — the render worker itself is idempotent on pdf_storage_key).
func (w *RfqPriceWorker) enqueueRender(ctx context.Context, tx pgx.Tx, args PriceArgs, quoteID uuid.UUID) error {
	if w.Inserter == nil {
		return errors.New("jobs: rfq_price: Inserter not configured")
	}
	_, err := w.Inserter.InsertTx(ctx, tx, RenderArgs{
		OrganizationID: args.OrganizationID,
		CorrelationID:  args.CorrelationID,
		RfqID:          args.RfqID,
		QuoteID:        quoteID,
	}, nil)
	if err != nil {
		return fmt.Errorf("jobs: rfq_price: enqueue rfq_render %s: %w", quoteID, err)
	}
	return nil
}

// audit appends one tenant_audit_logs row for the RFQ — worker actions
// carry no actor (a machine did this, not a user).
func (w *RfqPriceWorker) audit(ctx context.Context, tx pgx.Tx, args PriceArgs, rfqID uuid.UUID, action string, meta map[string]any) error {
	if _, err := platform.NewStore(tx).InsertAuditLog(ctx, platform.AuditEntry{
		OrganizationID: args.OrganizationID,
		Action:         action,
		EntityType:     "rfq",
		EntityID:       rfqID,
		Metadata:       meta,
	}); err != nil {
		return fmt.Errorf("jobs: rfq_price: audit %q on %s: %w", action, rfqID, err)
	}
	return nil
}
