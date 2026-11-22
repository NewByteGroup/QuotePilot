package jobs

// White-box unit tests for rfq_price's pure helpers — the pieces real
// Postgres cannot reach today (no extracted_rates_json decoder exists
// yet, so no COMPLETE quote can carry a tariff-sourced freight line;
// the never-silent-fallback invariant is proven here instead and the
// gap is flagged on the PR).

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/carrier"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
)

// tariffFreightLine is a tariff-sourced INTL_FREIGHT line — the shape the
// fallback provenance reads.
func tariffFreightLine(flags ...string) pricing.Line {
	return pricing.Line{
		Category:   pricing.CategoryIntlFreight,
		SourceType: pricing.SourcePrivateTariff,
		Flags:      flags,
	}
}

// TestComposeFallbackReasonMissing: freight owed + no OFFER_FOUND row
// supplied + tariff-sourced winner => "missing" — NO_OFFER, FETCH_FAILED
// and NOT_APPLICABLE all conflate here, as does an absent checkpoint.
func TestComposeFallbackReasonMissing(t *testing.T) {
	out := pricing.Output{Lines: []pricing.Line{tariffFreightLine()}}
	assert.Equal(t, fallbackMissing, composeFallbackReason(pricing.ModalFCL, false, out))
}

// TestComposeFallbackReasonExpired: the engine flags the tariff line
// that replaced an expired offer — the flag wins the reason (spec S3b:
// OFFER_EXPIRED => expired).
func TestComposeFallbackReasonExpired(t *testing.T) {
	out := pricing.Output{Lines: []pricing.Line{tariffFreightLine(pricing.FlagOfferExpired)}}
	assert.Equal(t, fallbackExpired, composeFallbackReason(pricing.ModalFCL, true, out))
	// An expired flag without a supplied offer cannot occur; the flag
	// still names the truth if it did.
	assert.Equal(t, fallbackExpired, composeFallbackReason(pricing.ModalFCL, false, out))
}

// TestComposeFallbackReasonIncomplete: a supplied offer that failed
// strict coverage (or could not be priced) falls back to tariffs —
// "incomplete" is never silent.
func TestComposeFallbackReasonIncomplete(t *testing.T) {
	out := pricing.Output{Lines: []pricing.Line{tariffFreightLine()}}
	assert.Equal(t, fallbackIncomplete, composeFallbackReason(pricing.ModalFCL, true, out))
}

// TestComposeFallbackReasonNeverSilent is the invariant itself: every
// combination that produces a tariff-sourced freight line yields a
// non-empty reason.
func TestComposeFallbackReasonNeverSilent(t *testing.T) {
	for _, supplied := range []bool{true, false} {
		out := pricing.Output{Lines: []pricing.Line{tariffFreightLine()}}
		assert.NotEmpty(t, composeFallbackReason(pricing.ModalFCL, supplied, out),
			"a tariff-sourced freight line must never be a silent fallback")
	}
}

// TestComposeFallbackReasonAirNeverFetches is the modal-blindness
// regression: S3a only fetches an offer when the customer owes FCL
// freight (spec S3a), so a tariff-sourced freight line on AIR is not a
// fallback — persisting "missing" would falsely claim an offer was owed.
func TestComposeFallbackReasonAirNeverFetches(t *testing.T) {
	out := pricing.Output{Lines: []pricing.Line{tariffFreightLine()}}
	for _, supplied := range []bool{true, false} {
		assert.Empty(t, composeFallbackReason(pricing.ModalAIR, supplied, out),
			"AIR never reaches the offer call — no fallback exists to name")
		assert.Empty(t, composeFallbackReason(pricing.ModalLCL, supplied, out))
	}
}

// TestComposeFallbackReasonNoFallback: an offer-sourced freight line (or
// no freight line at all) is not a fallback — the reason stays empty.
func TestComposeFallbackReasonNoFallback(t *testing.T) {
	offerLine := pricing.Line{Category: pricing.CategoryIntlFreight, SourceType: pricing.SourceCarrierOffer}
	assert.Empty(t, composeFallbackReason(pricing.ModalFCL, true, pricing.Output{Lines: []pricing.Line{offerLine}}))
	customsOnly := pricing.Line{Category: pricing.CategoryCustomsDispatch, SourceType: pricing.SourcePrivateTariff}
	assert.Empty(t, composeFallbackReason(pricing.ModalFCL, false, pricing.Output{Lines: []pricing.Line{customsOnly}}))
	assert.Empty(t, composeFallbackReason(pricing.ModalFCL, false, pricing.Output{}))
}

// TestDeriveDirection covers the BR-endpoint rule: BR destination =>
// IMPORT, BR origin => EXPORT, both/neither => ambiguous (RFC-001 Q15).
func TestDeriveDirection(t *testing.T) {
	cases := []struct {
		origin, dest string
		want         pricing.Direction
		ok           bool
	}{
		{"CNSHA", "BRSSZ", pricing.DirectionImport, true},
		{"BRSSZ", "CNSHA", pricing.DirectionExport, true},
		{"BRSSZ", "BRSSZ", "", false},
		{"CNSHA", "DEHAM", "", false},
	}
	for _, c := range cases {
		got, ok := deriveDirection(c.origin, c.dest)
		assert.Equal(t, c.ok, ok, "%s -> %s", c.origin, c.dest)
		assert.Equal(t, c.want, got, "%s -> %s", c.origin, c.dest)
	}
}

// TestRequiredLocode enforces the contract Locode shape after
// normalization — a prose value is unparsable, hence missing.
func TestRequiredLocode(t *testing.T) {
	fields := map[string]extractedField{
		"origin_locode": {Value: " cnsha "},
	}
	got, ok := requiredLocode(fields, "origin_locode")
	assert.True(t, ok)
	assert.Equal(t, "CNSHA", got, "locodes normalize upper+trim")

	_, ok = requiredLocode(fields, "dest_locode")
	assert.False(t, ok, "absent key is missing")
	fields["dest_locode"] = extractedField{Value: "the port of santos"}
	_, ok = requiredLocode(fields, "dest_locode")
	assert.False(t, ok, "a non-locode value is unparsable")
}

// TestRequiredContainerCount pins the FCL quantity contract: positive
// integral DecimalString; zero, negative, fractional and garbage all
// count as missing rather than reaching engine validation.
func TestRequiredContainerCount(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
		ok    bool
	}{
		{"2", 2, true},
		{" 3 ", 3, true},
		{"0", 0, false},
		{"-1", 0, false},
		{"2.5", 0, false},
		{"two", 0, false},
	} {
		fields := map[string]extractedField{"quantity": {Value: tc.value}}
		got, ok := requiredContainerCount(fields)
		assert.Equal(t, tc.ok, ok, "quantity %q", tc.value)
		assert.Equal(t, tc.want, got, "quantity %q", tc.value)
	}
}

// TestQuoteDateField: ready_date wins when present and parseable; the
// pricedAt date is the deterministic fallback — truncated to day
// granularity like the tariff validity windows.
func TestQuoteDateField(t *testing.T) {
	now := time.Date(2026, 3, 10, 15, 30, 0, 0, time.UTC)
	withReady := map[string]extractedField{"ready_date": {Value: "2026-03-15"}}
	assert.Equal(t, time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC), quoteDateField(withReady, now))
	assert.Equal(t, time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC),
		quoteDateField(map[string]extractedField{}, now), "absent ready_date falls back to pricedAt's day")
	bad := map[string]extractedField{"ready_date": {Value: "not-a-date"}}
	assert.Equal(t, time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC), quoteDateField(bad, now),
		"an unparseable ready_date is ignored, never guessed")
}

// TestRateRequestHashStableAcrossDays pins the Devin Review round-3
// contract: absent an extracted ready_date, rateRequest resolves the
// fallback off the RFQ's received_at — a stable per-entity timestamp —
// so the digest is identical no matter which day the worker runs or the
// reprocess recomputes. A different received_at day still re-keys the
// request (the date component stays sensitive).
func TestRateRequestHashStableAcrossDays(t *testing.T) {
	fields := map[string]extractedField{
		"incoterm":          {Value: "FOB"},
		"modal":             {Value: "FCL"},
		"origin_locode":     {Value: "CNSHA"},
		"dest_locode":       {Value: "BRSSZ"},
		"equipment_or_unit": {Value: "40HC"},
		"quantity":          {Value: "2"},
		// no ready_date — the fallback path under test
	}
	arrived := time.Date(2026, 3, 1, 8, 30, 0, 0, time.UTC)

	reqA, _ := rateRequest(fields, arrived)
	assert.Equal(t, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), reqA.ReadyDate,
		"the fallback resolves to the received_at day")

	// Same arrival day at a different clock time — same digest.
	reqB, _ := rateRequest(fields, arrived.Add(5*time.Hour))
	assert.Equal(t, carrier.RequestHash(reqA), carrier.RequestHash(reqB),
		"day truncation makes the hash insensitive to the time component")

	// A different arrival day re-keys the request — the date component
	// still participates, so the test is not vacuous.
	reqC, _ := rateRequest(fields, arrived.AddDate(0, 0, 1))
	assert.NotEqual(t, carrier.RequestHash(reqA), carrier.RequestHash(reqC),
		"a different received_at day yields a different hash")
}

// TestMissingGroupCategory maps the MISSING_TARIFF_GROUP parameter back
// to the private_tariffs charge_category the group draws from — freight
// is modal-dependent; pass-through groups map to their charge category.
func TestMissingGroupCategory(t *testing.T) {
	got, ok := missingGroupCategory(pricing.ModalFCL, string(pricing.CategoryIntlFreight))
	assert.True(t, ok)
	assert.Equal(t, pricing.ChargeOceanFreight, got)
	got, ok = missingGroupCategory(pricing.ModalAIR, string(pricing.ChargeAirFreight))
	assert.True(t, ok)
	assert.Equal(t, pricing.ChargeAirFreight, got)
	got, ok = missingGroupCategory(pricing.ModalFCL, string(pricing.CategoryOriginCharges))
	assert.True(t, ok)
	assert.Equal(t, pricing.ChargeOriginCharge, got)
	got, ok = missingGroupCategory(pricing.ModalFCL, string(pricing.CategoryCustomsDispatch))
	assert.True(t, ok)
	assert.Equal(t, pricing.ChargeCustoms, got)
	_, ok = missingGroupCategory(pricing.ModalFCL, "NOT_A_GROUP")
	assert.False(t, ok)
}

// TestBuildShipmentHappyFields: the assembled Input reads the extraction
// contract exactly — normalized locodes, derived direction, container
// count, ready_date as quote date, the flagged nil/empty candidates.
func TestBuildShipmentHappyFields(t *testing.T) {
	now := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	fields := map[string]extractedField{
		"incoterm":          {Value: "dap"},
		"modal":             {Value: "fcl"},
		"origin_locode":     {Value: "cnsha"},
		"dest_locode":       {Value: "brssz"},
		"equipment_or_unit": {Value: "40hc"},
		"quantity":          {Value: "2"},
		"ready_date":        {Value: "2026-03-15"},
	}
	v := buildShipment(fields, now)
	assert.Empty(t, string(v.park), "complete fields build, park empty")
	in := v.in
	assert.Equal(t, pricing.ModalFCL, in.Modal)
	assert.Equal(t, pricing.IncotermDAP, in.Incoterm)
	assert.Equal(t, "CNSHA", in.OriginLOCODE)
	assert.Equal(t, "BRSSZ", in.DestLOCODE)
	assert.Equal(t, pricing.DirectionImport, in.Direction)
	assert.Equal(t, "40HC", in.EquipmentOrUnit)
	assert.Equal(t, 2, in.ContainerCount)
	assert.Equal(t, time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC), in.QuoteDate)
	assert.Equal(t, now, in.PricedAt)
	assert.False(t, in.TerminalRequested, "terminal is never requested — not extracted")
	assert.Nil(t, in.Snapshots, "no snapshot decoder exists yet")
	assert.Empty(t, in.FXRates)
}

// TestBuildShipmentParkOrder: required fields are checked before
// direction ambiguity (the locodes must parse before direction can be
// derived), and both produce the documented park codes.
func TestBuildShipmentParkOrder(t *testing.T) {
	now := time.Now()
	missingModal := map[string]extractedField{
		"incoterm": {Value: "DAP"},
	}
	v := buildShipment(missingModal, now)
	assert.Equal(t, ReviewReasonRequiredFieldMissing, v.park)
	assert.Equal(t, "modal", v.field)

	bothBR := map[string]extractedField{
		"incoterm":          {Value: "DAP"},
		"modal":             {Value: "FCL"},
		"origin_locode":     {Value: "BRSSZ"},
		"dest_locode":       {Value: "BRRIO"},
		"equipment_or_unit": {Value: "40HC"},
		"quantity":          {Value: "1"},
	}
	v = buildShipment(bothBR, now)
	assert.Equal(t, ReviewReasonDirectionAmbiguous, v.park)
}

// TestBuildShipmentMalformedOptional: a present-but-unparsable optional
// numeric parks PRICING_INCOMPLETE — never silently zeroed.
func TestBuildShipmentMalformedOptional(t *testing.T) {
	now := time.Now()
	fields := map[string]extractedField{
		"incoterm":          {Value: "DAP"},
		"modal":             {Value: "FCL"},
		"origin_locode":     {Value: "CNSHA"},
		"dest_locode":       {Value: "BRSSZ"},
		"equipment_or_unit": {Value: "40HC"},
		"quantity":          {Value: "2"},
		"gross_weight_kg":   {Value: "heavy"},
	}
	v := buildShipment(fields, now)
	assert.Equal(t, ReviewReasonPricingIncomplete, v.park)
	assert.Equal(t, "gross_weight_kg", v.field)
}

// TestBuildShipmentDeclaredPair: a declared amount without its currency
// parks REQUIRED_FIELD_MISSING — a half-pair is never guessed.
func TestBuildShipmentDeclaredPair(t *testing.T) {
	now := time.Now()
	base := map[string]extractedField{
		"incoterm":          {Value: "DAP"},
		"modal":             {Value: "FCL"},
		"origin_locode":     {Value: "CNSHA"},
		"dest_locode":       {Value: "BRSSZ"},
		"equipment_or_unit": {Value: "40HC"},
		"quantity":          {Value: "2"},
	}
	fields := map[string]extractedField{}
	for k, v := range base {
		fields[k] = v
	}
	fields["declared_freight_amount"] = extractedField{Value: "1200.00"}
	v := buildShipment(fields, now)
	assert.Equal(t, ReviewReasonRequiredFieldMissing, v.park)
	assert.Equal(t, "declared_freight_currency", v.field)

	fields["declared_freight_currency"] = extractedField{Value: "usd"}
	v = buildShipment(fields, now)
	assert.Empty(t, string(v.park))
	if v.in.DeclaredFreight == nil {
		t.Fatal("a complete pair must build the Money")
	}
	assert.Equal(t, "USD", v.in.DeclaredFreight.Currency)
	assert.Equal(t, "1200", v.in.DeclaredFreight.Amount.String())
}
