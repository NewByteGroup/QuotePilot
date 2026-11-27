package ops_test

// Unit coverage for the ops surface: RequirePlatformAdmin's admit/deny
// matrix through the production middleware shape, and the three strict
// handlers mounted behind the real generated mux with fake read seams —
// the same rig shape the auth fake suites keep (in-memory seams, real
// middleware, real authapi routing). No Postgres here; the role-model
// proofs live in ops_integration_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/inbox/inboxdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/ops"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// opsConfig is the request-edge config the rigs sign cookies against — one
// 32-byte key, the same shape sessionConfig/approveConfig keep.
func opsConfig() *platform.Config {
	return &platform.Config{
		WebURL:       "https://web.test",
		JWTKeys:      map[string]string{"k1": strings.Repeat("s", 32)},
		ActiveKID:    "k1",
		AccessTTL:    15 * time.Minute,
		RefreshTTL:   30 * 24 * time.Hour,
		LoginTTL:     15 * time.Minute,
		CookieSecure: true,
	}
}

// fakeGate records the user id it was asked about and answers the scripted
// decision — the AdminGate seam *platform.Store fills in production.
type fakeGate struct {
	admin   bool
	err     error
	gotUser uuid.UUID
	called  int
}

func (f *fakeGate) IsPlatformAdmin(_ context.Context, userID uuid.UUID) (bool, error) {
	f.called++
	f.gotUser = userID
	return f.admin, f.err
}

// fakeCatalog serves the two sqlc list seams — *inboxdb.Queries' twin.
type fakeCatalog struct {
	connectors []inboxdb.ListOpsConnectorsRow
	sessions   []inboxdb.ListOpsHealingSessionsRow
	err        error
}

func (f *fakeCatalog) ListOpsConnectors(context.Context) ([]inboxdb.ListOpsConnectorsRow, error) {
	return f.connectors, f.err
}

func (f *fakeCatalog) ListOpsHealingSessions(context.Context) ([]inboxdb.ListOpsHealingSessionsRow, error) {
	return f.sessions, f.err
}

// fakeQuarantine serves the function-family page seam and records the page
// bounds the handler computed — the clamping assertion.
type fakeQuarantine struct {
	rows      []platform.InboundQuarantineRow
	total     int64
	err       error
	gotLimit  int
	gotOffset int
}

func (f *fakeQuarantine) ListInboundQuarantine(_ context.Context, limit, offset int) ([]platform.InboundQuarantineRow, int64, error) {
	f.gotLimit, f.gotOffset = limit, offset
	return f.rows, f.total, f.err
}

// mintAccess signs a real qp_access cookie — Authenticate's only input.
func mintAccess(t *testing.T, cfg *platform.Config, userID uuid.UUID) *http.Cookie {
	t.Helper()
	raw, err := cfg.SignAccess(userID, sessionIDForTest(t), time.Now())
	require.NoError(t, err)
	return &http.Cookie{Name: "qp_access", Value: raw}
}

func sessionIDForTest(t *testing.T) uuid.UUID {
	t.Helper()
	return uuid.New()
}

// opsRig mounts the strict surface exactly as cmd/server's buildMux does:
// Authenticate → RequirePlatformAdmin → auth.NewServer. Only the ops
// delegate is wired; the auth endpoints' own dependencies are irrelevant
// to these routes, so they pass nil like the fake rigs' unused seams.
type opsRig struct {
	handler http.Handler
	cfg     *platform.Config
	gate    *fakeGate
	catalog *fakeCatalog
	quar    *fakeQuarantine
}

func newOpsRig(t *testing.T) *opsRig {
	t.Helper()
	cfg := opsConfig()
	gate := &fakeGate{admin: true}
	catalog := &fakeCatalog{}
	quar := &fakeQuarantine{}
	endpoints := auth.NewEndpoints(cfg, nil, nil, nil, nil, nil, nil, time.Now)
	endpoints.Ops = ops.NewEndpoints(catalog, quar)
	handler := platform.Authenticate(cfg)(ops.RequirePlatformAdmin(gate)(auth.NewServer(endpoints)))
	return &opsRig{handler: handler, cfg: cfg, gate: gate, catalog: catalog, quar: quar}
}

// get fires path at the rig with the given user's cookie — nil cookie for
// the unauthenticated case.
func (r *opsRig) get(t *testing.T, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	return rec
}

// TestGateAdmitsPlatformAdmin proves the happy path end-to-end: a flagged
// user reaches the handler — the 200 is the connectors list answering.
func TestGateAdmitsPlatformAdmin(t *testing.T) {
	rig := newOpsRig(t)
	userID := uuid.New()
	rec := rig.get(t, "/ops/connectors", mintAccess(t, rig.cfg, userID))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, rig.gate.called)
	assert.Equal(t, userID, rig.gate.gotUser,
		"the gate must check the token's sub, not a request-supplied id")
	assert.JSONEq(t, `{"items":[]}`, rec.Body.String())
}

// TestGateDeniesNonAdmin proves a valid session without the flag is the
// uniform 403 — never the handler.
func TestGateDeniesNonAdmin(t *testing.T) {
	rig := newOpsRig(t)
	rig.gate.admin = false
	rec := rig.get(t, "/ops/connectors", mintAccess(t, rig.cfg, uuid.New()))
	require.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, `{"error":"forbidden","message":"you do not have permission to perform this action"}`,
		rec.Body.String())
}

// TestGateDeniesUnauthenticated proves a missing/forged cookie never
// reaches the gate: Authenticate's 401 comes first.
func TestGateDeniesUnauthenticated(t *testing.T) {
	rig := newOpsRig(t)
	rec := rig.get(t, "/ops/connectors", nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Zero(t, rig.gate.called, "an unauthenticated request must not spend a gate lookup")
}

// TestGateErrorIsInternal proves a gate lookup failure is the uniform 500
// — a server fault, not an authorization answer (checkMembership's rule).
func TestGateErrorIsInternal(t *testing.T) {
	rig := newOpsRig(t)
	rig.gate.err = errors.New("conn dead")
	rec := rig.get(t, "/ops/connectors", mintAccess(t, rig.cfg, uuid.New()))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, `{"error":"internal","message":"an unexpected server error occurred"}`,
		rec.Body.String())
}

// TestGateMisMountedIsUnauthenticated proves the fail-closed answer when
// the middleware runs without Authenticate — no user context exists.
func TestGateMisMountedIsUnauthenticated(t *testing.T) {
	gate := &fakeGate{admin: true}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest(http.MethodGet, "/ops/connectors", nil)
	rec := httptest.NewRecorder()
	ops.RequirePlatformAdmin(gate)(next).ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Zero(t, gate.called, "no user context means no gate lookup")
}

// TestListOpsConnectorsPopulated checks the row→contract mapping: every
// column survives camelCased, confidence_score is a decimal string, and a
// NULL last_scraped_at is JSON null.
func TestListOpsConnectorsPopulated(t *testing.T) {
	rig := newOpsRig(t)
	id, scraped := uuid.New(), time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	rig.catalog.connectors = []inboxdb.ListOpsConnectorsRow{{
		ID:              pgtype.UUID{Bytes: id, Valid: true},
		Slug:            "cma_cgm",
		DisplayName:     "CMA CGM",
		TargetUrl:       "https://www.cma-cgm.com/ebusiness",
		Status:          "DEGRADED",
		ConfidenceScore: ptr(decimal.NewFromFloat(0.875)),
		LastScrapedAt:   pgtype.Timestamptz{Time: scraped, Valid: true},
		CreatedAt:       pgtype.Timestamptz{Time: scraped.Add(-time.Hour), Valid: true},
		UpdatedAt:       pgtype.Timestamptz{Time: scraped, Valid: true},
	}}
	rec := rig.get(t, "/ops/connectors", mintAccess(t, rig.cfg, uuid.New()))
	require.Equal(t, http.StatusOK, rec.Code)
	var page struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Len(t, page.Items, 1)
	c := page.Items[0]
	assert.Equal(t, id.String(), c["id"])
	assert.Equal(t, "cma_cgm", c["slug"])
	assert.Equal(t, "CMA CGM", c["displayName"])
	assert.Equal(t, "https://www.cma-cgm.com/ebusiness", c["targetUrl"])
	assert.Equal(t, "DEGRADED", c["status"])
	assert.Equal(t, "0.875", c["confidenceScore"], "numeric must cross the wire as a string")
	assert.Equal(t, scraped.Format(time.RFC3339), c["lastScrapedAt"])
}

// TestListOpsConnectorsEmpty proves the empty registry answers a bare
// items array — never null, never an error.
func TestListOpsConnectorsEmpty(t *testing.T) {
	rig := newOpsRig(t)
	rec := rig.get(t, "/ops/connectors", mintAccess(t, rig.cfg, uuid.New()))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"items":[]}`, rec.Body.String())
}

// TestListOpsHealingSessionsPopulated checks the healing row mapping
// including the nullable set — NULLs must cross as JSON null.
func TestListOpsHealingSessionsPopulated(t *testing.T) {
	rig := newOpsRig(t)
	id, connID := uuid.New(), uuid.New()
	rig.catalog.sessions = []inboxdb.ListOpsHealingSessionsRow{{
		ID:              pgtype.UUID{Bytes: id, Valid: true},
		ConnectorID:     pgtype.UUID{Bytes: connID, Valid: true},
		Status:          "OPEN",
		CreatedAt:       pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		PytestPassed:    ptr(true),
		DevinSessionUrl: ptr("https://app.devin.ai/sessions/x"),
	}}
	rec := rig.get(t, "/ops/healing", mintAccess(t, rig.cfg, uuid.New()))
	require.Equal(t, http.StatusOK, rec.Code)
	var page struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Len(t, page.Items, 1)
	s := page.Items[0]
	assert.Equal(t, id.String(), s["id"])
	assert.Equal(t, connID.String(), s["connectorId"])
	assert.Equal(t, "OPEN", s["status"])
	assert.Equal(t, true, s["pytestPassed"])
	assert.Equal(t, "https://app.devin.ai/sessions/x", s["devinSessionUrl"])
	assert.Nil(t, s["resolvedAt"], "an OPEN session's resolved_at must be null")
	assert.Nil(t, s["githubPrUrl"])
}

// TestListOpsQuarantinePopulated checks the function-row mapping plus
// total, and proves the page defaults reach the source unclamped.
func TestListOpsQuarantinePopulated(t *testing.T) {
	rig := newOpsRig(t)
	id, orgA, orgB := uuid.New(), uuid.New(), uuid.New()
	received := time.Date(2026, 11, 21, 8, 30, 0, 0, time.UTC)
	rig.quar.rows = []platform.InboundQuarantineRow{{
		ID:              id,
		ResendMessageID: "re_123",
		Reason:          "multi_match",
		Recipients:      []string{"a@in.test", "b@in.test"},
		MatchedOrgIDs:   []uuid.UUID{orgA, orgB},
		Subject:         "RFQ: 40 boxes",
		FromAddr:        "ops@example.com",
		ReceivedAt:      received,
		QuarantinedAt:   received.Add(time.Minute),
	}}
	rig.quar.total = 7
	rec := rig.get(t, "/ops/quarantine", mintAccess(t, rig.cfg, uuid.New()))
	require.Equal(t, http.StatusOK, rec.Code)
	var page struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	assert.Equal(t, 7, page.Total)
	require.Len(t, page.Items, 1)
	q := page.Items[0]
	assert.Equal(t, id.String(), q["id"])
	assert.Equal(t, "re_123", q["resendMessageId"])
	assert.Equal(t, "multi_match", q["reason"])
	assert.Equal(t, []any{"a@in.test", "b@in.test"}, q["recipients"])
	assert.Equal(t, []any{orgA.String(), orgB.String()}, q["matchedOrgIds"])
	assert.Equal(t, "ops@example.com", q["fromAddr"])
	assert.Equal(t, received.Format(time.RFC3339), q["receivedAt"])
	assert.Equal(t, received.Add(time.Minute).Format(time.RFC3339), q["quarantinedAt"])
	assert.Equal(t, 50, rig.quar.gotLimit, "absent limit must default to 50")
	assert.Equal(t, 0, rig.quar.gotOffset)
}

// TestListOpsQuarantineEmptyPage proves an out-of-range page still answers
// a well-formed empty page with the live total — not an error.
func TestListOpsQuarantineEmptyPage(t *testing.T) {
	rig := newOpsRig(t)
	rig.quar.total = 3
	rec := rig.get(t, "/ops/quarantine?offset=999", mintAccess(t, rig.cfg, uuid.New()))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"items":[],"total":3}`, rec.Body.String())
	assert.Equal(t, 999, rig.quar.gotOffset)
}

// TestListOpsQuarantineClampsPage proves out-of-contract bounds clamp
// before the call — negative offset, zero and over-max limit.
func TestListOpsQuarantineClampsPage(t *testing.T) {
	for _, tc := range []struct {
		query                 string
		wantLimit, wantOffset int
	}{
		{"?limit=0&offset=-5", 1, 0},
		{"?limit=9999&offset=2", 200, 2},
		{"?limit=25", 25, 0},
	} {
		rig := newOpsRig(t)
		rec := rig.get(t, "/ops/quarantine"+tc.query, mintAccess(t, rig.cfg, uuid.New()))
		require.Equal(t, http.StatusOK, rec.Code, tc.query)
		assert.Equal(t, tc.wantLimit, rig.quar.gotLimit, tc.query)
		assert.Equal(t, tc.wantOffset, rig.quar.gotOffset, tc.query)
	}
}

// TestListOpsReadErrorIs500 proves a read failure collapses to the uniform
// 500 — driver detail never crosses the wire.
func TestListOpsReadErrorIs500(t *testing.T) {
	rig := newOpsRig(t)
	rig.catalog.err = errors.New("pool exhausted")
	rec := rig.get(t, "/ops/connectors", mintAccess(t, rig.cfg, uuid.New()))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, `{"error":"internal","message":"an unexpected server error occurred"}`+"\n",
		rec.Body.String())
}

// TestOpsMisMountedHandlersIs401 proves the handlers' own fail-closed
// answer: called without a user context (the middleware chain skipped),
// each strict method returns the uniform 401 response object.
func TestOpsMisMountedHandlersIs401(t *testing.T) {
	ep := ops.NewEndpoints(&fakeCatalog{}, &fakeQuarantine{})
	ctx := context.Background()
	resp, err := ep.ListOpsConnectors(ctx, authapi.ListOpsConnectorsRequestObject{})
	require.NoError(t, err)
	assert.IsType(t, authapi.ListOpsConnectors401JSONResponse{}, resp)
	resp2, err := ep.ListOpsHealingSessions(ctx, authapi.ListOpsHealingSessionsRequestObject{})
	require.NoError(t, err)
	assert.IsType(t, authapi.ListOpsHealingSessions401JSONResponse{}, resp2)
	resp3, err := ep.ListOpsQuarantine(ctx, authapi.ListOpsQuarantineRequestObject{})
	require.NoError(t, err)
	assert.IsType(t, authapi.ListOpsQuarantine401JSONResponse{}, resp3)
}

// ptr is the tests' generic pointer helper for nullable row fields.
func ptr[T any](v T) *T { return &v }
