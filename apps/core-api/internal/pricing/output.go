package pricing

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Output is the priced quote envelope (spec §2). Everything an analyst
// must audit is a Line. Groups — the per-currency totals over the
// lines' original-currency amounts — is the customer-facing payload;
// TotalCostBRL/FinalPriceBRL are internal-only for the margin check and
// the audit (RFC-001 §5). Both are sums of the rounded line amounts so
// the math always adds up (spec §7).
type Output struct {
	Status        Status          `json:"status"`
	Reasons       []Reason        `json:"reasons"`
	Lines         []Line          `json:"lines"`
	Alternatives  []Line          `json:"alternatives"`
	Groups        []GroupTotal    `json:"groups"`
	TotalCostBRL  decimal.Decimal `json:"total_cost_brl"`
	FinalPriceBRL decimal.Decimal `json:"final_price_brl"`
	Warnings      []string        `json:"warnings"`
	Assumptions   []Assumption    `json:"assumptions"`
	FXApplied     []FXApplied     `json:"fx_applied"`
	InputsHash    string          `json:"inputs_hash"`
}

// Line is one auditable quote line: its payload group, source pin,
// original currency amount and sell, the FX rate actually applied,
// rounded BRL cost and sell, and machine flags (spec §2).
type Line struct {
	Category         Category        `json:"category"`
	Group            PayloadGroup    `json:"group"`
	ChargeCode       string          `json:"charge_code"`
	Description      string          `json:"description"`
	SourceType       SourceType      `json:"source_type"`
	PrivateTariffID  *uuid.UUID      `json:"private_tariff_id,omitempty"`
	PublicSnapshotID *uuid.UUID      `json:"public_snapshot_id,omitempty"`
	CarrierOfferID   *uuid.UUID      `json:"carrier_offer_id,omitempty"`
	Currency         string          `json:"currency"`
	OriginalAmount   decimal.Decimal `json:"original_amount"`
	SellOriginal     decimal.Decimal `json:"sell_original"`
	FXRateUsed       decimal.Decimal `json:"fx_rate_used"`
	CostBRL          decimal.Decimal `json:"cost_brl"`
	SellBRL          decimal.Decimal `json:"sell_brl"`
	Flags            []string        `json:"flags"`
}

// GroupTotal is one (payload group, currency) customer-facing total:
// the summed original-currency cost and sell over the emitted lines —
// never over alternatives (RFC-001 §5: the customer sees grouped
// per-currency totals, never a single BRL figure).
type GroupTotal struct {
	Group    PayloadGroup    `json:"group"`
	Currency string          `json:"currency"`
	Cost     decimal.Decimal `json:"cost"`
	Sell     decimal.Decimal `json:"sell"`
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

// NegativeMargin reports whether the quote sells below cost on the internal
// BRL-normalized totals — the margin basis per RFC-001 section 5's currency
// decision: BRL stays internal-only for the margin check and the audit even
// though customer-facing output groups by original currency. Negative
// margin is a hard block (spec 2026-11-11 S3b): approve (P5) and send (S5)
// must both refuse when this is true — defense in depth, so the signal
// lives on the output both endpoints read.
func (o Output) NegativeMargin() bool {
	return o.FinalPriceBRL.LessThan(o.TotalCostBRL)
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
