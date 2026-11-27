package ops_test

// Real-Postgres proofs for the ops surface: the production chain —
// Authenticate + RequirePlatformAdmin + the strict surface — over the real
// role model. app.is_platform_admin and app.list_inbound_quarantine only
// exist under real migrations, and the Layer A/C seed rows go through
// qp_migrator — REVOKE ALL makes the owner the only writer (the
// session_integration_test.go pattern). Nothing here mocks the database:
// the point is that qp_app's function path carries the reads.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/inbox/inboxdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/ops"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// opsIntegRig mounts the strict surface behind the production ops chain on
// the real qp_app pool — buildMux's exact wrap order.
type opsIntegRig struct {
	handler http.Handler
	cfg     *platform.Config
}

func newOpsIntegRig(t *testing.T, db *tenantdb.DB) *opsIntegRig {
	t.Helper()
	cfg := opsConfig()
	store := platform.NewStore(db.Pool())
	endpoints := auth.NewEndpoints(cfg, store, store.InStorePgxTx, db,
		func(context.Context, pgx.Tx, uuid.UUID) error {
			t.Fatal("ops never stages mail")
			return nil
		},
		nil, nil, time.Now)
	endpoints.Ops = ops.NewEndpoints(inboxdb.New(db.Pool()), store)
	return &opsIntegRig{
		cfg: cfg,
		handler: platform.Authenticate(cfg)(
			ops.RequirePlatformAdmin(store)(auth.NewServer(endpoints))),
	}
}

// seedOpsUser inserts an app.users row with the flag set as given —
// qp_migrator writes Layer A (seedSessionUser's twin).
func seedOpsUser(t *testing.T, migrator *pgxpool.Pool, admin bool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, migrator.QueryRow(context.Background(),
		`INSERT INTO app.users (email, full_name, is_platform_admin)
		 VALUES ($1, 'Ops Test', $2) RETURNING id`,
		fmt.Sprintf("ops-%s@test.dev", uuid.NewString()), admin).Scan(&id))
	return id
}

// seedOpsConnector inserts the Layer C registry row the ops list reads.
func seedOpsConnector(t *testing.T, migrator *pgxpool.Pool) (id uuid.UUID, slug string) {
	t.Helper()
	slug = "ops-test-" + uuid.NewString()[:8]
	require.NoError(t, migrator.QueryRow(context.Background(),
		`INSERT INTO app.connectors (slug, display_name, target_url, status, confidence_score, last_scraped_at)
		 VALUES ($1, 'Ops Connector', 'https://ops.test/rates', 'DEGRADED', 0.875, now())
		 RETURNING id`, slug).Scan(&id))
	return id, slug
}

// seedOpsHealing inserts a session against the connector (FK order).
func seedOpsHealing(t *testing.T, migrator *pgxpool.Pool, connectorID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, migrator.QueryRow(context.Background(),
		`INSERT INTO app.healing_sessions (connector_id, status, devin_session_url, pytest_passed)
		 VALUES ($1, 'OPEN', 'https://app.devin.ai/sessions/ops-test', NULL)
		 RETURNING id`, connectorID).Scan(&id))
	return id
}

// seedOpsQuarantine inserts drop rows directly. received_at starts one day
// above the table's current max, so each call's seeds are strictly the
// newest rows — leftovers from earlier tests and earlier runs can never
// outrank or interleave them on the received_at DESC ordering.
func seedOpsQuarantine(t *testing.T, migrator *pgxpool.Pool, n int) []string {
	t.Helper()
	prefix := "opsq-" + uuid.NewString()[:8]
	var base time.Time
	require.NoError(t, migrator.QueryRow(context.Background(),
		`SELECT coalesce(max(received_at), '2000-01-01'::timestamptz) + interval '1 day'
		 FROM app.inbound_quarantine`).Scan(&base))
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		msgID := fmt.Sprintf("%s-%d", prefix, i)
		_, err := migrator.Exec(context.Background(), `
			INSERT INTO app.inbound_quarantine
				(resend_message_id, reason, recipients, matched_org_ids, subject, from_addr, received_at)
			VALUES ($1, 'multi_match', '["a@in.test","b@in.test"]'::jsonb, '{}'::uuid[],
			        'RFQ ops seed', 'ops@example.com', $2::timestamptz + $3 * interval '1 second')`,
			msgID, base, i)
		require.NoError(t, err)
		ids = append(ids, msgID)
	}
	return ids
}

// opsGet fires a GET through the mounted chain.
func (r *opsIntegRig) get(t *testing.T, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	return rec
}

// TestOpsQuarantineEndToEnd proves the admin path reads quarantine through
// the 0018 function: page one of the seeds lands, total covers the whole
// table, and the camelCase mapping survives the real driver types.
func TestOpsQuarantineEndToEnd(t *testing.T) {
	db := testenv.Open(t)
	migrator := testenv.MigratorPool(t)
	rig := newOpsIntegRig(t, db)
	seeds := seedOpsQuarantine(t, migrator, 3)
	admin := seedOpsUser(t, migrator, true)

	rec := rig.get(t, "/ops/quarantine?limit=2", mintAccess(t, rig.cfg, admin))
	require.Equal(t, http.StatusOK, rec.Code)
	var page struct {
		Items []struct {
			ID              string   `json:"id"`
			ResendMessageID string   `json:"resendMessageId"`
			Reason          string   `json:"reason"`
			Recipients      []string `json:"recipients"`
			Subject         string   `json:"subject"`
			ReceivedAt      string   `json:"receivedAt"`
		} `json:"items"`
		Total int `json:"total"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	assert.GreaterOrEqual(t, page.Total, 3, "total must cover the whole table")
	require.Len(t, page.Items, 2, "limit=2 pages two rows")
	// received_at DESC puts the last seed first.
	assert.Equal(t, seeds[2], page.Items[0].ResendMessageID)
	assert.Equal(t, seeds[1], page.Items[1].ResendMessageID)
	assert.Equal(t, "multi_match", page.Items[0].Reason)
	assert.Equal(t, []string{"a@in.test", "b@in.test"}, page.Items[0].Recipients)
}

// TestOpsQuarantineSecondPage proves offset advances the same stable
// ordering — the remaining seed leads page two.
func TestOpsQuarantineSecondPage(t *testing.T) {
	db := testenv.Open(t)
	migrator := testenv.MigratorPool(t)
	rig := newOpsIntegRig(t, db)
	seeds := seedOpsQuarantine(t, migrator, 3)
	admin := seedOpsUser(t, migrator, true)

	rec := rig.get(t, "/ops/quarantine?limit=2&offset=2", mintAccess(t, rig.cfg, admin))
	require.Equal(t, http.StatusOK, rec.Code)
	var page struct {
		Items []struct {
			ResendMessageID string `json:"resendMessageId"`
		} `json:"items"`
		Total int `json:"total"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.NotEmpty(t, page.Items)
	assert.Equal(t, seeds[0], page.Items[0].ResendMessageID,
		"page two continues the received_at DESC ordering")
}

// TestOpsConnectorsAndHealingEndToEnd seeds one connector plus its healing
// session and proves both list reads carry them — the Layer C plain-SELECT
// path qp_app holds.
func TestOpsConnectorsAndHealingEndToEnd(t *testing.T) {
	db := testenv.Open(t)
	migrator := testenv.MigratorPool(t)
	rig := newOpsIntegRig(t, db)
	connID, slug := seedOpsConnector(t, migrator)
	sessionID := seedOpsHealing(t, migrator, connID)
	admin := seedOpsUser(t, migrator, true)

	rec := rig.get(t, "/ops/connectors", mintAccess(t, rig.cfg, admin))
	require.Equal(t, http.StatusOK, rec.Code)
	var connPage struct {
		Items []struct {
			ID              string `json:"id"`
			Slug            string `json:"slug"`
			Status          string `json:"status"`
			ConfidenceScore string `json:"confidenceScore"`
			TargetURL       string `json:"targetUrl"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &connPage))
	var found bool
	for _, c := range connPage.Items {
		if c.Slug == slug {
			found = true
			assert.Equal(t, connID.String(), c.ID)
			assert.Equal(t, "DEGRADED", c.Status)
			assert.Equal(t, "0.875", c.ConfidenceScore,
				"numeric(5,4) renders as a decimal string, never a float")
			assert.Equal(t, "https://ops.test/rates", c.TargetURL)
		}
	}
	assert.True(t, found, "seeded connector must appear in /ops/connectors")

	rec = rig.get(t, "/ops/healing", mintAccess(t, rig.cfg, admin))
	require.Equal(t, http.StatusOK, rec.Code)
	var healPage struct {
		Items []struct {
			ID              string  `json:"id"`
			ConnectorID     string  `json:"connectorId"`
			Status          string  `json:"status"`
			DevinSessionURL *string `json:"devinSessionUrl"`
			PytestPassed    *bool   `json:"pytestPassed"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &healPage))
	found = false
	for _, s := range healPage.Items {
		if s.ID == sessionID.String() {
			found = true
			assert.Equal(t, connID.String(), s.ConnectorID)
			assert.Equal(t, "OPEN", s.Status)
			require.NotNil(t, s.DevinSessionURL)
			assert.Equal(t, "https://app.devin.ai/sessions/ops-test", *s.DevinSessionURL)
		}
	}
	assert.True(t, found, "seeded healing session must appear in /ops/healing")
}

// TestOpsGateDeniesNonAdminAndUnknown proves the gate under the real
// function: a user row without the flag and a session for a user id that
// does not exist both get the uniform 403 — the coalesce collapses them.
func TestOpsGateDeniesNonAdminAndUnknown(t *testing.T) {
	db := testenv.Open(t)
	migrator := testenv.MigratorPool(t)
	rig := newOpsIntegRig(t, db)

	rec := rig.get(t, "/ops/connectors",
		mintAccess(t, rig.cfg, seedOpsUser(t, migrator, false)))
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"a valid session without the flag is forbidden")
	assert.JSONEq(t,
		`{"error":"forbidden","message":"you do not have permission to perform this action"}`,
		rec.Body.String())

	rec = rig.get(t, "/ops/connectors", mintAccess(t, rig.cfg, uuid.New()))
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"a session whose user no longer exists is forbidden, not an error")
}
