package pricing

import (
	"sort"

	"github.com/shopspring/decimal"
)

// render.go is the output-shaping layer: candidate → Line conversion,
// deterministic ordering, and set draining. Everything that decides how
// an Output *looks* (never how it is *computed*) lives here.

// passThroughSell is spec §5's "every other line sells at its rounded
// cost".
func passThroughSell(c candLine) decimal.Decimal {
	return roundMoney(c.netBRL())
}

// toLine renders a priced candidate into an audit Line: source pin,
// original currency amount, applied rate, rounded cost and sell, flags
// sorted and deduplicated.
func toLine(c candLine, sell decimal.Decimal) Line {
	return Line{
		Category:         c.cat,
		ChargeCode:       c.code,
		Description:      c.desc,
		SourceType:       c.src,
		PrivateTariffID:  c.tariffID,
		PublicSnapshotID: c.snapID,
		CarrierOfferID:   c.offerID,
		Currency:         c.ccy,
		OriginalAmount:   c.net,
		FXRateUsed:       c.rate,
		CostBRL:          roundMoney(c.netBRL()),
		SellBRL:          sell,
		Flags:            sortedDedupe(c.flags),
	}
}

// sortLines orders by category asc then charge_code (spec §2), with
// source and pin as deterministic tiebreakers.
func sortLines(ls []Line) []Line {
	out := make([]Line, len(ls))
	copy(out, ls)
	sort.SliceStable(out, func(i, j int) bool { return lineLess(out[i], out[j]) })
	return out
}

// lineLess is the full output ordering: category, charge code, then
// source/currency/amount/description as deterministic tiebreakers.
func lineLess(a, b Line) bool {
	if a.Category != b.Category {
		return a.Category < b.Category
	}
	if a.ChargeCode != b.ChargeCode {
		return a.ChargeCode < b.ChargeCode
	}
	if a.SourceType != b.SourceType {
		return a.SourceType < b.SourceType
	}
	if a.Currency != b.Currency {
		return a.Currency < b.Currency
	}
	if c := a.OriginalAmount.Cmp(b.OriginalAmount); c != 0 {
		return c < 0
	}
	return a.Description < b.Description
}

// sortedSet drains a string-kind set into an ordered slice so no map
// iteration order can reach the Output.
func sortedSet[T ~string](set map[T]struct{}) []T {
	out := make([]T, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortedDedupe(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
