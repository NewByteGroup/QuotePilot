package pricing

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Output is the priced quote envelope (spec §2). Everything an analyst
// must audit is a Line; totals are sums of the rounded line amounts so
// the math always adds up (spec §7).
type Output struct {
	Status        Status          `json:"status"`
	Reasons       []Reason        `json:"reasons"`
	Lines         []Line          `json:"lines"`
	Alternatives  []Line          `json:"alternatives"`
	TotalCostBRL  decimal.Decimal `json:"total_cost_brl"`
	FinalPriceBRL decimal.Decimal `json:"final_price_brl"`
	Warnings      []string        `json:"warnings"`
	Assumptions   []Assumption    `json:"assumptions"`
	FXApplied     []FXApplied     `json:"fx_applied"`
	InputsHash    string          `json:"inputs_hash"`
}

// Line is one auditable quote line: its source pin, original currency
// and amount, the FX rate actually applied, rounded BRL cost and sell,
// and machine flags (spec §2).
type Line struct {
	Category         Category        `json:"category"`
	ChargeCode       string          `json:"charge_code"`
	Description      string          `json:"description"`
	SourceType       SourceType      `json:"source_type"`
	PrivateTariffID  *uuid.UUID      `json:"private_tariff_id,omitempty"`
	PublicSnapshotID *uuid.UUID      `json:"public_snapshot_id,omitempty"`
	CarrierOfferID   *uuid.UUID      `json:"carrier_offer_id,omitempty"`
	Currency         string          `json:"currency"`
	OriginalAmount   decimal.Decimal `json:"original_amount"`
	FXRateUsed       decimal.Decimal `json:"fx_rate_used"`
	CostBRL          decimal.Decimal `json:"cost_brl"`
	SellBRL          decimal.Decimal `json:"sell_brl"`
	Flags            []string        `json:"flags"`
}

// FXApplied is one applied exchange rate (spec §6): the pinned base
// rate plus the org spread actually used, deduped and sorted by
// (source, currency, date) — maps to fx_rates_applied_json.
type FXApplied struct {
	Source   string          `json:"source"`
	Currency string          `json:"currency"`
	Date     time.Time       `json:"date"`
	Rate     decimal.Decimal `json:"rate"`
	Spread   decimal.Decimal `json:"spread"`
}

// CanonicalJSON marshals the output deterministically: struct field
// order plus pre-sorted slices make the bytes stable, which is what the
// determinism test compares (spec §10).
//
// Example:
//
//	b, err := out.CanonicalJSON()
//	// b is byte-identical across runs of the same Input; persist it
//	// with the quote for the audit view.
func (o Output) CanonicalJSON() ([]byte, error) {
	return json.Marshal(o)
}
