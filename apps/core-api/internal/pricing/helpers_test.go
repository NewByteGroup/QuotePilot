package pricing

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// d builds a decimal from a literal; tests must never touch floats.
func d(s string) decimal.Decimal {
	return decimal.RequireFromString(s)
}

// dp returns a pointer to a decimal literal.
func dp(s string) *decimal.Decimal {
	v := d(s)
	return &v
}

// uid parses a fixed UUID string so tests stay deterministic.
func uid(s string) uuid.UUID {
	return uuid.MustParse(s)
}

// day returns a UTC date at midnight.
func day(y int, m time.Month, dd int) time.Time {
	return time.Date(y, m, dd, 0, 0, 0, 0, time.UTC)
}

// ts returns a UTC timestamp.
func ts(y int, m time.Month, dd, hh, mm int) time.Time {
	return time.Date(y, m, dd, hh, mm, 0, 0, time.UTC)
}

var (
	testUUID1 = uid("11111111-1111-1111-1111-111111111111")
	testUUID2 = uid("22222222-2222-2222-2222-222222222222")
	testUUID3 = uid("33333333-3333-3333-3333-333333333333")
	testUUID4 = uid("44444444-4444-4444-4444-444444444444")
	testUUID5 = uid("55555555-5555-5555-5555-555555555555")
	testUUID6 = uid("66666666-6666-6666-6666-666666666666")
)

// 2026-11-16 is a Monday; the PTAX previous business day is Friday 2026-11-13.
var (
	testQuoteDate = day(2026, 11, 16)
	testPrevBD    = day(2026, 11, 13)
	testPricedAt  = ts(2026, 11, 16, 12, 0)
)

// tariffRow builds an activated, in-window tariff candidate.
func tariffRow(id, batch uuid.UUID, carrier string, cat ChargeCategory, code, ccy, net string) TariffCandidate {
	act := ts(2026, 11, 10, 9, 0)
	return TariffCandidate{
		ID:                id,
		BatchID:           batch,
		CarrierOrAgent:    carrier,
		Modal:             ModalFCL,
		OriginLOCODE:      "CNSHA",
		DestLOCODE:        "BRSSZ",
		EquipmentOrUnit:   "40HC",
		ChargeCategory:    cat,
		ChargeCode:        code,
		Currency:          ccy,
		NetAmount:         d(net),
		SearchDescription: carrier + " " + code,
		ValidFrom:         day(2026, 1, 1),
		ValidUntil:        day(2026, 12, 31),
		CreatedAt:         ts(2026, 11, 10, 9, 0),
		BatchActivatedAt:  &act,
	}
}

// fobFCLInput returns a minimal valid FOB FCL import input: freight +
// terminal + customs are payable, origin is not.
func fobFCLInput() Input {
	return Input{
		Direction:       DirectionImport,
		Incoterm:        IncotermFOB,
		Modal:           ModalFCL,
		OriginLOCODE:    "CNSHA",
		DestLOCODE:      "BRSSZ",
		EquipmentOrUnit: "40HC",
		ContainerCount:  2,
		GrossKg:         d("18000.000"),
		VolumeM3:        d("54.000"),
		CargoValue:      d("20000.00"),
		CargoCurrency:   "USD",
		MarkupRate:      d("0.1500"),
		FXSafetySpread:  d("0.0200"),
		QuoteDate:       testQuoteDate,
		PricedAt:        testPricedAt,
	}
}

// usdRates returns the standard FX set: carrier, PTAX and customs USD rates.
func usdRates() []FXRate {
	return []FXRate{
		{Source: FXSourceCarrier, Currency: "USD", Date: testQuoteDate, Rate: d("5.400000")},
		{Source: FXSourcePTAX, Currency: "USD", Date: testPrevBD, Rate: d("5.300000")},
		{Source: FXSourceCustoms, Currency: "USD", Date: testQuoteDate, Rate: d("5.350000")},
	}
}

// fobTariffInput is the standard FOB FCL quote: USD rates, one freight
// tariff, a surcharge tariff, a customs tariff and the matching band.
func fobTariffInput(freightNet string) Input {
	in := fobFCLInput()
	in.FXRates = usdRates()
	in.Tariffs = []TariffCandidate{
		tariffRow(testUUID1, testUUID2, "MSC", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", freightNet),
		tariffRow(testUUID5, testUUID2, "HAPAG", ChargeSurcharge, "BAF", "USD", "120.0000"),
		tariffRow(testUUID6, testUUID2, "AGENT", ChargeCustoms, "CUSTOMS_CLEARANCE", "BRL", "450.0000"),
	}
	in.Snapshots = []FeeBand{band("BRSSZ", "100000", "200000", 5, "1400.0000")}
	return in
}

// airFCAInput is the standard AIR FCA base (no container count).
func airFCAInput() Input {
	in := fobFCLInput()
	in.Modal = ModalAIR
	in.ContainerCount = 0
	in.Incoterm = IncotermFCA
	in.EquipmentOrUnit = "AWB"
	return in
}

// airTariffs prepends the air-freight row to the shared AIR tail
// (origin THC, surcharge FSC, customs), all stamped modal AIR.
func airTariffs(row TariffCandidate) []TariffCandidate {
	rows := []TariffCandidate{
		row,
		tariffRow(testUUID3, testUUID2, "AGENT", ChargeOriginCharge, "THC_ORI", "USD", "250.0000"),
		tariffRow(testUUID5, testUUID2, "AIRLINE", ChargeSurcharge, "FSC", "USD", "100.0000"),
		tariffRow(testUUID6, testUUID2, "AGENT", ChargeCustoms, "CUSTOMS_CLEARANCE", "BRL", "450.0000"),
	}
	for i := 1; i < len(rows); i++ {
		rows[i].Modal = ModalAIR
		rows[i].EquipmentOrUnit = "AWB"
	}
	return rows
}

func clock() func() time.Time { return func() time.Time { return testPricedAt } }

// testOffer builds a carrier offer expiring at the given time.
func testOffer(id uuid.UUID, expiry time.Time, charges ...OfferCharge) *CarrierOffer {
	return &CarrierOffer{ID: id, Carrier: "HAPAG", ExpiresAt: expiry, Charges: charges}
}

// oceanCharge builds an OCEAN_FREIGHT offer charge in USD.
func oceanCharge(amount, equipment string) OfferCharge {
	return offerCharge(ChargeOceanFreight, "OCEAN_FREIGHT", amount, "USD", equipment)
}

// offerCharge builds one offer charge from string literals.
func offerCharge(cat ChargeCategory, code, amount, ccy, equip string) OfferCharge {
	return OfferCharge{
		Category:        cat,
		Code:            code,
		Amount:          d(amount),
		Currency:        ccy,
		EquipmentOrUnit: equip,
	}
}

// mustPrice returns the output or fails on a returned error.
func mustPrice(t *testing.T, in Input) Output {
	t.Helper()
	out, err := Price(in, clock())
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	return out
}

// mustComplete additionally requires StatusComplete.
func mustComplete(t *testing.T, in Input) Output {
	t.Helper()
	out := mustPrice(t, in)
	if out.Status != StatusComplete {
		t.Fatalf("COMPLETE expected, reasons %v", out.Reasons)
	}
	return out
}

// requireLine returns the matching line or fails the test.
func requireLine(t *testing.T, lines []Line, cat Category, code string) *Line {
	t.Helper()
	l := findLine(lines, cat, code)
	if l == nil {
		t.Fatalf("line %s/%s missing in %+v", cat, code, lines)
	}
	return l
}

// freightLines filters the INTL_FREIGHT category out of a line set.
func freightLines(lines []Line) []Line {
	out := make([]Line, 0, len(lines))
	for _, l := range lines {
		if l.Category == CategoryIntlFreight {
			out = append(out, l)
		}
	}
	return out
}

func sumSells(lines []Line) decimal.Decimal {
	s := decimal.Zero
	for _, l := range lines {
		s = s.Add(l.SellBRL)
	}
	return s
}

func requireErr(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", substr)
	}
	if got := err.Error(); !strings.Contains(got, substr) {
		t.Fatalf("error %q does not contain %q", got, substr)
	}
}
