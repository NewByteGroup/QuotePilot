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

// toLine renders a priced candidate into an audit Line: payload group,
// source pin, original currency amount and sell, applied rate, rounded
// BRL cost and sell, flags sorted and deduplicated. sellOriginal is the
// line's already-computed original-currency sell — the caller picks the
// rule (pass-through or marked-up) below.
func toLine(c candLine, sellBRL, sellOriginal decimal.Decimal) Line {
	return Line{
		Category:         c.cat,
		Group:            payloadGroup(c.cat),
		ChargeCode:       c.code,
		Description:      c.desc,
		SourceType:       c.src,
		PrivateTariffID:  c.tariffID,
		PublicSnapshotID: c.snapID,
		CarrierOfferID:   c.offerID,
		Currency:         c.ccy,
		OriginalAmount:   roundMoney(c.net),
		SellOriginal:     sellOriginal,
		FXRateUsed:       c.rate,
		CostBRL:          roundMoney(c.netBRL()),
		SellBRL:          sellBRL,
		Flags:            sortedDedupe(c.flags),
	}
}

// passThroughSellOriginal is a pass-through line's original-currency
// sell: it sells at cost (spec §5), so the displayed sell is the
// original amount — the same minor-unit figure as Line.OriginalAmount.
// Both display amounts round the candidate net to minor units; the
// unrounded net stays in the candidate for the aggregate math.
func passThroughSellOriginal(c candLine) decimal.Decimal {
	return roundMoney(c.net)
}

// markedUpSellOriginal converts one marked-up sell back at the line's
// own pinned rate (RFC-001 §5) — for whole-option alternates, where a
// single line carries the option's sell so no cent allocation is
// needed. The markup is computed in the internal BRL space for
// determinism and the margin check; dividing the rounded BRL sell by
// the same rate expresses the same markup in the customer currency —
// the percentage is invariant, and the USD-denominated floor correctly
// lands as a freight-currency amount because the floor IS USD.
func markedUpSellOriginal(c candLine, sellBRL decimal.Decimal) decimal.Decimal {
	return roundMoney(sellBRL.Div(c.rate))
}

// originalSells converts the marked-up winners' BRL sells into
// original-currency sells with the same aggregate-then-weights cent
// allocation the BRL totals use (spec §7): per currency subgroup the
// rounded sum of the unrounded original sells is the subtotal,
// allocated back over those unrounded shares. The displayed lines then
// sum exactly in the customer-visible space — an independent per-line
// round could lose or gain a cent against the rounded aggregate. The
// BRL allocation stays canonical: it feeds the internal margin check.
func originalSells(winners []candLine, sellsBRL []decimal.Decimal) []decimal.Decimal {
	raw := make([]decimal.Decimal, len(winners))
	for i, c := range winners {
		raw[i] = sellsBRL[i].Div(c.rate)
	}
	out := make([]decimal.Decimal, len(winners))
	for _, idxs := range currencySubgroups(winners) {
		weights := make([]decimal.Decimal, len(idxs))
		for j, i := range idxs {
			weights[j] = raw[i]
		}
		shares := allocateCents(roundMoney(sum(weights)), weights)
		for j, i := range idxs {
			out[i] = shares[j]
		}
	}
	return out
}

// currencySubgroups groups winner indexes by line currency in
// first-encounter order — deterministic because the winners slice order
// is fixed before this runs; the map is lookup-only.
func currencySubgroups(winners []candLine) [][]int {
	pos := make(map[string]int, len(winners))
	var groups [][]int
	for i, c := range winners {
		if g, ok := pos[c.ccy]; ok {
			groups[g] = append(groups[g], i)
			continue
		}
		pos[c.ccy] = len(groups)
		groups = append(groups, []int{i})
	}
	return groups
}

// groupTotals sums the original-currency cost and sell per (payload
// group, currency) over the emitted lines — the customer-facing totals
// (RFC-001 §5). Alternatives never contribute. The pairs accumulate in
// a map for lookup only; emission sorts by the fixed group order then
// currency ascending, so no map order reaches the Output.
func groupTotals(lines []Line) []GroupTotal {
	type key struct {
		group PayloadGroup
		ccy   string
	}
	sums := make(map[key]GroupTotal)
	keys := make([]key, 0, len(lines))
	for _, l := range lines {
		k := key{l.Group, l.Currency}
		if _, seen := sums[k]; !seen {
			keys = append(keys, k)
		}
		t := sums[k]
		t.Cost = t.Cost.Add(l.OriginalAmount)
		t.Sell = t.Sell.Add(l.SellOriginal)
		sums[k] = t
	}
	sort.Slice(keys, func(i, j int) bool {
		gi, gj := groupOrder(keys[i].group), groupOrder(keys[j].group)
		if gi != gj {
			return gi < gj
		}
		return keys[i].ccy < keys[j].ccy
	})
	out := make([]GroupTotal, 0, len(keys))
	for _, k := range keys {
		t := sums[k]
		out = append(out, GroupTotal{Group: k.group, Currency: k.ccy, Cost: t.Cost, Sell: t.Sell})
	}
	return out
}

// groupOrder is the fixed customer-facing group order: international
// freight, local fees, service taxes.
func groupOrder(g PayloadGroup) int {
	switch g {
	case GroupInternationalFreight:
		return 0
	case GroupLocalFees:
		return 1
	default:
		return 2
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
