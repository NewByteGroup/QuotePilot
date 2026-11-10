package pricing

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// Task 6 (spec §6): rate lookup by (source, currency, date) — carrier
// rates take the org spread, PTAX fallback flags FX_FALLBACK, the
// customs rate is required for the goods value, and rates older than
// one business day are flagged STALE_FX.

func TestResolve_CarrierRateWithSpread(t *testing.T) {
	in := freightInput()
	fxr := newFXResolver(in)
	rate, flags, ok := fxr.resolve(true, "USD")
	if !ok {
		t.Fatal("carrier USD rate must resolve")
	}
	// 5.40 x 1.02 = 5.508
	if !rate.Equal(d("5.508")) {
		t.Fatalf("carrier rate with spread: got %s", rate)
	}
	if len(flags) != 0 {
		t.Fatalf("no flags expected on a fresh carrier rate: %v", flags)
	}
}

func TestResolve_CarrierFallbackToPTAXWithSpread(t *testing.T) {
	in := freightInput()
	in.FXRates = []FXRate{
		{Source: FXSourcePTAX, Currency: "USD", Date: testPrevBD, Rate: d("5.300000")},
	}
	fxr := newFXResolver(in)
	rate, flags, ok := fxr.resolve(true, "USD")
	if !ok {
		t.Fatal("PTAX fallback must resolve")
	}
	// 5.30 x 1.02 = 5.406
	if !rate.Equal(d("5.406")) {
		t.Fatalf("PTAX fallback with spread: got %s", rate)
	}
	if len(flags) != 1 || flags[0] != FlagFXFallback {
		t.Fatalf("FX_FALLBACK flag expected: %v", flags)
	}
}

func TestResolve_NonCarrierUsesPTAXWithoutSpread(t *testing.T) {
	in := freightInput()
	fxr := newFXResolver(in)
	rate, flags, ok := fxr.resolve(false, "USD")
	if !ok {
		t.Fatal("PTAX rate must resolve")
	}
	if !rate.Equal(d("5.300000")) {
		t.Fatalf("non-carrier must use raw PTAX (no spread): got %s", rate)
	}
	if len(flags) != 0 {
		t.Fatalf("no flags expected: %v", flags)
	}
}

func TestResolve_BRLSkipsLookup(t *testing.T) {
	in := freightInput()
	in.FXRates = nil
	fxr := newFXResolver(in)
	rate, _, ok := fxr.resolve(true, "BRL")
	if !ok || !rate.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("BRL must short-circuit to rate 1: %s ok=%v", rate, ok)
	}
	if len(fxr.applied) != 0 {
		t.Fatalf("BRL must not record an applied rate: %+v", fxr.applied)
	}
}

func TestResolve_MissingCurrencyRecorded(t *testing.T) {
	in := freightInput()
	in.FXRates = nil
	fxr := newFXResolver(in)
	if _, _, ok := fxr.resolve(false, "EUR"); ok {
		t.Fatal("no EUR rate must fail resolution")
	}
	miss := fxr.missingCurrencies()
	if len(miss) != 1 || miss[0] != "EUR" {
		t.Fatalf("EUR must be recorded missing: %v", miss)
	}
}

func TestResolve_StaleRateFlagged(t *testing.T) {
	in := freightInput()
	// PTAX dated Wednesday 2026-11-04; quote date Monday 2026-11-16.
	// Eight business days later -> far past one -> STALE_FX.
	in.FXRates = []FXRate{
		{Source: FXSourcePTAX, Currency: "USD", Date: day(2026, 11, 4), Rate: d("5.100000")},
	}
	fxr := newFXResolver(in)
	_, flags, ok := fxr.resolve(false, "USD")
	if !ok {
		t.Fatal("stale rate must still resolve")
	}
	if len(flags) != 1 || flags[0] != FlagStaleFX {
		t.Fatalf("STALE_FX expected: %v", flags)
	}
}

func TestResolve_PrevBusinessDayRateIsNeverStale(t *testing.T) {
	in := freightInput()
	fxr := newFXResolver(in)
	_, flags, ok := fxr.resolve(false, "USD")
	if !ok || len(flags) != 0 {
		t.Fatalf("prev-business-day rate must not be stale: %v", flags)
	}
}

func TestResolve_LatestAtOrBelowTargetSkipsFuture(t *testing.T) {
	in := freightInput()
	in.FXRates = []FXRate{
		{Source: FXSourcePTAX, Currency: "USD", Date: day(2026, 11, 11), Rate: d("5.100000")}, // too old
		{Source: FXSourcePTAX, Currency: "USD", Date: testPrevBD, Rate: d("5.300000")},        // prev biz day
		{Source: FXSourcePTAX, Currency: "USD", Date: day(2026, 11, 20), Rate: d("9.900000")}, // future
	}
	fxr := newFXResolver(in)
	rate, _, ok := fxr.resolve(false, "USD")
	if !ok || !rate.Equal(d("5.300000")) {
		t.Fatalf("newest rate at or below prev business day must win: %s", rate)
	}
}

func TestCustoms_ResolvesAndMarksUsed(t *testing.T) {
	in := freightInput()
	fxr := newFXResolver(in)
	rate, flags, ok := fxr.customs("USD")
	if !ok || !rate.Equal(d("5.350000")) {
		t.Fatalf("customs rate: got %s", rate)
	}
	if !fxr.customsUsed {
		t.Fatal("customs usage must be tracked for CUSTOMS_RATE_UNCONFIRMED")
	}
	if len(flags) != 0 {
		t.Fatalf("fresh customs rate: %v", flags)
	}
}

func TestCustoms_MissingIsDedicatedGap(t *testing.T) {
	in := freightInput()
	in.FXRates = nil
	fxr := newFXResolver(in)
	if _, _, ok := fxr.customs("USD"); ok {
		t.Fatal("missing customs rate must fail")
	}
	if !fxr.customsMiss {
		t.Fatal("missing customs rate must feed MISSING_CUSTOMS_FX")
	}
}

func TestCustoms_BRLSkipsLookup(t *testing.T) {
	in := freightInput()
	in.FXRates = nil
	fxr := newFXResolver(in)
	rate, _, ok := fxr.customs("BRL")
	if !ok || !rate.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("BRL customs must be 1: %s", rate)
	}
}

func TestPrevBusinessDay(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
		want time.Time
	}{
		{"monday->friday", day(2026, 11, 16), day(2026, 11, 13)},
		{"sunday->friday", day(2026, 11, 15), day(2026, 11, 13)},
		{"saturday->friday", day(2026, 11, 14), day(2026, 11, 13)},
		{"wednesday->tuesday", day(2026, 11, 18), day(2026, 11, 17)},
	}
	for _, tc := range cases {
		if got := prevBusinessDay(tc.in); !got.Equal(tc.want) {
			t.Fatalf("%s: got %s want %s", tc.name, got, tc.want)
		}
	}
}

func TestBusinessDaysBetween(t *testing.T) {
	// Fri->Mon counts Monday only: 1.
	if got := businessDaysBetween(day(2026, 11, 13), day(2026, 11, 16)); got != 1 {
		t.Fatalf("Fri->Mon: got %d", got)
	}
	// Wed 2026-11-04 -> Mon 2026-11-16: 8 business days later.
	if got := businessDaysBetween(day(2026, 11, 4), day(2026, 11, 16)); got != 8 {
		t.Fatalf("Wed->Mon: got %d", got)
	}
	if got := businessDaysBetween(testQuoteDate, testQuoteDate); got != 0 {
		t.Fatalf("same day: got %d", got)
	}
}

func TestAppliedRates_DedupedAndSorted(t *testing.T) {
	in := freightInput()
	fxr := newFXResolver(in)
	fxr.resolve(true, "USD")  // carrier+spread
	fxr.resolve(true, "USD")  // dedupe
	fxr.resolve(false, "USD") // PTAX no spread
	got := fxr.appliedRates()
	if len(got) != 2 {
		t.Fatalf("carrier+PTAX applications dedupe to 2: %+v", got)
	}
	if got[0].Source != FXSourceCarrier || got[1].Source != FXSourcePTAX {
		t.Fatalf("applied rates must sort by source: %+v", got)
	}
	if !got[0].Spread.Equal(d("0.0200")) || !got[1].Spread.IsZero() {
		t.Fatalf("spread must be recorded per entry: %+v", got)
	}
}
