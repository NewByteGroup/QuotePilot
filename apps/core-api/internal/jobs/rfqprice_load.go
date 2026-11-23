package jobs

// rfqprice_load.go — S3b's read side: the locked RFQ row, the
// dumb-filtered candidate loads the engine expects (rules, tariffs,
// offer checkpoint), the INCOMPLETE reason mapping and the quote
// bookkeeping helpers (fallback provenance, counter/version/slug,
// persisted envelopes). Worker lifecycle lives in rfqprice.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
)

// ---------------------------------------------------------------------------
// Row loading
// ---------------------------------------------------------------------------

// priceRfq is the locked slice of app.rfq_requests the worker reads.
// ReceivedAt is the RFQ's arrival timestamp — S3a reads it as the
// request_hash ready_date fallback so the digest is stable across days
// (Devin Review round 3).
type priceRfq struct {
	ID         uuid.UUID
	Status     string
	Payload    []byte
	Confidence decimal.NullDecimal
	ReceivedAt time.Time
}

// lockPriceRfq SELECTs the RFQ FOR UPDATE — the lock serializes two
// racing price jobs so the loser reads the winner's committed status and
// no-ops instead of colliding on (organization_id, rfq_id, version).
func lockPriceRfq(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID) (priceRfq, bool, error) {
	var r priceRfq
	err := tx.QueryRow(ctx, `
		SELECT id, status::text, coalesce(extracted_payload_json::text,''), extraction_confidence,
		       received_at
		FROM app.rfq_requests WHERE id = $1 FOR UPDATE`, rfqID).
		Scan(&r.ID, &r.Status, &r.Payload, &r.Confidence, &r.ReceivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return priceRfq{}, false, nil
	}
	return r, err == nil, err
}

// ---------------------------------------------------------------------------
// Candidate loading (the "dumb" filters — org comes from RLS)
// ---------------------------------------------------------------------------

// priceRule is the resolved pricing_rules row after route>modal>ALL
// precedence.
type priceRule struct {
	markup    decimal.Decimal
	minMarkup *decimal.Decimal
	fxSpread  decimal.Decimal
	fixedFees []pricing.FixedFee
}

// loadPriceRule resolves org parameters by precedence (spec §2 +
// migration 0002 comment): exact (modal + both locodes) > modal-only
// default > ALL-modal route > ALL-modal global. One row per combination
// is enforced by the UNIQUE NULLS NOT DISTINCT key, so a LIMIT 1 pick
// under the ranked ORDER BY is deterministic.
func loadPriceRule(ctx context.Context, tx pgx.Tx, in pricing.Input) (priceRule, bool, error) {
	var markup, routeMarkup, spread decimal.NullDecimal
	var minMarkup decimal.NullDecimal
	var fixedRaw []byte
	err := tx.QueryRow(ctx, `
		SELECT default_target_margin, route_target_margin, fx_safety_spread,
		       min_markup_usd, fixed_fees_json::text
		FROM app.pricing_rules
		WHERE (modal = $1::app.pricing_modal AND upper(origin_locode) = $2 AND upper(dest_locode) = $3)
		   OR (modal = $1::app.pricing_modal AND origin_locode IS NULL AND dest_locode IS NULL)
		   OR (modal = 'ALL' AND upper(origin_locode) = $2 AND upper(dest_locode) = $3)
		   OR (modal = 'ALL' AND origin_locode IS NULL AND dest_locode IS NULL)
		ORDER BY CASE
			WHEN modal = $1::app.pricing_modal AND origin_locode IS NOT NULL THEN 1
			WHEN modal = $1::app.pricing_modal THEN 2
			WHEN origin_locode IS NOT NULL THEN 3
			ELSE 4 END
		LIMIT 1`,
		string(in.Modal), in.OriginLOCODE, in.DestLOCODE).
		Scan(&markup, &routeMarkup, &spread, &minMarkup, &fixedRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return priceRule{}, false, nil
	}
	if err != nil {
		return priceRule{}, false, err
	}
	r := priceRule{markup: markup.Decimal, fxSpread: spread.Decimal}
	// route_target_margin applies to route-scoped rules when set;
	// otherwise the row's default_target_margin wins (0002 comment).
	if routeMarkup.Valid {
		r.markup = routeMarkup.Decimal
	}
	if minMarkup.Valid {
		m := minMarkup.Decimal
		r.minMarkup = &m
	}
	if len(fixedRaw) > 0 {
		if err := json.Unmarshal(fixedRaw, &r.fixedFees); err != nil {
			return priceRule{}, false, fmt.Errorf("%w: fixed_fees_json: %v", errUndecodable, err)
		}
	}
	if r.fixedFees == nil {
		r.fixedFees = []pricing.FixedFee{}
	}
	return r, true, nil
}

// loadTariffCandidates applies only the dumb filters (spec §2 Input
// comment): modal, both locodes and equipment normalized like the engine
// compares them, plus the validity window vs QuoteDate. Tombstones and
// unactivated batches stay in the set — version resolution is engine
// logic.
func loadTariffCandidates(ctx context.Context, tx pgx.Tx, in pricing.Input) ([]pricing.TariffCandidate, error) {
	rows, err := tx.Query(ctx, `
		SELECT t.id, t.batch_id, t.carrier_or_agent, t.modal::text,
		       t.origin_locode, t.dest_locode, t.equipment_or_unit,
		       t.charge_category::text, t.charge_code, t.currency,
		       t.net_amount, t.min_amount, t.search_description,
		       t.valid_from, t.valid_until, t.is_removed, t.created_at,
		       b.activated_at
		FROM app.private_tariffs AS t
		JOIN app.private_tariff_batches AS b ON b.id = t.batch_id
		WHERE t.modal = $1::app.freight_modal
		  AND upper(t.origin_locode) = $2
		  AND upper(t.dest_locode) = $3
		  AND upper(t.equipment_or_unit) = $4
		  AND t.valid_from <= $5::date AND t.valid_until >= $5::date`,
		string(in.Modal), in.OriginLOCODE, in.DestLOCODE, in.EquipmentOrUnit,
		in.QuoteDate.Format(isoDate))
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, rowToTariffCandidate)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []pricing.TariffCandidate{}
	}
	return out, nil
}

func rowToTariffCandidate(row pgx.CollectableRow) (pricing.TariffCandidate, error) {
	var c pricing.TariffCandidate
	var minAmount decimal.NullDecimal
	err := row.Scan(&c.ID, &c.BatchID, &c.CarrierOrAgent, &c.Modal,
		&c.OriginLOCODE, &c.DestLOCODE, &c.EquipmentOrUnit,
		&c.ChargeCategory, &c.ChargeCode, &c.Currency, &c.NetAmount,
		&minAmount, &c.SearchDescription, &c.ValidFrom, &c.ValidUntil,
		&c.IsRemoved, &c.CreatedAt, &c.BatchActivatedAt)
	if minAmount.Valid {
		m := minAmount.Decimal
		c.MinAmount = &m
	}
	return c, err
}

// loadLatestOffer returns the newest OFFER_FOUND checkpoint's payload
// decoded as pricing.CarrierOffer, or nil — the absence is the S3a
// "no offer supplied" signal the fallback provenance reads. offer.ID is
// pinned to the row id: line-level carrier_offer_id pins must satisfy
// the FK regardless of what identifier the payload carries.
func loadLatestOffer(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID) (*pricing.CarrierOffer, error) {
	var rowID uuid.UUID
	var payload []byte
	err := tx.QueryRow(ctx, `
		SELECT id, offer_payload_json::text
		FROM app.carrier_offers
		WHERE rfq_id = $1 AND outcome = 'OFFER_FOUND'
		ORDER BY fetched_at DESC, id DESC
		LIMIT 1`, rfqID).Scan(&rowID, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var offer pricing.CarrierOffer
	if err := json.Unmarshal(payload, &offer); err != nil {
		return nil, fmt.Errorf("%w: offer_payload_json: %v", errUndecodable, err)
	}
	offer.ID = rowID
	return &offer, nil
}

// ---------------------------------------------------------------------------
// INCOMPLETE mapping + quote bookkeeping
// ---------------------------------------------------------------------------

// missingTariffGroupPrefix is the engine's parameterised reason family
// (spec §9): the parameter names the quote group or freight charge
// category that produced nothing.
const missingTariffGroupPrefix = "MISSING_TARIFF_GROUP:"

// missingTariffGroups extracts the charge categories behind each
// MISSING_TARIFF_GROUP:<param> reason — the engine emits the freight
// charge category (INTL_FREIGHT for FCL, AIR_FREIGHT for AIR) and the
// quote-line category for the pass-through groups.
func missingTariffGroups(modal pricing.Modal, reasons []pricing.Reason) []pricing.ChargeCategory {
	var groups []pricing.ChargeCategory
	for _, r := range reasons {
		s := string(r)
		if !strings.HasPrefix(s, missingTariffGroupPrefix) {
			continue
		}
		if cat, ok := missingGroupCategory(modal, strings.TrimPrefix(s, missingTariffGroupPrefix)); ok {
			groups = append(groups, cat)
		}
	}
	return groups
}

// missingGroupCategory maps the reason parameter to the private_tariffs
// charge_category the group draws from.
func missingGroupCategory(modal pricing.Modal, param string) (pricing.ChargeCategory, bool) {
	switch param {
	case string(pricing.CategoryIntlFreight), string(pricing.ChargeAirFreight):
		if modal == pricing.ModalAIR {
			return pricing.ChargeAirFreight, true
		}
		return pricing.ChargeOceanFreight, true
	case string(pricing.CategoryOriginCharges):
		return pricing.ChargeOriginCharge, true
	case string(pricing.CategoryCustomsDispatch):
		return pricing.ChargeCustoms, true
	default:
		return "", false
	}
}

// anyTariffGroupEmpty distinguishes MISSING_TARIFF from STALE_TARIFF
// (spec S3b): it re-runs the dumb filters WITHOUT the validity window —
// a required group with no rows at all is missing; rows that exist but
// fell outside the quote date are stale. Rows in unactivated batches
// count as existing (the truthful reading of "rows exist"): an analyst
// fixes them through the same review path.
func anyTariffGroupEmpty(ctx context.Context, tx pgx.Tx, in pricing.Input, groups []pricing.ChargeCategory) (bool, error) {
	cats := make([]string, len(groups))
	for i, g := range groups {
		cats[i] = string(g)
	}
	rows, err := tx.Query(ctx, `
		SELECT charge_category::text, count(*)
		FROM app.private_tariffs
		WHERE modal = $1::app.freight_modal
		  AND upper(origin_locode) = $2
		  AND upper(dest_locode) = $3
		  AND upper(equipment_or_unit) = $4
		  AND charge_category = ANY($5::app.charge_category[])
		GROUP BY charge_category`,
		string(in.Modal), in.OriginLOCODE, in.DestLOCODE, in.EquipmentOrUnit, cats)
	if err != nil {
		return false, err
	}
	counts := map[string]int{}
	for rows.Next() {
		var cat string
		var n int
		if err := rows.Scan(&cat, &n); err != nil {
			rows.Close()
			return false, err
		}
		counts[cat] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, g := range groups {
		if counts[string(g)] == 0 {
			return true, nil
		}
	}
	return false, nil
}

// composeFallbackReason implements spec S3b rate provenance — a fallback
// is never silent. The invariant is scoped to "the customer owes FCL
// freight": S3a only reaches the carrier call for FCL, so a
// tariff-sourced freight line on any other modal is not a fallback at
// all — persisting "missing" there would falsely claim an offer was
// owed. When FCL freight was owed and the winning freight line is
// tariff-sourced, the reason is: expired when the engine flagged the
// replacement line OFFER_EXPIRED, missing when no OFFER_FOUND
// checkpoint was supplied, otherwise incomplete (the offer arrived but
// failed strict coverage or could not be priced — both are "the offer
// did not cover the required carrier groups" for the audit). No
// tariff-sourced freight line means no fallback happened.
func composeFallbackReason(modal pricing.Modal, offerSupplied bool, out pricing.Output) string {
	if modal != pricing.ModalFCL {
		return ""
	}
	var tariffSourced, expired bool
	for _, l := range out.Lines {
		if l.Category != pricing.CategoryIntlFreight || l.SourceType != pricing.SourcePrivateTariff {
			continue
		}
		tariffSourced = true
		for _, f := range l.Flags {
			if f == pricing.FlagOfferExpired {
				expired = true
			}
		}
	}
	if !tariffSourced {
		return ""
	}
	switch {
	case expired:
		return fallbackExpired
	case !offerSupplied:
		return fallbackMissing
	default:
		return fallbackIncomplete
	}
}

// pricingResultJSON is the persisted engine envelope plus the
// machine-readable fallback reason (spec S3b: it lives inside
// pricing_result_json, never a new column). fallback_reason is always
// present — null when no fallback occurred, so the audit view reads
// provenance without a second lookup.
func pricingResultJSON(out pricing.Output, fallback string) ([]byte, error) {
	raw, err := out.CanonicalJSON()
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if fallback == "" {
		doc["fallback_reason"] = nil
	} else {
		doc["fallback_reason"] = fallback
	}
	return json.Marshal(doc)
}

// allocQuoteSeq allocates the next org_quote_counters value the table
// comment prescribes: the row lock inside the tenant tx serializes
// allocation per org (the same shape org_rfq_counters uses, 0009).
func allocQuoteSeq(ctx context.Context, tx pgx.Tx, org uuid.UUID) (int, error) {
	var seq int
	err := tx.QueryRow(ctx, `
		INSERT INTO app.org_quote_counters (organization_id, next_seq)
		VALUES ($1, 2)
		ON CONFLICT (organization_id) DO UPDATE
			SET next_seq = org_quote_counters.next_seq + 1
		RETURNING next_seq - 1`, org).Scan(&seq)
	return seq, err
}

// nextQuoteVersion is max(existing versions for the RFQ) + 1 — rule 9:
// recalculation creates a new version, never an overwrite.
func nextQuoteVersion(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID) (int, error) {
	var v int
	err := tx.QueryRow(ctx,
		`SELECT coalesce(max(version),0)+1 FROM app.quotes WHERE rfq_id = $1`, rfqID).Scan(&v)
	return v, err
}

// orgSlug reads the tenant's slug through get_current_organization —
// qp_app holds no direct grant on organizations (Layer A), and inside
// the tenant tx the function scopes to the resolved org by construction.
func orgSlug(ctx context.Context, tx pgx.Tx) (string, error) {
	var slug string
	err := tx.QueryRow(ctx, `SELECT (app.get_current_organization()).slug`).Scan(&slug)
	return slug, err
}

// marshalSlice renders a slice as JSON, normalizing nil to [] so a
// persisted jsonb column never stores null in place of an empty set.
func marshalSlice[T any](s []T) ([]byte, error) {
	if s == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(s)
}
