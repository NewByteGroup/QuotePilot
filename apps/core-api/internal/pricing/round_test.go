package pricing

import (
	"testing"

	"github.com/shopspring/decimal"
)

// Task 7 (spec §5, §7): half-up cent rounding, the FCL markup formula
// with its per-container floor flag, the AIR base markup, pass-through
// sell=cost, and deterministic cent allocation that keeps
// FinalPriceBRL = sum of line sells.

func TestRoundMoney_HalfUp(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1.005", "1.01"},
		{"1.004", "1.00"},
		{"1.015", "1.02"},
		{"2.675", "2.68"},
		{"0.001", "0.00"},
	}
	for _, tc := range cases {
		if got := roundMoney(d(tc.in)); !got.Equal(d(tc.want)) {
			t.Fatalf("round %s: got %s want %s", tc.in, got, tc.want)
		}
	}
}

func TestFCLFreightSell_ProportionalMarkupWins(t *testing.T) {
	// net 10000 x 15% = 1500 > floor 200 -> sell 11500.
	sell, bound := fclFreightSell(d("10000.00"), d("0.1500"), d("200.00"))
	if !sell.Equal(d("11500.00")) {
		t.Fatalf("proportional sell: got %s", sell)
	}
	if bound {
		t.Fatal("floor must not bind when proportional markup is higher")
	}
}

func TestFCLFreightSell_FloorBindsAndFlags(t *testing.T) {
	// net 1600 x 15% = 240 < floor 1620 -> sell 3220, flagged.
	sell, bound := fclFreightSell(d("1600.00"), d("0.1500"), d("1620.00"))
	if !sell.Equal(d("3220.00")) {
		t.Fatalf("floor sell: got %s", sell)
	}
	if !bound {
		t.Fatal("floor must bind when above the proportional markup")
	}
}

func TestFCLFreightSell_EqualFloorDoesNotBind(t *testing.T) {
	sell, bound := fclFreightSell(d("1000.00"), d("0.1000"), d("100.00"))
	if !sell.Equal(d("1100.00")) {
		t.Fatalf("sell: got %s", sell)
	}
	if bound {
		t.Fatal("floor equal to proportional markup does not bind")
	}
}

func TestAirFreightSell(t *testing.T) {
	if got := airFreightSell(d("350.0000"), d("0.1500")); !got.Equal(d("402.50")) {
		t.Fatalf("air sell = net x (1+M): got %s", got)
	}
}

func TestAllocateCents_SingleShareGetsAll(t *testing.T) {
	got := allocateCents(d("11500.00"), []decimal.Decimal{d("1600")})
	if len(got) != 1 || !got[0].Equal(d("11500.00")) {
		t.Fatalf("single share: %v", got)
	}
}

func TestAllocateCents_ResidualToLargestRemainder(t *testing.T) {
	// shares: 0.0133 each -> 0.01 each = 0.03, residual +0.01.
	got := allocateCents(d("0.04"), []decimal.Decimal{d("1"), d("1"), d("1")})
	if len(got) != 3 {
		t.Fatalf("len: %v", got)
	}
	sum := decimal.Zero
	for _, s := range got {
		sum = sum.Add(s)
	}
	if !sum.Equal(d("0.04")) {
		t.Fatalf("allocation must sum to total: %v sum %s", got, sum)
	}
	// All remainders equal -> lowest index gets the cent.
	if !got[0].Equal(d("0.02")) || !got[1].Equal(d("0.01")) || !got[2].Equal(d("0.01")) {
		t.Fatalf("largest-remainder/tie rule: %v", got)
	}
}

func TestAllocateCents_NegativeResidual(t *testing.T) {
	// shares 0.0167 each -> 0.02 each = 0.06, residual -0.01: subtract
	// from the largest-remainder (first tied) line.
	got := allocateCents(d("0.05"), []decimal.Decimal{d("1"), d("1"), d("1")})
	sum := decimal.Zero
	for _, s := range got {
		sum = sum.Add(s)
	}
	if !sum.Equal(d("0.05")) {
		t.Fatalf("allocation must sum to total: %v sum %s", got, sum)
	}
	if !got[0].Equal(d("0.01")) || !got[1].Equal(d("0.02")) || !got[2].Equal(d("0.02")) {
		t.Fatalf("negative residual to largest remainder: %v", got)
	}
}

func TestAllocateCents_ProportionalSplit(t *testing.T) {
	// total 3220 across nets 2000+1200 (5:3): 2012.50 + 1207.50 exact.
	got := allocateCents(d("3220.00"), []decimal.Decimal{d("2000"), d("1200")})
	if !got[0].Equal(d("2012.50")) || !got[1].Equal(d("1207.50")) {
		t.Fatalf("proportional split: %v", got)
	}
}

func TestAllocateCents_LargestRemainderWinsCent(t *testing.T) {
	// total 100.00, weights 1:1:1 -> shares 33.3333 -> 33.33 each,
	// residual +0.01 to index 0 (all remainders equal).
	got := allocateCents(d("100.00"), []decimal.Decimal{d("1"), d("1"), d("1")})
	if !got[0].Equal(d("33.34")) || !got[1].Equal(d("33.33")) || !got[2].Equal(d("33.33")) {
		t.Fatalf("got %v", got)
	}
}

func TestAllocateCents_ZeroWeights(t *testing.T) {
	got := allocateCents(d("10.00"), []decimal.Decimal{decimal.Zero, decimal.Zero})
	sum := decimal.Zero
	for _, s := range got {
		sum = sum.Add(s)
	}
	if !sum.Equal(d("10.00")) {
		t.Fatalf("zero weights must still preserve the total: %v", got)
	}
}
