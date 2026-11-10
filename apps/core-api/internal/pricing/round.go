package pricing

import (
	"sort"

	"github.com/shopspring/decimal"
)

// cent is one BRL cent; the residual allocator moves whole cents only.
var cent = decimal.RequireFromString("0.01")

// roundMoney rounds half-up to the cent (spec §7; shopspring's Round
// is half-up away from zero — banker's rounding is explicitly rejected
// by Q11).
func roundMoney(d decimal.Decimal) decimal.Decimal {
	return d.Round(2)
}

// fclFreightSell implements spec §5 FCL:
// sell = Net_Ocean + max(Net_Ocean x MarkupRate, floorBRL) where
// floorBRL is min_markup_usd x containers converted to BRL (a NULL
// min_markup_usd means floor 0). Computed on the unrounded aggregate so
// the sell total splits exactly afterwards; the result is rounded once.
// floorBound reports MIN_MARKUP_APPLIED: the floor strictly exceeds the
// proportional markup.
func fclFreightSell(netBRL, markupRate, floorBRL decimal.Decimal) (decimal.Decimal, bool) {
	proportional := netBRL.Mul(markupRate)
	markup := proportional
	floorBound := floorBRL.GreaterThan(proportional)
	if floorBound {
		markup = floorBRL
	}
	return roundMoney(netBRL.Add(markup)), floorBound
}

// airFreightSell implements spec §5 AIR: sell = base net x (1 + M).
func airFreightSell(netBRL, markupRate decimal.Decimal) decimal.Decimal {
	return roundMoney(netBRL.Mul(decimal.NewFromInt(1).Add(markupRate)))
}

// allocateCents distributes an already-rounded aggregate across parts
// proportional to their weights (spec §7): each share rounds half-up,
// then residual cents go to the largest absolute remainder, ties to the
// lowest index. The result always sums to total.
func allocateCents(total decimal.Decimal, weights []decimal.Decimal) []decimal.Decimal {
	shares := rawShares(total, weights)
	rounded := roundShares(shares)
	residual := total.Sub(sum(rounded))
	applyResidual(rounded, shares, residual)
	return rounded
}

// rawShares is each part's unrounded proportional share of total.
func rawShares(total decimal.Decimal, weights []decimal.Decimal) []decimal.Decimal {
	sumW := decimal.Zero
	for _, w := range weights {
		sumW = sumW.Add(w)
	}
	shares := make([]decimal.Decimal, len(weights))
	if sumW.IsZero() {
		if len(weights) > 0 {
			shares[0] = total // degenerate: no basis, keep the total whole
		}
		return shares
	}
	for i, w := range weights {
		shares[i] = total.Mul(w).Div(sumW)
	}
	return shares
}

func roundShares(shares []decimal.Decimal) []decimal.Decimal {
	out := make([]decimal.Decimal, len(shares))
	for i, s := range shares {
		out[i] = roundMoney(s)
	}
	return out
}

func sum(ds []decimal.Decimal) decimal.Decimal {
	t := decimal.Zero
	for _, d := range ds {
		t = t.Add(d)
	}
	return t
}

// applyResidual moves the residual cent-by-cent onto the parts ordered
// by largest absolute remainder (ties: lowest index), spec §7.
func applyResidual(rounded, shares []decimal.Decimal, residual decimal.Decimal) {
	order := remainderOrder(rounded, shares)
	step := cent
	if residual.IsNegative() {
		step = cent.Neg()
	}
	i := 0
	for !residual.IsZero() && i < len(order)*2 {
		idx := order[i%len(order)]
		rounded[idx] = rounded[idx].Add(step)
		residual = residual.Sub(step)
		i++
	}
}

// remainderOrder sorts part indexes by descending absolute remainder
// (share minus its rounding), ties by lowest index first.
func remainderOrder(rounded, shares []decimal.Decimal) []int {
	idx := make([]int, len(shares))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		ra := shares[idx[a]].Sub(rounded[idx[a]]).Abs()
		rb := shares[idx[b]].Sub(rounded[idx[b]]).Abs()
		if c := ra.Cmp(rb); c != 0 {
			return c > 0
		}
		return idx[a] < idx[b]
	})
	return idx
}
