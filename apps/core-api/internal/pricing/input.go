package pricing

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Input is everything the worker resolved for one RFQ (spec §2): the
// extracted cargo, org parameters already resolved route>modal>ALL, the
// dumb-filtered tariff/offer/snapshot candidates, FX rates and the two
// timestamps that drive every window check. Taken by value.
type Input struct {
	Direction    Direction `json:"direction"`
	Incoterm     Incoterm  `json:"incoterm"`
	Modal        Modal     `json:"modal"`
	OriginLOCODE string    `json:"origin_locode"`
	DestLOCODE   string    `json:"dest_locode"`

	// EquipmentOrUnit is matched to tariffs and offers ('40HC', ...).
	EquipmentOrUnit string          `json:"equipment_or_unit"`
	ContainerCount  int             `json:"container_count"`
	GrossKg         decimal.Decimal `json:"gross_kg"`
	VolumeM3        decimal.Decimal `json:"volume_m3"`

	// CargoValue/CargoCurrency feed the import CIF base (spec §4 step 6).
	CargoValue    decimal.Decimal `json:"cargo_value"`
	CargoCurrency string          `json:"cargo_currency"`

	// DeclaredFreight is seller-paid freight for the CIF-family base;
	// it is an input, never a charge line (RFC-001 §4 note).
	DeclaredFreight   *Money `json:"declared_freight,omitempty"`
	DeclaredInsurance *Money `json:"declared_insurance,omitempty"`

	// TerminalRequested gates TERMINAL_STORAGE_THC under DAP/DPU (spec §3).
	TerminalRequested bool `json:"terminal_requested"`

	// Org parameters, resolved upstream by precedence (spec §2).
	MarkupRate     decimal.Decimal  `json:"markup_rate"`
	MinMarkupUSD   *decimal.Decimal `json:"min_markup_usd,omitempty"`
	FXSafetySpread decimal.Decimal  `json:"fx_safety_spread"`
	FixedFees      []FixedFee       `json:"fixed_fees"`

	// Candidates pre-filtered by the worker (org, modal, locodes,
	// equipment, validity). Version/tombstone resolution is engine logic.
	Tariffs      []TariffCandidate `json:"tariffs"`
	Snapshots    []FeeBand         `json:"snapshots"`
	CarrierOffer *CarrierOffer     `json:"carrier_offer,omitempty"`

	FXRates   []FXRate  `json:"fx_rates"`
	QuoteDate time.Time `json:"quote_date"`
	PricedAt  time.Time `json:"priced_at"`
}

// TariffCandidate mirrors app.private_tariffs columns the engine needs,
// plus BatchActivatedAt joined from private_tariff_batches (spec §2).
type TariffCandidate struct {
	ID                uuid.UUID        `json:"id"`
	BatchID           uuid.UUID        `json:"batch_id"`
	CarrierOrAgent    string           `json:"carrier_or_agent"`
	Modal             Modal            `json:"modal"`
	OriginLOCODE      string           `json:"origin_locode"`
	DestLOCODE        string           `json:"dest_locode"`
	EquipmentOrUnit   string           `json:"equipment_or_unit"`
	ChargeCategory    ChargeCategory   `json:"charge_category"`
	ChargeCode        string           `json:"charge_code"`
	Currency          string           `json:"currency"`
	NetAmount         decimal.Decimal  `json:"net_amount"`
	MinAmount         *decimal.Decimal `json:"min_amount,omitempty"`
	SearchDescription string           `json:"search_description"`
	ValidFrom         time.Time        `json:"valid_from"`
	ValidUntil        time.Time        `json:"valid_until"`
	IsRemoved         bool             `json:"is_removed"`
	CreatedAt         time.Time        `json:"created_at"`
	// BatchActivatedAt is NULL while the batch is PENDING_REVIEW; nil
	// rows are never eligible (spec §4 step 3).
	BatchActivatedAt *time.Time `json:"batch_activated_at,omitempty"`
}

// FeeBand is one terminal/storage band decoded from a pinned public
// snapshot: Location matches DestLOCODE and the CIF range brackets the
// quote's CIF base; weight/volume bounds apply when set (spec §4 step 5).
type FeeBand struct {
	SnapshotID  uuid.UUID        `json:"snapshot_id"`
	Location    string           `json:"location"`
	CIFMin      decimal.Decimal  `json:"cif_min"`
	CIFMax      decimal.Decimal  `json:"cif_max"`
	WeightMinKg *decimal.Decimal `json:"weight_min_kg,omitempty"`
	VolumeMinM3 *decimal.Decimal `json:"volume_min_m3,omitempty"`
	// PeriodDays identifies the storage period; only the first period is
	// priced in the MVP (Q9, STORAGE_FIRST_PERIOD_ONLY).
	PeriodDays int             `json:"period_days"`
	Amount     decimal.Decimal `json:"amount"`
	Currency   string          `json:"currency"`
}

// FixedFee is one org fixed-fee rule decoded from fixed_fees_json;
// Category scopes the quote group it feeds (spec §4 step 5).
type FixedFee struct {
	ChargeCode  string          `json:"charge_code"`
	Description string          `json:"description"`
	Category    Category        `json:"category"`
	Amount      decimal.Decimal `json:"amount"`
	Currency    string          `json:"currency"`
}

// CarrierOffer is the validated carrier (Hapag) payload pinned before
// pricing so a quote stays reproducible (FCL only, spec §2).
type CarrierOffer struct {
	ID        uuid.UUID     `json:"id"`
	Carrier   string        `json:"carrier"`
	Charges   []OfferCharge `json:"charges"`
	ExpiresAt time.Time     `json:"expires_at"`
}

// OfferCharge is one charge line inside a carrier offer.
type OfferCharge struct {
	Category        ChargeCategory  `json:"category"`
	Code            string          `json:"code"`
	Amount          decimal.Decimal `json:"amount"`
	Currency        string          `json:"currency"`
	EquipmentOrUnit string          `json:"equipment_or_unit"`
}

// validate implements spec §4 step 1: every programmer/config error
// (negative rates or cargo, contradictory container counts, missing
// currencies, unknown enums) rejects with an error, never INCOMPLETE.
func (in Input) validate() error {
	if err := in.validateEnums(); err != nil {
		return err
	}
	if err := in.validateNumbers(); err != nil {
		return err
	}
	if err := in.validateCurrencies(); err != nil {
		return err
	}
	return in.validateCandidates()
}

func (in Input) validateEnums() error {
	switch in.Direction {
	case DirectionImport, DirectionExport:
	default:
		return fmt.Errorf("pricing: unknown direction %q", in.Direction)
	}
	switch in.Modal {
	case ModalFCL, ModalLCL, ModalAIR:
	default:
		return fmt.Errorf("pricing: unknown modal %q", in.Modal)
	}
	if !in.Incoterm.known() {
		return fmt.Errorf("pricing: unknown incoterm %q", in.Incoterm)
	}
	return nil
}

func (in Input) validateNumbers() error {
	if err := in.checkNonNegative(); err != nil {
		return err
	}
	return in.checkRates()
}

func (in Input) checkNonNegative() error {
	for _, c := range []struct {
		name string
		v    decimal.Decimal
	}{
		{"gross kg", in.GrossKg},
		{"volume m3", in.VolumeM3},
		{"cargo value", in.CargoValue},
	} {
		if c.v.IsNegative() {
			return fmt.Errorf("pricing: negative %s %s", c.name, c.v)
		}
	}
	return nil
}

// checkRates bounds the resolved org rates to their schema CHECK
// domains: markup and the FX safety spread are both NUMERIC fractions.
func (in Input) checkRates() error {
	if in.MarkupRate.IsNegative() || in.MarkupRate.GreaterThan(decimal.NewFromInt(1)) {
		return fmt.Errorf("pricing: markup rate %s outside [0,1]", in.MarkupRate)
	}
	if in.FXSafetySpread.IsNegative() || in.FXSafetySpread.GreaterThan(decimal.NewFromInt(1)) {
		return fmt.Errorf("pricing: fx safety spread %s outside [0,1]", in.FXSafetySpread)
	}
	if in.MinMarkupUSD != nil && in.MinMarkupUSD.IsNegative() {
		return fmt.Errorf("pricing: negative min markup usd %s", *in.MinMarkupUSD)
	}
	return in.validateContainers()
}

func (in Input) validateContainers() error {
	if in.ContainerCount < 0 {
		return fmt.Errorf("pricing: negative container count %d", in.ContainerCount)
	}
	if in.Modal == ModalFCL && in.ContainerCount < 1 {
		return fmt.Errorf("pricing: FCL requires container count >= 1, got %d", in.ContainerCount)
	}
	if in.Modal == ModalAIR && in.ContainerCount > 0 {
		return fmt.Errorf("pricing: AIR takes no container count, got %d", in.ContainerCount)
	}
	return nil
}

func (in Input) validateCurrencies() error {
	if !in.CargoValue.IsZero() && in.CargoCurrency == "" {
		return errors.New("pricing: cargo currency required when cargo value set")
	}
	if err := validateDeclared(in.DeclaredFreight, in.DeclaredInsurance); err != nil {
		return err
	}
	return in.validateRequired()
}

func validateDeclared(monies ...*Money) error {
	for _, m := range monies {
		if m == nil {
			continue
		}
		if m.Currency == "" {
			return errors.New("pricing: declared money requires a currency")
		}
		if m.Amount.IsNegative() {
			return fmt.Errorf("pricing: negative declared amount %s", m.Amount)
		}
	}
	return nil
}

func (in Input) validateRequired() error {
	if in.QuoteDate.IsZero() {
		return errors.New("pricing: quote date required")
	}
	if in.PricedAt.IsZero() {
		return errors.New("pricing: priced at required")
	}
	if in.OriginLOCODE == "" || in.DestLOCODE == "" {
		return errors.New("pricing: origin and dest locode required")
	}
	return nil
}

func (in Input) validateCandidates() error {
	if err := validateEach("tariff", in.Tariffs, TariffCandidate.validate); err != nil {
		return err
	}
	if err := validateEach("fixed fee", in.FixedFees, FixedFee.validate); err != nil {
		return err
	}
	if err := validateEach("fee band", in.Snapshots, FeeBand.validate); err != nil {
		return err
	}
	if in.CarrierOffer != nil {
		if err := in.CarrierOffer.validate(); err != nil {
			return fmt.Errorf("pricing: carrier offer: %w", err)
		}
	}
	return in.validateFXRates()
}

// validateEach runs a row validator over a candidate slice, labeling
// errors with the row kind and index.
func validateEach[T any](what string, rows []T, check func(T) error) error {
	for i, r := range rows {
		if err := check(r); err != nil {
			return fmt.Errorf("pricing: %s %d: %w", what, i, err)
		}
	}
	return nil
}

func (r TariffCandidate) validate() error {
	if err := r.validateAmounts(); err != nil {
		return err
	}
	return r.validateShape()
}

func (r TariffCandidate) validateAmounts() error {
	if r.Currency == "" {
		return errors.New("currency required")
	}
	if r.NetAmount.IsNegative() {
		return fmt.Errorf("negative net amount %s", r.NetAmount)
	}
	if r.MinAmount != nil && r.MinAmount.IsNegative() {
		return fmt.Errorf("negative min amount %s", *r.MinAmount)
	}
	return nil
}

func (r TariffCandidate) validateShape() error {
	if r.ChargeCode == "" {
		return errors.New("charge code required")
	}
	switch r.ChargeCategory {
	case ChargeOceanFreight, ChargeAirFreight, ChargeSurcharge, ChargeOriginCharge,
		ChargeDestinationCharge, ChargeTerminal, ChargeCustoms, ChargeOther:
	default:
		return fmt.Errorf("unknown charge category %q", r.ChargeCategory)
	}
	if r.ValidFrom.IsZero() || r.ValidUntil.IsZero() {
		return errors.New("valid_from and valid_until required")
	}
	if r.ValidFrom.After(r.ValidUntil) {
		return fmt.Errorf("valid_from %s after valid_until %s", r.ValidFrom, r.ValidUntil)
	}
	return nil
}

func (f FixedFee) validate() error {
	if f.Currency == "" {
		return errors.New("currency required")
	}
	if f.Amount.IsNegative() {
		return fmt.Errorf("negative amount %s", f.Amount)
	}
	if f.ChargeCode == "" {
		return errors.New("charge code required")
	}
	switch f.Category {
	case CategoryIntlFreight, CategoryOriginCharges, CategoryTerminalStorage,
		CategoryCustomsDispatch, CategorySurcharge:
		return nil
	default:
		return fmt.Errorf("unknown category %q", f.Category)
	}
}

func (b FeeBand) validate() error {
	if b.Location == "" {
		return errors.New("location required")
	}
	if b.Currency == "" {
		return errors.New("currency required")
	}
	if b.Amount.IsNegative() || b.CIFMin.IsNegative() || b.CIFMax.IsNegative() {
		return fmt.Errorf("negative band field: cif [%s,%s] amount %s", b.CIFMin, b.CIFMax, b.Amount)
	}
	if b.CIFMin.GreaterThan(b.CIFMax) {
		return fmt.Errorf("cif_min %s above cif_max %s", b.CIFMin, b.CIFMax)
	}
	if b.SnapshotID == uuid.Nil {
		return errors.New("snapshot id required")
	}
	return nil
}

func (o CarrierOffer) validate() error {
	if o.ID == uuid.Nil {
		return errors.New("offer id required")
	}
	for i, c := range o.Charges {
		if c.Currency == "" {
			return fmt.Errorf("charge %d: currency required", i)
		}
		if c.Amount.IsNegative() {
			return fmt.Errorf("charge %d: negative amount %s", i, c.Amount)
		}
		switch c.Category {
		case ChargeOceanFreight, ChargeAirFreight, ChargeSurcharge, ChargeOriginCharge,
			ChargeDestinationCharge, ChargeTerminal, ChargeCustoms, ChargeOther:
		default:
			return fmt.Errorf("charge %d: unknown category %q", i, c.Category)
		}
	}
	return nil
}

func (in Input) validateFXRates() error {
	for i, r := range in.FXRates {
		if r.Currency == "" {
			return fmt.Errorf("pricing: fx rate %d: currency required", i)
		}
		switch r.Source {
		case FXSourceCarrier, FXSourcePTAX, FXSourceCustoms:
		default:
			return fmt.Errorf("pricing: fx rate %d: unknown source %q", i, r.Source)
		}
		if r.Date.IsZero() {
			return fmt.Errorf("pricing: fx rate %d: date required", i)
		}
		if !r.Rate.IsPositive() {
			return fmt.Errorf("pricing: fx rate %d: non-positive rate %s", i, r.Rate)
		}
	}
	return nil
}
