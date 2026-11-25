package jobs

// White-box unit tests for the FX helpers: the currency derivation, the
// latest-at-or-before pick and the cargo_cif_brl computation — the
// pieces real Postgres cannot reach without rows.

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
)

// TestNeededCurrencies: the union is the three extracted currency
// fields (BRL excluded) plus the USD/EUR constants — sorted,
// deduplicated, malformed values ignored.
func TestNeededCurrencies(t *testing.T) {
	fields := map[string]extractedField{
		"cargo_value_currency":         {Value: "usd"},
		"declared_freight_currency":    {Value: "BRL"},
		"declared_insurance_currency":  {Value: "JPY"},
		"modal":                        {Value: "FCL"},
		"not_a_currency_but_confusing": {Value: "EUR"},
	}
	got := neededCurrencies(fields)
	assert.Equal(t, []string{"EUR", "JPY", "USD"}, got,
		"JPY joins the USD/EUR constants; BRL is skipped; only the three keys are read")
}

// TestNeededCurrenciesNoKeys: an extraction with no currency keys still
// warms USD+EUR — the constants exist because tariff/offer payloads are
// DB-side and overwhelmingly those currencies.
func TestNeededCurrenciesNoKeys(t *testing.T) {
	got := neededCurrencies(map[string]extractedField{"modal": {Value: "FCL"}})
	assert.Equal(t, []string{"EUR", "USD"}, got)
}

// TestNeededCurrenciesMalformed: a non-ISO value in a currency key is
// ignored here — the main path parks it as REQUIRED_FIELD_MISSING, the
// peek must not turn it into a bogus fetch.
func TestNeededCurrenciesMalformed(t *testing.T) {
	fields := map[string]extractedField{
		"cargo_value_currency": {Value: "dollars"},
	}
	assert.Equal(t, []string{"EUR", "USD"}, neededCurrencies(fields))
}

// cifInput is the minimum Input shape cargoCIFBRL reads.
func cifInput(cargo, cargoCcy string, freight, insurance *pricing.Money, rates []pricing.FXRate) pricing.Input {
	return pricing.Input{
		CargoValue:        decimal.RequireFromString(cargo),
		CargoCurrency:     cargoCcy,
		DeclaredFreight:   freight,
		DeclaredInsurance: insurance,
		FXRates:           rates,
		QuoteDate:         time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC),
	}
}

func money(amount, ccy string) *pricing.Money {
	return &pricing.Money{Amount: decimal.RequireFromString(amount), Currency: ccy}
}

func fxRate(source, ccy, rate, day string) pricing.FXRate {
	d, _ := time.Parse(isoDate, day)
	return pricing.FXRate{Source: source, Currency: ccy, Date: d,
		Rate: decimal.RequireFromString(rate)}
}

// TestCargoCIFBRLHappy: all three declared components convert at the
// CUSTOMS rate newest-at-or-before QuoteDate — the engine's own CIF
// rule (spec §6), not the carrier path.
func TestCargoCIFBRLHappy(t *testing.T) {
	in := cifInput("10000", "USD", money("1200", "USD"), money("300", "USD"),
		[]pricing.FXRate{
			fxRate(pricing.FXSourceCustoms, "USD", "5.000000", "2026-03-10"),
			fxRate(pricing.FXSourceCustoms, "USD", "5.500000", "2026-03-14"),
			fxRate(pricing.FXSourceCustoms, "USD", "9.900000", "2026-03-20"), // after quote date: untouched
			fxRate(pricing.FXSourcePTAX, "USD", "9.000000", "2026-03-14"),    // wrong source: untouched
		})
	got, ok := cargoCIFBRL(in)
	assert.True(t, ok)
	// (10000 + 1200 + 300) × 5.5 = 63250.00 — the 03-14 customs rate,
	// never the PTAX row of the same date.
	assert.Equal(t, "63250", got.String())
}

// TestCargoCIFBRLMixedCurrencies: each component resolves its own
// currency's customs rate; BRL needs none.
func TestCargoCIFBRLMixedCurrencies(t *testing.T) {
	in := cifInput("10000", "USD", money("100", "BRL"), money("600", "EUR"),
		[]pricing.FXRate{
			fxRate(pricing.FXSourceCustoms, "USD", "5.000000", "2026-03-14"),
			fxRate(pricing.FXSourceCustoms, "EUR", "6.000000", "2026-03-14"),
		})
	got, ok := cargoCIFBRL(in)
	assert.True(t, ok)
	// 10000×5 + 100 + 600×6 = 53700.00
	assert.Equal(t, "53700", got.String())
}

// TestCargoCIFBRLAbsentComponent: any absent component — cargo value,
// declared freight or declared insurance — leaves NULL (never a
// partial sum).
func TestCargoCIFBRLAbsentComponent(t *testing.T) {
	rates := []pricing.FXRate{fxRate(pricing.FXSourceCustoms, "USD", "5.0", "2026-03-14")}
	for _, in := range []pricing.Input{
		cifInput("0", "USD", money("1200", "USD"), money("300", "USD"), rates),
		cifInput("10000", "USD", nil, money("300", "USD"), rates),
		cifInput("10000", "USD", money("1200", "USD"), nil, rates),
	} {
		got, ok := cargoCIFBRL(in)
		assert.False(t, ok)
		assert.True(t, got.IsZero())
	}
}

// TestCargoCIFBRLMissingRate: a component whose customs rate is absent
// (or too new) leaves NULL — an unresolvable rate is never fabricated.
func TestCargoCIFBRLMissingRate(t *testing.T) {
	in := cifInput("10000", "USD", money("1200", "USD"), money("300", "USD"),
		[]pricing.FXRate{
			fxRate(pricing.FXSourceCustoms, "USD", "5.0", "2026-03-20"), // after quote date
			fxRate(pricing.FXSourcePTAX, "USD", "5.0", "2026-03-14"),    // wrong source
		})
	_, ok := cargoCIFBRL(in)
	assert.False(t, ok)
}

// TestCargoCIFBRLKnownZero: a zero-valued declared Money is a known
// zero — it resolves no rate, mirroring cifAcc.add (spec §6).
func TestCargoCIFBRLKnownZero(t *testing.T) {
	in := cifInput("10000", "USD", money("0", "EUR"), money("300", "USD"),
		[]pricing.FXRate{fxRate(pricing.FXSourceCustoms, "USD", "5.0", "2026-03-14")})
	got, ok := cargoCIFBRL(in)
	assert.True(t, ok, "EUR freight is a known zero — its missing rate does not NULL the column")
	// 10000×5 + 0 + 300×5 = 51500.00
	assert.Equal(t, "51500", got.String())
}

// TestLatestRate mirrors the resolver contract directly: newest
// at-or-before, day granularity, other sources/currencies invisible.
func TestLatestRate(t *testing.T) {
	rates := []pricing.FXRate{
		fxRate(pricing.FXSourcePTAX, "USD", "5.0", "2026-03-10"),
		fxRate(pricing.FXSourcePTAX, "USD", "5.2", "2026-03-12"),
		fxRate(pricing.FXSourcePTAX, "USD", "5.4", "2026-03-16"),
		fxRate(pricing.FXSourceCustoms, "USD", "9.9", "2026-03-12"),
	}
	max := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	r, ok := latestRate(rates, pricing.FXSourcePTAX, "USD", max)
	assert.True(t, ok)
	assert.Equal(t, "5.2", r.Rate.String())

	_, ok = latestRate(rates, pricing.FXSourcePTAX, "EUR", max)
	assert.False(t, ok)
	_, ok = latestRate(rates, pricing.FXSourcePTAX, "USD",
		time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC))
	assert.True(t, ok, "the 03-10 row is at-or-before 03-10")
}
