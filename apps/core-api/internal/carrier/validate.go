package carrier

// validate.go — the sanity bounds core runs on every untrusted offer
// payload before it may persist (AGENTS.md rule 6 + spec S3a). A
// violation is *OfferViolation: the worker treats it like any fetch
// failure (retry inside the attempt budget, FETCH_FAILED on the last).

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
)

// Sanity bounds (spec S3a): the payload stays under the wire's 64 KiB
// body cap, charges cap at fifty rows and codes at 64 chars.
const (
	maxOfferBytes   = 64 * 1024
	maxOfferCharges = 50
	maxChargeCode   = 64
)

// currencyPattern is the Iso4217Currency shape the offer contract uses.
var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// OfferViolation reports an offer payload that failed the sanity bounds.
// It is never transient: the same bytes would fail again, so retrying
// only burns the attempt budget the worker owns.
type OfferViolation struct {
	Detail string
}

// Error implements error.
func (e *OfferViolation) Error() string {
	return "carrier: offer sanity bounds: " + e.Detail
}

// Transient reports false — a contract violation is never retried on its
// own merit (mirrors aiengine.EngineError.Contract semantics; the S3a
// worker still bounds it inside the job's attempt budget).
func (e *OfferViolation) Transient() bool { return false }

// ValidateOffer decodes raw into OfferPayload and enforces the sanity
// bounds: required keys present (carrier, non-empty charges, expires_at —
// the engine treats a missing expiry as expired, so a DOA offer is a
// violation, not a surprise), every charge a known category with a
// bounded code, positive decimal amount, ISO currency and equipment.
func ValidateOffer(raw json.RawMessage) (*OfferPayload, error) {
	if len(raw) > maxOfferBytes {
		return nil, &OfferViolation{Detail: fmt.Sprintf("payload %d bytes exceeds %d", len(raw), maxOfferBytes)}
	}
	var p OfferPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &OfferViolation{Detail: fmt.Sprintf("decode: %s", err)}
	}
	if strings.TrimSpace(p.Carrier) == "" {
		return nil, &OfferViolation{Detail: "carrier is required"}
	}
	if p.ExpiresAt.IsZero() {
		return nil, &OfferViolation{Detail: "expires_at is required (the engine treats a missing expiry as expired)"}
	}
	if len(p.Charges) == 0 {
		return nil, &OfferViolation{Detail: "charges must be non-empty"}
	}
	if len(p.Charges) > maxOfferCharges {
		return nil, &OfferViolation{Detail: fmt.Sprintf("%d charges exceeds %d", len(p.Charges), maxOfferCharges)}
	}
	for i, c := range p.Charges {
		if err := validateCharge(i, c); err != nil {
			return nil, err
		}
	}
	return &p, nil
}

// validateCharge bounds one charge line: the engine's charge_category
// enum, a bounded code, a strictly positive amount, ISO currency and
// non-empty equipment (the coverage check matches it verbatim).
func validateCharge(i int, c pricing.OfferCharge) error {
	switch c.Category {
	case pricing.ChargeOceanFreight, pricing.ChargeAirFreight, pricing.ChargeSurcharge,
		pricing.ChargeOriginCharge, pricing.ChargeDestinationCharge, pricing.ChargeTerminal,
		pricing.ChargeCustoms, pricing.ChargeOther:
	default:
		return &OfferViolation{Detail: fmt.Sprintf("charge %d: unknown category %q", i, c.Category)}
	}
	if code := strings.TrimSpace(c.Code); code == "" || len(code) > maxChargeCode {
		return &OfferViolation{Detail: fmt.Sprintf("charge %d: code %q must be 1..%d chars", i, c.Code, maxChargeCode)}
	}
	if !c.Amount.IsPositive() {
		return &OfferViolation{Detail: fmt.Sprintf("charge %d: amount %s must be > 0", i, c.Amount)}
	}
	if !currencyPattern.MatchString(c.Currency) {
		return &OfferViolation{Detail: fmt.Sprintf("charge %d: currency %q must match ^[A-Z]{3}$", i, c.Currency)}
	}
	if strings.TrimSpace(c.EquipmentOrUnit) == "" {
		return &OfferViolation{Detail: fmt.Sprintf("charge %d: equipment_or_unit is required", i)}
	}
	return nil
}
