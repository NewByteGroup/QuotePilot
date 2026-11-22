package platform_test

// HTMLToText proofs: the scanner must never leak markup to the wire, CSS
// inside <style>/<script> must never survive, block markup becomes
// paragraph breaks, and entities unescape once — an HTML-only inbound
// email renders as real text for both the audit view and the ai-engine
// body_text.

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
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
			// Open and close both mark the boundary — downstream readers
			// drop the empty paragraphs the doubled breaks would make.
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
		{
			"less-than space is customer text",
			`I <3 freight and pay < 500 per box`,
			"I <3 freight and pay < 500 per box",
		},
		{
			"empty and digit tags are literal",
			`a<>b<3 kg>c`,
			"a<>b<3 kg>c",
		},
		{
			"closing slash then space is literal",
			`x</ y`,
			"x</ y",
		},
		{
			"doctype is skipped, not text",
			`<!DOCTYPE html>body`,
			"body",
		},
		{
			"xml prolog is skipped, not text",
			`<?xml version="1.0"?>body`,
			"body",
		},
		{
			"gt inside a quoted attribute",
			`<a href="http://x?a>b">link</a>`,
			"link",
		},
		{
			"unterminated quote eats the tag",
			`text<a href="unclosed`,
			"text",
		},
		{
			// A quote closing exactly at EOF is closed — the unfinished
			// tag falls back to literal text, not a discarded suffix.
			"quoted run closing at EOF is literal",
			`cost <b title="x"`,
			`cost <b title="x"`,
		},
		{
			"single-quoted run closing at EOF is literal",
			`cost <b title='x'`,
			`cost <b title='x'`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, platform.HTMLToText(tc.in))
		})
	}
}

// TestHTMLToTextLiteralLessThan: the review's audit repro — a customer
// constraint like "Weight < 500 kg" must survive the scan as text; the
// old parser read '< 500 kg</p' as one broken tag and the constraint
// vanished.
func TestHTMLToTextLiteralLessThan(t *testing.T) {
	got := platform.HTMLToText(`<p>Weight < 500 kg; need 2x40HC</p>`)
	assert.Contains(t, got, "Weight < 500 kg", "the '<' is literal text, not a tag opener")
	assert.Contains(t, got, "2x40HC", "the rest of the paragraph still renders")
}

// TestHTMLToTextComments: comment contents are dropped through the
// first '-->' — hidden markup must never reach the wire.
func TestHTMLToTextComments(t *testing.T) {
	got := platform.HTMLToText(`a<!-- <td>hidden</td> -->b`)
	assert.Equal(t, "ab", got)
	assert.NotContains(t, got, "hidden")

	assert.Equal(t, "a", platform.HTMLToText(`a<!-- never ends`),
		"an unterminated comment swallows the rest rather than leaking")
}

// TestHTMLToTextTableCells: td/th open tags emit a separating space so
// adjacent cells stay separate tokens — row boundaries still come from
// tr/table's paragraph breaks.
func TestHTMLToTextTableCells(t *testing.T) {
	got := platform.HTMLToText(`<table><tr><td>10</td><td>20</td></tr><tr><th>a</th><th>b</th></tr></table>`)
	assert.Contains(t, got, "10 20", "adjacent cells must not merge into one token")
	assert.NotContains(t, got, "1020")
	assert.Contains(t, got, "a b")
}

// TestHTMLToTextHiddenBlocks: script and style contents — the CSS every
// HTML email carries — are dropped outright, open or not, case aside.
func TestHTMLToTextHiddenBlocks(t *testing.T) {
	in := `<html><head><style>body { color: red; } p > a { color: blue }</style></head>` +
		`<body><p>Real quote text</p><script>alert("x")</script></body></html>`
	got := platform.HTMLToText(in)
	assert.NotContains(t, got, "color", "style contents must never reach the wire")
	assert.NotContains(t, got, "alert", "script contents must never reach the wire")
	assert.NotContains(t, got, "<", "raw markup must never reach the wire")
	assert.Contains(t, got, "Real quote text")

	// An unclosed style block swallows the rest — a broken tag cannot
	// leak markup either.
	in = `<p>hi</p><style>unclosed`
	assert.Equal(t, "\n\nhi\n\n", platform.HTMLToText(in))
}
