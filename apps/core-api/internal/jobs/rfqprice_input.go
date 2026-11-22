package jobs

// rfqprice_input.go — S3b's untrusted-extraction side: the FieldValue
// envelope decode and the assembly of the shipment half of
// pricing.Input, including every park verdict the worker can commit
// before the engine runs. Persistence and row loading live in
// rfqprice.go / rfqprice_load.go.

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
)

// isoDate is the ready_date contract format (IsoDate).
const isoDate = "2006-01-02"

// locodePattern is the contract Locode shape (common.schema.json):
// two-letter ISO country plus three-alphanumeric location code.
var locodePattern = regexp.MustCompile(`^[A-Z]{2}[A-Z0-9]{3}$`)

// currencyPattern is the contract Iso4217Currency shape.
var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// extractedField is the one member of a FieldValue envelope the worker
// reads (the rest is contract data core validated at ingest).
type extractedField struct {
	Value string `json:"value"`
}

// decodePriceExtraction reads the fields map off the persisted
// ExtractionResult — the same document shape webapi decodes for the
// audit view. A NULL/empty payload or a missing fields object is an
// error: S2 writes both in one commit.
func decodePriceExtraction(raw []byte) (map[string]extractedField, error) {
	if len(raw) == 0 {
		return nil, errors.New("extracted_payload_json is NULL")
	}
	var doc struct {
		Fields map[string]extractedField `json:"fields"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if doc.Fields == nil {
		return nil, errors.New("no fields object")
	}
	return doc.Fields, nil
}

// ---------------------------------------------------------------------------
// Input assembly (untrusted extraction -> pricing.Input)
// ---------------------------------------------------------------------------

// shipmentVerdict is buildShipment's tri-state: the assembled Input, or
// the park the worker commits instead. park is the zero value ("") when
// the Input is usable; field names the extraction key behind the park
// for the audit row.
type shipmentVerdict struct {
	in    pricing.Input
	park  ReviewReason
	field string
}

func (v shipmentVerdict) missing(field string) shipmentVerdict {
	v.park = ReviewReasonRequiredFieldMissing
	v.field = field
	return v
}

// buildShipment turns the extraction's FieldValue map into the shipment
// half of pricing.Input. FieldValue.Value is a string per contract; a
// required key absent or unparsable parks REQUIRED_FIELD_MISSING, a
// malformed optional key parks PRICING_INCOMPLETE — never a guess.
//
// Flagged decisions (all surfaced in the PR):
//   - Direction is derived, not extracted: a BR destination means IMPORT,
//     a BR origin means EXPORT; both-BR and neither-BR park
//     DIRECTION_AMBIGUOUS (RFC-001 Q15 is open).
//   - TerminalRequested is always false: it is not an extracted field.
//   - QuoteDate is ready_date when extracted, else the pricedAt date —
//     truncated to day granularity like the tariff validity windows.
//   - Snapshots stays nil: no extracted_rates_json shape is a repo
//     contract yet, so no decoder is invented here (flagged).
//   - FXRates stays empty here: the worker pins the window
//     fx.LoadRates returns after the candidate loads (migration 0011);
//     a currency no feed covered still parks via the engine's own
//     MISSING_FX reason.
func buildShipment(fields map[string]extractedField, pricedAt time.Time) shipmentVerdict {
	v := shipmentVerdict{}
	modal, ok := parseModalField(fields)
	if !ok {
		return v.missing("modal")
	}
	v.in.Modal = modal
	inc, ok := parseIncotermField(fields)
	if !ok {
		return v.missing("incoterm")
	}
	v.in.Incoterm = inc
	origin, ok := requiredLocode(fields, "origin_locode")
	if !ok {
		return v.missing("origin_locode")
	}
	dest, ok := requiredLocode(fields, "dest_locode")
	if !ok {
		return v.missing("dest_locode")
	}
	v.in.OriginLOCODE, v.in.DestLOCODE = origin, dest
	dir, ok := deriveDirection(origin, dest)
	if !ok {
		v.park = ReviewReasonDirectionAmbiguous
		v.field = "origin_locode/dest_locode"
		return v
	}
	v.in.Direction = dir
	if modal != pricing.ModalLCL {
		equip, ok := requiredString(fields, "equipment_or_unit")
		if !ok {
			return v.missing("equipment_or_unit")
		}
		v.in.EquipmentOrUnit = strings.ToUpper(equip)
	}
	if modal == pricing.ModalFCL {
		n, ok := requiredContainerCount(fields)
		if !ok {
			return v.missing("quantity")
		}
		v.in.ContainerCount = n
	}
	if !v.optionalMeasures(fields) {
		return v
	}
	if !v.declaredMonies(fields) {
		return v
	}
	v.in.QuoteDate = quoteDateField(fields, pricedAt)
	v.in.PricedAt = pricedAt
	// TerminalRequested, Snapshots, FXRates: see the doc comment flags —
	// FXRates is pinned later in price(), never from the extraction.
	v.in.TerminalRequested = false
	v.in.Snapshots = nil
	v.in.FXRates = []pricing.FXRate{}
	return v
}

// fieldString returns the trimmed value of a key, ok=false when the key
// is absent or holds only whitespace.
func fieldString(fields map[string]extractedField, key string) (string, bool) {
	f, ok := fields[key]
	if !ok {
		return "", false
	}
	v := strings.TrimSpace(f.Value)
	return v, v != ""
}

func requiredString(fields map[string]extractedField, key string) (string, bool) {
	return fieldString(fields, key)
}

// requiredLocode normalizes a locode field (upper + trim) and enforces
// the contract shape — a non-locode value is unparsable, i.e. missing.
func requiredLocode(fields map[string]extractedField, key string) (string, bool) {
	v, ok := fieldString(fields, key)
	if !ok {
		return "", false
	}
	v = strings.ToUpper(v)
	if !locodePattern.MatchString(v) {
		return "", false
	}
	return v, true
}

// requiredContainerCount parses the FCL quantity: a positive integral
// DecimalString. Zero, negative or fractional counts are data gaps, not
// engine input — left to the engine they'd be a config-error cancel.
func requiredContainerCount(fields map[string]extractedField) (int, bool) {
	v, ok := fieldString(fields, "quantity")
	if !ok {
		return 0, false
	}
	d, err := decimal.NewFromString(v)
	if err != nil || !d.IsPositive() || !d.Equal(d.Truncate(0)) {
		return 0, false
	}
	return int(d.IntPart()), true
}

func parseModalField(fields map[string]extractedField) (pricing.Modal, bool) {
	v, ok := fieldString(fields, "modal")
	if !ok {
		return "", false
	}
	switch m := pricing.Modal(strings.ToUpper(v)); m {
	case pricing.ModalFCL, pricing.ModalLCL, pricing.ModalAIR:
		return m, true
	default:
		return "", false
	}
}

func parseIncotermField(fields map[string]extractedField) (pricing.Incoterm, bool) {
	v, ok := fieldString(fields, "incoterm")
	if !ok {
		return "", false
	}
	switch i := pricing.Incoterm(strings.ToUpper(v)); i {
	case pricing.IncotermEXW, pricing.IncotermFCA, pricing.IncotermFAS,
		pricing.IncotermFOB, pricing.IncotermCFR, pricing.IncotermCIF,
		pricing.IncotermCPT, pricing.IncotermCIP, pricing.IncotermDAP,
		pricing.IncotermDPU, pricing.IncotermDDP:
		return i, true
	default:
		return "", false
	}
}

// deriveDirection implements the task's derivation rule: the BR endpoint
// decides. Cabotage (both BR) and foreign-to-foreign (neither BR) have no
// direction under RFC-001 Q15 — open — so they park rather than guess.
func deriveDirection(origin, dest string) (pricing.Direction, bool) {
	oBR, dBR := strings.HasPrefix(origin, "BR"), strings.HasPrefix(dest, "BR")
	switch {
	case dBR && !oBR:
		return pricing.DirectionImport, true
	case oBR && !dBR:
		return pricing.DirectionExport, true
	default:
		return "", false
	}
}

// optionalMeasures parses the optional DecimalString measures; a
// present-but-unparsable one parks PRICING_INCOMPLETE rather than
// silently zeroing an extracted fact.
func (v *shipmentVerdict) optionalMeasures(fields map[string]extractedField) bool {
	for _, m := range []struct {
		key string
		dst *decimal.Decimal
	}{
		{"gross_weight_kg", &v.in.GrossKg},
		{"volume_m3", &v.in.VolumeM3},
		{"cargo_value_amount", &v.in.CargoValue},
	} {
		s, ok := fieldString(fields, m.key)
		if !ok {
			continue
		}
		d, err := decimal.NewFromString(s)
		if err != nil {
			v.park = ReviewReasonPricingIncomplete
			v.field = m.key
			return false
		}
		*m.dst = d
	}
	return v.currency(fields)
}

// currency resolves cargo_value_currency: required once
// cargo_value_amount is present, ignored otherwise.
func (v *shipmentVerdict) currency(fields map[string]extractedField) bool {
	s, ok := fieldString(fields, "cargo_value_currency")
	if !ok {
		if !v.in.CargoValue.IsZero() {
			v.park = ReviewReasonRequiredFieldMissing
			v.field = "cargo_value_currency"
			return false
		}
		return true
	}
	s = strings.ToUpper(s)
	if !currencyPattern.MatchString(s) {
		v.park = ReviewReasonRequiredFieldMissing
		v.field = "cargo_value_currency"
		return false
	}
	v.in.CargoCurrency = s
	return true
}

// declaredMonies resolves the declared freight/insurance pairs: a
// present amount demands a well-formed currency (an amount-less currency
// is meaningless and ignored, never a partial guess).
func (v *shipmentVerdict) declaredMonies(fields map[string]extractedField) bool {
	freight, ok := declaredMoney(fields, "declared_freight_amount", "declared_freight_currency", v)
	if !ok {
		return false
	}
	v.in.DeclaredFreight = freight
	ins, ok := declaredMoney(fields, "declared_insurance_amount", "declared_insurance_currency", v)
	if !ok {
		return false
	}
	v.in.DeclaredInsurance = ins
	return true
}

func declaredMoney(fields map[string]extractedField, amountKey, currencyKey string, v *shipmentVerdict) (*pricing.Money, bool) {
	s, ok := fieldString(fields, amountKey)
	if !ok {
		return nil, true
	}
	amount, err := decimal.NewFromString(s)
	if err != nil {
		v.park = ReviewReasonPricingIncomplete
		v.field = amountKey
		return nil, false
	}
	cur, ok := fieldString(fields, currencyKey)
	cur = strings.ToUpper(cur)
	if !ok || !currencyPattern.MatchString(cur) {
		v.park = ReviewReasonRequiredFieldMissing
		v.field = currencyKey
		return nil, false
	}
	return &pricing.Money{Amount: amount, Currency: cur}, true
}

// quoteDateField is ready_date when extracted and parseable, else the
// caller's fallback day — both truncated to day granularity, matching how
// the engine compares tariff validity windows (eligibleRow truncates
// too). The fallback is the caller's choice: S3b passes pricedAt (the
// quote's own date), S3a and the reprocess recompute pass the RFQ's
// received_at so request_hash is stable across days (Devin Review
// round 3).
func quoteDateField(fields map[string]extractedField, fallback time.Time) time.Time {
	if s, ok := fieldString(fields, "ready_date"); ok {
		if d, err := time.Parse(isoDate, s); err == nil {
			return d
		}
	}
	return time.Date(fallback.Year(), fallback.Month(), fallback.Day(), 0, 0, 0, 0, time.UTC)
}
