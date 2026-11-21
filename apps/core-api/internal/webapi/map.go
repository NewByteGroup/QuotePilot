package webapi

// Row→contract mapping shared by the inbox list and the quote detail: the
// detail's `quote` member is the same InboxQuote the list emits, so one
// mapper keeps status, SLA, route, equipment, confidence and total
// identical between the two responses.
import (
	"fmt"
	"sort"
	"time"
	"unicode"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/shopspring/decimal"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
)

// inboxRow is the slice of app.rfq_requests — plus the latest quote
// version's pricing blob — that one InboxQuote maps from. pricingJSON is
// nil while no priced version exists; extraction likewise before stage 3.
type inboxRow struct {
	id           uuid.UUID
	rfqNumber    int
	customerName string
	subject      string
	status       string
	receivedAt   time.Time
	extraction   []byte
	confidence   decimal.NullDecimal
	pricingJSON  []byte
}

// mapInboxQuote folds one row into the contract item. now comes from the
// endpoint's injected clock so the SLA flag is testable — never time.Now
// inside the mapper.
func mapInboxQuote(r inboxRow, now time.Time) (authapi.InboxQuote, error) {
	item := authapi.InboxQuote{
		ID:           openapi_types.UUID(r.id),
		RfqNumber:    r.rfqNumber,
		CustomerName: r.customerName,
		Subject:      r.subject,
		Status:       authapi.InboxQuoteStatus(inboxStatus(r.status)),
		ReceivedAt:   r.receivedAt,
	}
	if err := itemFromExtraction(&item, r); err != nil {
		return item, err
	}
	if err := itemFromPricing(&item, r); err != nil {
		return item, err
	}
	applySLA(&item, r, now)
	return item, nil
}

// itemFromExtraction fills route, equipment and confidence off the
// persisted ExtractionResult. A blob that does not decode is contract
// drift — ingest validated it before persisting — so it is an error, not
// a quiet null.
func itemFromExtraction(item *authapi.InboxQuote, r inboxRow) error {
	if len(r.extraction) == 0 {
		return nil
	}
	fields, err := decodeExtraction(r.extraction)
	if err != nil {
		return fmt.Errorf("webapi: extraction on rfq %s: %w", r.id, err)
	}
	item.Route, item.Equipment = routeAndEquipment(fields)
	if r.confidence.Valid {
		pct := int(r.confidence.Decimal.Mul(decimal.NewFromInt(100)).Round(0).IntPart())
		item.ConfidencePercent = &pct
	}
	return nil
}

// itemFromPricing fills estimatedTotal off the latest version's pinned
// output — the same sellTotal proposalTotal uses on the detail route, so
// the list figure and the audit figure can never disagree.
func itemFromPricing(item *authapi.InboxQuote, r inboxRow) error {
	if len(r.pricingJSON) == 0 {
		return nil
	}
	out, err := decodePricing(r.pricingJSON)
	if err != nil {
		return fmt.Errorf("webapi: pricing on rfq %s: %w", r.id, err)
	}
	total, currency, ok := sellTotal(out.Groups)
	if !ok {
		return nil
	}
	item.EstimatedTotal = &authapi.Money{Amount: total.String(), Currency: currency}
	return nil
}

// applySLA stamps the response deadline: sent quotes leave slaDueAt null
// (the contract's "done" signal) and can never be at risk; everything else
// is due responseSLA after receipt and flags once under slaRiskWindow —
// overdue RFQs stay flagged.
func applySLA(item *authapi.InboxQuote, r inboxRow, now time.Time) {
	if r.status == statusSentDB {
		return
	}
	due := r.receivedAt.Add(responseSLA)
	item.SLADueAt = &due
	item.SLAAtRisk = now.After(due.Add(-slaRiskWindow))
}

// routeAndEquipment reads the lane and modal off the fields map. Route
// needs both locodes present to be meaningful — a half-known lane is null
// like the contract intends ("null until extraction resolves the lane").
// Equipment maps modal plus the extracted unit onto the four web values:
// 20DV/40HC are the only FCL boxes the enum names; other FCL units and a
// missing modal stay null rather than invent a label the enum does not
// carry.
func routeAndEquipment(fields extractionFields) (*authapi.Route, *authapi.Equipment) {
	var route *authapi.Route
	origin, dest := fields["origin_locode"].Value, fields["dest_locode"].Value
	if origin != "" && dest != "" {
		route = &authapi.Route{Origin: origin, Destination: dest}
	}
	var equipment *authapi.Equipment
	switch fields["modal"].Value {
	case "AIR":
		e := authapi.Air
		equipment = &e
	case "LCL":
		e := authapi.Lcl
		equipment = &e
	case "FCL":
		equipment = fclEquipment(fields["equipment_or_unit"].Value)
	}
	return route, equipment
}

// fclEquipment maps the two FCL units the web enum carries.
func fclEquipment(unit string) *authapi.Equipment {
	var e authapi.Equipment
	switch unit {
	case "20DV":
		e = authapi.Fcl20Dv
	case "40HC":
		e = authapi.Fcl40Hc
	default:
		return nil
	}
	return &e
}

// evidenceSpan is one exact_quote to look for in the email body plus the
// highlight class its field earned — inferred fields highlight as
// "inferred", read ones as "extracted".
type evidenceSpan struct {
	quote    string
	inferred bool
}

// bodySpans collects the EMAIL_BODY evidence quotes off the fields map in
// sorted-key order — map iteration order must never reach the response.
// Attachment evidence is skipped: those quotes live in the attachment
// bytes, not the body paragraphs.
func bodySpans(fields extractionFields) []evidenceSpan {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	spans := make([]evidenceSpan, 0, len(keys))
	for _, k := range keys {
		fv := fields[k]
		if fv.Evidence.Source == "EMAIL_BODY" && fv.Evidence.ExactQuote != "" {
			spans = append(spans, evidenceSpan{quote: fv.Evidence.ExactQuote, inferred: fv.Inferred})
		}
	}
	return spans
}

// paragraphs splits the raw body into paragraph-segment rows the audit
// view highlights. Empty paragraphs (leading/trailing blank runs) are
// dropped; a nil or empty body returns an empty — never null — slice.
func paragraphs(raw *string, spans []evidenceSpan) [][]authapi.EmailSegment {
	out := [][]authapi.EmailSegment{}
	if raw == nil || *raw == "" {
		return out
	}
	for _, para := range paraSplit.Split(*raw, -1) {
		if para == "" {
			continue
		}
		out = append(out, highlightParagraph(para, spans))
	}
	return out
}

// spanMatch is one located evidence quote inside a paragraph.
type spanMatch struct {
	start, end int
	inferred   bool
}

// highlightParagraph emits the paragraph as alternating
// none/extracted|inferred segments. Matching is a case-folded substring
// search — the schema has core-api re-find exact_quote (NFKC, collapsed
// whitespace) rather than trust model offsets; v1 folds case only, so an
// unmatched or differently-normalized quote degrades to "none" text, never
// a fabricated highlight.
func highlightParagraph(para string, spans []evidenceSpan) []authapi.EmailSegment {
	matches := make([]spanMatch, 0, len(spans))
	for _, s := range spans {
		if start, end, ok := foldSpan(para, s.quote); ok {
			matches = append(matches, spanMatch{start: start, end: end, inferred: s.inferred})
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].start != matches[j].start {
			return matches[i].start < matches[j].start
		}
		return matches[i].end > matches[j].end // same start: the longer quote wins
	})
	return emitSegments(para, nonOverlapping(matches))
}

// foldSpan locates needle case-insensitively and returns the match's
// byte span in the ORIGINAL haystack. It compares rune-by-rune under
// unicode.ToLower rather than strings.ToLower on the whole string:
// whole-string folding changes byte length (evidence "İzmir" lowercases
// to 5-byte "izmir"), so a byte-index match can end past the paragraph's
// end — per-rune folding keeps rune count stable, and rune indexes map
// back to exact byte boundaries through the offset table. An unmatched
// quote is ok=false, never a fabricated span.
func foldSpan(haystack, needle string) (start, end int, ok bool) {
	hrunes := []rune(haystack)
	nrunes := []rune(needle)
	if len(nrunes) == 0 || len(nrunes) > len(hrunes) {
		return 0, 0, false
	}
	// Byte offset of every rune start — range yields exactly that — plus
	// the terminator so the end index translates without special-casing.
	offs := make([]int, 0, len(hrunes)+1)
	for i := range haystack {
		offs = append(offs, i)
	}
	offs = append(offs, len(haystack))
	for i := 0; i+len(nrunes) <= len(hrunes); i++ {
		if foldRunes(hrunes[i:i+len(nrunes)], nrunes) {
			return offs[i], offs[i+len(nrunes)], true
		}
	}
	return 0, 0, false
}

// foldRunes reports whether window matches needle rune-for-rune under
// per-rune case folding.
func foldRunes(window, needle []rune) bool {
	for j := range needle {
		if unicode.ToLower(window[j]) != unicode.ToLower(needle[j]) {
			return false
		}
	}
	return true
}

// nonOverlapping greedily keeps sorted matches that do not overlap — the
// first (earliest, then longest) claim on a span wins.
func nonOverlapping(matches []spanMatch) []spanMatch {
	out := matches[:0]
	last := -1
	for _, m := range matches {
		if m.start >= last {
			out = append(out, m)
			last = m.end
		}
	}
	return out
}

// emitSegments cuts the paragraph around the kept matches.
func emitSegments(para string, matches []spanMatch) []authapi.EmailSegment {
	out := []authapi.EmailSegment{}
	pos := 0
	for _, m := range matches {
		if m.start > pos {
			out = append(out, authapi.EmailSegment{Text: para[pos:m.start], Highlight: authapi.None})
		}
		highlight := authapi.Extracted
		if m.inferred {
			highlight = authapi.Inferred
		}
		out = append(out, authapi.EmailSegment{Text: para[m.start:m.end], Highlight: highlight})
		pos = m.end
	}
	if pos < len(para) {
		out = append(out, authapi.EmailSegment{Text: para[pos:], Highlight: authapi.None})
	}
	return out
}
