package pricing

import (
	"bytes"
	"slices"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// Task 8 (spec §4 order, §8, §9): orchestration edge cases, sorted and
// deduplicated collections, hash stability and run-twice determinism.

func TestPrice_Determinism_RunTwiceByteIdentical(t *testing.T) {
	in := exwGoldenInput()
	a, b := mustPrice(t, in), mustPrice(t, in)
	ba, _ := a.CanonicalJSON()
	bb, _ := b.CanonicalJSON()
	if !bytes.Equal(ba, bb) {
		t.Fatalf("two runs must produce byte-identical output:\n%s\n%s", ba, bb)
	}
}

func TestPrice_LinesSortedByCategoryThenCode(t *testing.T) {
	out := mustPrice(t, exwGoldenInput())
	prev := ""
	for _, l := range out.Lines {
		key := string(l.Category) + "|" + l.ChargeCode
		if prev != "" && key < prev {
			t.Fatalf("lines out of order: %q after %q", key, prev)
		}
		prev = key
	}
}

// expiredOfferInput: a freight offer expired before PricedAt and no
// freight tariff to replace it (spec §4 step 4, OFFER_EXPIRED).
func expiredOfferInput() Input {
	in := fobFCLInput()
	in.FXRates = usdRates()
	in.DeclaredFreight = &Money{Amount: d("1500.00"), Currency: "USD"}
	in.CarrierOffer = testOffer(testUUID4, ts(2026, 11, 15, 0, 0), oceanCharge("900", ""))
	in.Tariffs = []TariffCandidate{
		// no freight tariff to replace the expired offer
		tariffRow(testUUID5, testUUID2, "HAPAG", ChargeSurcharge, "BAF", "USD", "120.0000"),
		tariffRow(testUUID6, testUUID2, "AGENT", ChargeCustoms, "CUSTOMS_CLEARANCE", "BRL", "450.0000"),
	}
	in.Snapshots = []FeeBand{band("BRSSZ", "100000", "200000", 5, "1400.0000")}
	return in
}

func TestPrice_NoUsableOfferReason(t *testing.T) {
	out := mustPrice(t, expiredOfferInput())
	if out.Status != StatusIncomplete {
		t.Fatalf("INCOMPLETE expected: %+v", out)
	}
	if !hasReason(out, ReasonNoUsableOffer) || !hasReason(out, reasonMissingTariffGroup("INTL_FREIGHT")) {
		t.Fatalf("expected NO_USABLE_OFFER + MISSING_TARIFF_GROUP: %v", out.Reasons)
	}
	if len(out.Warnings) == 0 {
		t.Fatal("expiry must be surfaced as a warning too")
	}
}

func TestPrice_OfferUnpriceableFallsBackToTariff(t *testing.T) {
	in := fobTariffInput("950.0000")
	in.CarrierOffer = testOffer(testUUID4, ts(2026, 11, 20, 0, 0),
		offerCharge(ChargeOceanFreight, "OCEAN_FREIGHT", "2500.00", "EUR", "40HC"))
	out := mustPrice(t, in)
	if out.Status != StatusIncomplete {
		t.Fatalf("the missing EUR rate is a data gap: INCOMPLETE expected: %+v", out)
	}
	freight := requireLine(t, out.Lines, CategoryIntlFreight, "OCEAN_FREIGHT")
	if freight.SourceType != SourcePrivateTariff {
		t.Fatalf("tariff must replace the unpriceable offer: %+v", freight)
	}
	if !hasReason(out, reasonMissingFX("EUR")) {
		t.Fatalf("MISSING_FX:EUR expected: %v", out.Reasons)
	}
	if !slices.Contains(out.Warnings, "carrier offer could not be priced; tariff candidates used instead") {
		t.Fatalf("fallback must warn: %v", out.Warnings)
	}
}

func TestPrice_MissingFX(t *testing.T) {
	in := fobTariffInput("950.0000")
	in.DeclaredFreight = &Money{Amount: d("1500.00"), Currency: "USD"}
	in.FXRates = []FXRate{
		{Source: FXSourceCustoms, Currency: "USD", Date: testQuoteDate, Rate: d("5.350000")},
	} // no carrier, no PTAX -> every USD line fails
	out := mustPrice(t, in)
	if !hasReason(out, reasonMissingFX("USD")) {
		t.Fatalf("MISSING_FX:USD expected: %v", out.Reasons)
	}
	if !hasReason(out, reasonMissingTariffGroup("INTL_FREIGHT")) {
		t.Fatalf("freight group gap expected: %v", out.Reasons)
	}
	if out.Status != StatusIncomplete {
		t.Fatalf("INCOMPLETE expected: %+v", out)
	}
}

func TestPrice_MissingCargoValueForBand(t *testing.T) {
	in := fobTariffInput("950.0000")
	in.CargoValue = decimal.Zero
	in.CargoCurrency = ""
	out := mustPrice(t, in)
	if !hasReason(out, ReasonMissingCargoValue) {
		t.Fatalf("MISSING_CARGO_VALUE expected: %v", out.Reasons)
	}
}

func TestPrice_MissingTerminalBand(t *testing.T) {
	in := fobTariffInput("950.0000")
	in.Snapshots = nil
	out := mustPrice(t, in)
	if !hasReason(out, ReasonMissingTerminalBand) {
		t.Fatalf("MISSING_TERMINAL_BAND expected: %v", out.Reasons)
	}
}

// A band that matched the CIF base but whose currency cannot be
// resolved is an FX gap — MISSING_TERMINAL_BAND is only for a true
// no-match.
func TestPrice_BandMatchedButFXMissingIsNotBandGap(t *testing.T) {
	in := fobTariffInput("950.0000")
	b := band("BRSSZ", "100000", "200000", 5, "1400.0000")
	b.Currency = "EUR" // no EUR rate supplied
	in.Snapshots = []FeeBand{b}
	out := mustPrice(t, in)
	if !hasReason(out, reasonMissingFX("EUR")) {
		t.Fatalf("MISSING_FX:EUR expected: %v", out.Reasons)
	}
	if hasReason(out, ReasonMissingTerminalBand) {
		t.Fatal("a band matched the CIF base; only its FX is missing")
	}
	if out.Status != StatusIncomplete {
		t.Fatalf("INCOMPLETE expected: %+v", out)
	}
}

// A stale customs rate still applies (spec §6) but warns — no line can
// carry STALE_FX for the CIF base conversion.
func TestPrice_StaleCustomsFXWarns(t *testing.T) {
	in := fobTariffInput("950.0000")
	in.FXRates = []FXRate{
		{Source: FXSourceCarrier, Currency: "USD", Date: testQuoteDate, Rate: d("5.400000")},
		{Source: FXSourcePTAX, Currency: "USD", Date: testPrevBD, Rate: d("5.300000")},
		{Source: FXSourceCustoms, Currency: "USD", Date: day(2026, 11, 4), Rate: d("5.350000")}, // stale
	}
	out := mustComplete(t, in)
	if !slices.Contains(out.Warnings, "CUSTOMS_FX_STALE") {
		t.Fatalf("stale customs rate must warn CUSTOMS_FX_STALE: %v", out.Warnings)
	}
}

func TestPrice_MissingCustomsFX(t *testing.T) {
	in := fobTariffInput("950.0000")
	in.FXRates = []FXRate{
		{Source: FXSourceCarrier, Currency: "USD", Date: testQuoteDate, Rate: d("5.400000")},
		{Source: FXSourcePTAX, Currency: "USD", Date: testPrevBD, Rate: d("5.300000")},
	} // no CUSTOMS rate
	out := mustPrice(t, in)
	if !hasReason(out, ReasonMissingCustomsFX) {
		t.Fatalf("MISSING_CUSTOMS_FX expected: %v", out.Reasons)
	}
	if hasReason(out, ReasonMissingTerminalBand) {
		t.Fatal("the FX gap, not the band, is the reason here")
	}
}

func TestPrice_ReasonsSortedAndDeduped(t *testing.T) {
	in := fobTariffInput("950.0000")
	in.CargoValue = decimal.Zero
	in.CargoCurrency = ""
	in.FXRates = nil
	reasons := reasonsOf(t, in)
	for i := 1; i < len(reasons); i++ {
		if reasons[i] <= reasons[i-1] {
			t.Fatalf("reasons must be sorted and deduplicated: %v", reasons)
		}
	}
}

func reasonsOf(t *testing.T, in Input) []Reason {
	t.Helper()
	return mustPrice(t, in).Reasons
}

// dapInput: DAP (destination group only when requested) with a customs
// tariff, declared freight for the CIF base, and a matching band.
func dapInput() Input {
	in := fobFCLInput()
	in.Incoterm = IncotermDAP
	in.FXRates = usdRates()
	in.DeclaredFreight = &Money{Amount: d("1500.00"), Currency: "USD"}
	in.Tariffs = []TariffCandidate{
		tariffRow(testUUID6, testUUID2, "AGENT", ChargeCustoms, "CUSTOMS_CLEARANCE", "BRL", "450.0000"),
	}
	in.Snapshots = []FeeBand{band("BRSSZ", "100000", "200000", 5, "1400.0000")}
	return in
}

func TestPrice_DAPTerminalOnlyWhenRequested(t *testing.T) {
	out := mustComplete(t, dapInput())
	if len(out.Lines) != 1 {
		t.Fatalf("DAP without terminal request prices customs only: %+v", out)
	}
	in := dapInput()
	in.TerminalRequested = true
	out = mustComplete(t, in)
	if len(out.Lines) != 2 {
		t.Fatalf("DAP with terminal request adds the band: %+v", out)
	}
	if findLine(out.Lines, CategoryTerminalStorage, "TERMINAL_STORAGE") == nil {
		t.Fatal("terminal band line expected")
	}
}

func TestPrice_AirMissingFreightUsesAirParam(t *testing.T) {
	in := airFCAInput()
	in.FXRates = usdRates()
	out := mustPrice(t, in)
	if !hasReason(out, reasonMissingTariffGroup("AIR_FREIGHT")) {
		t.Fatalf("MISSING_TARIFF_GROUP:AIR_FREIGHT expected: %v", out.Reasons)
	}
}

// fcaPreCarriageInput: FCA with an out-of-MVP INLAND origin charge in
// the feed (spec §3 FCA note).
func fcaPreCarriageInput() Input {
	in := fobFCLInput()
	in.Incoterm = IncotermFCA
	in.FXRates = usdRates()
	in.Tariffs = []TariffCandidate{
		tariffRow(testUUID1, testUUID2, "MSC", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "950.0000"),
		tariffRow(testUUID3, testUUID2, "AGENT", ChargeOriginCharge, "INLAND", "USD", "300.0000"),
		tariffRow(testUUID4, testUUID2, "AGENT", ChargeOriginCharge, "THC_ORI", "USD", "250.0000"),
		tariffRow(testUUID5, testUUID2, "HAPAG", ChargeSurcharge, "BAF", "USD", "120.0000"),
		tariffRow(testUUID6, testUUID2, "AGENT", ChargeCustoms, "CUSTOMS_CLEARANCE", "BRL", "450.0000"),
	}
	in.Snapshots = []FeeBand{band("BRSSZ", "100000", "200000", 5, "1400.0000")}
	return in
}

func TestPrice_PreCarriageSkippedWithWarning(t *testing.T) {
	out := mustComplete(t, fcaPreCarriageInput())
	if findLine(out.Lines, CategoryOriginCharges, "INLAND") != nil {
		t.Fatal("pre-carriage charge must be skipped")
	}
	found := false
	for _, w := range out.Warnings {
		if w == "skipped out-of-MVP pre-carriage charge INLAND" {
			found = true
		}
	}
	if !found {
		t.Fatalf("pre-carriage skip must warn: %v", out.Warnings)
	}
}

func TestPrice_FixedFeeOnFreightGroupPassesThrough(t *testing.T) {
	in := fobTariffInput("950.0000")
	in.FixedFees = []FixedFee{
		{ChargeCode: "BOOKING_FEE", Category: CategoryIntlFreight, Amount: d("35.00"), Currency: "USD"},
	}
	out := mustPrice(t, in)
	fee := findLine(out.Lines, CategoryIntlFreight, "BOOKING_FEE")
	if fee == nil {
		t.Fatalf("freight-scoped fixed fee must emit a line: %+v", out.Lines)
	}
	requireMoneyEq(t, "fee cost", fee.CostBRL, d("185.50")) // 35 x PTAX 5.30 (org fee, not carrier-charged)
	requireMoneyEq(t, "fee sells at cost", fee.SellBRL, d("185.50"))
	if fee.SourceType != SourceOrgFixedRule {
		t.Fatalf("source: %+v", fee)
	}
}

func TestPrice_StaleFXEndToEnd(t *testing.T) {
	in := fobTariffInput("950.0000")
	in.FXRates = []FXRate{
		{Source: FXSourcePTAX, Currency: "USD", Date: day(2026, 11, 4), Rate: d("5.100000")},
		{Source: FXSourceCustoms, Currency: "USD", Date: testQuoteDate, Rate: d("5.350000")},
	}
	out := mustPrice(t, in)
	freight := findLine(out.Lines, CategoryIntlFreight, "OCEAN_FREIGHT")
	if !hasFlag(t, freight, FlagStaleFX) || !hasFlag(t, freight, FlagFXFallback) {
		t.Fatalf("stale PTAX fallback must flag STALE_FX+FX_FALLBACK: %+v", freight.Flags)
	}
}

func TestHashInput_StableAndSensitive(t *testing.T) {
	in := fobFCLInput()
	h1 := hashInput(in)
	h2 := hashInput(in)
	if h1 != h2 {
		t.Fatal("same input must hash identically")
	}
	if len(h1) != 64 {
		t.Fatalf("sha256 hex must be 64 chars: %q", h1)
	}
	in.MarkupRate = d("0.1501")
	if hashInput(in) == h1 {
		t.Fatal("a changed field must change the hash")
	}
}

func TestHashInput_FixedScaleRendersEquivalentDecimalsSame(t *testing.T) {
	in := fobFCLInput()
	in.MarkupRate = d("0.15")
	a := hashInput(in)
	in.MarkupRate = d("0.1500")
	b := hashInput(in)
	if a != b {
		t.Fatal("0.15 and 0.1500 are the same percentage and must hash equally")
	}
}

func TestHashInput_SliceOrderMatters(t *testing.T) {
	in := fobFCLInput()
	r1 := tariffRow(testUUID1, testUUID2, "A", ChargeCustoms, "X", "BRL", "1.0000")
	r2 := tariffRow(testUUID3, testUUID2, "B", ChargeCustoms, "Y", "BRL", "2.0000")
	in.Tariffs = []TariffCandidate{r1, r2}
	a := hashInput(in)
	in.Tariffs = []TariffCandidate{r2, r1}
	b := hashInput(in)
	if a == b {
		t.Fatal("slices hash in input order")
	}
}

func TestHashInput_SubSecondPrecisionMatters(t *testing.T) {
	in := fobFCLInput()
	a := hashInput(in)
	in.PricedAt = in.PricedAt.Add(time.Nanosecond)
	if hashInput(in) == a {
		t.Fatal("sub-second timestamp differences must change the hash")
	}
}

// strictCoverageInput: EXW with an offer carrying freight only — under
// Q12 strict coverage it is not usable — while tariffs cover every
// required group.
func strictCoverageInput() Input {
	in := fobFCLInput()
	in.Incoterm = IncotermEXW
	in.FXRates = usdRates()
	in.CarrierOffer = testOffer(testUUID4, ts(2026, 11, 20, 0, 0), oceanCharge("2500.00", "40HC"))
	in.Tariffs = []TariffCandidate{
		tariffRow(testUUID1, testUUID2, "MSC", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "950.0000"),
		tariffRow(testUUID3, testUUID2, "AGENT", ChargeOriginCharge, "THC_ORI", "USD", "250.0000"),
		tariffRow(testUUID5, testUUID2, "HAPAG", ChargeSurcharge, "BAF", "USD", "120.0000"),
		tariffRow(testUUID6, testUUID2, "AGENT", ChargeCustoms, "CUSTOMS_CLEARANCE", "BRL", "450.0000"),
	}
	in.Snapshots = []FeeBand{band("BRSSZ", "100000", "200000", 5, "1400.0000")}
	return in
}

// Strict coverage: the rejected offer falls back to the tariff path —
// a warning and the OFFER_STRICT_COVERAGE assumption record it, but no
// reason fires while every required group is priced.
func TestPrice_StrictCoverageOfferFallsBackToTariffs(t *testing.T) {
	out := mustComplete(t, strictCoverageInput())
	freight := requireLine(t, out.Lines, CategoryIntlFreight, "OCEAN_FREIGHT")
	if freight.SourceType != SourcePrivateTariff {
		t.Fatalf("tariff must replace the coverage-rejected offer: %+v", freight)
	}
	if !hasAssumption(out, AssumptionOfferStrictCoverage) {
		t.Fatalf("OFFER_STRICT_COVERAGE expected: %v", out.Assumptions)
	}
	if hasReason(out, ReasonNoUsableOffer) {
		t.Fatal("every group priced: NO_USABLE_OFFER must not fire")
	}
	if !slices.Contains(out.Warnings, "carrier offer ignored: does not cover every required carrier group") {
		t.Fatalf("the rejected offer must warn: %v", out.Warnings)
	}
}

// When the same rejected offer leaves a required group it could have
// covered unpriced, NO_USABLE_OFFER names the context next to the
// group's own MISSING_TARIFF_GROUP.
func TestPrice_StrictCoverageMissingOriginRaisesNoUsableOffer(t *testing.T) {
	in := strictCoverageInput()
	rows := make([]TariffCandidate, 0, len(in.Tariffs))
	for _, r := range in.Tariffs {
		if r.ChargeCategory != ChargeOriginCharge {
			rows = append(rows, r)
		}
	}
	in.Tariffs = rows
	out := mustPrice(t, in)
	if out.Status != StatusIncomplete {
		t.Fatalf("INCOMPLETE expected: %+v", out)
	}
	if !hasReason(out, reasonMissingTariffGroup("ORIGIN_CHARGES")) {
		t.Fatalf("MISSING_TARIFF_GROUP:ORIGIN_CHARGES expected: %v", out.Reasons)
	}
	if !hasReason(out, ReasonNoUsableOffer) {
		t.Fatalf("NO_USABLE_OFFER expected for the rejected offer: %v", out.Reasons)
	}
}

// The surcharge pool is never required: an FOB quote with no surcharge
// candidates still completes.
func TestPrice_EmptySurchargePoolIsNotAGap(t *testing.T) {
	in := fobTariffInput("950.0000")
	rows := make([]TariffCandidate, 0, len(in.Tariffs))
	for _, r := range in.Tariffs {
		if r.ChargeCategory != ChargeSurcharge {
			rows = append(rows, r)
		}
	}
	in.Tariffs = rows
	out := mustComplete(t, in)
	if findLine(out.Lines, CategorySurcharge, "BAF") != nil {
		t.Fatal("no surcharge candidates: no surcharge line")
	}
	if hasReason(out, reasonMissingTariffGroup("SURCHARGE")) {
		t.Fatal("an empty surcharge pool is never a MISSING_TARIFF_GROUP")
	}
}

// A zero min-markup floor can never bind, so it must not resolve USD:
// an all-BRL FCL quote completes with no FX rates at all (PR15).
func TestPrice_ZeroMinMarkupFloorNeedsNoUSDRate(t *testing.T) {
	in := fobFCLInput()
	in.CargoCurrency = "BRL"
	in.MinMarkupUSD = dp("0.00")
	in.FXRates = nil
	in.Tariffs = []TariffCandidate{
		tariffRow(testUUID1, testUUID2, "MSC", ChargeOceanFreight, "OCEAN_FREIGHT", "BRL", "950.0000"),
		tariffRow(testUUID6, testUUID2, "AGENT", ChargeCustoms, "CUSTOMS_CLEARANCE", "BRL", "450.0000"),
	}
	in.Snapshots = []FeeBand{band("BRSSZ", "0", "30000", 5, "1400.0000")}
	out := mustComplete(t, in)
	if hasReason(out, reasonMissingFX("USD")) {
		t.Fatal("a zero floor must not resolve USD")
	}
	freight := requireLine(t, out.Lines, CategoryIntlFreight, "OCEAN_FREIGHT")
	requireMoneyEq(t, "sell", freight.SellBRL, d("2185.00")) // 1900 x 1.15
}
