package webapi

// emailBody/paragraphs proofs: an HTML-only inbound email renders as real
// paragraphs — reduced to text by platform.HTMLToText (its own scanner
// proofs live in internal/platform) — instead of an empty page.

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
)

// TestHTMLToTextParagraphs: the emitted breaks feed paragraphs() so an
// HTML-only body segments exactly like a plain-text one.
func TestHTMLToTextParagraphs(t *testing.T) {
	src := `<div>Please quote <b>FCL</b> from Santos.</div><div>Need 2x40HC.</div>`
	text := platform.HTMLToText(src)
	paras := paragraphs(&text, []evidenceSpan{{quote: "2x40hc", inferred: true}})
	require.Len(t, paras, 2)
	require.Len(t, paras[0], 1)
	assert.Equal(t, "Please quote FCL from Santos.", paras[0][0].Text)
	require.Len(t, paras[1], 3)
	assert.Equal(t, authapi.Inferred, paras[1][1].Highlight)
	assert.Equal(t, "2x40HC", paras[1][1].Text)
}

// TestEmailBody: plain text wins when present; an empty or absent text
// part falls back to the HTML part reduced to text; neither is nil.
func TestEmailBody(t *testing.T) {
	text := "plain body"
	htmlBody := "<p>html <b>body</b></p>"

	got := emailBody(detailRow{rawText: &text})
	require.NotNil(t, got)
	assert.Equal(t, "plain body", *got, "the text part wins outright")

	got = emailBody(detailRow{rawHTML: &htmlBody})
	require.NotNil(t, got)
	assert.Equal(t, "\n\nhtml body\n\n", *got)

	empty := ""
	got = emailBody(detailRow{rawText: &empty, rawHTML: &htmlBody})
	require.NotNil(t, got)
	assert.Equal(t, "\n\nhtml body\n\n", *got, "an empty text part still falls back to HTML")

	assert.Nil(t, emailBody(detailRow{}), "no bodies is a nil body, not a converted one")
}

// TestAppliedFx: the applied rate is base×(1+spread), the reference is
// the pinned base labelled by its real source — and an empty list is a
// null fx block.
func TestAppliedFx(t *testing.T) {
	out := pricing.Output{FXApplied: []pricing.FXApplied{{
		Source: "CARRIER", Currency: "USD",
		Rate:   decimal.RequireFromString("5.32"),
		Spread: decimal.RequireFromString("0.05"),
	}}}
	fx := appliedFx(out)
	require.NotNil(t, fx)
	assert.Equal(t, "CARRIER USD", fx.Label)
	assert.Equal(t, "5.586", fx.Rate, "applied = 5.32 × 1.05")
	assert.Equal(t, "5.32", fx.ReferenceRate, "the reference is the pinned base")
	assert.Equal(t, "CARRIER", fx.ReferenceLabel, "the label is the real source, not a fabricated PTAX")

	out.FXApplied[0].Spread = decimal.Zero
	fx = appliedFx(out)
	assert.Equal(t, "5.32", fx.Rate, "zero spread means base == applied")

	assert.Nil(t, appliedFx(pricing.Output{}), "no applied rates is a null fx")
}

// TestPriceSectionsMixedCurrency: Groups is per (group, currency) — a
// group priced in two currencies emits ONE section, each line exactly
// once, currency per item.
func TestPriceSectionsMixedCurrency(t *testing.T) {
	out := pricing.Output{
		Groups: []pricing.GroupTotal{
			{Group: pricing.GroupLocalFees, Currency: "BRL", Sell: decimal.NewFromInt(575)},
			{Group: pricing.GroupLocalFees, Currency: "USD", Sell: decimal.NewFromInt(200)},
		},
		Lines: []pricing.Line{
			{Group: pricing.GroupLocalFees, ChargeCode: "THC", Description: "Terminal handling",
				Currency: "BRL", SellOriginal: decimal.NewFromInt(575)},
			{Group: pricing.GroupLocalFees, ChargeCode: "DOC", Description: "Documentation",
				Currency: "USD", SellOriginal: decimal.NewFromInt(200)},
		},
	}
	sections := priceSections(out)
	require.Len(t, sections, 1, "one section per distinct group — never one per currency")
	assert.Equal(t, "Taxas locais", sections[0].Title)
	require.Len(t, sections[0].Items, 2, "each line appears exactly once, currency per item")
	assert.Equal(t, authapi.Money{Amount: "575", Currency: "BRL"}, sections[0].Items[0].Amount)
	assert.Equal(t, authapi.Money{Amount: "200", Currency: "USD"}, sections[0].Items[1].Amount)
}
