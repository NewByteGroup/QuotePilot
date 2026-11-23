package webapi_test

// Real-Postgres proofs of the web v1 read surface (migration 0009 +
// the folded contract): session bootstrap order and hints, the org
// switch's qp_org cookie driving ResolveTenant's fallback, the inbox
// list/counts/summary across the status rollup, and the quote audit
// detail — every assertion off the mounted chain, so membership, RLS
// and the injected SLA clock are the live behavior, not a mock.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// now is the frozen SLA clock every rig in this file shares.
var now = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

// extractionFixture is the fields map the detail/inbox decode paths read:
// origin + dest build Route, FCL + 40HC the equipment enum, evidence
// quotes land the email highlights (inferred on equipment_or_unit).
var extractionFixture = []byte(`{"fields":{
	"origin_locode":     {"value":"BRSSZ","inferred":false,"confidence":0.9,"reasons":[],
	                     "evidence":{"source":"EMAIL_BODY","exact_quote":"from Santos"}},
	"dest_locode":       {"value":"DEHAM","inferred":false,"confidence":0.9,"reasons":[],
	                     "evidence":{"source":"EMAIL_BODY","exact_quote":"to Hamburg"}},
	"modal":             {"value":"FCL","inferred":false,"confidence":0.8,"reasons":[],
	                     "evidence":{"source":"EMAIL_BODY","exact_quote":"FCL"}},
	"equipment_or_unit": {"value":"40HC","inferred":true,"confidence":0.7,"reasons":[],
	                     "evidence":{"source":"EMAIL_BODY","exact_quote":"2x40HC"}},
	"customer_reference":{"value":"PO-77","inferred":false,"confidence":0.6,"reasons":[],
	                     "evidence":{"source":"ATTACHMENT","exact_quote":"PO-77"}}
}}`)

// TestGetSessionFirstMembershipFallback: no hints at all → the oldest
// membership is active, both orgs are listed with their registry rows,
// and the user block carries role/count/language the contract fixes.
func TestGetSessionFirstMembershipFallback(t *testing.T) {
	r := newRig(t, now)
	orgA, orgB := uuid.New(), uuid.New()
	slugA := testenv.SeedOrganization(t, orgA, "sess-a")
	slugB := testenv.SeedOrganization(t, orgB, "sess-b")
	uid, email := seedWebUser(t, "sess", "Ana Buyer")
	// B joined first — the fallback is oldest-membership, not seed order.
	seedWebMembership(t, orgB, uid, platform.RoleAdmin, now.Add(-48*time.Hour))
	seedWebMembership(t, orgA, uid, platform.RolePricingAnalyst, now.Add(-24*time.Hour))

	rec := r.get(t, uid, "/v1/session", "")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var s authapi.Session
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&s))
	assert.Equal(t, uuid.UUID(s.Organization.ID), orgB, "the oldest membership is the fallback org")
	require.Len(t, s.Organizations, 2)
	assert.Equal(t, uuid.UUID(s.Organizations[0].ID), orgB)
	assert.Equal(t, uuid.UUID(s.Organizations[1].ID), orgA)
	assert.Equal(t, slugB+"@in.test", string(s.Organizations[0].InboundAddress))
	assert.Equal(t, slugA+"@in.test", string(s.Organizations[1].InboundAddress))
	assert.Equal(t, "standard", s.Organization.PlanName)
	assert.Equal(t, "Ana Buyer", s.User.Name)
	assert.Equal(t, authapi.Admin, s.User.Role, "ADMIN membership maps to the admin enum")
	assert.Equal(t, 2, s.User.OrganizationCount)
	assert.Equal(t, authapi.Pt, s.User.Language)
	require.NotNil(t, s.User.Email)
	assert.Equal(t, email, string(*s.User.Email))
}

// TestGetSessionHints: the qp_org cookie picks its org; the header beats
// it outright; a stale cookie (non-member org) quietly falls back.
func TestGetSessionHints(t *testing.T) {
	r := newRig(t, now)
	orgA, orgB, orgC := uuid.New(), uuid.New(), uuid.New()
	testenv.SeedOrganization(t, orgA, "hint-a")
	testenv.SeedOrganization(t, orgB, "hint-b")
	testenv.SeedOrganization(t, orgC, "hint-c")
	uid, _ := seedWebUser(t, "sess", "Hint User")
	seedWebMembership(t, orgA, uid, platform.RolePricingAnalyst, now.Add(-48*time.Hour))
	seedWebMembership(t, orgB, uid, platform.RoleAdmin, now.Add(-24*time.Hour))
	// No membership on C — the stale-cookie case.

	// Cookie only → B.
	rec := r.get(t, uid, "/v1/session", "", &http.Cookie{Name: "qp_org", Value: orgB.String()})
	require.Equal(t, http.StatusOK, rec.Code)
	var s authapi.Session
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&s))
	assert.Equal(t, uuid.UUID(s.Organization.ID), orgB)

	// Header + cookie → header wins.
	rec = r.get(t, uid, "/v1/session", orgA.String(),
		&http.Cookie{Name: "qp_org", Value: orgB.String()})
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&s))
	assert.Equal(t, uuid.UUID(s.Organization.ID), orgA, "the header must beat the cookie")

	// Stale cookie (non-member org) → silent fallback to first membership.
	rec = r.get(t, uid, "/v1/session", "", &http.Cookie{Name: "qp_org", Value: orgC.String()})
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&s))
	assert.Equal(t, uuid.UUID(s.Organization.ID), orgA,
		"an unusable cookie must not fail the bootstrap — it falls back")

	// Malformed cookie → same silent fallback; the cookie is a hint, not
	// an explicit transport.
	rec = r.get(t, uid, "/v1/session", "", &http.Cookie{Name: "qp_org", Value: "not-a-uuid"})
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&s))
	assert.Equal(t, uuid.UUID(s.Organization.ID), orgA)
}

// TestGetSessionBadHeader400: a malformed header is the explicit
// transport failing — ResolveTenant's missing_organization 400, not a
// silent fallback. The user needs a real membership: with none at all the
// bootstrap has nothing to resolve hints against and the no-orgs 404
// comes first.
func TestGetSessionBadHeader400(t *testing.T) {
	r := newRig(t, now)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "bad-a")
	uid, _ := seedWebUser(t, "sess", "Bad Hint")
	seedWebMembership(t, org, uid, platform.RoleAdmin, now.Add(-24*time.Hour))

	rec := r.get(t, uid, "/v1/session", "definitely-not-a-uuid")

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, `{"error":"missing_organization"}`, rec.Body.String())
}

// TestGetSessionNonMemberHeader404: a well-formed header naming an org
// outside the membership list is the uniform not_found — indistinguishable
// from a nonexistent org.
func TestGetSessionNonMemberHeader404(t *testing.T) {
	r := newRig(t, now)
	orgA, orgB := uuid.New(), uuid.New()
	testenv.SeedOrganization(t, orgA, "nm-a")
	testenv.SeedOrganization(t, orgB, "nm-b")
	uid, _ := seedWebUser(t, "sess", "Non Member")
	seedWebMembership(t, orgA, uid, platform.RoleAdmin, now.Add(-24*time.Hour))

	rec := r.get(t, uid, "/v1/session", orgB.String())

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.JSONEq(t, `{"error":"not_found"}`, rec.Body.String())
}

// TestSwitchOrganizationSetsCookie: the 200 carries the switched Session
// and writes qp_org — and the very next org-scoped read resolves the
// tenant off that cookie alone, with RLS proving the org boundary.
func TestSwitchOrganizationSetsCookie(t *testing.T) {
	r := newRig(t, now)
	orgA, orgB := uuid.New(), uuid.New()
	testenv.SeedOrganization(t, orgA, "sw-a")
	testenv.SeedOrganization(t, orgB, "sw-b")
	uid, _ := seedWebUser(t, "switch", "Switch User")
	seedWebMembership(t, orgA, uid, platform.RoleAdmin, now.Add(-48*time.Hour))
	seedWebMembership(t, orgB, uid, platform.RolePricingAnalyst, now.Add(-24*time.Hour))

	// Org B owns the only RFQ — the post-switch inbox must see exactly
	// it, proving both cookie resolution and RLS scoping.
	seedRfq(t, r.db, orgB, rfqSeed{
		number: 41, customerEmail: "b@cust.dev", subject: "B freight",
		status: "READY", receivedAt: now.Add(-time.Hour),
	})
	seedRfq(t, r.db, orgA, rfqSeed{
		number: 7, customerEmail: "a@cust.dev", subject: "A freight",
		status: "READY", receivedAt: now.Add(-time.Hour),
	})

	rec := r.post(t, uid, "/v1/session/organization", `{"organizationId":"`+orgB.String()+`"}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var s authapi.Session
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&s))
	assert.Equal(t, uuid.UUID(s.Organization.ID), orgB)

	var orgCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "qp_org" {
			orgCookie = c
		}
	}
	require.NotNil(t, orgCookie, "the switch must write qp_org")
	assert.Equal(t, orgB.String(), orgCookie.Value)
	assert.True(t, orgCookie.HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, orgCookie.SameSite)

	// The cookie alone carries the tenant into the org-scoped read.
	rec = r.get(t, uid, "/v1/inbox/quotes", "", orgCookie)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var page authapi.InboxQuotePage
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&page))
	require.Len(t, page.Items, 1)
	assert.Equal(t, 41, page.Items[0].RfqNumber,
		"only org B's RFQ is visible through its own cookie tenant")
}

// TestSwitchOrganization404s: non-member and nonexistent targets are the
// same uniform 404; a missing body is bad_request.
func TestSwitchOrganization404s(t *testing.T) {
	r := newRig(t, now)
	orgA, orgB := uuid.New(), uuid.New()
	testenv.SeedOrganization(t, orgA, "sw404-a")
	testenv.SeedOrganization(t, orgB, "sw404-b")
	uid, _ := seedWebUser(t, "sw404", "Switch 404")
	seedWebMembership(t, orgA, uid, platform.RoleAdmin, now.Add(-24*time.Hour))

	for _, tc := range []struct {
		name string
		body string
	}{
		{"non-member org", `{"organizationId":"` + orgB.String() + `"}`},
		{"nonexistent org", `{"organizationId":"` + uuid.NewString() + `"}`},
	} {
		rec := r.post(t, uid, "/v1/session/organization", tc.body)
		assert.Equal(t, http.StatusNotFound, rec.Code, tc.name)
		assert.JSONEq(t, `{"error":"not_found"}`, rec.Body.String(), tc.name)
	}
	rec := r.post(t, uid, "/v1/session/organization", "")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// inboxOrg is the shared fixture org for the list/counts/summary tests:
// one RFQ per web bucket plus the excluded shapes (IGNORED, TARIFF_UPDATE).
func seedInboxOrg(t *testing.T, r *rig, org uuid.UUID) {
	t.Helper()
	h := time.Hour
	seedRfq(t, r.db, org, rfqSeed{
		number: 1, customerEmail: "p1@cust.dev", customerName: strPtr("Proc One"),
		subject: "Processing FCL", status: "RECEIVED", receivedAt: now.Add(-h),
		extraction: extractionFixture, confidence: strPtr("0.85"),
	})
	seedRfq(t, r.db, org, rfqSeed{
		number: 2, customerEmail: "p2@cust.dev", subject: "Parsing LCL",
		status: "PARSING", receivedAt: now.Add(-2 * h),
	})
	seedRfq(t, r.db, org, rfqSeed{
		number: 3, customerEmail: "rv@cust.dev", subject: "Review me",
		status: "NEEDS_REVIEW", receivedAt: now.Add(-26 * h), // yesterday + deep SLA risk
	})
	seedRfq(t, r.db, org, rfqSeed{
		number: 4, customerEmail: "fl@cust.dev", subject: "Failed parse",
		status: "FAILED", receivedAt: now.Add(-h),
	})
	seedRfq(t, r.db, org, rfqSeed{
		number: 5, customerEmail: "rdy@cust.dev", subject: "Ready FCL Santos",
		status: "READY", receivedAt: now.Add(-h), extraction: extractionFixture,
		confidence: strPtr("0.90"),
	})
	seedRfq(t, r.db, org, rfqSeed{
		number: 6, customerEmail: "sent@cust.dev", subject: "Already sent",
		status: "APPROVED_SENT", receivedAt: now.Add(-30 * h),
	})
	seedRfq(t, r.db, org, rfqSeed{
		number: 7, customerEmail: "ign@cust.dev", subject: "Ignored spam",
		status: "IGNORED", receivedAt: now.Add(-h),
	})
	seedRfq(t, r.db, org, rfqSeed{
		number: 8, customerEmail: "tar@cust.dev", subject: "New tariff table",
		status: "READY", kind: "TARIFF_UPDATE", receivedAt: now.Add(-h),
	})
}

// TestInboxListCountsAndStatus: the rollup, the excluded shapes, the SLA
// fields and the counts buckets — all from the unfiltered page.
func TestInboxListCountsAndStatus(t *testing.T) {
	r := newRig(t, now)
	org, other := uuid.New(), uuid.New()
	testenv.SeedOrganization(t, org, "inb-a")
	testenv.SeedOrganization(t, other, "inb-b")
	uid, _ := seedWebUser(t, "inb", "Inbox User")
	seedWebMembership(t, org, uid, platform.RoleAdmin, now.Add(-72*time.Hour))
	seedInboxOrg(t, r, org)
	// Cross-tenant noise must never surface.
	seedRfq(t, r.db, other, rfqSeed{
		number: 99, customerEmail: "x@other.dev", subject: "Other org freight",
		status: "READY", receivedAt: now.Add(-time.Hour),
	})

	rec := r.get(t, uid, "/v1/inbox/quotes", org.String())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var page authapi.InboxQuotePage
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&page))

	// 6 visible: IGNORED and TARIFF_UPDATE never reach the inbox.
	require.Len(t, page.Items, 6)
	byNum := map[int]authapi.InboxQuote{}
	for _, it := range page.Items {
		byNum[it.RfqNumber] = it
		assert.NotEqual(t, 99, it.RfqNumber, "the other org's RFQ must not leak")
	}
	assert.Equal(t, authapi.Processing, byNum[1].Status)
	assert.Equal(t, authapi.Processing, byNum[2].Status)
	assert.Equal(t, authapi.AwaitingReview, byNum[3].Status)
	assert.Equal(t, authapi.AwaitingReview, byNum[4].Status, "FAILED joins the review bucket")
	assert.Equal(t, authapi.ReadyToSend, byNum[5].Status)
	assert.Equal(t, authapi.Sent, byNum[6].Status)

	// Extraction → route/equipment/confidence on item 5.
	require.NotNil(t, byNum[5].Route)
	assert.Equal(t, authapi.Route{Origin: "BRSSZ", Destination: "DEHAM"}, *byNum[5].Route)
	require.NotNil(t, byNum[5].Equipment)
	assert.Equal(t, authapi.Fcl40Hc, *byNum[5].Equipment)
	require.NotNil(t, byNum[5].ConfidencePercent)
	assert.Equal(t, 90, *byNum[5].ConfidencePercent)
	// No extraction → the lane stays null, never fabricated.
	assert.Nil(t, byNum[3].Route)
	assert.Nil(t, byNum[3].Equipment)

	// SLA: sent item carries no deadline; the 26h-old review item is at risk.
	assert.Nil(t, byNum[6].SLADueAt)
	assert.False(t, byNum[6].SLAAtRisk)
	require.NotNil(t, byNum[3].SLADueAt)
	assert.True(t, byNum[3].SLAAtRisk)
	// A 1h-old item: due at received+4h, comfortably inside the window.
	require.NotNil(t, byNum[1].SLADueAt)
	assert.Equal(t, now.Add(-time.Hour).Add(4*time.Hour).Unix(), byNum[1].SLADueAt.Unix())
	assert.False(t, byNum[1].SLAAtRisk)

	assert.Equal(t, authapi.InboxStatusCounts{
		All: 6, Processing: 2, AwaitingReview: 2, ReadyToSend: 1, Sent: 1,
	}, page.StatusCounts)
	assert.True(t, page.RefreshedAt.Equal(now))
}

// TestInboxStatusFilter: ?status= collapses the list, never the counts.
func TestInboxStatusFilter(t *testing.T) {
	r := newRig(t, now)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "flt-a")
	uid, _ := seedWebUser(t, "flt", "Filter User")
	seedWebMembership(t, org, uid, platform.RoleAdmin, now.Add(-72*time.Hour))
	seedInboxOrg(t, r, org)

	rec := r.get(t, uid, "/v1/inbox/quotes?status=ready_to_send", org.String())
	require.Equal(t, http.StatusOK, rec.Code)
	var page authapi.InboxQuotePage
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&page))
	require.Len(t, page.Items, 1)
	assert.Equal(t, 5, page.Items[0].RfqNumber)
	assert.Equal(t, 6, page.StatusCounts.All, "counts describe the whole inbox, not the filter")

	rec = r.get(t, uid, "/v1/inbox/quotes?status=sent", org.String())
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&page))
	require.Len(t, page.Items, 1)
	assert.Equal(t, 6, page.Items[0].RfqNumber)

	rec = r.get(t, uid, "/v1/inbox/quotes?status=nope", org.String())
	assert.Equal(t, http.StatusBadRequest, rec.Code, "an unknown enum member is a hard 400")
}

// TestInboxQueryFilter: q matches subject, customer name and rfq_number
// case-insensitively — and a LIKE metacharacter is a literal, never a
// pattern.
func TestInboxQueryFilter(t *testing.T) {
	r := newRig(t, now)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "q-a")
	uid, _ := seedWebUser(t, "q", "Query User")
	seedWebMembership(t, org, uid, platform.RoleAdmin, now.Add(-72*time.Hour))
	seedInboxOrg(t, r, org)

	decodePage := func(path string) authapi.InboxQuotePage {
		rec := r.get(t, uid, path, org.String())
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var p authapi.InboxQuotePage
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&p))
		return p
	}

	page := decodePage("/v1/inbox/quotes?q=santos")
	require.Len(t, page.Items, 1)
	assert.Equal(t, 5, page.Items[0].RfqNumber, "subject match is case-insensitive")

	page = decodePage("/v1/inbox/quotes?q=proc%20one")
	require.Len(t, page.Items, 1)
	assert.Equal(t, 1, page.Items[0].RfqNumber, "customer name matches")

	page = decodePage("/v1/inbox/quotes?q=rfq")
	assert.Len(t, page.Items, 0, "a needle no subject, customer or number carries stays empty")

	page = decodePage("/v1/inbox/quotes?q=3")
	require.Len(t, page.Items, 1)
	assert.Equal(t, 3, page.Items[0].RfqNumber, "rfq_number's text form matches")

	page = decodePage("/v1/inbox/quotes?q=%25") // literal '%' needle
	assert.Len(t, page.Items, 0, "the LIKE escape must keep % literal")
}

// TestInboxSummary: the header counters off the seeded org — day buckets
// (UTC), ready queue, SLA risk tally + most urgent, response target, and
// the fallbacks (no approved quotes → 0 avg; no catch-all rule → 100).
func TestInboxSummary(t *testing.T) {
	r := newRig(t, now)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "sum-a")
	uid, _ := seedWebUser(t, "sum", "Summary User")
	seedWebMembership(t, org, uid, platform.RoleAdmin, now.Add(-72*time.Hour))
	seedInboxOrg(t, r, org)

	rec := r.get(t, uid, "/v1/inbox/quotes", org.String())
	require.Equal(t, http.StatusOK, rec.Code)
	var page authapi.InboxQuotePage
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&page))
	s := page.Summary

	assert.Equal(t, 4, s.ReceivedToday, "the four same-day rows (UTC) count; IGNORED/TARIFF_UPDATE are out")
	assert.Equal(t, 2, s.ReceivedYesterday, "the 26h-old review item and the 30h-old sent item are yesterday")
	assert.Equal(t, 1, s.ReadyToSend)
	// At risk = received before now-3.5h and not sent: only the 26h-old
	// review item; the 2h-old parsing item is still inside the window and
	// the 30h-old sent item is out by status.
	assert.Equal(t, 1, s.SLAAtRiskCount)
	require.NotNil(t, s.MostUrgentSLARisk)
	assert.Equal(t, "rv@cust.dev", s.MostUrgentSLARisk.CustomerName,
		"the customer falls back to the email when name is NULL")
	assert.Equal(t, now.Add(-26*time.Hour).Add(4*time.Hour).Unix(),
		s.MostUrgentSLARisk.SLADueAt.Unix())
	assert.Equal(t, 240, s.ResponseTargetMinutes)
	assert.Equal(t, 100, s.AutoApproveConfidencePercent, "no catch-all rule → the 1.0 default")
	assert.Equal(t, 0, s.AvgResponseMinutes, "no approved quotes yet → the 0 fallback")
}

// TestInboxAvgResponseAndAutoApprove: a priced+sent quote and a catch-all
// rule move the two fallback counters to their real values.
func TestInboxAvgResponseAndAutoApprove(t *testing.T) {
	r := newRig(t, now)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "avg-a")
	uid, _ := seedWebUser(t, "avg", "Avg User")
	seedWebMembership(t, org, uid, platform.RoleAdmin, now.Add(-72*time.Hour))

	received := now.Add(-6 * time.Hour)
	rfq := seedRfq(t, r.db, org, rfqSeed{
		number: 11, customerEmail: "avg@cust.dev", subject: "Sent freight",
		status: "APPROVED_SENT", receivedAt: received,
	})
	approved := received.Add(2 * time.Hour)
	require.NoError(t, r.db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.quotes
				(organization_id, rfq_id, quote_number, version, status,
				 total_cost_brl, target_margin, final_price_brl, overall_confidence, approved_at)
			VALUES ($1, $2, 'Q-1', 1, 'APPROVED_SENT', 1000, 0.2, 1200, 0.9, $3)`,
			org, rfq, approved)
		return err
	}))
	// Catch-all rule: 0.95 threshold, 3-day validity.
	require.NoError(t, r.db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.pricing_rules
				(organization_id, modal, default_target_margin, auto_approve_threshold, quote_validity_days)
			VALUES ($1, 'ALL', 0.2, 0.95, 3)`, org)
		return err
	}))

	rec := r.get(t, uid, "/v1/inbox/quotes", org.String())
	require.Equal(t, http.StatusOK, rec.Code)
	var page authapi.InboxQuotePage
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&page))
	assert.Equal(t, 120, page.Summary.AvgResponseMinutes, "2h receipt→approval")
	assert.Equal(t, 95, page.Summary.AutoApproveConfidencePercent)
}

// TestInboxQuoteVersionID: the approve handle rides the list — a priced
// row exposes its latest version's id (the POST /quotes/{id}/approve
// path parameter) while a pre-pricing row stays null, so the client
// never offers approval on a version that does not exist.
func TestInboxQuoteVersionID(t *testing.T) {
	r := newRig(t, now)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "qvid-a")
	uid, _ := seedWebUser(t, "qvid", "Version User")
	seedWebMembership(t, org, uid, platform.RoleAdmin, now.Add(-72*time.Hour))
	version := seedVersionPair(t, r, org, now)
	assertVersionHandle(t, inboxPage(t, r, uid, org), version)
}

// seedVersionPair plants a priced RFQ plus a pre-pricing one — the
// with/without-quote pair the approve-handle assertions compare.
func seedVersionPair(t *testing.T, r *rig, org uuid.UUID, now time.Time) uuid.UUID {
	t.Helper()
	priced := seedRfq(t, r.db, org, rfqSeed{
		number: 61, customerEmail: "priced@cust.dev", subject: "Priced freight",
		status: "READY", receivedAt: now.Add(-time.Hour),
	})
	seedRfq(t, r.db, org, rfqSeed{
		number: 62, customerEmail: "early@cust.dev", subject: "Early freight",
		status: "PARSING", receivedAt: now.Add(-2 * time.Hour),
	})
	return seedQuote(t, r.db, org, priced, "0.15", "100", "115", "0.9", pricingFixture(t), nil)
}

// inboxPage fetches and decodes the first inbox page for uid in org.
func inboxPage(t *testing.T, r *rig, uid, org uuid.UUID) authapi.InboxQuotePage {
	t.Helper()
	rec := r.get(t, uid, "/v1/inbox/quotes", org.String())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var page authapi.InboxQuotePage
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&page))
	return page
}

// assertVersionHandle checks the priced row carries the version id the
// approve path takes and the pre-pricing row carries none.
func assertVersionHandle(t *testing.T, page authapi.InboxQuotePage, version uuid.UUID) {
	t.Helper()
	require.Len(t, page.Items, 2)
	byNum := map[int]authapi.InboxQuote{}
	for _, it := range page.Items {
		byNum[it.RfqNumber] = it
	}
	require.NotNil(t, byNum[61].QuoteVersionID)
	assert.Equal(t, version, uuid.UUID(*byNum[61].QuoteVersionID),
		"app.quotes.id, not the RFQ id — the approve path takes the version")
	assert.Nil(t, byNum[62].QuoteVersionID, "no version → no approve handle")
}

// TestQuoteDetail: the audit page end to end — quote member matching the
// inbox mapper, email with highlighted paragraphs, the sorted extraction
// list, and the pinned pricing composition.
func TestQuoteDetail(t *testing.T) {
	r := newRig(t, now)
	org := uuid.New()
	slug := testenv.SeedOrganization(t, org, "det-a")
	uid, _ := seedWebUser(t, "det", "Detail User")
	seedWebMembership(t, org, uid, platform.RoleAdmin, now.Add(-72*time.Hour))
	// Catch-all rule → validityHours 72.
	require.NoError(t, r.db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.pricing_rules
				(organization_id, modal, default_target_margin, quote_validity_days)
			VALUES ($1, 'ALL', 0.2, 3)`, org)
		return err
	}))

	body := "Please quote FCL from Santos to Hamburg.\n\nWe need 2x40HC next week."
	received := now.Add(-time.Hour)
	rfq := seedRfq(t, r.db, org, rfqSeed{
		number: 17, customerEmail: "det@customer.dev", customerName: strPtr("Detail Co"),
		subject: "FCL Santos-Hamburg", status: "READY", receivedAt: received,
		rawText: &body, extraction: extractionFixture, confidence: strPtr("0.91"),
	})
	// One attachment row.
	require.NoError(t, r.db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO app.rfq_attachments (organization_id, rfq_id, filename, storage_key, size_bytes)
			VALUES ($1, $2, 'spec.pdf', $3, 2048)`,
			org, rfq, "tenants/"+org.String()+"/rfqs/"+rfq.String()+"/spec.pdf")
		return err
	}))
	version := seedQuote(t, r.db, org, rfq, "0.15", "9800.00", "11270.00", "0.91", pricingFixture(t), nil)

	rec := r.get(t, uid, "/v1/quotes/"+rfq.String(), org.String())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var d authapi.QuoteDetail
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&d))

	// The quote member is the same InboxQuote the list emits.
	assert.Equal(t, uuid.UUID(d.Quote.ID), rfq)
	require.NotNil(t, d.Quote.QuoteVersionID)
	assert.Equal(t, version, uuid.UUID(*d.Quote.QuoteVersionID),
		"the approve path's parameter is the version id, not the RFQ id")
	assert.Equal(t, 17, d.Quote.RfqNumber)
	assert.Equal(t, "Detail Co", d.Quote.CustomerName)
	assert.Equal(t, authapi.ReadyToSend, d.Quote.Status)
	require.NotNil(t, d.Quote.EstimatedTotal)
	assert.Equal(t, "2070", d.Quote.EstimatedTotal.Amount, "sellTotal leads on the freight currency")
	assert.Equal(t, "USD", d.Quote.EstimatedTotal.Currency)

	// Email block.
	assert.Equal(t, "det@customer.dev", string(d.Email.From))
	assert.Equal(t, slug+"@in.test", string(d.Email.To), "to = the org's inbound alias")
	require.Len(t, d.Email.Paragraphs, 2)
	para1 := d.Email.Paragraphs[0]
	// "Please quote " none + "FCL" extracted + " " none + "from Santos"
	// extracted + " " none + "to Hamburg" extracted + "." none.
	require.Len(t, para1, 7)
	assert.Equal(t, authapi.Extracted, para1[1].Highlight)
	assert.Equal(t, "FCL", para1[1].Text)
	assert.Equal(t, "from Santos", para1[3].Text)
	assert.Equal(t, "to Hamburg", para1[5].Text)
	para2 := d.Email.Paragraphs[1]
	require.Len(t, para2, 3)
	assert.Equal(t, authapi.Inferred, para2[1].Highlight,
		"the inferred field highlights as inferred, not extracted")
	assert.Equal(t, "2x40HC", para2[1].Text)
	require.Len(t, d.Email.Attachments, 1)
	assert.Equal(t, "spec.pdf", d.Email.Attachments[0].FileName)
	assert.Equal(t, 2048, d.Email.Attachments[0].SizeBytes)

	// Extraction list: sorted keys (customer_reference leads
	// alphabetically), humanized labels, ATTACHMENT evidence still listed
	// (it is a field — only paragraph spans skip it).
	require.Len(t, d.Extraction, 5)
	assert.Equal(t, "Customer reference", d.Extraction[0].Label)
	assert.Equal(t, "PO-77", d.Extraction[0].Value)
	assert.False(t, d.Extraction[0].Inferred)
	assert.Equal(t, "Dest locode", d.Extraction[1].Label)
	assert.Equal(t, "DEHAM", d.Extraction[1].Value)
	assert.Equal(t, "Equipment or unit", d.Extraction[2].Label)
	assert.Equal(t, "40HC", d.Extraction[2].Value)
	assert.True(t, d.Extraction[2].Inferred)
	assert.Equal(t, "Modal", d.Extraction[3].Label)
	assert.Equal(t, "Origin locode", d.Extraction[4].Label)

	// Pricing composition off the pinned engine output.
	p := d.Pricing
	require.NotNil(t, p)
	require.Len(t, p.Sections, 2)
	assert.Equal(t, "Frete internacional", p.Sections[0].Title)
	require.Len(t, p.Sections[0].Items, 1)
	assert.Equal(t, "Ocean freight", p.Sections[0].Items[0].Label)
	assert.Equal(t, "OFE", *p.Sections[0].Items[0].Detail)
	assert.Equal(t, authapi.Money{Amount: "2070", Currency: "USD"}, p.Sections[0].Items[0].Amount)
	assert.Equal(t, "Taxas locais", p.Sections[1].Title)
	assert.Equal(t, authapi.Money{Amount: "575", Currency: "BRL"}, p.Sections[1].Items[0].Amount)
	assert.Equal(t, authapi.Money{Amount: "9800", Currency: "BRL"}, p.TotalCost)
	assert.Equal(t, "15.00", p.MarginPercent)
	assert.Equal(t, authapi.Money{Amount: "1470", Currency: "BRL"}, p.Margin, "finalPrice − totalCost")
	assert.Equal(t, authapi.Money{Amount: "2070", Currency: "USD"}, p.ProposalTotal)
	assert.Equal(t, 91, p.ConfidencePercent)
	assert.Equal(t, 72, p.ValidityHours)
	assert.Equal(t, "5.0", p.RecalcFxThresholdPercent)
	require.NotNil(t, p.Fx)
	assert.Equal(t, "CARRIER USD", p.Fx.Label)
	assert.Equal(t, "5.586", p.Fx.Rate, "applied = base 5.32 × (1 + spread 0.05)")
	assert.Equal(t, "CARRIER", p.Fx.ReferenceLabel, "the reference is labelled by its real source")
	assert.Equal(t, "5.32", p.Fx.ReferenceRate, "the pinned base rate")
}

// TestQuoteDetailNoPricing: a pre-pricing RFQ (no quote row) answers
// with pricing null — the contract marks it optional.
func TestQuoteDetailNoPricing(t *testing.T) {
	r := newRig(t, now)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "np-a")
	uid, _ := seedWebUser(t, "np", "No Price")
	seedWebMembership(t, org, uid, platform.RoleAdmin, now.Add(-72*time.Hour))
	rfq := seedRfq(t, r.db, org, rfqSeed{
		number: 21, customerEmail: "np@cust.dev", subject: "Early RFQ",
		status: "PARSING", receivedAt: now.Add(-time.Hour),
	})

	rec := r.get(t, uid, "/v1/quotes/"+rfq.String(), org.String())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var d authapi.QuoteDetail
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&d))
	assert.Nil(t, d.Pricing)
	assert.Nil(t, d.Quote.EstimatedTotal)
	assert.Nil(t, d.Quote.QuoteVersionID, "no version → no approve handle")
	assert.Equal(t, []authapi.ExtractedField{}, d.Extraction, "absent payload is an empty list")
	assert.Equal(t, [][]authapi.EmailSegment{}, d.Email.Paragraphs)
}

// TestQuoteDetail404s: unknown and cross-tenant ids are the same uniform
// 404 — the second org's RFQ id answered under org A's context is RLS's
// proof, not a WHERE clause.
func TestQuoteDetail404s(t *testing.T) {
	r := newRig(t, now)
	orgA, orgB := uuid.New(), uuid.New()
	testenv.SeedOrganization(t, orgA, "d404-a")
	testenv.SeedOrganization(t, orgB, "d404-b")
	uid, _ := seedWebUser(t, "d404", "Detail 404")
	seedWebMembership(t, orgA, uid, platform.RoleAdmin, now.Add(-72*time.Hour))
	otherRfq := seedRfq(t, r.db, orgB, rfqSeed{
		number: 1, customerEmail: "b@cust.dev", subject: "B freight",
		status: "READY", receivedAt: now.Add(-time.Hour),
	})

	for _, tc := range []struct {
		name string
		id   string
	}{
		{"unknown id", uuid.NewString()},
		{"cross-tenant id", otherRfq.String()},
	} {
		rec := r.get(t, uid, "/v1/quotes/"+tc.id, orgA.String())
		assert.Equal(t, http.StatusNotFound, rec.Code, tc.name)
		assert.JSONEq(t, `{"error":"not_found"}`, rec.Body.String(), tc.name)
	}
}

// TestInboxEmptyOrg: a member with no RFQs still gets the page — empty
// items, zero counts, all-zero summary — never an error.
func TestInboxEmptyOrg(t *testing.T) {
	r := newRig(t, now)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "mt-a")
	uid, _ := seedWebUser(t, "mt", "Empty Org")
	seedWebMembership(t, org, uid, platform.RoleAdmin, now.Add(-24*time.Hour))

	rec := r.get(t, uid, "/v1/inbox/quotes", org.String())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var page authapi.InboxQuotePage
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&page))
	assert.Equal(t, []authapi.InboxQuote{}, page.Items)
	assert.Equal(t, authapi.InboxStatusCounts{}, page.StatusCounts)
	assert.Nil(t, page.Summary.MostUrgentSLARisk)
	assert.Equal(t, 240, page.Summary.ResponseTargetMinutes)
}

func strPtr(s string) *string { return &s }

// TestFreshSessionLoadsInboxAndDetail is the review's repro end to end:
// a fresh login — qp_access only, no qp_org anywhere — must serve the
// session bootstrap AND the two org-scoped reads. The session answer
// stamps qp_org for the resolved org; the reads also answer hintless via
// ResolveTenantDefault's first-membership fallback.
func TestFreshSessionLoadsInboxAndDetail(t *testing.T) {
	r := newRig(t, now)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "fresh-a")
	uid, _ := seedWebUser(t, "fresh", "Fresh User")
	seedWebMembership(t, org, uid, platform.RoleAdmin, now.Add(-24*time.Hour))
	rfq := seedRfq(t, r.db, org, rfqSeed{
		number: 31, customerEmail: "fresh@cust.dev", subject: "Fresh freight",
		status: "READY", receivedAt: now.Add(-time.Hour), extraction: extractionFixture,
	})

	// The bootstrap sets qp_org for the active org.
	rec := r.get(t, uid, "/v1/session", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var s authapi.Session
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&s))
	assert.Equal(t, uuid.UUID(s.Organization.ID), org)
	var orgCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "qp_org" {
			orgCookie = c
		}
	}
	require.NotNil(t, orgCookie, "GET /v1/session must stamp qp_org for the resolved org")
	assert.Equal(t, org.String(), orgCookie.Value)

	// The cookie now resolves both org-scoped reads outright.
	rec = r.get(t, uid, "/v1/inbox/quotes", "", orgCookie)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var page authapi.InboxQuotePage
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&page))
	require.Len(t, page.Items, 1)
	assert.Equal(t, 31, page.Items[0].RfqNumber)

	rec = r.get(t, uid, "/v1/quotes/"+rfq.String(), "", orgCookie)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// And without the cookie at all the default fallback still serves
	// them — the repro's exact no-hint case.
	rec = r.get(t, uid, "/v1/inbox/quotes", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&page))
	require.Len(t, page.Items, 1)

	rec = r.get(t, uid, "/v1/quotes/"+rfq.String(), "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestQuoteDetailHTMLOnlyEmail: an inbound mail that carried only an HTML
// part renders real paragraphs — reduced to text by platform.HTMLToText — instead
// of the empty list a rawText-only read produced.
func TestQuoteDetailHTMLOnlyEmail(t *testing.T) {
	r := newRig(t, now)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "html-a")
	uid, _ := seedWebUser(t, "html", "HTML User")
	seedWebMembership(t, org, uid, platform.RoleAdmin, now.Add(-72*time.Hour))

	rfq := seedRfq(t, r.db, org, rfqSeed{
		number: 33, customerEmail: "html@cust.dev", subject: "HTML freight",
		status: "READY", receivedAt: now.Add(-time.Hour),
	})
	require.NoError(t, r.db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE app.rfq_requests SET raw_html_body = $1 WHERE id = $2`,
			`<div>Please quote <b>FCL</b> from Santos.</div><div>We need 2x40HC &amp; docs.</div><style>p{display:none}</style>`,
			rfq)
		return err
	}))

	rec := r.get(t, uid, "/v1/quotes/"+rfq.String(), org.String())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var d authapi.QuoteDetail
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&d))

	require.Len(t, d.Email.Paragraphs, 2, "the HTML body must render as paragraphs, not an empty page")
	var flat string
	for _, para := range d.Email.Paragraphs {
		for _, seg := range para {
			flat += seg.Text
		}
	}
	assert.Contains(t, flat, "Please quote FCL from Santos.")
	assert.Contains(t, flat, "We need 2x40HC & docs.")
	assert.NotContains(t, flat, "display:none", "style contents must never reach the wire")
	assert.NotContains(t, flat, "<div", "raw markup must never reach the wire")
}
