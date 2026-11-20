package webapi

// htmlToText reduces an email's raw_html_body to the plain text
// paragraphs() can segment — a byte-wise scanner, not a regex soup. Only
// three shapes matter: tags (skipped, except <script>/<style> whose
// contents — the CSS HTML emails carry — must never reach the wire),
// block-level tags (paragraph breaks so paraSplit sees the structure the
// sender wrote), and &entities (unescaped once at the end). Raw HTML
// itself never reaches the response.
import (
	"html"
	"strings"
)

// blockTags are the tag names that mark a paragraph boundary in email
// markup — each emits \n\n, the separator paraSplit reads.
var blockTags = map[string]bool{
	"p": true, "div": true, "li": true, "tr": true, "table": true,
	"ul": true, "ol": true, "blockquote": true, "section": true,
	"article": true, "header": true, "footer": true, "hr": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
}

// htmlToText strips src's markup to plain text: element contents are
// copied verbatim, block tags emit a paragraph break, br a line break,
// script/style contents are dropped outright, and entities are
// unescaped once everything is assembled.
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

// emitTag consumes one '<' at src[at] and returns bytes consumed. An
// unterminated tag is literal text — emit '<' and move on.
func emitTag(b *strings.Builder, src string, at int) int {
	i := at + 1
	closing := i < len(src) && src[i] == '/'
	if closing {
		i++
	}
	start := i
	for i < len(src) && isTagName(src[i]) {
		i++
	}
	name := strings.ToLower(src[start:i])
	for i < len(src) && src[i] != '>' {
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
	emitBlockBreak(b, name)
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
// newlines for a paragraph boundary, one for br, nothing for inline
// markup — the text itself already carries the flow.
func emitBlockBreak(b *strings.Builder, name string) {
	switch {
	case name == "br":
		b.WriteByte('\n')
	case blockTags[name]:
		b.WriteString("\n\n")
	}
}

// isTagName accepts the ASCII letters/digits HTML tag names are made of;
// anything else ends the name scan and the parser falls through to '>'.
func isTagName(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
