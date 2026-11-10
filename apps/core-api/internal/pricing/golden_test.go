package pricing

import (
	"testing"

	"github.com/shopspring/decimal"
)

// Task 8 (spec §10): golden cases — the numbers below are hand-computed
// from spec §3-§7 and act as the money-test anchor (ADR-008).

func findLine(lines []Line, cat Category, code string) *Line {
	for i := range lines {
		if lines[i].Category == cat && lines[i].ChargeCode == code {
			return &lines[i]
		}
	}
	return nil
}

func requireMoneyEq(t *testing.T, name string, got, want decimal.Decimal) {
	t.Helper()
	if !got.Equal(want) {
		t.Fatalf("%s: got %s want %s", name, got, want)
	}
}

// hasFlag reports whether the line carries flag. A nil line fails the
// test cleanly instead of panicking — a missing line is a test bug.
func hasFlag(t *testing.T, l *Line, flag string) bool {
	t.Helper()
	if l == nil {
		t.Fatalf("line missing when checking flag %q", flag)
	}
	for _, f := range l.Flags {
		if f == flag {
			return true
		}
	}
	return false
}

func hasReason(out Output, r Reason) bool {
	for _, x := range out.Reasons {
		if x == r {
			return true
		}
	}
	return false
}

func hasAssumption(out Output, a Assumption) bool {
	for _, x := range out.Assumptions {
		if x == a {
			return true
		}
	}
	return false
}

// exwGoldenInput: EXW FCL 2x40HC with a usable offer carrying freight,
// an origin charge and a surcharge, competing tariffs, an origin fixed
// fee, a customs tariff and a matching destination band.
func exwGoldenInput() Input {
	in := fobFCLInput()
	in.Incoterm = IncotermEXW
	in.FXRates = usdRates()
	in.CarrierOffer = testOffer(testUUID4, ts(2026, 11, 20, 0, 0),
		oceanCharge("2500.00", "40HC"),
		offerCharge(ChargeOriginCharge, "THC_ORI", "280.00", "USD", "40HC"),
		offerCharge(ChargeSurcharge, "BAF", "150.00", "USD", ""),
	)
	in.Tariffs = []TariffCandidate{
		tariffRow(testUUID1, testUUID2, "MSC", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "800.0000"),
		tariffRow(testUUID3, testUUID2, "AGENT", ChargeOriginCharge, "THC_ORI", "USD", "250.0000"),
		tariffRow(testUUID5, testUUID2, "HAPAG", ChargeSurcharge, "BAF", "USD", "120.0000"),
		tariffRow(testUUID6, testUUID2, "AGENT", ChargeCustoms, "CUSTOMS_CLEARANCE", "BRL", "450.0000"),
	}
	in.FixedFees = []FixedFee{
		{ChargeCode: "DOC", Description: "documentation", Category: CategoryOriginCharges, Amount: d("60.00"), Currency: "BRL"},
	}
	in.Snapshots = []FeeBand{band("BRSSZ", "100000", "200000", 5, "1400.0000")}
	return in
}

// Spec §10 case 1: the usable offer beats the tariff on freight, the
// losing tariff survives as an audit alternative.
func TestGolden_EXW_FCL_OfferBeatsTariffAlternative(t *testing.T) {
	out := mustComplete(t, exwGoldenInput())
	if len(out.Lines) != 6 {
		t.Fatalf("expected 6 lines, got %d: %+v", len(out.Lines), out.Lines)
	}
	freight := requireLine(t, out.Lines, CategoryIntlFreight, "OCEAN_FREIGHT")
	if freight.SourceType != SourceCarrierOffer {
		t.Fatalf("offer must win freight: %+v", freight)
	}
	requireMoneyEq(t, "freight cost", freight.CostBRL, d("13770.00")) // 2500 x 5.508
	requireMoneyEq(t, "freight sell", freight.SellBRL, d("15835.50")) // x1.15
	if freight.CarrierOfferID == nil || *freight.CarrierOfferID != testUUID4 {
		t.Fatalf("offer pin missing: %+v", freight)
	}
	assertEXWAlternatives(t, out)
}

func assertEXWAlternatives(t *testing.T, out Output) {
	t.Helper()
	if len(out.Alternatives) != 3 {
		t.Fatalf("3 losing options expected: %+v", out.Alternatives)
	}
	alt := requireLine(t, out.Alternatives, CategoryIntlFreight, "OCEAN_FREIGHT")
	requireMoneyEq(t, "alt cost", alt.CostBRL, d("8812.80"))  // 1600 x 5.508
	requireMoneyEq(t, "alt sell", alt.SellBRL, d("10134.72")) // x1.15
	if alt.PrivateTariffID == nil || *alt.PrivateTariffID != testUUID1 {
		t.Fatalf("alt must pin the losing tariff: %+v", alt)
	}
}

// The same EXW quote: pass-through groups sell at cost and the totals
// are the exact sum of line amounts.
func TestGolden_EXW_PassThroughLinesAndTotals(t *testing.T) {
	out := mustComplete(t, exwGoldenInput())
	thc := requireLine(t, out.Lines, CategoryOriginCharges, "THC_ORI")
	requireMoneyEq(t, "origin THC cost", thc.CostBRL, d("1325.00")) // 250 x PTAX 5.30
	requireMoneyEq(t, "origin THC sell", thc.SellBRL, d("1325.00")) // pass-through
	doc := requireLine(t, out.Lines, CategoryOriginCharges, "DOC")
	requireMoneyEq(t, "fixed fee", doc.CostBRL, d("60.00"))
	if doc.SourceType != SourceOrgFixedRule {
		t.Fatalf("fixed fee source: %+v", doc)
	}
	assertEXWTerminalAndTotals(t, out)
}

func assertEXWTerminalAndTotals(t *testing.T, out Output) {
	t.Helper()
	requireMoneyEq(t, "BAF cost", requireLine(t, out.Lines, CategorySurcharge, "BAF").CostBRL, d("660.96"))
	// CIF base = (20000 + 2500) x customs 5.35 = 120375 -> band [100k,200k).
	term := requireLine(t, out.Lines, CategoryTerminalStorage, "TERMINAL_STORAGE")
	requireMoneyEq(t, "terminal", term.CostBRL, d("1400.00"))
	customs := requireLine(t, out.Lines, CategoryCustomsDispatch, "CUSTOMS_CLEARANCE")
	requireMoneyEq(t, "customs", customs.CostBRL, d("450.00"))
	requireMoneyEq(t, "TotalCostBRL", out.TotalCostBRL, d("17665.96"))
	requireMoneyEq(t, "FinalPriceBRL", out.FinalPriceBRL, d("19731.46"))
}

// The EXW quote's audit surface: applied FX records, assumptions and
// the stable inputs_hash.
func TestGolden_EXW_AuditMetadata(t *testing.T) {
	out := mustComplete(t, exwGoldenInput())
	if len(out.FXApplied) != 3 {
		t.Fatalf("carrier+PTAX+customs applications: %+v", out.FXApplied)
	}
	for _, a := range []Assumption{AssumptionImportMatrixOnly, AssumptionTaxesExcluded,
		AssumptionNoInsuranceLine, AssumptionPTAXPrevBusinessDay,
		AssumptionCustomsRateUnconfirmed, AssumptionStorageFirstPeriodOnly,
		AssumptionOfferStrictCoverage} {
		if !hasAssumption(out, a) {
			t.Fatalf("missing assumption %s in %v", a, out.Assumptions)
		}
	}
	if out.InputsHash == "" {
		t.Fatal("inputs_hash must be populated")
	}
}

// Spec §10 case 2: FOB with tariff-only freight, no competition.
func TestGolden_FOB_FCL_TariffOnly(t *testing.T) {
	out := mustComplete(t, fobTariffInput("950.0000"))
	// CIF = (20000 + 1900) x 5.35 = 117165 -> band.
	freight := requireLine(t, out.Lines, CategoryIntlFreight, "OCEAN_FREIGHT")
	requireMoneyEq(t, "cost", freight.CostBRL, d("10465.20")) // 1900 x 5.508
	requireMoneyEq(t, "sell", freight.SellBRL, d("12034.98")) // x1.15
	requireMoneyEq(t, "total cost", out.TotalCostBRL, d("12976.16"))
	requireMoneyEq(t, "final price", out.FinalPriceBRL, d("14545.94"))
	if len(out.Alternatives) != 0 {
		t.Fatalf("no alternatives without competition: %+v", out.Alternatives)
	}
}

// Spec §10 case 3: CFR — freight and surcharges are the seller's; the
// declared freight feeds the CIF base instead (DECLARED_BASIS).
func TestGolden_CFR_DeclaredFreightFeedsCIF(t *testing.T) {
	in := fobTariffInput("950.0000") // stray freight+BAF tariffs must not be charged
	in.Incoterm = IncotermCFR
	in.DeclaredFreight = &Money{Amount: d("1500.00"), Currency: "USD"}
	out := mustComplete(t, in)
	if findLine(out.Lines, CategoryIntlFreight, "OCEAN_FREIGHT") != nil {
		t.Fatal("CFR freight is the seller's: no freight line allowed")
	}
	if findLine(out.Lines, CategorySurcharge, "BAF") != nil {
		t.Fatal("surcharges follow the freight payer: none under CFR")
	}
	// CIF = (20000 + 1500) x 5.35 = 114875 -> band, declared basis flagged.
	term := requireLine(t, out.Lines, CategoryTerminalStorage, "TERMINAL_STORAGE")
	if !hasFlag(t, term, FlagDeclaredBasis) {
		t.Fatalf("terminal band must carry DECLARED_BASIS: %+v", term)
	}
	requireMoneyEq(t, "final", out.FinalPriceBRL, d("1850.00")) // 450 + 1400
}

// airMinInput: AIR FCA with a USD 1.00/kg air tariff and a USD 250
// min_amount; gross 50 kg vs 166.67 kg volumetric.
func airMinInput() Input {
	in := airFCAInput()
	in.GrossKg = d("50")
	in.VolumeM3 = d("1.0") // chargeable = max(50, 166.67) = 166.67 kg
	in.FXRates = usdRates()
	row := tariffRow(testUUID1, testUUID2, "AIRLINE", ChargeAirFreight, "AIR_FREIGHT", "USD", "1.0000")
	row.Modal = ModalAIR
	row.EquipmentOrUnit = "AWB"
	row.MinAmount = dp("250.0000") // 166.67 x 1.00 < 250 -> min binds
	in.Tariffs = airTariffs(row)
	in.Snapshots = []FeeBand{band("BRSSZ", "0", "200000", 5, "1400.0000")}
	return in
}

// Spec §10 case 4a: min_amount binds when the chargeable-weight net is
// below the floor.
func TestGolden_AIR_MinAmountBinds(t *testing.T) {
	out := mustComplete(t, airMinInput())
	freight := requireLine(t, out.Lines, CategoryIntlFreight, "AIR_FREIGHT")
	requireMoneyEq(t, "air net", freight.OriginalAmount, d("250.0000")) // min binds
	requireMoneyEq(t, "air cost", freight.CostBRL, d("1377.00"))        // 250 x 5.508
	requireMoneyEq(t, "air sell", freight.SellBRL, d("1583.55"))        // x1.15
	if !hasAssumption(out, AssumptionAirFactor) {
		t.Fatalf("AIR_FACTOR_166_67 assumption required: %v", out.Assumptions)
	}
}

// Spec §10 case 4b: 2.0 m3 -> 333.34 kg x 1.00 beats the min floor.
func TestGolden_AIR_VolumetricWins(t *testing.T) {
	in := airMinInput()
	in.VolumeM3 = d("2.0")
	out := mustComplete(t, in)
	freight := requireLine(t, out.Lines, CategoryIntlFreight, "AIR_FREIGHT")
	requireMoneyEq(t, "volumetric net", freight.OriginalAmount, d("333.34"))
	requireMoneyEq(t, "volumetric cost", freight.CostBRL, d("1836.04")) // 333.34 x 5.508
	requireMoneyEq(t, "volumetric sell", freight.SellBRL, d("2111.44")) // x1.15
}

// airFallbackInput: AIR FCA priced entirely off PTAX (no carrier rate),
// so the carrier path falls back with FX_FALLBACK (spec §10 case 5).
func airFallbackInput() Input {
	in := airFCAInput()
	in.FXRates = []FXRate{
		{Source: FXSourcePTAX, Currency: "USD", Date: testPrevBD, Rate: d("5.300000")},
		{Source: FXSourceCustoms, Currency: "USD", Date: testQuoteDate, Rate: d("5.350000")},
	}
	row := tariffRow(testUUID1, testUUID2, "AIRLINE", ChargeAirFreight, "AIR_FREIGHT", "USD", "3.5000")
	row.Modal = ModalAIR
	row.EquipmentOrUnit = "AWB"
	in.Tariffs = airTariffs(row)
	in.Snapshots = []FeeBand{band("BRSSZ", "0", "2000000", 5, "1400.0000")}
	return in
}

func TestGolden_AIR_FXFallback(t *testing.T) {
	out := mustComplete(t, airFallbackInput())
	freight := requireLine(t, out.Lines, CategoryIntlFreight, "AIR_FREIGHT")
	// chargeable = max(18000, 9000.18) = 18000; net = 63000; PTAX 5.30 x1.02.
	if !hasFlag(t, freight, FlagFXFallback) {
		t.Fatalf("freight line must flag FX_FALLBACK: %+v", freight.Flags)
	}
	requireMoneyEq(t, "fallback rate", freight.FXRateUsed, d("5.406"))
	requireMoneyEq(t, "fallback cost", freight.CostBRL, d("340578.00")) // 63000 x 5.406
}

// Spec §10 case 6: the minimum markup floor binds over the
// proportional markup.
func TestGolden_MinMarkupFloorBinds(t *testing.T) {
	in := fobTariffInput("800.0000")
	in.MinMarkupUSD = dp("400.00")
	out := mustComplete(t, in)
	freight := requireLine(t, out.Lines, CategoryIntlFreight, "OCEAN_FREIGHT")
	// net 1600 x 5.508 = 8812.80; proportional 15% = 1321.92;
	// floor = 400 x 2 = 800 USD x 5.508 = 4406.40 BRL -> binds.
	requireMoneyEq(t, "floor sell", freight.SellBRL, d("13219.20")) // 8812.80 + 4406.40
	if !hasFlag(t, freight, FlagMinMarkupApplied) {
		t.Fatalf("MIN_MARKUP_APPLIED required: %+v", freight.Flags)
	}
}

// Spec §10 case 7: DDP completes with zero payable lines.
func TestGolden_DDP_EmptyCompleteQuote(t *testing.T) {
	in := fobFCLInput()
	in.Incoterm = IncotermDDP
	out := mustPrice(t, in)
	if out.Status != StatusComplete || len(out.Lines) != 0 {
		t.Fatalf("DDP must complete with zero lines: %+v", out)
	}
	requireMoneyEq(t, "total", out.FinalPriceBRL, d("0"))
	if !hasAssumption(out, AssumptionDDPNothingPayable) {
		t.Fatalf("DDP_NOTHING_PAYABLE required: %v", out.Assumptions)
	}
	if out.InputsHash == "" {
		t.Fatal("inputs_hash must be populated even for an empty quote")
	}
}

// Spec §10 case 8: export RFQs are INCOMPLETE with EXPORT_MATRIX_OPEN.
func TestGolden_ExportIsIncomplete(t *testing.T) {
	in := fobFCLInput()
	in.Direction = DirectionExport
	out := mustPrice(t, in)
	if out.Status != StatusIncomplete || len(out.Reasons) != 1 || out.Reasons[0] != ReasonExportMatrixOpen {
		t.Fatalf("EXPORT must be INCOMPLETE{EXPORT_MATRIX_OPEN}: %+v", out)
	}
	if len(out.Lines) != 0 {
		t.Fatalf("no lines for export: %+v", out.Lines)
	}
}

// Spec §10 case 9: a newer activated tombstone drops the whole natural
// key, leaving the freight group unsatisfied.
func TestGolden_TombstoneDropsFreightKey(t *testing.T) {
	in := fobTariffInput("950.0000")
	in.DeclaredFreight = &Money{Amount: d("1500.00"), Currency: "USD"}
	in.Tariffs = append(in.Tariffs, tombstoneRow())
	out := mustPrice(t, in)
	if out.Status != StatusIncomplete {
		t.Fatalf("tombstoned freight must be INCOMPLETE: %+v", out)
	}
	if !hasReason(out, reasonMissingTariffGroup("INTL_FREIGHT")) {
		t.Fatalf("MISSING_TARIFF_GROUP:INTL_FREIGHT expected: %v", out.Reasons)
	}
	if findLine(out.Lines, CategoryIntlFreight, "OCEAN_FREIGHT") != nil {
		t.Fatal("tombstoned key must produce no freight line")
	}
}

// tombstoneRow shares the live freight row's natural key but carries a
// later-activated tombstone.
func tombstoneRow() TariffCandidate {
	tomb := tariffRow(testUUID3, testUUID4, "MSC", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "0")
	tomb.IsRemoved = true
	act := ts(2026, 11, 15, 9, 0)
	tomb.BatchActivatedAt = &act
	tomb.CreatedAt = ts(2026, 11, 15, 9, 0)
	return tomb
}

// centAllocInput: two same-code offer freight lines with odd cent
// amounts, so the aggregate sell must be split by cent allocation.
func centAllocInput() Input {
	in := fobFCLInput()
	in.FXRates = usdRates()
	in.CarrierOffer = testOffer(testUUID4, ts(2026, 11, 20, 0, 0),
		oceanCharge("1000.01", "40HC"),
		oceanCharge("999.99", "40HC"),
	)
	in.Tariffs = []TariffCandidate{
		tariffRow(testUUID5, testUUID2, "HAPAG", ChargeSurcharge, "BAF", "USD", "120.0000"),
		tariffRow(testUUID6, testUUID2, "AGENT", ChargeCustoms, "CUSTOMS_CLEARANCE", "BRL", "450.0000"),
	}
	in.Snapshots = []FeeBand{band("BRSSZ", "100000", "200000", 5, "1400.0000")}
	return in
}

// Spec §10 case 10: the aggregate sell is split proportionally and the
// residual cent goes to the largest remainder.
func TestGolden_CentAllocationAcrossContainerLines(t *testing.T) {
	out := mustComplete(t, centAllocInput())
	freight := freightLines(out.Lines)
	if len(freight) != 2 {
		t.Fatalf("two container lines expected: %+v", out.Lines)
	}
	// Sorted output puts the smaller original amount first.
	requireMoneyEq(t, "line0 cost", freight[0].CostBRL, d("5507.94")) // 999.99 x 5.508
	requireMoneyEq(t, "line1 cost", freight[1].CostBRL, d("5508.06")) // 1000.01 x 5.508
	// Aggregate sell = 11016.00 x 1.15 = 12668.40, split proportionally.
	requireMoneyEq(t, "allocated sell sum", sumSells(freight), d("12668.40"))
	requireMoneyEq(t, "line0 sell", freight[0].SellBRL, d("6334.14"))
	requireMoneyEq(t, "line1 sell", freight[1].SellBRL, d("6334.26"))
	// The quote price is exactly the sum of line sells (spec §7).
	requireMoneyEq(t, "FinalPriceBRL == sum(SellBRL)", out.FinalPriceBRL, sumSells(out.Lines))
}

// residualAllocInput: three same-code offer freight lines whose rounded
// shares sum one cent short of the aggregate sell — the residual is
// assigned by largest-remainder order, not line order.
func residualAllocInput() Input {
	in := fobFCLInput()
	in.FXRates = usdRates()
	in.CarrierOffer = testOffer(testUUID4, ts(2026, 11, 20, 0, 0),
		oceanCharge("900.00", "40HC"),
		oceanCharge("900.01", "40HC"),
		oceanCharge("900.04", "40HC"),
	)
	in.Tariffs = []TariffCandidate{
		tariffRow(testUUID5, testUUID2, "HAPAG", ChargeSurcharge, "BAF", "USD", "120.0000"),
		tariffRow(testUUID6, testUUID2, "AGENT", ChargeCustoms, "CUSTOMS_CLEARANCE", "BRL", "450.0000"),
	}
	in.Snapshots = []FeeBand{band("BRSSZ", "100000", "200000", 5, "1400.0000")}
	return in
}

// Spec §10 case 10b (nonzero residual): net sum 2700.05 x 5.508 =
// 14871.8754; aggregate sell = 17102.66. Shares 5700.7811 / 5700.8444 /
// 5701.0345 round to ...78/...84/...03 — one cent short — and the
// largest remainder (the 900.04 line's 0.004465 vs 0.004439) takes it.
func TestGolden_CentAllocationNonzeroResidual(t *testing.T) {
	out := mustComplete(t, residualAllocInput())
	freight := freightLines(out.Lines)
	if len(freight) != 3 {
		t.Fatalf("three container lines expected: %+v", out.Lines)
	}
	// Sorted output orders by OriginalAmount: 900.00, 900.01, 900.04.
	requireMoneyEq(t, "line0 cost", freight[0].CostBRL, d("4957.20")) // 900.00 x 5.508
	requireMoneyEq(t, "line1 cost", freight[1].CostBRL, d("4957.26")) // 900.01 x 5.508
	requireMoneyEq(t, "line2 cost", freight[2].CostBRL, d("4957.42")) // 900.04 x 5.508
	requireMoneyEq(t, "line0 sell", freight[0].SellBRL, d("5700.78"))
	requireMoneyEq(t, "line1 sell", freight[1].SellBRL, d("5700.84"))
	requireMoneyEq(t, "line2 sell", freight[2].SellBRL, d("5701.04")) // +0.01 residual
	requireMoneyEq(t, "allocated sell sum", sumSells(freight), d("17102.66"))
	requireMoneyEq(t, "FinalPriceBRL == sum(SellBRL)", out.FinalPriceBRL, sumSells(out.Lines))
}
