package webapi

// Pure-mapper unit tests — the row→contract folding behind both the
// inbox list and the detail page. No database: every input is the row
// struct the SQL fills, every assertion is on the contract value.

import (
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
)

// TestInboxStatusRollup pins every app.rfq_status onto its web bucket —
// the contract's four-status rollup, drift-loud on an unknown status.
func TestInboxStatusRollup(t *testing.T) {
	cases := map[string]string{
		"RECEIVED":      "processing",
		"PARSING":       "processing",
		"PRICING":       "processing",
		"NEEDS_REVIEW":  "awaiting_review",
		"FAILED":        "awaiting_review",
		"READY":         "ready_to_send",
		"APPROVED_SENT": "sent",
		"IGNORED":       "processing", // unreachable — the query filters it; the default is the loud fallback
		"FUTURE_STATUS": "processing", // unknown statuses bucket, they must not panic
	}
	for db, web := range cases {
		assert.Equal(t, web, inboxStatus(db), "db status %s", db)
	}
}

// TestRouteAndEquipment: the lane needs both locodes; FCL maps only the
// two boxes the web enum carries; a missing modal stays null.
func TestRouteAndEquipment(t *testing.T) {
	fv := func(v string) fieldValue { return fieldValue{Value: v} }

	route, eq := routeAndEquipment(extractionFields{
		"origin_locode": fv("BRSSZ"), "dest_locode": fv("DEHAM"),
		"modal": fv("FCL"), "equipment_or_unit": fv("40HC"),
	})
	require.NotNil(t, route)
	assert.Equal(t, authapi.Route{Origin: "BRSSZ", Destination: "DEHAM"}, *route)
	require.NotNil(t, eq)
	assert.Equal(t, authapi.Fcl40Hc, *eq)

	route, _ = routeAndEquipment(extractionFields{"origin_locode": fv("BRSSZ")})
	assert.Nil(t, route, "a half-known lane is null, never half a route")

	_, eq = routeAndEquipment(extractionFields{"modal": fv("AIR")})
	require.NotNil(t, eq)
	assert.Equal(t, authapi.Air, *eq)

	_, eq = routeAndEquipment(extractionFields{"modal": fv("LCL")})
	require.NotNil(t, eq)
	assert.Equal(t, authapi.Lcl, *eq)

	_, eq = routeAndEquipment(extractionFields{
		"modal": fv("FCL"), "equipment_or_unit": fv("45FT"),
	})
	assert.Nil(t, eq, "an FCL unit the enum does not name stays null")

	_, eq = routeAndEquipment(extractionFields{"equipment_or_unit": fv("40HC")})
	assert.Nil(t, eq, "equipment without a modal is null")
}

// TestApplySLA: sent items carry no deadline; everything else is due
// received+4h and flags inside the 30-minute window — including overdue.
func TestApplySLA(t *testing.T) {
	received := time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)
	row := func(status string) inboxRow {
		return inboxRow{status: status, receivedAt: received}
	}
	now := received.Add(3 * time.Hour) // 1h before deadline

	var item authapi.InboxQuote
	applySLA(&item, row(statusSentDB), now)
	assert.Nil(t, item.SLADueAt, "sent ends the clock")
	assert.False(t, item.SLAAtRisk)

	item = authapi.InboxQuote{}
	applySLA(&item, row("READY"), received.Add(time.Hour))
	require.NotNil(t, item.SLADueAt)
	assert.Equal(t, received.Add(4*time.Hour), *item.SLADueAt)
	assert.False(t, item.SLAAtRisk, "3h out is comfortably inside the window")

	item = authapi.InboxQuote{}
	applySLA(&item, row("READY"), received.Add(3*time.Hour+31*time.Minute))
	assert.True(t, item.SLAAtRisk, "under 30 minutes flags")

	item = authapi.InboxQuote{}
	applySLA(&item, row("NEEDS_REVIEW"), received.Add(5*time.Hour))
	assert.True(t, item.SLAAtRisk, "overdue stays flagged")
}

// TestSellTotal: the freight group's currency leads; same-currency
// groups sum; empty reports not-ok so callers emit null.
func TestSellTotal(t *testing.T) {
	groups := []pricing.GroupTotal{
		{Group: pricing.GroupLocalFees, Currency: "BRL", Sell: decimal.NewFromInt(575)},
		{Group: pricing.GroupInternationalFreight, Currency: "USD", Sell: decimal.NewFromInt(2070)},
		{Group: pricing.GroupServiceTaxes, Currency: "USD", Sell: decimal.NewFromInt(200)},
	}
	total, currency, ok := sellTotal(groups)
	require.True(t, ok)
	assert.Equal(t, "USD", currency, "the freight group's currency leads")
	assert.True(t, decimal.NewFromInt(2270).Equal(total), "USD groups sum; the BRL fees do not blend")

	total, currency, ok = sellTotal(groups[:1])
	require.True(t, ok)
	assert.Equal(t, "BRL", currency, "with no freight group the first currency leads")
	assert.True(t, decimal.NewFromInt(575).Equal(total))

	_, _, ok = sellTotal(nil)
	assert.False(t, ok, "no groups → no honest total")
}

// TestParagraphsHighlight: the audit's segment emission — none/extracted/
// inferred spans around located quotes, case-folded match, unmatched
// quotes degrade to plain text, overlapping claims keep the longest.
func TestParagraphsHighlight(t *testing.T) {
	body := "Please quote FCL from Santos to Hamburg.\n\nWe need 2x40HC next week."
	spans := []evidenceSpan{
		{quote: "from santos", inferred: false}, // case-folded match
		{quote: "to Hamburg", inferred: false},
		{quote: "FCL", inferred: false},
		{quote: "2x40HC", inferred: true},
		{quote: "not in the body", inferred: false},
	}
	paras := paragraphs(&body, spans)
	require.Len(t, paras, 2)

	var kinds []authapi.EmailSegmentHighlight
	var texts []string
	for _, seg := range paras[0] {
		kinds = append(kinds, seg.Highlight)
		texts = append(texts, seg.Text)
	}
	assert.Equal(t, []authapi.EmailSegmentHighlight{
		authapi.None, authapi.Extracted, authapi.None, authapi.Extracted,
		authapi.None, authapi.Extracted, authapi.None,
	}, kinds)
	assert.Equal(t, body[:strings.Index(body, "\n")], strings.Join(texts, ""),
		"the segments reassemble the paragraph byte-for-byte")
	require.Len(t, paras[1], 3)
	assert.Equal(t, authapi.Inferred, paras[1][1].Highlight)
	assert.Equal(t, "2x40HC", paras[1][1].Text)

	assert.Equal(t, [][]authapi.EmailSegment{}, paragraphs(nil, spans),
		"a nil body is an empty list, never null")
	empty := ""
	assert.Equal(t, [][]authapi.EmailSegment{}, paragraphs(&empty, spans))
	only := "plain body, no evidence"
	require.Len(t, paragraphs(&only, spans), 1)
	assert.Equal(t, authapi.None, paragraphs(&only, spans)[0][0].Highlight)
}

// TestBodySpansSkipsAttachment: only EMAIL_BODY evidence can highlight
// body text; attachment quotes live in the attachment bytes.
func TestBodySpansSkipsAttachment(t *testing.T) {
	var src fieldValue
	src.Evidence.Source = "EMAIL_BODY"
	src.Evidence.ExactQuote = "in body"
	var att fieldValue
	att.Evidence.Source = "ATTACHMENT"
	att.Evidence.ExactQuote = "in pdf"

	spans := bodySpans(extractionFields{"a_field": att, "z_field": src})
	require.Len(t, spans, 1)
	assert.Equal(t, "in body", spans[0].quote)
}

// TestHumanizeField: snake_case key → sentence label; an unknown key
// degrades to a readable label instead of an empty cell.
func TestHumanizeField(t *testing.T) {
	assert.Equal(t, "Origin locode", humanizeField("origin_locode"))
	assert.Equal(t, "Equipment or unit", humanizeField("equipment_or_unit"))
	assert.Equal(t, "Customs code", humanizeField("customs_code"))
	assert.Equal(t, "", humanizeField(""))
}

// TestDisplayName: the name field's fallbacks — registry name first,
// then the email's local part, then the raw email.
func TestDisplayName(t *testing.T) {
	assert.Equal(t, "Ana Buyer", displayName("Ana Buyer", "ana@c.dev"))
	assert.Equal(t, "ops", displayName("", "ops@forwarder.dev"))
	assert.Equal(t, "  ", displayName("", "  "), "even a degenerate email is never empty")
	assert.Equal(t, "Ana Buyer", displayName("  Ana Buyer  ", "a@c.dev"))
}

// TestSessionRole: OWNER/ADMIN administer; every other membership role
// is the operator.
func TestSessionRole(t *testing.T) {
	assert.Equal(t, authapi.Admin, sessionRole("OWNER"))
	assert.Equal(t, authapi.Admin, sessionRole("ADMIN"))
	assert.Equal(t, authapi.Operator, sessionRole("PRICING_ANALYST"))
	assert.Equal(t, authapi.Operator, sessionRole("ANYTHING_ELSE"))
}

// TestInboxFilters: the status filter expands, the q needle is trimmed,
// escaped and capped; both nil when unfiltered.
func TestInboxFilters(t *testing.T) {
	statuses, pattern := inboxFilters(authapi.ListInboxQuotesParams{})
	assert.Nil(t, statuses)
	assert.Nil(t, pattern)

	st := authapi.ReadyToSend
	q := "  50%_\\x  "
	statuses, pattern = inboxFilters(authapi.ListInboxQuotesParams{Status: &st, Q: &q})
	assert.Equal(t, []string{"READY"}, statuses)
	require.NotNil(t, pattern)
	pat, ok := pattern.(string)
	require.True(t, ok)
	// Every metachar escaped: \ -> \\, % -> \%, _ -> \_
	assert.Equal(t, "%50\\%\\_\\\\x%", pat)

	blank := "   "
	_, pattern = inboxFilters(authapi.ListInboxQuotesParams{Q: &blank})
	assert.Nil(t, pattern, "a whitespace-only q is no filter")

	long := strings.Repeat("z", maxQueryLen+50)
	_, pattern = inboxFilters(authapi.ListInboxQuotesParams{Q: &long})
	pat, _ = pattern.(string)
	assert.Equal(t, "%"+strings.Repeat("z", maxQueryLen)+"%", pat,
		"the needle caps at maxQueryLen runes")
}

// TestTruncateRunes bounds by runes, not bytes.
func TestTruncateRunes(t *testing.T) {
	assert.Equal(t, "abc", truncateRunes("abcdef", 3))
	assert.Equal(t, "ok", truncateRunes("ok", 3), "within the cap returns the string itself")
	assert.Equal(t, "short", truncateRunes("short", 10))
	assert.Equal(t, "", truncateRunes("", 5))
}

// TestLikeEscaper maps each LIKE metacharacter under the ESCAPE '\'
// the SQL declares.
func TestLikeEscaper(t *testing.T) {
	assert.Equal(t, `\\`, likeEscaper.Replace(`\`))
	assert.Equal(t, `\%`, likeEscaper.Replace(`%`))
	assert.Equal(t, `\_`, likeEscaper.Replace(`_`))
	assert.Equal(t, `a\%b\_c\\d`, likeEscaper.Replace(`a%b_c\d`))
}
