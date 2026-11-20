package webapi

// htmlToText proofs: the scanner must never leak markup to the wire, CSS
// inside <style>/<script> must never survive, block markup becomes the
// paragraph breaks paraSplit reads, and entities unescape once — an
// HTML-only inbound email renders as real paragraphs, not an empty page.

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
)

// TestHTMLToText: markup stripped, entities unescaped, block tags are
// paragraph boundaries, br a line break — and a stray literal '<' is
// text, not a parse error.
func TestHTMLToText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"inline tags stripped",
			`<p>Hello <b>world</b>, <a href="x">link</a></p>`,
			"\n\nHello world, link\n\n",
		},
		{
			"block tags are breaks",
			`<div>one</div><div>two</div><p>three</p>`,
			// Open and close both mark the boundary — paragraphs() drops
			// the empty paragraphs the doubled breaks would make.
			"\n\none\n\n\n\ntwo\n\n\n\nthree\n\n",
		},
		{
			"br is a line break",
			`line one<br>line two<br/>line three`,
			"line one\nline two\nline three",
		},
		{
			"entities unescaped once",
			`<p>Fish &amp; Chips&nbsp;Co &lt;ok&gt;</p>`,
			"\n\nFish & Chips\u00a0Co <ok>\n\n",
		},
		{
			"literal less-than survives",
			`price < 500`,
			"price < 500",
		},
		{
			"unterminated tag is literal",
			`cost <b`,
			"cost <b",
		},
		{
			"closing tag still marks its boundary",
			`text</p>tail`,
			"text\n\ntail",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, htmlToText(tc.in))
		})
	}
}

// TestHTMLToTextHiddenBlocks: script and style contents — the CSS every
// HTML email carries — are dropped outright, open or not, case aside.
func TestHTMLToTextHiddenBlocks(t *testing.T) {
	in := `<html><head><style>body { color: red; } p > a { color: blue }</style></head>` +
		`<body><p>Real quote text</p><script>alert("x")</script></body></html>`
	got := htmlToText(in)
	assert.NotContains(t, got, "color", "style contents must never reach the wire")
	assert.NotContains(t, got, "alert", "script contents must never reach the wire")
	assert.NotContains(t, got, "<", "raw markup must never reach the wire")
	assert.Contains(t, got, "Real quote text")

	// An unclosed style block swallows the rest — a broken tag cannot
	// leak markup either.
	in = `<p>hi</p><style>unclosed`
	assert.Equal(t, "\n\nhi\n\n", htmlToText(in))
}

// TestHTMLToTextParagraphs: the emitted breaks feed paragraphs() so an
// HTML-only body segments exactly like a plain-text one.
func TestHTMLToTextParagraphs(t *testing.T) {
	src := `<div>Please quote <b>FCL</b> from Santos.</div><div>Need 2x40HC.</div>`
	text := htmlToText(src)
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
	require.Len(t, sections[0].Items, 2, "each line appears exactly once")
	assert.Equal(t, authapi.Money{Amount: "575", Currency: "BRL"}, sections[0].Items[0].Amount)
	assert.Equal(t, authapi.Money{Amount: "200", Currency: "USD"}, sections[0].Items[1].Amount)
}
