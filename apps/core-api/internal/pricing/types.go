// Package pricing is the deterministic pricing engine: it turns one
// extracted RFQ plus resolved org parameters, tariff candidates, pinned
// snapshots and FX rates into the exact quote lines a customer sees.
//
// Architecture rules 5 and 6 (AGENTS.md): the package is pure — no I/O,
// no clock reads, no randomness, no map iteration reaching Output — and
// every money operation uses shopspring/decimal. A float here is a bug.
//
// Governing spec: docs/specs/2026-11-10-pricing-engine-v1.3.md.
package pricing

import (
	"time"

	"github.com/shopspring/decimal"
)

// Direction mirrors the extraction contract (RFC-004).
type Direction string

const (
	DirectionImport Direction = "IMPORT"
	DirectionExport Direction = "EXPORT"
)

// Modal mirrors app.freight_modal. LCL is a valid enum value but outside
// the MVP (spec §3): it prices as INCOMPLETE, never as an error.
type Modal string

const (
	ModalFCL Modal = "FCL"
	ModalLCL Modal = "LCL"
	ModalAIR Modal = "AIR"
)

// Incoterm mirrors Incoterms 2020, all eleven values.
type Incoterm string

const (
	IncotermEXW Incoterm = "EXW"
	IncotermFCA Incoterm = "FCA"
	IncotermFAS Incoterm = "FAS"
	IncotermFOB Incoterm = "FOB"
	IncotermCFR Incoterm = "CFR"
	IncotermCIF Incoterm = "CIF"
	IncotermCPT Incoterm = "CPT"
	IncotermCIP Incoterm = "CIP"
	IncotermDAP Incoterm = "DAP"
	IncotermDPU Incoterm = "DPU"
	IncotermDDP Incoterm = "DDP"
)

// known reports whether the value is one of the eleven Incoterms 2020.
func (i Incoterm) known() bool {
	switch i {
	case IncotermEXW, IncotermFCA, IncotermFAS, IncotermFOB, IncotermCFR,
		IncotermCIF, IncotermCPT, IncotermCIP, IncotermDAP, IncotermDPU,
		IncotermDDP:
		return true
	default:
		return false
	}
}

// Category mirrors app.quote_line_category: the groups a quote line can
// belong to, also used to sort Output.Lines (asc, then charge_code).
type Category string

const (
	CategoryIntlFreight     Category = "INTL_FREIGHT"
	CategoryOriginCharges   Category = "ORIGIN_CHARGES"
	CategoryTerminalStorage Category = "TERMINAL_STORAGE_THC"
	CategoryCustomsDispatch Category = "CUSTOMS_DISPATCH"
	CategorySurcharge       Category = "SURCHARGE"
)

// ChargeCategory mirrors app.charge_category on private tariff rows and
// carrier offer charges: what the money is for, before the engine maps
// it to a quote line group.
type ChargeCategory string

const (
	ChargeOceanFreight      ChargeCategory = "OCEAN_FREIGHT"
	ChargeAirFreight        ChargeCategory = "AIR_FREIGHT"
	ChargeSurcharge         ChargeCategory = "SURCHARGE"
	ChargeOriginCharge      ChargeCategory = "ORIGIN_CHARGE"
	ChargeDestinationCharge ChargeCategory = "DESTINATION_CHARGE"
	ChargeTerminal          ChargeCategory = "TERMINAL"
	ChargeCustoms           ChargeCategory = "CUSTOMS"
	ChargeOther             ChargeCategory = "OTHER"
)

// SourceType mirrors app.line_source_type.
type SourceType string

const (
	SourcePrivateTariff   SourceType = "PRIVATE_TARIFF"
	SourceFirecrawlPublic SourceType = "FIRECRAWL_PUBLIC"
	SourceOrgFixedRule    SourceType = "ORG_FIXED_RULE"
	SourceManualOverride  SourceType = "MANUAL_OVERRIDE"
	SourceCarrierOffer    SourceType = "CARRIER_OFFER"
)

// Status is the engine verdict: COMPLETE means every required group was
// priced; INCOMPLETE carries machine-readable Reasons (spec §2).
type Status string

const (
	StatusComplete   Status = "COMPLETE"
	StatusIncomplete Status = "INCOMPLETE"
)

// Reason is a machine-readable gap code (spec §9). Parameterised codes
// embed their subject: MISSING_TARIFF_GROUP:INTL_FREIGHT, MISSING_FX:USD.
type Reason string

const (
	ReasonMissingTerminalBand Reason = "MISSING_TERMINAL_BAND"
	ReasonMissingCargoValue   Reason = "MISSING_CARGO_VALUE"
	ReasonMissingCustomsFX    Reason = "MISSING_CUSTOMS_FX"
	ReasonNoUsableOffer       Reason = "NO_USABLE_OFFER"
	ReasonExportMatrixOpen    Reason = "EXPORT_MATRIX_OPEN"
	ReasonSeaIncotermOnAir    Reason = "SEA_INCOTERM_ON_AIR"
	ReasonModalOutOfMVP       Reason = "MODAL_OUT_OF_MVP"
)

// reasonMissingTariffGroup reports a required group that produced no
// line. what is the line category or freight charge category (spec §9).
func reasonMissingTariffGroup(what string) Reason {
	return Reason("MISSING_TARIFF_GROUP:" + what)
}

// reasonMissingFX reports a currency with no resolvable rate.
func reasonMissingFX(currency string) Reason {
	return Reason("MISSING_FX:" + currency)
}

// Assumption records an open-question default the engine applied, so the
// audit view never shows a silent choice (spec §9).
type Assumption string

const (
	AssumptionImportMatrixOnly       Assumption = "IMPORT_MATRIX_ONLY"
	AssumptionStorageFirstPeriodOnly Assumption = "STORAGE_FIRST_PERIOD_ONLY"
	AssumptionCustomsRateUnconfirmed Assumption = "CUSTOMS_RATE_UNCONFIRMED"
	AssumptionPTAXPrevBusinessDay    Assumption = "PTAX_PREV_BUSINESS_DAY_SELL"
	AssumptionNoInsuranceLine        Assumption = "NO_INSURANCE_LINE"
	AssumptionAirFactor              Assumption = "AIR_FACTOR_166_67"
	AssumptionTaxesExcluded          Assumption = "TAXES_EXCLUDED"
	AssumptionDDPNothingPayable      Assumption = "DDP_NOTHING_PAYABLE"
	AssumptionOfferStrictCoverage    Assumption = "OFFER_STRICT_COVERAGE"
)

// Line flag vocabulary (spec §9), applied to Line.Flags.
const (
	FlagFXFallback       = "FX_FALLBACK"
	FlagStaleFX          = "STALE_FX"
	FlagMinMarkupApplied = "MIN_MARKUP_APPLIED"
	FlagOfferExpired     = "OFFER_EXPIRED"
	FlagDeclaredBasis    = "DECLARED_BASIS"
)

// FX source keys the worker writes on FXRate rows (spec §6).
const (
	FXSourceCarrier = "CARRIER"
	FXSourcePTAX    = "PTAX"
	FXSourceCustoms = "CUSTOMS"
)

// Money is an amount in a specific ISO-4217 currency.
type Money struct {
	Amount   decimal.Decimal `json:"amount"`
	Currency string          `json:"currency"`
}

// FXRate is one pinned exchange rate, keyed by (Source, Currency, Date).
// Rate converts one unit of Currency into BRL (NUMERIC(10,6)).
type FXRate struct {
	Source   string          `json:"source"`
	Currency string          `json:"currency"`
	Date     time.Time       `json:"date"`
	Rate     decimal.Decimal `json:"rate"`
}
