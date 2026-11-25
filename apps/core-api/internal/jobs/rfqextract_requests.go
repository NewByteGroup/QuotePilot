package jobs

// Wire-shape helpers for rfq_extract (provisional contract,
// packages/contracts/openapi/ai-engine.yaml): what the worker puts on the
// ai-engine wire — the resolved textual body, the classify/extract
// request builders and their byte caps. Raw HTML never crosses the wire:
// core reduces it to text first (confirmed with the engine owner).

import (
	"strings"
	"unicode/utf8"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/aiengine"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// hasUsableText is the no-LLM-call guard: a non-empty resolved body or
// parsed markdown counts; everything empty parks NO_USABLE_TEXT (spec S2).
// body arrives already resolved by requestBody, so a markup-only HTML
// part does not fake "usable".
func hasUsableText(body string, markdowns []aiengine.ParsedMarkdown) bool {
	if strings.TrimSpace(body) != "" {
		return true
	}
	for _, md := range markdowns {
		if strings.TrimSpace(md.Markdown) != "" {
			return true
		}
	}
	return false
}

// requestBody resolves the single textual body the provisional contract
// sends: the text part as-is when it carries content, else the HTML part
// reduced by platform.HTMLToText. The conversion runs BEFORE truncation
// so a big HTML document still yields a full limit of usable text.
func requestBody(rfq extractRfq) string {
	if rfq.RawText != nil && strings.TrimSpace(*rfq.RawText) != "" {
		return *rfq.RawText
	}
	if rfq.RawHTML == nil {
		return ""
	}
	return platform.HTMLToText(*rfq.RawHTML)
}

// classifyReq builds the classify call — correlation id plus tenant
// content, never organization_id (rule 2). body_text is bounded at the
// ADR-006 16 KiB classify input.
func classifyReq(args ExtractArgs, rfq extractRfq, atts []extractAttachment, body string) aiengine.ClassifyRequest {
	metas := make([]aiengine.AttachmentMeta, 0, len(atts))
	for _, a := range atts {
		metas = append(metas, aiengine.AttachmentMeta{Filename: a.Filename, ContentType: a.MimeType})
	}
	return aiengine.ClassifyRequest{
		CorrelationID:   args.CorrelationID,
		Subject:         rfq.Subject,
		Sender:          rfq.CustomerEmail,
		BodyText:        truncateBodyBytes(body, classifyBodyLimit),
		AttachmentMetas: metas,
	}
}

// extractReq builds the extraction call: the same resolved body at the
// wider 64 KiB cap, {filename, markdown} pairs and the classification the
// engine echoes in the ExtractionResult.
func extractReq(args ExtractArgs, rfq extractRfq, body string, markdowns []aiengine.ParsedMarkdown, res *aiengine.ClassifyResult) aiengine.ExtractRequest {
	return aiengine.ExtractRequest{
		CorrelationID:   args.CorrelationID,
		Subject:         rfq.Subject,
		Sender:          rfq.CustomerEmail,
		BodyText:        truncateBodyBytes(body, extractBodyLimit),
		ParsedMarkdowns: markdowns,
		Classification:  *res,
	}
}

// truncateBodyBytes cuts s at limit bytes without splitting a multi-byte
// rune — it backs off to the last rune boundary at or under the limit
// (utf8.RuneStart is false for continuation bytes).
func truncateBodyBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	i := limit
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}
