// Package webapi implements the web v1 read surface folded from
// web-api.yaml into core-api.yaml: GET /v1/session, POST
// /v1/session/organization, GET /v1/inbox/quotes and GET
// /v1/quotes/{quoteId}. oapi-codegen emits one StrictServerInterface for
// the whole contract, so these handlers mount on the auth strict surface
// through the auth.WebAPI delegate seam — the same post-build wiring as
// the quote approver. Every tenant read runs inside
// tenantdb.TenantRunner's InTenantReadTx under RLS (ADR-002); the auth
// store functions carry the Layer A reads.
package webapi

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shopspring/decimal"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
)

// Endpoints implements the four web v1 handlers on the shared strict
// surface. runner is the only tenant-DB path; store carries the Layer A
// function reads (profile, memberships); cfg drives the qp_org cookie
// flags; now is the one clock the SLA math reads.
type Endpoints struct {
	runner tenantdb.TenantRunner
	store  *platform.Store
	cfg    *platform.Config
	now    func() time.Time
}

// NewEndpoints binds the surface. Production wires the tenant runner, the
// pool-backed store, the live config and time.Now:
//
//	endpoints.Web = webapi.NewEndpoints(db, store, &cfg, time.Now)
func NewEndpoints(runner tenantdb.TenantRunner, store *platform.Store, cfg *platform.Config, now func() time.Time) *Endpoints {
	return &Endpoints{runner: runner, store: store, cfg: cfg, now: now}
}

// SLA constants of the web contract: the platform answers a customer's RFQ
// inside responseSLA (4h, spec section 5.2.5's service target) and flags
// the item once under slaRiskWindow remains. The same pair drives the
// inbox's slaDueAt/slaAtRisk and the summary's risk counters — one clock,
// one definition.
const (
	responseSLA   = 4 * time.Hour
	slaRiskWindow = 30 * time.Minute
)

// statusSentDB is the only app.rfq_status that ends the SLA clock (and maps
// to the web's "sent" bucket). See dbStatuses for the full rollup.
const statusSentDB = "APPROVED_SENT"

// dbStatuses is the web→DB status rollup of core-api.yaml's
// InboxQuoteStatus: coarser than app.rfq_status — RECEIVED/PARSING/PRICING
// are pipeline internals, FAILED joins the human-review bucket with
// NEEDS_REVIEW. A nil filter admits every status except IGNORED (kind
// 'OTHER'/'TARIFF_UPDATE' rows never reach the inbox at all — the query
// pins kind='RFQ').
var dbStatuses = map[string][]string{
	"processing":      {"RECEIVED", "PARSING", "PRICING"},
	"awaiting_review": {"NEEDS_REVIEW", "FAILED"},
	"ready_to_send":   {"READY"},
	"sent":            {"APPROVED_SENT"},
}

// inboxStatus rolls one app.rfq_status up to its web bucket — the inverse
// of dbStatuses, duplicated as a switch so an unknown status surfaces
// loudly (contract drift) rather than silently bucketing.
func inboxStatus(dbStatus string) string {
	switch dbStatus {
	case "RECEIVED", "PARSING", "PRICING":
		return "processing"
	case "NEEDS_REVIEW", "FAILED":
		return "awaiting_review"
	case "READY":
		return "ready_to_send"
	case statusSentDB:
		return "sent"
	default:
		return "processing"
	}
}

// likeEscaper neutralizes the LIKE metacharacters in a user `q` so the
// substring filter can never become a pattern-language injection. The SQL
// declares ESCAPE '\' to match.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// maxQueryLen mirrors the contract's maxLength on `q`; the generated
// binder does not enforce it, so the handler truncates rather than
// rejects — a longer needle can only narrow a substring match.
const maxQueryLen = 200

// truncateRunes bounds s to n runes for the `q` cap.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// extractionFields is the `fields` map of the persisted ExtractionResult
// (packages/contracts/schemas/extraction_result.schema.json): one
// FieldValue envelope per extractable key. Only the members the web
// surface needs are decoded — the rest of the envelope is contract data
// core-api already validated at ingest.
type extractionFields map[string]fieldValue

type fieldValue struct {
	Value    string `json:"value"`
	Inferred bool   `json:"inferred"`
	Evidence struct {
		Source     string `json:"source"`
		ExactQuote string `json:"exact_quote"`
	} `json:"evidence"`
}

type extractionDoc struct {
	Fields extractionFields `json:"fields"`
}

// decodeExtraction reads the fields map off a persisted
// extracted_payload_json blob.
func decodeExtraction(raw []byte) (extractionFields, error) {
	var doc extractionDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode extraction fields: %w", err)
	}
	return doc.Fields, nil
}

// decodePricing reads the pinned pricing.Output envelope off
// pricing_result_json — the same decode the approve endpoint and the send
// worker run on the same column.
func decodePricing(raw []byte) (pricing.Output, error) {
	var out pricing.Output
	if err := json.Unmarshal(raw, &out); err != nil {
		return pricing.Output{}, fmt.Errorf("decode pricing_result_json: %w", err)
	}
	return out, nil
}

// sellTotal is the customer-facing money rollup behind both
// estimatedTotal and proposalTotal: the summed sell of every group priced
// in the freight group's currency (the customer's freight currency — the
// mock contract's single displayed total). Without a freight group the
// first group's currency leads; an empty group list reports ok=false so
// the caller can emit null instead of a fabricated zero.
func sellTotal(groups []pricing.GroupTotal) (decimal.Decimal, string, bool) {
	if len(groups) == 0 {
		return decimal.Zero, "", false
	}
	currency := groups[0].Currency
	for _, g := range groups {
		if g.Group == pricing.GroupInternationalFreight {
			currency = g.Currency
			break
		}
	}
	total := decimal.Zero
	for _, g := range groups {
		if g.Currency == currency {
			total = total.Add(g.Sell)
		}
	}
	return total, currency, true
}

// humanizeField renders an extraction key for the audit list —
// `origin_locode` → "Origin locode" — with the first letter capitalized
// and the rest verbatim, so unfamiliar keys degrade to a readable label
// instead of an empty cell.
func humanizeField(key string) string {
	s := strings.ReplaceAll(key, "_", " ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// paraSplit marks paragraph boundaries in an email body: a blank line,
// CRLF-tolerant, whitespace-only lines included.
var paraSplit = regexp.MustCompile(`\r?\n[ \t\r]*\n`)
