package pricing

import (
	"slices"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// Task 4 (spec §4 steps 2-5, §5): chargeable quantity, air base net,
// FCL ocean selection with the carrier offer preferred when usable
// (Q12), and OFFER_EXPIRED flagging on the tariff that replaces it.

func TestChargeableKg_PicksGreaterOfGrossAndVolumetric(t *testing.T) {
	if got := chargeableKg(d("100"), d("0.5")); !got.Equal(d("100")) {
		t.Fatalf("gross wins: got %s", got)
	}
	// 1.0 m3 x 166.67 = 166.67 > 100 kg gross.
	if got := chargeableKg(d("100"), d("1.0")); !got.Equal(d("166.67")) {
		t.Fatalf("volumetric wins: got %s", got)
	}
}

func TestAirBaseNet_MinAmountBindsBelowChargeable(t *testing.T) {
	row := tariffRow(testUUID1, testUUID2, "AIRLINE", ChargeAirFreight, "AIR_FREIGHT", "USD", "3.5000")
	row.Modal = ModalAIR
	row.MinAmount = dp("250.0000")
	// 50 kg x 3.50 = 175 < 250 min -> min wins.
	if got := airBaseNet(row, d("50")); !got.Equal(d("250.0000")) {
		t.Fatalf("min amount must bind: got %s", got)
	}
	// 100 kg x 3.50 = 350 > 250 -> per-kg wins.
	if got := airBaseNet(row, d("100")); !got.Equal(d("350.0000")) {
		t.Fatalf("chargeable product must win: got %s", got)
	}
}

func freightInput() Input {
	in := fobFCLInput()
	in.FXRates = usdRates()
	return in
}

func TestSelectFreight_FCLCheapestTariffWins(t *testing.T) {
	in := freightInput()
	rows := []TariffCandidate{
		tariffRow(testUUID1, testUUID2, "HAPAG", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "950.0000"),
		tariffRow(testUUID3, testUUID2, "MAERSK", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "800.0000"),
	}
	fxr := newFXResolver(in)
	res := selectFreight(in, rows, offerVerdict{usable: nil}, fxr)
	if len(res.winners) != 1 || !res.winners[0].net.Equal(d("1600")) {
		t.Fatalf("expected MAERSK 800x2 to win: %+v", res.winners)
	}
	if len(res.alternates) != 1 {
		t.Fatalf("losing option must be an alternative: %+v", res.alternates)
	}
}

func TestSelectFreight_OfferPreferredOverCheaperTariff(t *testing.T) {
	in := freightInput()
	in.CarrierOffer = testOffer(testUUID4, ts(2026, 11, 20, 0, 0), oceanCharge("2500.00", "40HC"))
	rows := []TariffCandidate{
		tariffRow(testUUID1, testUUID2, "MSC", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "800.0000"),
	}
	res := selectFreight(in, rows, assessOffer(in, &groups{freight: true}), newFXResolver(in))
	if len(res.winners) != 1 || res.winners[0].src != SourceCarrierOffer {
		t.Fatalf("usable offer must be preferred: %+v", res.winners)
	}
	if !res.winners[0].net.Equal(d("2500.00")) {
		t.Fatalf("offer amount must be the charge amount: %s", res.winners[0].net)
	}
	if len(res.alternates) != 1 {
		t.Fatalf("tariff must drop to alternatives: %+v", res.alternates)
	}
}

func TestAssessOffer_ExpiredIsNotUsable(t *testing.T) {
	in := freightInput()
	in.CarrierOffer = testOffer(testUUID4, ts(2026, 11, 15, 0, 0), oceanCharge("900", ""))
	v := assessOffer(in, &groups{freight: true})
	if v.offer != in.CarrierOffer || v.usable != nil || !v.expired {
		t.Fatalf("expired offer must be unusable: %+v", v)
	}
}

func TestAssessOffer_MissingOceanChargeIsNotUsable(t *testing.T) {
	in := freightInput()
	in.CarrierOffer = testOffer(testUUID4, ts(2026, 11, 20, 0, 0),
		offerCharge(ChargeOriginCharge, "THC_ORI", "300", "USD", ""))
	v := assessOffer(in, &groups{freight: true})
	if v.usable != nil {
		t.Fatalf("offer without ocean freight charge must be unusable: %+v", v)
	}
}

func TestAssessOffer_EquipmentMismatchIsNotUsable(t *testing.T) {
	in := freightInput()
	in.CarrierOffer = testOffer(testUUID4, ts(2026, 11, 20, 0, 0), oceanCharge("900", "20DV"))
	v := assessOffer(in, &groups{freight: true})
	if v.usable != nil {
		t.Fatalf("offer for another equipment must be unusable: %+v", v)
	}
}

func TestAssessOffer_GenericEquipmentMatches(t *testing.T) {
	in := freightInput()
	in.CarrierOffer = testOffer(testUUID4, ts(2026, 11, 20, 0, 0), oceanCharge("900", ""))
	v := assessOffer(in, &groups{freight: true})
	if v.usable == nil {
		t.Fatalf("equipment-agnostic offer charge must match: %+v", v)
	}
}

func TestSelectFreight_ExpiredOfferFlagsReplacementTariff(t *testing.T) {
	in := freightInput()
	in.CarrierOffer = testOffer(testUUID4, ts(2026, 11, 15, 0, 0), oceanCharge("900", ""))
	rows := []TariffCandidate{
		tariffRow(testUUID1, testUUID2, "MSC", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "800.0000"),
	}
	res := selectFreight(in, rows, assessOffer(in, &groups{freight: true}), newFXResolver(in))
	if len(res.winners) != 1 {
		t.Fatalf("tariff must replace expired offer: %+v", res)
	}
	if !slices.Contains(res.winners[0].flags, FlagOfferExpired) {
		t.Fatalf("replacement tariff line must carry OFFER_EXPIRED: %+v", res.winners[0].flags)
	}
}

func TestSelectFreight_UnpriceableOfferFallsBackToTariff(t *testing.T) {
	in := freightInput()
	in.CarrierOffer = testOffer(testUUID4, ts(2026, 11, 20, 0, 0),
		offerCharge(ChargeOceanFreight, "OCEAN_FREIGHT", "2500.00", "EUR", "40HC"))
	rows := []TariffCandidate{
		tariffRow(testUUID1, testUUID2, "MSC", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "800.0000"),
	}
	res := selectFreight(in, rows, assessOffer(in, &groups{freight: true}), newFXResolver(in))
	if !res.offerFailed || len(res.winners) != 1 || res.winners[0].src != SourcePrivateTariff {
		t.Fatalf("unpriceable offer must fall back to tariffs: %+v", res)
	}
	if slices.Contains(res.winners[0].flags, FlagOfferExpired) {
		t.Fatal("an unpriceable offer did not expire: no OFFER_EXPIRED flag")
	}
}

func TestSelectFreight_NoCandidatesIsMissing(t *testing.T) {
	in := freightInput()
	fxr := newFXResolver(in)
	res := selectFreight(in, nil, offerVerdict{}, fxr)
	if !res.missing {
		t.Fatalf("no candidates must be missing: %+v", res)
	}
}

// The freight collector applies the same fail-closed applicability
// gate (PR15): a cheaper row on the wrong route must not win.
func TestFreightTariffCands_RejectsMisplacedRows(t *testing.T) {
	in := freightInput()
	otherRoute := tariffRow(testUUID1, testUUID2, "MSC", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "400.0000")
	otherRoute.DestLOCODE = "BRPAR"
	wrongEquip := tariffRow(testUUID4, testUUID2, "MSC", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "300.0000")
	wrongEquip.EquipmentOrUnit = "20DV"
	right := tariffRow(testUUID3, testUUID2, "MSC", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "800.0000")
	fxr := newFXResolver(in)
	got := freightTariffCands(in, []TariffCandidate{otherRoute, wrongEquip, right}, fxr)
	if len(got) != 1 || !got[0].net.Equal(d("1600")) {
		t.Fatalf("only this route's row may price: %+v", got)
	}
}

func TestSelectFreight_AIRCheapestBaseNetWins(t *testing.T) {
	in := freightInput()
	in.Modal = ModalAIR
	in.ContainerCount = 0
	in.Incoterm = IncotermCPT
	a := tariffRow(testUUID1, testUUID2, "AIRLINE_A", ChargeAirFreight, "AIR_FREIGHT", "USD", "3.5000")
	a.Modal = ModalAIR
	b := tariffRow(testUUID3, testUUID2, "AIRLINE_B", ChargeAirFreight, "AIR_FREIGHT", "USD", "3.0000")
	b.Modal = ModalAIR
	fxr := newFXResolver(in)
	res := selectFreight(in, []TariffCandidate{a, b}, offerVerdict{}, fxr)
	// chargeable = max(18000, 54x166.67=9000.18) = 18000 kg; B: 54000 < A: 63000.
	if len(res.winners) != 1 || res.winners[0].tariffID == nil || *res.winners[0].tariffID != testUUID3 {
		t.Fatalf("cheapest air base net must win: %+v", res.winners)
	}
}

// The freight pool competes per charge_code like every other group:
// distinct codes are additive legs of the same freight, never dropped.
func TestSelectFreight_DistinctChargeCodesAllWin(t *testing.T) {
	in := freightInput()
	rows := []TariffCandidate{
		tariffRow(testUUID1, testUUID2, "MSC", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "950.0000"),
		tariffRow(testUUID3, testUUID2, "MAERSK", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "800.0000"),
		tariffRow(testUUID5, testUUID2, "MSC", ChargeOceanFreight, "ISPS", "USD", "50.0000"),
	}
	res := selectFreight(in, rows, offerVerdict{}, newFXResolver(in))
	if len(res.winners) != 2 {
		t.Fatalf("distinct freight codes must each win: %+v", res.winners)
	}
	// Codes sort alphabetically: ISPS (50x2=100) then OCEAN_FREIGHT.
	if res.winners[0].code != "ISPS" || !res.winners[0].net.Equal(d("100")) {
		t.Fatalf("ISPS leg missing: %+v", res.winners)
	}
	if res.winners[1].code != "OCEAN_FREIGHT" || !res.winners[1].net.Equal(d("1600")) {
		t.Fatalf("cheapest OCEAN_FREIGHT row must win: %+v", res.winners)
	}
	if len(res.alternates) != 1 || !res.alternates[0].net.Equal(d("1900")) {
		t.Fatalf("pricier same-code row must be the alternative: %+v", res.alternates)
	}
}

// A zero ExpiresAt is untrusted data: fail closed, exactly like a past
// expiry — the offer is absent and its tariff replacement is flagged.
func TestAssessOffer_ZeroExpiresAtFailsClosed(t *testing.T) {
	in := freightInput()
	in.CarrierOffer = testOffer(testUUID4, time.Time{}, oceanCharge("900", ""))
	v := assessOffer(in, &groups{freight: true})
	if !v.expired || v.usable != nil {
		t.Fatalf("a missing expiry must fail closed as expired: %+v", v)
	}
}

func TestSelectFreight_ZeroExpiryOfferFlagsReplacementTariff(t *testing.T) {
	in := freightInput()
	in.CarrierOffer = testOffer(testUUID4, time.Time{}, oceanCharge("900", ""))
	rows := []TariffCandidate{
		tariffRow(testUUID1, testUUID2, "MSC", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "800.0000"),
	}
	res := selectFreight(in, rows, assessOffer(in, &groups{freight: true}), newFXResolver(in))
	if len(res.winners) != 1 || !slices.Contains(res.winners[0].flags, FlagOfferExpired) {
		t.Fatalf("zero-expiry offer must flag its replacement OFFER_EXPIRED: %+v", res.winners)
	}
}

// Q12 strict coverage: under EXW/FCA the offer must carry origin
// charges too — freight-only is not usable and the offer is rejected
// whole rather than combined with other carriers' rows.
func TestAssessOffer_FreightOnlyUnderEXWIsNotUsable(t *testing.T) {
	in := freightInput()
	in.Incoterm = IncotermEXW
	in.CarrierOffer = testOffer(testUUID4, ts(2026, 11, 20, 0, 0), oceanCharge("2500.00", "40HC"))
	v := assessOffer(in, &groups{origin: true, freight: true})
	if v.usable != nil || !v.incomplete {
		t.Fatalf("freight-only offer must fail strict coverage under EXW: %+v", v)
	}
}

func TestAssessOffer_OriginCoverageUnderEXWIsUsable(t *testing.T) {
	in := freightInput()
	in.Incoterm = IncotermEXW
	in.CarrierOffer = testOffer(testUUID4, ts(2026, 11, 20, 0, 0),
		oceanCharge("2500.00", "40HC"),
		offerCharge(ChargeOriginCharge, "THC_ORI", "280.00", "USD", "40HC"))
	v := assessOffer(in, &groups{origin: true, freight: true})
	if v.usable == nil {
		t.Fatalf("offer covering freight+origin must be usable under EXW: %+v", v)
	}
}

// The AIR aggregate markup applies to the summed bases of every winning
// code, then splits across the lines — a second code must not lose its
// line.
func TestFreightSells_AirDistinctCodesSumAndSplit(t *testing.T) {
	in := freightInput()
	in.Modal = ModalAIR
	in.ContainerCount = 0
	a := newAssembly(in)
	winners := []candLine{
		{cat: CategoryIntlFreight, code: "AIR_FREIGHT", ccy: "USD", net: d("54000"), carrier: true, rate: d("5.508")},
		{cat: CategoryIntlFreight, code: "WAR", ccy: "USD", net: d("9000"), carrier: true, rate: d("5.508")},
	}
	weights := []decimal.Decimal{winners[0].netBRL(), winners[1].netBRL()}
	sells, _ := a.freightSells(weights, decimal.Zero)
	if len(sells) != 2 {
		t.Fatalf("two AIR codes must emit two sells: %v", sells)
	}
	// aggregate = 63000 x 5.508 = 347004; x1.15 = 399054.60; shares 6:1.
	if !sells[0].Equal(d("342046.80")) || !sells[1].Equal(d("57007.80")) {
		t.Fatalf("sells must split the marked-up aggregate: %v", sells)
	}
}

// End to end: a freight charge modeled as two distinct tariff codes
// keeps both legs, sharing one aggregate marked-up sell.
func TestPrice_FreightDistinctCodesShareAggregateSell(t *testing.T) {
	in := fobTariffInput("950.0000")
	in.Tariffs = append(in.Tariffs,
		tariffRow(testUUID3, testUUID2, "MSC", ChargeOceanFreight, "ISPS", "USD", "50.0000"))
	out := mustComplete(t, in)
	freight := freightLines(out.Lines)
	if len(freight) != 2 {
		t.Fatalf("distinct freight codes must emit separate lines: %+v", freight)
	}
	// nets: ISPS 50x2=100, OCEAN 950x2=1900 -> 2000 USD aggregate;
	// sell = 2000 x 5.508 x 1.15 = 12668.40, split 1:19.
	requireMoneyEq(t, "ISPS sell", freight[0].SellBRL, d("633.42"))
	requireMoneyEq(t, "OCEAN sell", freight[1].SellBRL, d("12034.98"))
	requireMoneyEq(t, "aggregate sell", sumSells(freight), d("12668.40"))
}

// A losing freight row's alternative sell is the whole-option total it
// would produce — replacing its code's winner while the other codes
// keep theirs — not the row priced standalone (spec §4 step 5, PR15).
func TestPrice_FreightAlternativeIsWholeOptionSell(t *testing.T) {
	in := fobTariffInput("950.0000")
	in.Tariffs = append(in.Tariffs,
		tariffRow(testUUID3, testUUID2, "MSC", ChargeOceanFreight, "ISPS", "USD", "50.0000"),
		tariffRow(testUUID4, testUUID2, "MAERSK", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "700.0000"))
	out := mustComplete(t, in)
	// Winners: ISPS 50x2 and OCEAN 700x2 (MAERSK) -> 1500 USD net.
	// The 950-row alternative = ISPS 100 + OCEAN 1900 -> 2000 USD.
	alt := requireLine(t, out.Alternatives, CategoryIntlFreight, "OCEAN_FREIGHT")
	requireMoneyEq(t, "alt cost stays the row's own", alt.CostBRL, d("10465.20")) // 1900 x 5.508
	requireMoneyEq(t, "alt whole-option sell", alt.SellBRL, d("12668.40"))        // 2000 x 5.508 x 1.15
}

// An alternative is always priced against its OWN pool's per-code
// winners: when a carrier offer wins, a losing tariff row still prices
// as the whole TARIFF option — offer lines never mix into it (Q12
// forbids the hybrid, PR15 review).
func TestPrice_FreightAlternativeUsesOwnPoolBaseline(t *testing.T) {
	in := fobTariffInput("950.0000")
	in.CarrierOffer = testOffer(testUUID4, ts(2026, 11, 20, 0, 0), oceanCharge("2500.00", "40HC"))
	in.Tariffs = append(in.Tariffs,
		tariffRow(testUUID3, testUUID2, "MSC", ChargeOceanFreight, "ISPS", "USD", "50.0000"))
	out := mustComplete(t, in)
	// The offer wins. Every tariff row's alternative is the tariff
	// pool's own option — OCEAN 950x2 + ISPS 50x2 -> 2000 USD.
	ocean := requireLine(t, out.Alternatives, CategoryIntlFreight, "OCEAN_FREIGHT")
	requireMoneyEq(t, "ocean alt cost stays the row's own", ocean.CostBRL, d("10465.20"))
	requireMoneyEq(t, "ocean alt whole-option sell", ocean.SellBRL, d("12668.40"))
	isps := requireLine(t, out.Alternatives, CategoryIntlFreight, "ISPS")
	requireMoneyEq(t, "isps alt cost", isps.CostBRL, d("550.80"))
	requireMoneyEq(t, "isps alt whole-option sell", isps.SellBRL, d("12668.40"))
}

// A bound alternative carries the floor's FX flags like the winners
// do — here the floor resolves via the PTAX fallback (no carrier USD
// rate), so FX_FALLBACK must reach the alternate too (PR15 review).
func TestPrice_FreightAlternativeBoundFloorCarriesFXFlags(t *testing.T) {
	in := fobFCLInput()
	in.MinMarkupUSD = dp("5000.00") // 5000x2x5.406 = 54060 BRL floor.
	in.FXRates = []FXRate{
		{Source: FXSourceCarrier, Currency: "EUR", Date: testQuoteDate, Rate: d("5.900000")},
		{Source: FXSourcePTAX, Currency: "USD", Date: testPrevBD, Rate: d("5.300000")},
		{Source: FXSourceCustoms, Currency: "EUR", Date: testQuoteDate, Rate: d("6.000000")},
		{Source: FXSourceCustoms, Currency: "USD", Date: testQuoteDate, Rate: d("5.350000")},
	}
	in.Tariffs = []TariffCandidate{
		tariffRow(testUUID1, testUUID2, "MSC", ChargeOceanFreight, "OCEAN_FREIGHT", "EUR", "950.0000"),
		tariffRow(testUUID3, testUUID2, "MAERSK", ChargeOceanFreight, "OCEAN_FREIGHT", "EUR", "700.0000"),
		tariffRow(testUUID5, testUUID2, "HAPAG", ChargeSurcharge, "BAF", "USD", "120.0000"),
		tariffRow(testUUID6, testUUID2, "AGENT", ChargeCustoms, "CUSTOMS_CLEARANCE", "BRL", "450.0000"),
	}
	in.Snapshots = []FeeBand{band("BRSSZ", "100000", "200000", 5, "1400.0000")}
	out := mustComplete(t, in)
	// Winner: 700x2 EUR x 6.018 (5.9 + 2% spread) = 8425.20; floor binds.
	winner := requireLine(t, out.Lines, CategoryIntlFreight, "OCEAN_FREIGHT")
	if !hasFlag(t, winner, FlagMinMarkupApplied) || !hasFlag(t, winner, FlagFXFallback) {
		t.Fatalf("bound winner must carry the floor's FX flags: %+v", winner)
	}
	// The 950-row alternative: 1900x6.018 = 11434.20 + 54060 = 65494.20.
	alt := requireLine(t, out.Alternatives, CategoryIntlFreight, "OCEAN_FREIGHT")
	requireMoneyEq(t, "bound alt sell", alt.SellBRL, d("65494.20"))
	if !hasFlag(t, alt, FlagMinMarkupApplied) || !hasFlag(t, alt, FlagFXFallback) {
		t.Fatalf("bound alternative must carry the floor's FX flags: %+v", alt)
	}
}

// Offer-charge equipment matching normalizes like LOCODEs: padding
// and case are noise, an empty equipment on the charge stays generic.
func TestOfferCharges_EquipmentNormalized(t *testing.T) {
	o := testOffer(testUUID4, ts(2026, 11, 20, 0, 0), oceanCharge("900.00", " 40hc "))
	if got := offerCharges(o, ChargeOceanFreight, "40HC"); len(got) != 1 {
		t.Fatal("equipment match must normalize case and padding")
	}
}

// An offer whose only origin charge is pre-carriage fails Q12 coverage
// under EXW/FCA: inland codes cannot satisfy the origin group (PR15).
func TestPrice_OfferOnlyPreCarriageOriginFailsCoverage(t *testing.T) {
	in := strictCoverageInput() // EXW; tariffs cover every required group.
	in.CarrierOffer = testOffer(testUUID4, ts(2026, 11, 20, 0, 0),
		oceanCharge("2500.00", "40HC"),
		offerCharge(ChargeOriginCharge, "INLAND", "300.00", "USD", "40HC"))
	out := mustComplete(t, in)
	if findLine(out.Lines, CategoryOriginCharges, "INLAND") != nil {
		t.Fatal("pre-carriage must not satisfy coverage nor price")
	}
	if !slices.Contains(out.Warnings, "carrier offer ignored: does not cover every required carrier group") {
		t.Fatalf("coverage must reject the offer: %v", out.Warnings)
	}
	freight := requireLine(t, out.Lines, CategoryIntlFreight, "OCEAN_FREIGHT")
	if freight.SourceType != SourcePrivateTariff {
		t.Fatal("the tariff path must price the rejected offer's route")
	}
}
