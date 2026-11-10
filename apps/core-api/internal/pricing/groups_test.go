package pricing

import (
	"slices"
	"testing"

	"github.com/shopspring/decimal"
)

// Task 5 (spec §4 step 5): required groups collect resolved tariff rows
// and scoped fixed fees; inside a group the same charge_code is an
// either/or (cheapest net wins, losers are Alternatives) while distinct
// codes are additive.

func TestCollectGroup_SameCodeCompetesDistinctCodesAdd(t *testing.T) {
	in := freightInput()
	rows := []TariffCandidate{
		tariffRow(testUUID1, testUUID2, "AGENT_A", ChargeOriginCharge, "THC_ORI", "USD", "300.0000"),
		tariffRow(testUUID3, testUUID2, "AGENT_B", ChargeOriginCharge, "THC_ORI", "USD", "250.0000"),
		tariffRow(testUUID4, testUUID2, "AGENT_A", ChargeOriginCharge, "DOC_FEE", "USD", "80.0000"),
	}
	fxr := newFXResolver(in)
	cands := tariffCandsFor(ChargeOriginCharge, CategoryOriginCharges, in, rows, fxr)
	res := collectGroup(cands)
	if len(res.winners) != 2 {
		t.Fatalf("distinct codes must both emit lines: %+v", res.winners)
	}
	if len(res.alternates) != 1 || !res.alternates[0].net.Equal(d("300.0000")) {
		t.Fatalf("pricier same-code row must be the alternative: %+v", res.alternates)
	}
	for _, w := range res.winners {
		if w.code == "THC_ORI" && !w.net.Equal(d("250.0000")) {
			t.Fatalf("cheapest same-code row must win: %+v", w)
		}
	}
}

func TestTariffCandsFor_IgnoresOtherCategories(t *testing.T) {
	in := freightInput()
	rows := []TariffCandidate{
		tariffRow(testUUID1, testUUID2, "HAPAG", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "900.0000"),
		tariffRow(testUUID3, testUUID2, "AGENT", ChargeCustoms, "CUSTOMS_CLEARANCE", "BRL", "450.0000"),
	}
	fxr := newFXResolver(in)
	got := tariffCandsFor(ChargeCustoms, CategoryCustomsDispatch, in, rows, fxr)
	if len(got) != 1 || got[0].code != "CUSTOMS_CLEARANCE" {
		t.Fatalf("category filter failed: %+v", got)
	}
}

func TestFixedFeeCands_ScopedByLineCategory(t *testing.T) {
	in := freightInput()
	in.FixedFees = []FixedFee{
		{ChargeCode: "HBL_FEE", Description: "house BL", Category: CategoryOriginCharges, Amount: d("60.00"), Currency: "USD"},
		{ChargeCode: "DOC", Description: "docs", Category: CategoryCustomsDispatch, Amount: d("120.00"), Currency: "BRL"},
	}
	fxr := newFXResolver(in)
	origin := fixedFeeCands(CategoryOriginCharges, in.FixedFees, fxr)
	customs := fixedFeeCands(CategoryCustomsDispatch, in.FixedFees, fxr)
	if len(origin) != 1 || origin[0].code != "HBL_FEE" || origin[0].src != SourceOrgFixedRule {
		t.Fatalf("origin fixed fee not scoped: %+v", origin)
	}
	if len(customs) != 1 || customs[0].code != "DOC" {
		t.Fatalf("customs fixed fee not scoped: %+v", customs)
	}
	// BRL fee: rate 1, no FX lookup.
	if !customs[0].rate.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("BRL fixed fee must use rate 1: %s", customs[0].rate)
	}
}

func TestOfferChargesJoinGroupPools(t *testing.T) {
	in := freightInput()
	offer := &CarrierOffer{
		ID: testUUID4, Carrier: "HAPAG",
		Charges: []OfferCharge{
			{Category: ChargeOriginCharge, Code: "THC_ORI", Amount: d("280.00"), Currency: "USD", EquipmentOrUnit: "40HC"},
			{Category: ChargeSurcharge, Code: "BAF", Amount: d("150.00"), Currency: "USD"},
		},
		ExpiresAt: ts(2026, 11, 20, 0, 0),
	}
	fxr := newFXResolver(in)
	origin := offerCandsFor(ChargeOriginCharge, CategoryOriginCharges, offer, in, fxr)
	surch := offerCandsFor(ChargeSurcharge, CategorySurcharge, offer, in, fxr)
	if len(origin) != 1 || origin[0].src != SourceCarrierOffer {
		t.Fatalf("offer origin charge must join pool: %+v", origin)
	}
	if len(surch) != 1 || !surch[0].carrier {
		t.Fatalf("offer surcharge must be carrier-charged: %+v", surch)
	}
}

func TestIsPreCarriage(t *testing.T) {
	for _, code := range []string{"INLAND", "INLAND_HAULAGE", "PRE_CARRIAGE", "inland haulage", "PRE-CARRIAGE"} {
		if !isPreCarriage(code) {
			t.Fatalf("%q must be detected as pre-carriage", code)
		}
	}
	// INLAND must be a whole token: MAINLAND is not a pre-carriage code.
	if isPreCarriage("MAINLAND_FEE") || isPreCarriage("THC_ORI") || isPreCarriage("OCEAN_FREIGHT") {
		t.Fatal("ordinary codes must not be skipped")
	}
}

// The worker's candidate filters are untrusted (rule 6): a row stamped
// for another modal must never price into this quote.
func TestTariffCandsFor_RejectsMismatchedModal(t *testing.T) {
	in := freightInput() // FCL
	origin := tariffRow(testUUID1, testUUID2, "AGENT", ChargeOriginCharge, "THC_ORI", "USD", "250.0000")
	origin.Modal = ModalAIR
	fxr := newFXResolver(in)
	if got := tariffCandsFor(ChargeOriginCharge, CategoryOriginCharges, in, []TariffCandidate{origin}, fxr); len(got) != 0 {
		t.Fatalf("mismatched modal must be rejected: %+v", got)
	}
	freight := tariffRow(testUUID3, testUUID2, "MSC", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "900.0000")
	freight.Modal = ModalAIR
	if got := freightTariffCands(in, []TariffCandidate{freight}, fxr); len(got) != 0 {
		t.Fatalf("mismatched modal must be rejected in freight too: %+v", got)
	}
}

// A misplaced-but-valid row — another route or equipment — must never
// become a payable line (rule 6, PR15): collectors re-verify the
// worker's route/equipment filters, not just the modal.
func TestTariffCandsFor_RejectsMisplacedRows(t *testing.T) {
	in := freightInput()
	wrongRoute := tariffRow(testUUID1, testUUID2, "AGENT", ChargeCustoms, "CUSTOMS_CLEARANCE", "BRL", "450.0000")
	wrongRoute.OriginLOCODE = "DEHAM"
	wrongEquip := tariffRow(testUUID3, testUUID2, "AGENT", ChargeCustoms, "DOC_FEE", "BRL", "80.0000")
	wrongEquip.EquipmentOrUnit = "20DV"
	right := tariffRow(testUUID5, testUUID2, "AGENT", ChargeCustoms, "CUSTOMS_CLEARANCE", "BRL", "460.0000")
	fxr := newFXResolver(in)
	got := tariffCandsFor(ChargeCustoms, CategoryCustomsDispatch, in,
		[]TariffCandidate{wrongRoute, wrongEquip, right}, fxr)
	if len(got) != 1 || !got[0].net.Equal(d("460.0000")) {
		t.Fatalf("only the exact-route/equipment row may price: %+v", got)
	}
}

func band(loc, cifMin, cifMax string, days int, amount string) FeeBand {
	return FeeBand{
		SnapshotID: testUUID3, Location: loc,
		CIFMin: d(cifMin), CIFMax: d(cifMax),
		PeriodDays: days, Amount: d(amount), Currency: "BRL",
	}
}

func TestBandCands_PicksMatchingBand(t *testing.T) {
	in := freightInput()
	in.Snapshots = []FeeBand{
		band("BRSSZ", "0", "50000", 5, "900.0000"),
		band("BRSSZ", "50000", "100000", 5, "1400.0000"),
	}
	fxr := newFXResolver(in)
	c, outcome := bandCand(in, d("75000.00"), fxr)
	if outcome != bandPriced || !c.net.Equal(d("1400.0000")) {
		t.Fatalf("CIF band [50000,100000) must match: %+v", c)
	}
	if c.cat != CategoryTerminalStorage || c.src != SourceFirecrawlPublic || c.snapID == nil {
		t.Fatalf("band line shape wrong: %+v", c)
	}
}

func TestBandCands_UpperBoundExclusive(t *testing.T) {
	in := freightInput()
	in.Snapshots = []FeeBand{band("BRSSZ", "0", "50000", 5, "900.0000")}
	fxr := newFXResolver(in)
	if _, outcome := bandCand(in, d("50000.00"), fxr); outcome != bandNoMatch {
		t.Fatal("CIFMax is exclusive: 50000 must not match [0,50000)")
	}
}

func TestBandCands_FirstPeriodOnly(t *testing.T) {
	in := freightInput()
	in.Snapshots = []FeeBand{
		band("BRSSZ", "0", "100000", 10, "1500.0000"), // second period
		band("BRSSZ", "0", "100000", 5, "900.0000"),   // first period
	}
	fxr := newFXResolver(in)
	c, outcome := bandCand(in, d("75000.00"), fxr)
	if outcome != bandPriced || !c.net.Equal(d("900.0000")) {
		t.Fatalf("smallest PeriodDays (first period) must win: %+v", c)
	}
}

func TestBandCands_WeightBoundRespected(t *testing.T) {
	in := freightInput()
	b := band("BRSSZ", "0", "100000", 5, "900.0000")
	b.WeightMinKg = dp("20000") // input gross is 18000 -> below minimum
	in.Snapshots = []FeeBand{b}
	fxr := newFXResolver(in)
	if _, outcome := bandCand(in, d("75000.00"), fxr); outcome != bandNoMatch {
		t.Fatal("weight bound must exclude cargo below the minimum")
	}
	b.WeightMinKg = dp("17000")
	in.Snapshots = []FeeBand{b}
	if _, outcome := bandCand(in, d("75000.00"), fxr); outcome != bandPriced {
		t.Fatal("cargo at or above the minimum must match")
	}
}

func TestBandCands_NoMatchIsMissing(t *testing.T) {
	in := freightInput()
	in.Snapshots = []FeeBand{band("BRRIO", "0", "100000", 5, "900.0000")} // wrong location
	fxr := newFXResolver(in)
	if _, outcome := bandCand(in, d("75000.00"), fxr); outcome != bandNoMatch {
		t.Fatal("no matching band must report missing")
	}
}

// A band that matched but cannot be priced is an FX gap, never a band
// gap: bandUnpriced keeps the two states distinct (MISSING_FX is
// recorded by the resolver).
func TestBandCands_MatchedButUnpriceableIsDistinct(t *testing.T) {
	in := freightInput()
	b := band("BRSSZ", "0", "100000", 5, "900.0000")
	b.Currency = "EUR" // no EUR rate in the standard set
	in.Snapshots = []FeeBand{b}
	fxr := newFXResolver(in)
	if _, outcome := bandCand(in, d("75000.00"), fxr); outcome != bandUnpriced {
		t.Fatalf("matched band with missing FX must be bandUnpriced: %v", outcome)
	}
	miss := fxr.missingCurrencies()
	if len(miss) != 1 || miss[0] != "EUR" {
		t.Fatalf("EUR must be recorded missing: %v", miss)
	}
}

func TestBandCands_LocationNormalized(t *testing.T) {
	in := freightInput()
	in.Snapshots = []FeeBand{band(" brssz ", "0", "100000", 5, "900.0000")}
	fxr := newFXResolver(in)
	if _, outcome := bandCand(in, d("75000.00"), fxr); outcome != bandPriced {
		t.Fatal("location match must normalize case and spaces")
	}
}

// CIF family: the band is sized on a CIF base that needs the seller-paid
// declared freight — a nil one is absent input, never a silent zero
// (Devin review, PR15). The quote must not complete on the wrong band.
func TestPrice_CFRNilDeclaredFreightIsIncomplete(t *testing.T) {
	in := fobTariffInput("950.0000")
	in.Incoterm = IncotermCFR // DeclaredFreight stays nil.
	out := mustPrice(t, in)
	if out.Status != StatusIncomplete {
		t.Fatalf("INCOMPLETE expected: %+v", out)
	}
	if !hasReason(out, ReasonMissingDeclaredFreight) {
		t.Fatalf("MISSING_DECLARED_FREIGHT expected: %v", out.Reasons)
	}
	if findLine(out.Lines, CategoryTerminalStorage, "TERMINAL_STORAGE") != nil {
		t.Fatal("band selection must be skipped without the declared freight")
	}
}

// An explicit zero-valued Money is a known zero, not an absent input:
// the band is sized on cargo alone and the quote completes (PR15).
func TestPrice_CFRZeroDeclaredFreightIsAKnownZero(t *testing.T) {
	in := fobTariffInput("950.0000")
	in.Incoterm = IncotermCFR
	in.DeclaredFreight = &Money{Amount: decimal.Zero, Currency: "USD"}
	out := mustComplete(t, in)
	term := requireLine(t, out.Lines, CategoryTerminalStorage, "TERMINAL_STORAGE")
	if !hasFlag(t, term, FlagDeclaredBasis) {
		t.Fatalf("a known zero still marks DECLARED_BASIS: %+v", term)
	}
}

// DAP with the terminal requested has the same declared-freight
// requirement as the CIF family (PR15).
func TestPrice_DAPRequestedTerminalNilDeclaredFreight(t *testing.T) {
	in := dapInput()
	in.TerminalRequested = true
	in.DeclaredFreight = nil
	out := mustPrice(t, in)
	if out.Status != StatusIncomplete {
		t.Fatalf("INCOMPLETE expected: %+v", out)
	}
	if !hasReason(out, ReasonMissingDeclaredFreight) {
		t.Fatalf("MISSING_DECLARED_FREIGHT expected: %v", out.Reasons)
	}
}

// A usable offer's pre-carriage charge is out of MVP exactly like a
// tariff row: excluded from the pool and skipped with the same warning
// (spec §3 FCA note, PR15).
func TestPrice_OfferPreCarriageChargeSkipped(t *testing.T) {
	in := fcaPreCarriageInput() // FCA; an INLAND tariff row already skips.
	in.CarrierOffer = testOffer(testUUID4, ts(2026, 11, 20, 0, 0),
		oceanCharge("2500.00", "40HC"),
		offerCharge(ChargeOriginCharge, "ORI_FEE", "280.00", "USD", "40HC"),
		offerCharge(ChargeOriginCharge, "INLAND", "300.00", "USD", "40HC"))
	out := mustComplete(t, in)
	if findLine(out.Lines, CategoryOriginCharges, "INLAND") != nil {
		t.Fatal("a pre-carriage offer charge must never price")
	}
	if !slices.Contains(out.Warnings, "skipped out-of-MVP pre-carriage charge INLAND") {
		t.Fatalf("the offer skip must warn like the tariff skip: %v", out.Warnings)
	}
}

// Org fixed fees get the same pre-carriage guard (PR15).
func TestPrice_FixedFeePreCarriageSkipped(t *testing.T) {
	in := fcaPreCarriageInput()
	in.FixedFees = []FixedFee{
		{ChargeCode: "INLAND_HAULAGE", Category: CategoryOriginCharges, Amount: d("300.00"), Currency: "USD"},
	}
	out := mustComplete(t, in)
	if findLine(out.Lines, CategoryOriginCharges, "INLAND_HAULAGE") != nil {
		t.Fatal("a pre-carriage fixed fee must never price")
	}
	if !slices.Contains(out.Warnings, "skipped out-of-MVP pre-carriage charge INLAND_HAULAGE") {
		t.Fatalf("the fee skip must warn: %v", out.Warnings)
	}
}

// A pre-carriage fixed fee in a group the matrix never opens was never
// a candidate, so its skip must not warn (PR15 review).
func TestPrice_FixedFeePreCarriageInClosedGroupDoesNotWarn(t *testing.T) {
	in := fobTariffInput("950.0000") // FOB: origin group is closed.
	in.FixedFees = []FixedFee{
		{ChargeCode: "INLAND_HAULAGE", Category: CategoryOriginCharges, Amount: d("300.00"), Currency: "USD"},
	}
	out := mustComplete(t, in)
	if findLine(out.Lines, CategoryOriginCharges, "INLAND_HAULAGE") != nil {
		t.Fatal("a fee in a closed group must never price")
	}
	if slices.Contains(out.Warnings, "skipped out-of-MVP pre-carriage charge INLAND_HAULAGE") {
		t.Fatalf("a fee that was never a candidate must not warn: %v", out.Warnings)
	}
}

// A zero-valued declared component is a known zero: it contributes
// nothing to the CIF base and resolves no rate — a zero insurance in
// a rate-less currency must not raise MISSING_CUSTOMS_FX (PR15 review).
func TestPrice_ZeroDeclaredInsuranceNeedsNoFXRate(t *testing.T) {
	in := fobTariffInput("950.0000") // USD rates only.
	in.DeclaredInsurance = &Money{Amount: decimal.Zero, Currency: "EUR"}
	out := mustComplete(t, in)
	if hasReason(out, ReasonMissingCustomsFX) {
		t.Fatalf("a known-zero amount must not resolve FX: %v", out.Reasons)
	}
}
