package tenantdb_test

// Teardown-path enforcement for the v1.3 data model: the deliberate
// platform_delete_organization path (spec section 4) exercised as qp_admin —
// the only role with EXECUTE — plus the abuse case it must survive: qp_app
// forging the app.tenant_teardown GUC outside a SECURITY DEFINER context.
// Split from schema_enforcement_test.go to keep both files under the
// 500-line style limit (AGENTS.md).

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// TestSchemaEnforcementDeleteOrganization exercises
// platform_delete_organization end to end: one call removes the org and
// every tenant row the cascades can reach — including an APPROVED_SENT quote,
// whose immutability triggers yield only to the transaction-local teardown
// flag (spec section 4). A second call raises.
func TestSchemaEnforcementDeleteOrganization(t *testing.T) {
	db := testenv.Open(t)
	admin := testenv.AdminDB(t)
	migrator := testenv.MigratorPool(t)
	ctx := context.Background()

	org := uuid.New()
	testenv.SeedOrganization(t, org, "teardown-org")
	var userID uuid.UUID
	require.NoError(t, migrator.QueryRow(ctx,
		`INSERT INTO app.users (email, full_name) VALUES ($1, 'Gone User') RETURNING id`,
		"gone-"+uuid.NewString()+"@test.dev").Scan(&userID))
	_, err := migrator.Exec(ctx,
		`INSERT INTO app.organization_memberships (organization_id, user_id, role) VALUES ($1, $2, 'ADMIN')`,
		org.String(), userID.String())
	require.NoError(t, err)
	// Layer C is qp_app-read-only, so the connector the preference points at
	// is seeded as qp_migrator.
	connectorSlug := "teardown-conn-" + uuid.NewString()
	_, err = migrator.Exec(ctx,
		`INSERT INTO app.connectors (slug, display_name, target_url) VALUES ($1, 'Teardown Conn', 'https://example.test/t')`,
		connectorSlug)
	require.NoError(t, err)

	// Seed the full tenant subtree as qp_app inside InTenantTx, ending with
	// the quote APPROVED_SENT so teardown must pass both immutability guards.
	require.NoError(t, db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		seedBatchTariff(t, ctx, tx, org)
		rfq := seedRFQ(t, ctx, tx, org)
		mustExec(t, ctx, tx, `
			INSERT INTO app.carrier_offers (organization_id, rfq_id, carrier, offer_payload_json, fetched_at)
			VALUES ($1, $2, 'HAPAG', '{}'::jsonb, now())`,
			org.String(), rfq.String())
		mustExec(t, ctx, tx, `
			INSERT INTO app.rfq_attachments (organization_id, rfq_id, storage_key, filename)
			VALUES ($1, $2, 'tenants/x/rfqs/y/a.pdf', 'a.pdf')`,
			org.String(), rfq.String())
		quote := seedQuote(t, ctx, tx, org, rfq)
		seedQuoteLine(t, ctx, tx, org, quote)
		approveQuote(t, ctx, tx, quote)
		mustExec(t, ctx, tx,
			`INSERT INTO app.org_quote_counters (organization_id, next_seq) VALUES ($1, 2)`, org.String())
		mustExec(t, ctx, tx,
			`INSERT INTO app.tenant_audit_logs (organization_id, action) VALUES ($1, 'QUOTE_APPROVED_SENT')`,
			org.String())
		mustExec(t, ctx, tx,
			`INSERT INTO app.inbound_messages (organization_id, resend_message_id) VALUES ($1, 'td-1')`,
			org.String())
		mustExec(t, ctx, tx,
			`INSERT INTO app.pricing_rules (organization_id, modal, default_target_margin) VALUES ($1, 'ALL', 0.1800)`,
			org.String())
		mustExec(t, ctx, tx,
			`INSERT INTO app.suppression_list (organization_id, email) VALUES ($1, 'bounced@test.dev')`,
			org.String())
		mustExec(t, ctx, tx, `
			INSERT INTO app.organization_connector_preferences (organization_id, connector_id)
			SELECT $1, c.id FROM app.connectors AS c WHERE c.slug = $2`,
			org.String(), connectorSlug)
		return nil
	}))

	// The teardown call itself runs as qp_admin — the only role with EXECUTE —
	// inside InTenantTx for the org being deleted: the function's scope guard
	// refuses anything else (Devin Review). Passing proves the triggers saw
	// current_user = qp_migrator inside the cascades — the SECURITY DEFINER
	// context the flag alone cannot fake.
	err = admin.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT app.platform_delete_organization($1)`, org.String())
		return err
	})
	require.NoError(t, err)

	// Organizations is Layer A (no RLS): the row must be gone outright.
	var orgRows int
	require.NoError(t, migrator.QueryRow(ctx,
		`SELECT count(*) FROM app.organizations WHERE id = $1`, org.String()).Scan(&orgRows))
	assert.Equal(t, 0, orgRows, "the org row itself must be gone")

	// Tenant rows: count inside one migrator tx with the dead org's context
	// pinned, so FORCE RLS cannot hide leftovers from the assertion itself.
	tx, err := migrator.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config('app.current_organization_id', $1, true)`, org.String())
	require.NoError(t, err)
	for _, table := range []string{
		"private_tariff_batches", "private_tariffs", "rfq_requests", "rfq_attachments",
		"carrier_offers", "quotes", "quote_line_items", "org_quote_counters",
		"tenant_audit_logs", "inbound_messages", "pricing_rules", "suppression_list",
		"organization_connector_preferences", "organization_memberships",
	} {
		var n int
		require.NoError(t, tx.QueryRow(ctx,
			fmt.Sprintf(`SELECT count(*) FROM app.%s WHERE organization_id = $1`, table),
			org.String()).Scan(&n))
		assert.Zero(t, n, "app.%s must be empty after teardown", table)
	}

	// The user row survives — memberships were org-scoped, identities are not.
	var userRows int
	require.NoError(t, migrator.QueryRow(ctx,
		`SELECT count(*) FROM app.users WHERE id = $1`, userID.String()).Scan(&userRows))
	assert.Equal(t, 1, userRows, "user identities outlive their orgs")

	// Replaying teardown on a missing org raises instead of silently passing
	// (still inside InTenantTx — unscoped calls now hit the scope guard
	// first, which the negative test below covers).
	err = admin.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT app.platform_delete_organization($1)`, org.String())
		return err
	})
	require.Error(t, err, "second teardown call must raise")
	assert.Contains(t, err.Error(), "unknown organization")
}

// TestSchemaEnforcementDeleteOrganizationScope is the scope-guard half of
// the teardown contract (Devin Review): qp_admin holds EXECUTE on
// platform_delete_organization, but the function must refuse to delete any
// org other than the one the caller's tenant transaction is pinned to —
// and any call with no tenant context at all. qp_app cannot reach the
// function at all (permission denied, not a scope error). Org B must
// survive every attempt fully intact.
func TestSchemaEnforcementDeleteOrganizationScope(t *testing.T) {
	db := testenv.Open(t)
	admin := testenv.AdminDB(t)
	migrator := testenv.MigratorPool(t)
	ctx := context.Background()

	orgA := uuid.New()
	testenv.SeedOrganization(t, orgA, "scope-org-a")
	orgB := uuid.New()
	testenv.SeedOrganization(t, orgB, "scope-org-b")

	// A tenant row under B proves "fully intact" afterwards.
	require.NoError(t, db.InTenantTx(ctx, orgB, func(ctx context.Context, tx pgx.Tx) error {
		mustExec(t, ctx, tx,
			`INSERT INTO app.inbound_messages (organization_id, resend_message_id) VALUES ($1, 'scope-b-1')`,
			orgB.String())
		return nil
	}))

	// Pinned to A but asking for B: the cross-scope call raises the guard.
	err := admin.InTenantTx(ctx, orgA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT app.platform_delete_organization($1)`, orgB.String())
		return err
	})
	require.Error(t, err, "teardown pinned to org A must refuse org B")
	assert.Contains(t, err.Error(), "must run inside an InTenantTx scoped to organization")

	// No tenant context at all raises the same guard.
	_, err = admin.Pool().Exec(ctx, `SELECT app.platform_delete_organization($1)`, orgB.String())
	require.Error(t, err, "unscoped teardown call must refuse")
	assert.Contains(t, err.Error(), "must run inside an InTenantTx scoped to organization")

	// qp_app — even pinned to org B — is refused before the scope guard ever
	// runs: EXECUTE is qp_admin's alone (Devin Review trust model).
	err = db.InTenantTx(ctx, orgB, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT app.platform_delete_organization($1)`, orgB.String())
		return err
	})
	require.Error(t, err, "qp_app must not EXECUTE platform_delete_organization")
	assert.Contains(t, err.Error(), "permission denied")

	// Org B is fully intact: the registry row and its tenant rows survived.
	var orgRows int
	require.NoError(t, migrator.QueryRow(ctx,
		`SELECT count(*) FROM app.organizations WHERE id = $1`, orgB.String()).Scan(&orgRows))
	assert.Equal(t, 1, orgRows, "org B must still exist")

	tx, err := migrator.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config('app.current_organization_id', $1, true)`, orgB.String())
	require.NoError(t, err)
	var msgRows int
	require.NoError(t, tx.QueryRow(ctx,
		`SELECT count(*) FROM app.inbound_messages WHERE organization_id = $1`,
		orgB.String()).Scan(&msgRows))
	assert.Equal(t, 1, msgRows, "org B tenant rows must survive refused teardowns")
}

// TestSchemaEnforcementTeardownFlagForge is the negative half of the
// teardown contract: qp_app can set app.tenant_teardown in its own
// transaction (set_config is unrestricted), but the immutability triggers now
// also demand a SECURITY DEFINER context (current_user <> session_user). A
// direct qp_app write can satisfy neither, so every forged-flag mutation of
// an APPROVED_SENT quote or its lines must still raise.
func TestSchemaEnforcementTeardownFlagForge(t *testing.T) {
	db := testenv.Open(t)
	ctx := context.Background()

	org := uuid.New()
	testenv.SeedOrganization(t, org, "teardown-forge")

	var quoteID, lineID uuid.UUID
	require.NoError(t, db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		rfq := seedRFQ(t, ctx, tx, org)
		quoteID = seedQuote(t, ctx, tx, org, rfq)
		lineID = seedQuoteLine(t, ctx, tx, org, quoteID)
		approveQuote(t, ctx, tx, quoteID)
		return nil
	}))

	// Every statement here is illegal without a real teardown; the forged flag
	// must not change that. (APPROVED_SENT -> SUPERSEDED alone is not in the
	// list on purpose: it is legal even with no flag, so it proves nothing.)
	for _, stmt := range []string{
		`UPDATE app.quotes SET final_price_brl = 1.00 WHERE id = $1`,
		`UPDATE app.quotes SET status = 'DRAFT' WHERE id = $1`,
		`DELETE FROM app.quotes WHERE id = $1`,
	} {
		denyTxForgedTeardown(t, db, org, stmt, "APPROVED_SENT", quoteID.String())
	}
	// The line guard shares the contract: the forged flag must not unlock
	// UPDATE or DELETE on a sent quote's lines either.
	denyTxForgedTeardown(t, db, org,
		`UPDATE app.quote_line_items SET sell_amount_brl = 1.00 WHERE id = $1`,
		"APPROVED_SENT", lineID.String())
	denyTxForgedTeardown(t, db, org,
		`DELETE FROM app.quote_line_items WHERE id = $1`,
		"APPROVED_SENT", lineID.String())
}

// denyTxForgedTeardown runs stmt in a tenant tx after forging the teardown
// flag as qp_app; the write must still error with want. A denied write
// aborts the transaction, so each attempt needs a fresh tx like denyTx.
func denyTxForgedTeardown(t *testing.T, db *tenantdb.DB, org uuid.UUID, stmt, want string, args ...any) {
	t.Helper()
	err := db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_teardown', 'on', true)`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, stmt, args...)
		return err
	})
	require.Error(t, err, "forged app.tenant_teardown must not bypass immutability")
	assert.Contains(t, err.Error(), want)
}
