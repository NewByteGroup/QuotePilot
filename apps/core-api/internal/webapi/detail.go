package webapi

// GET /v1/quotes/{quoteId} — the split-screen audit: the inbox item, the
// original email with extracted spans highlighted, the extraction list
// and the price composition, all off one InTenantReadTx snapshot.
import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/shopspring/decimal"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
)

// detailRow is the rfq_requests slice the audit page reads — the
// inboxRow's fields plus the email body and sender the list never needs.
type detailRow struct {
	inboxRow
	customerEmail string
	rawText       *string
}

// quoteRow is the latest version's audit slice: the margin triplet the
// composition reports and the pinned engine output the sections/fx come
// from.
type quoteRow struct {
	targetMargin decimal.Decimal
	totalCost    decimal.Decimal
	finalPrice   decimal.Decimal
	overallConf  decimal.Decimal
	pricingJSON  []byte
}

// recalcFxThresholdPercent is a display placeholder until the pricing
// policy owns a real recalculation threshold — the contract field exists
// so the client stops depending on a mock constant.
const recalcFxThresholdPercent = "5.0"

// GetQuoteDetail implements the audit read. quoteId is the RFQ's id (the
// inbox item id, stable across quote versions); RLS makes an unknown or
// other-tenant id the same empty set, mapped to the uniform 404 like
// ResolveTenant's own rule.
func (e *Endpoints) GetQuoteDetail(ctx context.Context, req authapi.GetQuoteDetailRequestObject) (authapi.GetQuoteDetailResponseObject, error) {
	orgID, ok := platform.OrgID(ctx)
	if !ok {
		return authapi.GetQuoteDetail401JSONResponse{
			UnauthorizedJSONResponse: authapi.UnauthorizedJSONResponse{Error: authapi.ErrorCodeUnauthenticated},
		}, nil
	}
	var detail *authapi.QuoteDetail
	err := e.runner.InTenantReadTx(ctx, orgID, func(ctx context.Context, tx pgx.Tx) error {
		d, found, err := e.quoteDetail(ctx, tx, uuid.UUID(req.QuoteID))
		if err != nil {
			return err
		}
		if found {
			detail = &d
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if detail == nil {
		return authapi.GetQuoteDetail404JSONResponse{
			NotFoundJSONResponse: authapi.NotFoundJSONResponse{Error: authapi.ErrorCodeNotFound},
		}, nil
	}
	return authapi.GetQuoteDetail200JSONResponse(*detail), nil
}

// quoteDetail runs the page's four reads — RFQ row, latest quote,
// attachments, the org's inbound alias and the validity rule — then folds
// them into the contract shape.
func (e *Endpoints) quoteDetail(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID) (authapi.QuoteDetail, bool, error) {
	rfq, found, err := loadDetailRfq(ctx, tx, rfqID)
	if err != nil || !found {
		return authapi.QuoteDetail{}, found, err
	}
	quote, hasQuote, err := loadLatestQuote(ctx, tx, rfqID)
	if err != nil {
		return authapi.QuoteDetail{}, false, err
	}
	var pricingOut pricing.Output
	pricingFound := false
	if hasQuote && len(quote.pricingJSON) > 0 {
		pricingOut, err = decodePricing(quote.pricingJSON)
		if err != nil {
			return authapi.QuoteDetail{}, false, fmt.Errorf("webapi: detail: pricing on %s: %w", rfqID, err)
		}
		pricingFound = true
	}
	d := authapi.QuoteDetail{}
	if d.Quote, err = mapDetailQuote(rfq, quote, e.now()); err != nil {
		return authapi.QuoteDetail{}, false, err
	}
	if d.Email, err = emailFor(ctx, tx, rfq); err != nil {
		return authapi.QuoteDetail{}, false, err
	}
	if d.Extraction, err = extractionList(rfq.extraction); err != nil {
		return authapi.QuoteDetail{}, false, fmt.Errorf("webapi: detail: extraction on %s: %w", rfqID, err)
	}
	if pricingFound {
		comp, err := e.priceComposition(ctx, tx, quote, pricingOut)
		if err != nil {
			return authapi.QuoteDetail{}, false, err
		}
		d.Pricing = &comp
	}
	return d, true, nil
}

// loadDetailRfq reads the audit slice off the inbox's own base set — an
// ignored or non-RFQ row is the same miss an unknown id is, consistent
// with its invisibility in the list.
func loadDetailRfq(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID) (detailRow, bool, error) {
	var r detailRow
	err := tx.QueryRow(ctx, `
		SELECT id, rfq_number, coalesce(customer_name, customer_email), subject,
		       status::text, received_at, extracted_payload_json, extraction_confidence,
		       customer_email, raw_text_body
		FROM app.rfq_requests
		WHERE id = $1 AND kind = 'RFQ' AND status <> 'IGNORED'`, rfqID).
		Scan(&r.id, &r.rfqNumber, &r.customerName, &r.subject,
			&r.status, &r.receivedAt, &r.extraction, &r.confidence,
			&r.customerEmail, &r.rawText)
	if errors.Is(err, pgx.ErrNoRows) {
		return detailRow{}, false, nil
	}
	if err != nil {
		return detailRow{}, false, fmt.Errorf("webapi: detail: rfq %s: %w", rfqID, err)
	}
	return r, true, nil
}

// loadLatestQuote reads the newest version — the one the audit displays;
// SUPERSEDED predecessors stay reproducible history, not the current
// answer. found=false means the RFQ is still pre-pricing.
func loadLatestQuote(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID) (quoteRow, bool, error) {
	var q quoteRow
	err := tx.QueryRow(ctx, `
		SELECT target_margin, total_cost_brl, final_price_brl,
		       overall_confidence, pricing_result_json
		FROM app.quotes
		WHERE rfq_id = $1
		ORDER BY version DESC LIMIT 1`, rfqID).
		Scan(&q.targetMargin, &q.totalCost, &q.finalPrice, &q.overallConf, &q.pricingJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return quoteRow{}, false, nil
	}
	if err != nil {
		return quoteRow{}, false, fmt.Errorf("webapi: detail: quote on %s: %w", rfqID, err)
	}
	return q, true, nil
}

// mapDetailQuote reuses the list's InboxQuote mapping with the latest
// version's pricing blob — the detail's quote member must match the row
// the inbox emitted.
func mapDetailQuote(rfq detailRow, quote quoteRow, now time.Time) (authapi.InboxQuote, error) {
	row := rfq.inboxRow
	row.pricingJSON = quote.pricingJSON
	return mapInboxQuote(row, now)
}

// emailFor assembles the contract's email block: sender/received off the
// RFQ, recipient off the org's registry row (the alias the inbound
// resolver matched), paragraphs with evidence highlights, attachments off
// rfq_attachments.
func emailFor(ctx context.Context, tx pgx.Tx, rfq detailRow) (authapi.QuoteEmail, error) {
	org, found, err := platform.NewStore(tx).GetCurrentOrganization(ctx)
	if err != nil {
		return authapi.QuoteEmail{}, fmt.Errorf("webapi: detail: org row: %w", err)
	}
	if !found {
		return authapi.QuoteEmail{}, fmt.Errorf("webapi: detail: org absent inside tenant tx for rfq %s", rfq.id)
	}
	var fields extractionFields
	if len(rfq.extraction) > 0 {
		if fields, err = decodeExtraction(rfq.extraction); err != nil {
			return authapi.QuoteEmail{}, fmt.Errorf("decode extraction fields: %w", err)
		}
	}
	atts, err := emailAttachments(ctx, tx, rfq.id)
	if err != nil {
		return authapi.QuoteEmail{}, err
	}
	return authapi.QuoteEmail{
		From:        openapi_types.Email(rfq.customerEmail),
		To:          openapi_types.Email(org.InboundEmailAlias),
		ReceivedAt:  rfq.receivedAt,
		Paragraphs:  paragraphs(rfq.rawText, bodySpans(fields)),
		Attachments: atts,
	}, nil
}

// emailAttachments lists the RFQ's stored attachments — metadata only;
// the bytes stay in object storage.
func emailAttachments(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID) ([]authapi.EmailAttachment, error) {
	rows, err := tx.Query(ctx, `
		SELECT filename, coalesce(size_bytes, 0)
		FROM app.rfq_attachments WHERE rfq_id = $1
		ORDER BY created_at, id`, rfqID)
	if err != nil {
		return nil, fmt.Errorf("webapi: detail: attachments on %s: %w", rfqID, err)
	}
	defer rows.Close()
	out := []authapi.EmailAttachment{}
	for rows.Next() {
		var a authapi.EmailAttachment
		if err := rows.Scan(&a.FileName, &a.SizeBytes); err != nil {
			return nil, fmt.Errorf("webapi: detail: attachment scan: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("webapi: detail: attachment rows: %w", err)
	}
	return out, nil
}

// extractionList renders the fields map as the audit's field rows —
// sorted keys so the response is deterministic, label humanized off the
// snake_case key. An absent payload is an empty, never null, list.
func extractionList(raw []byte) ([]authapi.ExtractedField, error) {
	out := []authapi.ExtractedField{}
	if len(raw) == 0 {
		return out, nil
	}
	fields, err := decodeExtraction(raw)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fv := fields[k]
		out = append(out, authapi.ExtractedField{
			Label:    humanizeField(k),
			Value:    fv.Value,
			Inferred: fv.Inferred,
		})
	}
	return out, nil
}

// priceComposition folds the quote row and the pinned output into the
// contract block. Money rides as decimal strings straight off the engine
// output — nothing re-rounds here.
func (e *Endpoints) priceComposition(ctx context.Context, tx pgx.Tx, q quoteRow, out pricing.Output) (authapi.PriceComposition, error) {
	comp := authapi.PriceComposition{
		Sections:                 priceSections(out),
		TotalCost:                authapi.Money{Amount: q.totalCost.String(), Currency: "BRL"},
		MarginPercent:            q.targetMargin.Mul(decimal.NewFromInt(100)).StringFixed(2),
		Margin:                   authapi.Money{Amount: q.finalPrice.Sub(q.totalCost).String(), Currency: "BRL"},
		ConfidencePercent:        int(q.overallConf.Mul(decimal.NewFromInt(100)).Round(0).IntPart()),
		Fx:                       appliedFx(out),
		RecalcFxThresholdPercent: recalcFxThresholdPercent,
	}
	if total, currency, ok := sellTotal(out.Groups); ok {
		comp.ProposalTotal = authapi.Money{Amount: total.String(), Currency: currency}
	} else {
		// A priced quote with no groups has no customer currency to name —
		// the BRL figure is the honest total then.
		comp.ProposalTotal = authapi.Money{Amount: q.finalPrice.String(), Currency: "BRL"}
	}
	if err := e.fillValidity(ctx, tx, &comp); err != nil {
		return comp, err
	}
	return comp, nil
}

// fillValidity stamps validityHours off the org's catch-all
// pricing_rules row (quote_validity_days × 24); the 7-day column default
// is the fallback when no 'ALL' rule exists.
func (e *Endpoints) fillValidity(ctx context.Context, tx pgx.Tx, comp *authapi.PriceComposition) error {
	return tx.QueryRow(ctx, `
		SELECT coalesce(max(quote_validity_days), 7) * 24
		FROM app.pricing_rules
		WHERE modal = 'ALL' AND origin_locode IS NULL AND dest_locode IS NULL`).
		Scan(&comp.ValidityHours)
}

// priceSectionTitles is the PT-BR title the mock's audit view shows per
// payload group — the enum values stay English in the contract data.
var priceSectionTitles = map[pricing.PayloadGroup]string{
	pricing.GroupInternationalFreight: "Frete internacional",
	pricing.GroupLocalFees:            "Taxas locais",
	pricing.GroupServiceTaxes:         "Serviços e impostos",
}

// priceSections emits one section per (group, currency) total in engine
// order, items from the group's own lines: the description labels, the
// charge code details, and each line's original-currency sell.
func priceSections(out pricing.Output) []authapi.PriceSection {
	sections := make([]authapi.PriceSection, 0, len(out.Groups))
	for _, g := range out.Groups {
		title, ok := priceSectionTitles[g.Group]
		if !ok {
			title = humanizeField(strings.ToLower(string(g.Group)))
		}
		sections = append(sections, authapi.PriceSection{
			Title: title,
			Items: priceItems(out.Lines, g.Group),
		})
	}
	return sections
}

// priceItems filters the lines of one group into contract items. inferred
// is always false — engine lines are computed, never assumed; the
// extraction list carries the inference flags.
func priceItems(lines []pricing.Line, group pricing.PayloadGroup) []authapi.PriceItem {
	items := []authapi.PriceItem{}
	for _, l := range lines {
		if l.Group != group {
			continue
		}
		item := authapi.PriceItem{
			Label: l.Description,
			Amount: authapi.Money{
				Amount:   l.SellOriginal.String(),
				Currency: l.Currency,
			},
		}
		if l.ChargeCode != "" {
			detail := l.ChargeCode
			item.Detail = &detail
		}
		items = append(items, item)
	}
	return items
}

// appliedFx renders the first applied rate — deduped and sorted by the
// engine, so index 0 is the canonical display. The label is source +
// currency (v1 keeps the raw source keys; the mock's "dólar armador" is
// display polish a later pass owns). The reference rate backs the spread
// out of the applied rate — best-effort per spec, PTAX labelled.
func appliedFx(out pricing.Output) *authapi.AppliedFxRate {
	if len(out.FXApplied) == 0 {
		return nil
	}
	f := out.FXApplied[0]
	reference := f.Rate
	if !f.Spread.IsZero() {
		reference = f.Rate.Sub(f.Spread)
	}
	return &authapi.AppliedFxRate{
		Label:          f.Source + " " + f.Currency,
		Rate:           f.Rate.String(),
		ReferenceLabel: "PTAX",
		ReferenceRate:  reference.String(),
	}
}
