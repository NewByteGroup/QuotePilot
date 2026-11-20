package webapi

// htmlToText reduces an email's raw_html_body to the plain text
// paragraphs() can segment — a byte-wise scanner, not a regex soup. A
// tag only exists when '<' is followed by an optional '/' and then an
// ASCII letter, so customer text like "Weight < 500 kg" or "I <3
// freight" survives verbatim instead of being eaten as broken markup.
// Comments, DOCTYPE/<??> declarations and quoted attribute values are
// skipped without emitting; <script>/<style> contents — the CSS HTML
// emails carry — must never reach the wire; block tags become the
// paragraph breaks paraSplit reads and open td/th cells a space so
// adjacent cells don't merge. Entities unescape once at the end. Raw
// HTML itself never reaches the response.
import (
	"html"
	"strings"
)

// blockTags are the tag names that mark a paragraph boundary in email
// markup — each emits \n\n, the separator paraSplit reads, open and
// closing alike (the doubled breaks collapse to empty paragraphs that
// paragraphs() drops). td/th are deliberately absent: they separate
// cells with a single space, and only on the open tag — the row's
// tr/table already carries the paragraph break.
var blockTags = map[string]bool{
	"p": true, "div": true, "li": true, "tr": true, "table": true,
	"ul": true, "ol": true, "blockquote": true, "section": true,
	"article": true, "header": true, "footer": true, "hr": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
}

// htmlToText strips src's markup to plain text: element contents are
// copied verbatim, block tags emit a paragraph break, br a line break,
// open table cells a separating space, script/style contents are
// dropped outright, and entities are unescaped once everything is
// assembled.
func htmlToText(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	for i := 0; i < len(src); {
		if src[i] != '<' {
			b.WriteByte(src[i])
			i++
			continue
		}
		adv := emitTag(&b, src, i)
		i += adv
	}
	return html.UnescapeString(b.String())
}

// emitTag consumes one '<' at src[at] and returns bytes consumed. The
// token rules, in order: comments ('<!--') vanish through the first
// '-->' (unterminated swallows the rest — broken markup cannot leak
// either way); '<!'/ '<?' declarations skip through the next '>'; a tag
// name must start with an ASCII letter, so '< 500', '<3', '</ ' and '<>'
// mean the '<' was literal text — emit it and return 1; an unterminated
// tag is likewise literal. Inside a tag, '"' or "'" opens a quoted
// attribute whose '>' is not the tag's end — an unterminated quote means
// the tag was junk, so the whole thing is consumed without emission.
func emitTag(b *strings.Builder, src string, at int) int {
	rest := src[at+1:]
	switch {
	case strings.HasPrefix(rest, "!--"):
		if end := strings.Index(rest[len("!--"):], "-->"); end >= 0 {
			return len("<!--") + end + len("-->")
		}
		return len(src) - at
	case strings.HasPrefix(rest, "!"), strings.HasPrefix(rest, "?"):
		if gt := strings.IndexByte(rest, '>'); gt >= 0 {
			return gt + 2
		}
		return len(src) - at
	}
	i := at + 1
	closing := i < len(src) && src[i] == '/'
	if closing {
		i++
	}
	start := i
	if i < len(src) && isTagStart(src[i]) {
		for i++; i < len(src) && isTagName(src[i]); i++ {
		}
	}
	name := strings.ToLower(src[start:i])
	if name == "" {
		b.WriteByte('<')
		return 1
	}
	for i < len(src) && src[i] != '>' {
		if q := src[i]; q == '"' || q == '\'' {
			i++
			for i < len(src) && src[i] != q {
				i++
			}
			if i >= len(src) {
				return len(src) - at // unterminated quote: the tag is junk — eat it
			}
			i++ // past the closing quote
			continue
		}
		i++
	}
	if i >= len(src) {
		b.WriteByte('<')
		return 1
	}
	i++ // past '>'
	if !closing && (name == "script" || name == "style") {
		return i - at + skipHiddenContent(src, i, name)
	}
	emitBlockBreak(b, name, closing)
	return i - at
}

// skipHiddenContent advances past the matching </name — case-insensitive;
// absent, the rest of the input is the element's body (a broken tag
// cannot leak markup either way).
func skipHiddenContent(src string, from int, name string) int {
	close := "</" + name
	rest := strings.ToLower(src[from:])
	if j := strings.Index(rest, close); j >= 0 {
		end := from + j + len(close)
		if gt := strings.IndexByte(src[end:], '>'); gt >= 0 {
			return end + gt + 1 - from
		}
		return len(src) - from
	}
	return len(src) - from
}

// emitBlockBreak writes the break a structural tag stands for: two
// newlines for a paragraph boundary, one for br, a single space for an
// OPEN td/th so adjacent cells stay separate tokens — and nothing for
// inline markup, whose text already carries the flow.
func emitBlockBreak(b *strings.Builder, name string, closing bool) {
	switch {
	case name == "br":
		b.WriteByte('\n')
	case (name == "td" || name == "th") && !closing:
		b.WriteByte(' ')
	case blockTags[name]:
		b.WriteString("\n\n")
	}
}

// isTagStart accepts the ASCII letters a tag name must begin with —
// anything else (' ', '3', '>') means the '<' was never markup.
func isTagStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// isTagName accepts the ASCII letters/digits that may follow a tag
// name's first letter; anything else ends the name scan.
func isTagName(c byte) bool {
	return isTagStart(c) || c >= '0' && c <= '9'
}
