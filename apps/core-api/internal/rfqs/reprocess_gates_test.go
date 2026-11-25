package rfqs_test

// Refusal gates for POST /v1/rfqs/{id}/reprocess (spec 2026-11-11 section 4
// "Reprocess"): the status gate — including the READY-awaiting-render
// carveout's refusal side — the uniform 404, the role matrix and the
// unauthenticated path. Fixtures, the rig and the read-back helpers live
// in reprocess_test.go — same package.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// ---------------------------------------------------------------------------
// Refusals — status gate, tenancy, auth
// ---------------------------------------------------------------------------

// TestReprocessConflictStatuses: every non-parked status is a 409 with
// rfq_not_parked plus the current app.rfq_status in the body — IGNORED is
// intentionally terminal (spec Reprocess). The seeded READY row carries
// no DRAFT quote, so it stays a refusal; the READY carveout needs a
// DRAFT awaiting its first render (covered in the stages file).
func TestReprocessConflictStatuses(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "reproc-409")
	user := seedUser(t)
	seedMembership(t, org, user, platform.RolePricingAnalyst)
	rig := newReprocessRig(t, db, 60*time.Second, func() time.Time { return rigNow })
	for _, status := range []string{"RECEIVED", "PARSING", "PRICING", "READY", "APPROVED_SENT", "IGNORED"} {
		t.Run(status, func(t *testing.T) {
			rfq := seedRfq(t, db, org, rfqSeed{status: status})
			rec := rig.postReprocess(t, user, org, rfq)
			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			var body authapi.ReprocessConflict
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, authapi.ErrorCodeRfqNotParked, body.Error)
			assert.NotEmpty(t, body.Message, "issue #61: every error body carries display text")
			assert.Equal(t, status, body.Status, "the current status rides the 409 body")
			assert.Empty(t, stagedJobs(t, db, rfq))
			r := loadRfq(t, db, org, rfq)
			assert.Equal(t, status, r.Status, "a 409 writes nothing")
		})
	}
}

// TestReprocessReadyRenderedDraft409: the READY carveout is exactly one
// shape — a DRAFT still awaiting its first render. A READY RFQ whose
// DRAFT already has its PDF is a normal 409 like any other live status.
func TestReprocessReadyRenderedDraft409(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "reproc-409-pdf")
	user := seedUser(t)
	seedMembership(t, org, user, platform.RolePricingAnalyst)
	rig := newReprocessRig(t, db, 60*time.Second, func() time.Time { return rigNow })
	rfq := seedRfq(t, db, org, rfqSeed{
		status:     "READY",
		textBody:   strptr("please quote"),
		payload:    extractionNotOwed,
		confidence: strptr("0.9100"),
	})
	pdf := "tenants/" + org.String() + "/quotes/x_v1.pdf"
	seedDraftQuote(t, db, org, rfq, &pdf)

	rec := rig.postReprocess(t, user, org, rfq)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var body authapi.ReprocessConflict
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, authapi.ErrorCodeRfqNotParked, body.Error)
	assert.NotEmpty(t, body.Message, "issue #61: every error body carries display text")
	assert.Equal(t, "READY", body.Status)
	assert.Empty(t, stagedJobs(t, db, rfq), "a 409 enqueues nothing")
}

// TestReprocessUnknownAndCrossTenant404 proves the two 404 cases: an id
// no tenant owns and an id another tenant owns both answer the uniform
// not_found — RLS makes the cross-tenant row invisible to the FOR UPDATE.
func TestReprocessUnknownAndCrossTenant404(t *testing.T) {
	db := testenv.Open(t)
	orgA, orgB := uuid.New(), uuid.New()
	testenv.SeedOrganization(t, orgA, "reproc-404a")
	testenv.SeedOrganization(t, orgB, "reproc-404b")
	user := seedUser(t)
	seedMembership(t, orgB, user, platform.RolePricingAnalyst)
	rig := newReprocessRig(t, db, 60*time.Second, func() time.Time { return rigNow })
	foreign := seedRfq(t, db, orgA, rfqSeed{status: "NEEDS_REVIEW"})

	rec := rig.postReprocess(t, user, orgB, uuid.New())
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"error":"not_found","message":"the requested resource does not exist"}`, rec.Body.String())

	rec = rig.postReprocess(t, user, orgB, foreign)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String(),
		"cross-tenant RFQ ids are invisible — the same uniform 404")

	r := loadRfq(t, db, orgA, foreign)
	assert.Equal(t, "NEEDS_REVIEW", r.Status, "a 404 never touches the row")
}

// TestReprocessRoles covers the reprocess matrix (spec Reprocess): every
// membership role may reprocess — membership_role only carries the
// three — so 403 is declared but unreachable today.
func TestReprocessRoles(t *testing.T) {
	db := testenv.Open(t)
	for _, role := range []string{platform.RoleOwner, platform.RoleAdmin, platform.RolePricingAnalyst} {
		t.Run(role, func(t *testing.T) {
			org := uuid.New()
			testenv.SeedOrganization(t, org, "reproc-role-"+strings.ToLower(role))
			user := seedUser(t)
			seedMembership(t, org, user, role)
			rig := newReprocessRig(t, db, 60*time.Second, func() time.Time { return rigNow })
			rfq := seedRfq(t, db, org, rfqSeed{status: "FAILED", failureReason: strptr("boom")})
			rec := rig.postReprocess(t, user, org, rfq)
			assert.Equal(t, http.StatusAccepted, rec.Code, "%s may reprocess: %s", role, rec.Body.String())
		})
	}
}

// TestReprocessUnauthenticated: no qp_access cookie → the middleware's
// 401 before any tenant work; a memberless user (membership revoked) →
// 404.
func TestReprocessUnauthenticated(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "reproc-unauth")
	user := seedUser(t)
	seedMembership(t, org, user, platform.RoleOwner)
	rig := newReprocessRig(t, db, 60*time.Second, func() time.Time { return rigNow })
	rfq := seedRfq(t, db, org, rfqSeed{status: "NEEDS_REVIEW"})

	req := httptest.NewRequest(http.MethodPost, "/v1/rfqs/"+rfq.String()+"/reprocess", nil)
	req.Header.Set("X-Organization-Id", org.String())
	rec := httptest.NewRecorder()
	rig.handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	// Valid JWT but no membership in this org → ResolveTenant's 404.
	stranger := seedUser(t)
	rec = rig.postReprocess(t, stranger, org, rfq)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	r := loadRfq(t, db, org, rfq)
	assert.Equal(t, "NEEDS_REVIEW", r.Status)
}
