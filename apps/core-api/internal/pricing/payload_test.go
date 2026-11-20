package pricing

import (
	"bytes"
	"testing"

	"github.com/shopspring/decimal"
)

// RFC-001 §5 (pilot meeting, 2026-11-16): the customer-facing payload is
// grouped and totalled in each original currency — international
// freight, local fees, service taxes. BRL totals stay internal-only.

func findGroup(gs []GroupTotal, g PayloadGroup, ccy string) *GroupTotal {
	for i := range gs {
		if gs[i].Group == g && gs[i].Currency == ccy {
			return &gs[i]
		}
	}
	return nil
}

func requireGroup(t *testing.T, gs []GroupTotal, g PayloadGroup, ccy string) *GroupTotal {
	t.Helper()
	gt := findGroup(gs, g, ccy)
	if gt == nil {
		t.Fatalf("group total %s/%s missing in %+v", g, ccy, gs)
	}
	return gt
}

// Pass-through lines sell at cost: SellOriginal is the original amount
// verbatim — no conversion, so even the stored scale is preserved.
func TestPayload_PassThroughSellOriginalIsOriginalAmount(t *testing.T) {
	out := mustComplete(t, fobTariffInput("950.0000"))
	customs := requireLine(t, out.Lines, CategoryCustomsDispatch, "CUSTOMS_CLEARANCE")
	if customs.Group != GroupLocalFees {
		t.Fatalf("customs must map to LOCAL_FEES, got %s", customs.Group)
	}
	requireMoneyEq(t, "pass-through sell_original", customs.SellOriginal, customs.OriginalAmount)
	if customs.SellOriginal.String() != customs.OriginalAmount.String() {
		t.Fatalf("sell_original must be the original amount verbatim: %s vs %s",
			customs.SellOriginal, customs.OriginalAmount)
	}
}

// Marked-up freight converts the rounded BRL sell back at the line's
// own pinned rate: 1900 USD net x 5.508 -> 12034.98 BRL sell -> 2185 USD.
func TestPayload_MarkedUpFreightSellOriginal(t *testing.T) {
	out := mustComplete(t, fobTariffInput("950.0000"))
	freight := requireLine(t, out.Lines, CategoryIntlFreight, "OCEAN_FREIGHT")
	if freight.Group != GroupInternationalFreight {
		t.Fatalf("freight must map to INTERNATIONAL_FREIGHT, got %s", freight.Group)
	}
	requireMoneyEq(t, "sell BRL", freight.SellBRL, d("12034.98"))
	requireMoneyEq(t, "sell_original", freight.SellOriginal, d("2185.00")) // 12034.98 / 5.508
	// The converted sell re-lands on the same amount the markup expresses:
	// 1900 x 1.15 = 2185 USD.
	requireMoneyEq(t, "markup invariant", freight.SellOriginal, d("1900").Mul(d("1.15")))
}

// The USD-denominated min-markup floor lands in the freight currency:
// 1600 USD net + 800 USD floor = 2400 USD sell — the BRL floor math is
// internal only.
func TestPayload_MinMarkupFloorLandsInFreightCurrency(t *testing.T) {
	in := fobTariffInput("800.0000")
	in.MinMarkupUSD = dp("400.00")
	out := mustComplete(t, in)
	freight := requireLine(t, out.Lines, CategoryIntlFreight, "OCEAN_FREIGHT")
	requireMoneyEq(t, "floor sell BRL", freight.SellBRL, d("13219.20")) // 8812.80 + 4406.40
	requireMoneyEq(t, "floor sell_original", freight.SellOriginal, d("2400.00"))
}

// mixedCurrencyInput: USD freight + EUR surcharge + BRL local fees —
// one GroupTotal per (group, currency) is expected.
func mixedCurrencyInput() Input {
	in := fobTariffInput("950.0000")
	in.FXRates = append(in.FXRates,
		FXRate{Source: FXSourceCarrier, Currency: "EUR", Date: testQuoteDate, Rate: d("6.000000")})
	for i := range in.Tariffs {
		if in.Tariffs[i].ChargeCategory == ChargeSurcharge {
			in.Tariffs[i].Currency = "EUR"
		}
	}
	return in
}

func TestPayload_GroupsMixedCurrencies(t *testing.T) {
	out := mustComplete(t, mixedCurrencyInput())
	// Fixed group order (freight, local fees, service taxes), then
	// currency ascending inside a group: BRL before EUR.
	want := []GroupTotal{
		{Group: GroupInternationalFreight, Currency: "USD", Cost: d("1900.0000"), Sell: d("2185.00")},
		{Group: GroupLocalFees, Currency: "BRL", Cost: d("1850.0000"), Sell: d("1850.0000")},
		{Group: GroupLocalFees, Currency: "EUR", Cost: d("120.0000"), Sell: d("120.0000")},
	}
	if len(out.Groups) != len(want) {
		t.Fatalf("expected %d group totals, got %+v", len(want), out.Groups)
	}
	for i, w := range want {
		got := out.Groups[i]
		if got.Group != w.Group || got.Currency != w.Currency {
			t.Fatalf("groups[%d] = %s/%s, want %s/%s", i, got.Group, got.Currency, w.Group, w.Currency)
		}
		requireMoneyEq(t, "group cost", got.Cost, w.Cost)
		requireMoneyEq(t, "group sell", got.Sell, w.Sell)
	}
	assertGroupSumsMatchLines(t, out)
}

// assertGroupSumsMatchLines proves every GroupTotal equals the sum of
// its lines' original-currency amounts — alternatives never feed it.
func assertGroupSumsMatchLines(t *testing.T, out Output) {
	t.Helper()
	for _, gt := range out.Groups {
		var cost, sell decimal.Decimal
		for _, l := range out.Lines {
			if l.Group == gt.Group && l.Currency == gt.Currency {
				cost = cost.Add(l.OriginalAmount)
				sell = sell.Add(l.SellOriginal)
			}
		}
		requireMoneyEq(t, string(gt.Group)+"/"+gt.Currency+" cost", gt.Cost, cost)
		requireMoneyEq(t, string(gt.Group)+"/"+gt.Currency+" sell", gt.Sell, sell)
	}
}

// An INCOMPLETE output still carries Groups with the partial sums of
// the lines that were emitted (here: no terminal band, so no band line).
func TestPayload_IncompleteStillCarriesGroups(t *testing.T) {
	in := fobTariffInput("950.0000")
	in.Snapshots = nil
	out := mustPrice(t, in)
	if out.Status != StatusIncomplete {
		t.Fatalf("INCOMPLETE expected: %+v", out)
	}
	if len(out.Groups) == 0 {
		t.Fatal("partial sums must still emit: freight and local lines priced")
	}
	freight := requireGroup(t, out.Groups, GroupInternationalFreight, "USD")
	requireMoneyEq(t, "freight group cost", freight.Cost, d("1900.0000"))
	requireMoneyEq(t, "freight group sell", freight.Sell, d("2185.00"))
	assertGroupSumsMatchLines(t, out)
}

// Early exits normalize Groups to an empty slice so the serialized
// payload is deterministic — "groups":[] for EXPORT and DDP alike.
func TestPayload_EarlyExitSerializesEmptyGroups(t *testing.T) {
	for _, name := range []string{"EXPORT", "DDP"} {
		in := fobFCLInput()
		if name == "EXPORT" {
			in.Direction = DirectionExport
		} else {
			in.Incoterm = IncotermDDP
		}
		out := mustPrice(t, in)
		if out.Groups == nil || len(out.Groups) != 0 {
			t.Fatalf("%s: groups must be an empty slice, got %+v", name, out.Groups)
		}
		b, err := out.CanonicalJSON()
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		if !bytes.Contains(b, []byte(`"groups":[]`)) {
			t.Fatalf("%s: payload must serialize \"groups\":[]: %s", name, b)
		}
	}
}

// Groups is additive payload surface: NegativeMargin still compares the
// internal BRL totals only.
func TestPayload_NegativeMarginUnaffected(t *testing.T) {
	out := Output{
		TotalCostBRL:  d("1000.00"),
		FinalPriceBRL: d("999.99"),
		Groups: []GroupTotal{
			{Group: GroupInternationalFreight, Currency: "USD", Cost: d("100.00"), Sell: d("500.00")},
		},
	}
	if !out.NegativeMargin() {
		t.Fatal("sell below BRL cost must still block regardless of group sums")
	}
	out.FinalPriceBRL = d("1150.00")
	if out.NegativeMargin() {
		t.Fatal("sell above BRL cost must pass regardless of group sums")
	}
}

// CanonicalJSON stays byte-identical with the new fields in the shape,
// and the payload keys are present in the serialized output.
func TestPayload_CanonicalJSONDeterministicWithGroups(t *testing.T) {
	in := exwGoldenInput()
	a, b := mustPrice(t, in), mustPrice(t, in)
	ba, _ := a.CanonicalJSON()
	bb, _ := b.CanonicalJSON()
	if !bytes.Equal(ba, bb) {
		t.Fatalf("two runs must produce byte-identical output:\n%s\n%s", ba, bb)
	}
	for _, key := range [][]byte{
		[]byte(`"groups":[{"group":"INTERNATIONAL_FREIGHT"`),
		[]byte(`"sell_original"`),
	} {
		if !bytes.Contains(ba, key) {
			t.Fatalf("canonical payload must contain %s: %s", key, ba)
		}
	}
}
