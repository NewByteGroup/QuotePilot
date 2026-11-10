package pricing

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Task 1 (spec §2, §4 step 1): validate() rejects programmer/config
// errors; data gaps stay INCOMPLETE and are covered by later tasks.

func TestValidate_AcceptsMinimalValidInput(t *testing.T) {
	in := fobFCLInput()
	if err := in.validate(); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
}

func TestValidate_RejectsNegativeMarkup(t *testing.T) {
	in := fobFCLInput()
	in.MarkupRate = d("-0.0100")
	requireErr(t, in.validate(), "markup")
}

func TestValidate_RejectsMarkupAboveOne(t *testing.T) {
	in := fobFCLInput()
	in.MarkupRate = d("1.5000")
	requireErr(t, in.validate(), "markup")
}

func TestValidate_RejectsNegativeSpread(t *testing.T) {
	in := fobFCLInput()
	in.FXSafetySpread = d("-0.5")
	requireErr(t, in.validate(), "spread")
}

func TestValidate_RejectsSpreadAboveOne(t *testing.T) {
	in := fobFCLInput()
	in.FXSafetySpread = d("1.5000")
	requireErr(t, in.validate(), "spread")
}

func TestValidate_RejectsNegativeMinMarkup(t *testing.T) {
	in := fobFCLInput()
	in.MinMarkupUSD = dp("-10.00")
	requireErr(t, in.validate(), "min markup")
}

func TestValidate_RejectsNegativeCargoFields(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Input)
	}{
		{"gross kg", func(in *Input) { in.GrossKg = d("-1") }},
		{"volume m3", func(in *Input) { in.VolumeM3 = d("-0.5") }},
		{"cargo value", func(in *Input) { in.CargoValue = d("-100") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := fobFCLInput()
			tc.mut(&in)
			requireErr(t, in.validate(), "negative")
		})
	}
}

func TestValidate_RejectsFCLWithoutContainers(t *testing.T) {
	in := fobFCLInput()
	in.ContainerCount = 0
	requireErr(t, in.validate(), "container")
}

func TestValidate_RejectsAIRWithContainers(t *testing.T) {
	in := fobFCLInput()
	in.Modal = ModalAIR
	in.Incoterm = IncotermDAP // FOB is sea-only; keep input coherent.
	in.ContainerCount = 1
	requireErr(t, in.validate(), "container")
}

func TestValidate_RejectsNegativeContainerCountAnyModal(t *testing.T) {
	for _, m := range []Modal{ModalAIR, ModalLCL, ModalFCL} {
		in := fobFCLInput()
		in.Modal = m
		in.ContainerCount = -1
		requireErr(t, in.validate(), "container")
	}
}

func TestValidate_RejectsUnknownEnums(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Input)
	}{
		{"direction", func(in *Input) { in.Direction = Direction("SIDEWAYS") }},
		{"incoterm", func(in *Input) { in.Incoterm = Incoterm("XXX") }},
		{"modal", func(in *Input) { in.Modal = Modal("RAIL") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := fobFCLInput()
			tc.mut(&in)
			requireErr(t, in.validate(), tc.name)
		})
	}
}

func TestValidate_AcceptsLCLModal(t *testing.T) {
	in := fobFCLInput()
	in.Modal = ModalLCL
	if err := in.validate(); err != nil {
		t.Fatalf("LCL is a known modal and must pass validation: %v", err)
	}
}

func TestValidate_RejectsMissingCurrencies(t *testing.T) {
	in := fobFCLInput()
	in.CargoCurrency = ""
	requireErr(t, in.validate(), "currency")
}

func TestValidate_CargoCurrencyOptionalWhenNoCargoValue(t *testing.T) {
	in := fobFCLInput()
	in.CargoValue = decimal.Zero
	in.CargoCurrency = ""
	if err := in.validate(); err != nil {
		t.Fatalf("empty cargo value does not need a currency: %v", err)
	}
}

func TestValidate_RejectsDeclaredMoneyWithoutCurrency(t *testing.T) {
	in := fobFCLInput()
	in.DeclaredFreight = &Money{Amount: d("1200.00"), Currency: ""}
	requireErr(t, in.validate(), "currency")
}

func TestValidate_RejectsNegativeDeclaredMoney(t *testing.T) {
	in := fobFCLInput()
	in.DeclaredInsurance = &Money{Amount: d("-5.00"), Currency: "USD"}
	requireErr(t, in.validate(), "negative")
}

func TestValidate_RejectsZeroDates(t *testing.T) {
	in := fobFCLInput()
	in.QuoteDate = time.Time{}
	requireErr(t, in.validate(), "quote date")
	in = fobFCLInput()
	in.PricedAt = time.Time{}
	requireErr(t, in.validate(), "priced at")
}

func TestValidate_RejectsMissingLOCODEs(t *testing.T) {
	in := fobFCLInput()
	in.OriginLOCODE = ""
	requireErr(t, in.validate(), "locode")
	in = fobFCLInput()
	in.DestLOCODE = ""
	requireErr(t, in.validate(), "locode")
}

// Equipment/unit is a required extraction field for the MVP modals: an
// empty one would slip an empty-vs-empty match past tariffApplies'
// fail-closed gate, so validation rejects it (PR15 review).
func TestValidate_RejectsMissingEquipment(t *testing.T) {
	for _, base := range []func() Input{fobFCLInput, airFCAInput} {
		in := base()
		in.EquipmentOrUnit = ""
		requireErr(t, in.validate(), "equipment")
		in.EquipmentOrUnit = "   "
		requireErr(t, in.validate(), "equipment")
	}
	// LCL is out of MVP: it exits on the modal before equipment matters.
	in := fobFCLInput()
	in.Modal = ModalLCL
	in.EquipmentOrUnit = ""
	if err := in.validate(); err != nil {
		t.Fatalf("LCL does not require equipment: %v", err)
	}
	if _, err := Price(in, clock()); err != nil {
		t.Fatalf("LCL empty equipment is a data gap, not a config error: %v", err)
	}
}

// An empty equipment reaches Price's error path, never a priced line.
func TestPrice_MissingEquipmentIsConfigError(t *testing.T) {
	in := fobFCLInput()
	in.EquipmentOrUnit = ""
	if _, err := Price(in, clock()); err == nil {
		t.Fatal("empty equipment must return a config error")
	}
}

var badTariffCases = []struct {
	name string
	mut  func(*TariffCandidate)
	want string
}{
	{"empty currency", func(r *TariffCandidate) { r.Currency = "" }, "currency"},
	{"negative net", func(r *TariffCandidate) { r.NetAmount = d("-1") }, "negative"},
	{"empty charge code", func(r *TariffCandidate) { r.ChargeCode = "" }, "charge code"},
	{"inverted window", func(r *TariffCandidate) {
		r.ValidFrom, r.ValidUntil = r.ValidUntil, r.ValidFrom
	}, "valid_from"},
	{"zero valid_from", func(r *TariffCandidate) { r.ValidFrom = time.Time{} }, "valid_from"},
	{"zero valid_until", func(r *TariffCandidate) { r.ValidUntil = time.Time{} }, "valid_until"},
}

func TestValidate_RejectsBadTariffCandidates(t *testing.T) {
	for _, tc := range badTariffCases {
		t.Run(tc.name, func(t *testing.T) {
			in := fobFCLInput()
			row := tariffRow(testUUID1, testUUID2, "HAPAG", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "950.0000")
			tc.mut(&row)
			in.Tariffs = []TariffCandidate{row}
			requireErr(t, in.validate(), tc.want)
		})
	}
}

func TestValidate_RejectsBadFixedFees(t *testing.T) {
	in := fobFCLInput()
	in.FixedFees = []FixedFee{
		{ChargeCode: "DOC", Category: CategoryCustomsDispatch, Amount: d("-1.00"), Currency: "BRL"},
	}
	requireErr(t, in.validate(), "negative")

	in = fobFCLInput()
	in.FixedFees = []FixedFee{
		{ChargeCode: "DOC", Category: CategoryCustomsDispatch, Amount: d("1.00"), Currency: ""},
	}
	requireErr(t, in.validate(), "currency")
}

func TestValidate_RejectsBadFeeBand(t *testing.T) {
	in := fobFCLInput()
	in.Snapshots = []FeeBand{
		{SnapshotID: testUUID3, Location: "", CIFMin: decimal.Zero, CIFMax: d("100"),
			Amount: d("500.0000"), Currency: "BRL"},
	}
	requireErr(t, in.validate(), "location")
}

func TestValidate_RejectsBadFXRate(t *testing.T) {
	in := fobFCLInput()
	in.FXRates = []FXRate{
		{Source: FXSourcePTAX, Currency: "USD", Date: testPrevBD, Rate: d("0")},
	}
	requireErr(t, in.validate(), "rate")
	in = fobFCLInput()
	in.FXRates = []FXRate{
		{Source: "BOGUS", Currency: "USD", Date: testPrevBD, Rate: d("5")},
	}
	requireErr(t, in.validate(), "source")
	in = fobFCLInput()
	in.FXRates = []FXRate{
		{Source: FXSourcePTAX, Currency: "USD", Date: time.Time{}, Rate: d("5.3")},
	}
	requireErr(t, in.validate(), "date")
}

func TestValidate_RejectsBadCarrierOffer(t *testing.T) {
	in := fobFCLInput()
	in.CarrierOffer = &CarrierOffer{
		ID:      uuid.Nil,
		Carrier: "HAPAG",
		Charges: []OfferCharge{{Category: ChargeOceanFreight, Code: "OCEAN_FREIGHT", Amount: d("900"), Currency: "USD"}},
	}
	requireErr(t, in.validate(), "offer")
}

func TestPrice_NilClockIsConfigError(t *testing.T) {
	in := fobFCLInput()
	if _, err := Price(in, nil); err == nil {
		t.Fatal("nil clock must return a config error")
	}
}

func TestPrice_InvalidInputReturnsErrorNotIncomplete(t *testing.T) {
	in := fobFCLInput()
	in.MarkupRate = d("-1")
	out, err := Price(in, func() time.Time { return testPricedAt })
	if err == nil {
		t.Fatalf("expected error, got output %+v", out)
	}
}
