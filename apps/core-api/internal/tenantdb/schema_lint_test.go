package tenantdb_test

// Schema lint for the v1.3 data model (spec section 7.1, comment 17a, plan
// task 3): mechanical assertions that fail CI when a table or grant drifts
// from the layer model, so a future migration cannot quietly open a hole.
//
// Catalog reads run on MIGRATOR_DSN; behaviour checks run on the real qp_app
// role through InTenantTx — never a mock (AGENTS.md, ADR-008 section 3).

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// tenantPolicyAllowlist is the sole exception to "organization_id implies
// ENABLE+FORCE RLS + tenant_isolation_policy": organization_memberships is a
// Layer A platform table (spec section 3, comment 9), function-gated because
// it is queried before tenant context exists. The same list lives in
// tenantdb.assertRLSForced and roles.sql assertion [M]; all three must be
// edited together.
var tenantPolicyAllowlist = []string{"organization_memberships"}

// platformTablesNoRLS is the full set of app tables with no tenant policy:
// Layer A identity plus Layer C globals plus goose's bookkeeping table
// (no organization_id, nothing for RLS to filter). A new unprotected table
// fails this assertion until the list is edited on purpose (ADR-008 section 3).
var platformTablesNoRLS = []string{
	"connectors",
	"goose_db_version",
	"healing_sessions",
	"organization_memberships",
	"organizations",
	"public_snapshots",
	"users",
}

// layerATables are revoked from qp_app entirely; the platform functions are
// the only path in (spec section 3).
var layerATables = []string{"organizations", "users", "organization_memberships"}

// layerCTables keep SELECT but no writes for qp_app (spec section 5).
var layerCTables = []string{"connectors", "public_snapshots", "healing_sessions"}

// appendOnlyTables keep SELECT/INSERT but lose UPDATE/DELETE (comments 11, 14).
var appendOnlyTables = []string{"private_tariffs", "tenant_audit_logs"}

// platformFunctions are the spec's SECURITY DEFINER surface; qp_app gets
// EXECUTE on exactly these signatures.
var platformFunctions = []string{
	"app.resolve_inbound_org(text)",
	"app.list_memberships_for_email(text)",
	"app.get_current_organization()",
	"app.platform_insert_public_snapshot(text, text, text, text, text, jsonb, numeric, jsonb, timestamptz)",
	"app.platform_update_connector_status(text, app.connector_status, numeric)",
	"app.platform_insert_healing_session(text, uuid, text, text, text, text)",
	"app.platform_update_healing_session(uuid, app.healing_status, text, boolean, text)",
}

// TestSchemaLintRLSCoverage fails the build when an app table has an
// organization_id column but lacks ENABLE+FORCE RLS or the
// tenant_isolation_policy (comment 17a), and when the set of unprotected
// tables differs from the explicit platform allowlist (ADR-008 section 3).
func TestSchemaLintRLSCoverage(t *testing.T) {
	testenv.RequirePostgres(t)
	migrator := testenv.MigratorPool(t)
	ctx := context.Background()

	var offenders []string
	rows, err := migrator.Query(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'app' AND c.relkind IN ('r','p')
		  AND EXISTS (SELECT 1 FROM pg_attribute a
		              WHERE a.attrelid = c.oid AND a.attname = 'organization_id'
		                AND a.attnum > 0 AND NOT a.attisdropped)
		  AND c.relname <> ALL($1::text[])
		  AND NOT (c.relrowsecurity AND c.relforcerowsecurity
		           AND EXISTS (SELECT 1 FROM pg_policy p
		                       WHERE p.polrelid = c.oid AND p.polname = 'tenant_isolation_policy'))
		ORDER BY c.relname`, tenantPolicyAllowlist)
	require.NoError(t, err)
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		offenders = append(offenders, name)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	assert.Empty(t, offenders,
		"tenant tables missing ENABLE+FORCE RLS or tenant_isolation_policy (allowlist: %v)",
		tenantPolicyAllowlist)

	// Every Camada B table carries the shared NULLIF expression (rule 4,
	// comment 16): no tenant context must yield NULL, never an ''::uuid error.
	// pg_get_expr output varies with casts, so the assertion is on the two
	// ingredients, not the literal expression text. NULL quals must fail, not
	// pass: a USING-only policy leaves polwithcheck NULL and `NULL NOT LIKE`
	// would silently keep the row out of the offender list.
	var badPolicy []string
	rows, err = migrator.Query(ctx, `
		SELECT c.relname
		FROM pg_policy p
		JOIN pg_class c ON c.oid = p.polrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'app' AND c.relkind IN ('r','p')
		  AND p.polname = 'tenant_isolation_policy'
		  AND (p.polqual IS NULL OR p.polwithcheck IS NULL
		       OR pg_get_expr(p.polqual, p.polrelid) NOT LIKE '%NULLIF%current_setting%app.current_organization_id%'
		       OR pg_get_expr(p.polwithcheck, p.polrelid) NOT LIKE '%NULLIF%current_setting%app.current_organization_id%')
		ORDER BY c.relname`)
	require.NoError(t, err)
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		badPolicy = append(badPolicy, name)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	assert.Empty(t, badPolicy,
		"tenant_isolation_policy without the NULLIF(current_setting(...)) expression")

	// The set of tables without RLS equals the platform allowlist exactly —
	// no new unprotected table slips in unnoticed.
	var noRLS []string
	rows, err = migrator.Query(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'app' AND c.relkind IN ('r','p')
		  AND NOT c.relrowsecurity
		ORDER BY c.relname`)
	require.NoError(t, err)
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		noRLS = append(noRLS, name)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	assert.Equal(t, platformTablesNoRLS, noRLS,
		"app tables without RLS must equal the Layer A + Layer C allowlist")
}

// TestSchemaLintGrants checks the layer model's qp_app privileges via
// has_table_privilege, which answers for any role regardless of the caller.
func TestSchemaLintGrants(t *testing.T) {
	testenv.RequirePostgres(t)
	migrator := testenv.MigratorPool(t)
	ctx := context.Background()

	// Layer A: REVOKE ALL — qp_app cannot even SELECT the registry.
	for _, table := range layerATables {
		for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
			var allowed bool
			require.NoError(t, migrator.QueryRow(ctx,
				`SELECT has_table_privilege('qp_app', $1, $2)`,
				"app."+table, priv).Scan(&allowed))
			assert.False(t, allowed,
				"qp_app must not hold %s on Layer A app.%s (access is function-gated)", priv, table)
		}
	}

	// Camada C: SELECT stays, every write is revoked.
	for _, table := range layerCTables {
		var sel bool
		require.NoError(t, migrator.QueryRow(ctx,
			`SELECT has_table_privilege('qp_app', $1, 'SELECT')`, "app."+table).Scan(&sel))
		assert.True(t, sel, "qp_app must keep SELECT on Camada C app.%s", table)
		for _, priv := range []string{"INSERT", "UPDATE", "DELETE"} {
			var allowed bool
			require.NoError(t, migrator.QueryRow(ctx,
				`SELECT has_table_privilege('qp_app', $1, $2)`, "app."+table, priv).Scan(&allowed))
			assert.False(t, allowed,
				"qp_app must not hold %s on Camada C app.%s (platform write path)", priv, table)
		}
	}

	// goose_db_version: pre-existing spike hole closed by the migration —
	// qp_app keeps SELECT but must not write goose's bookkeeping table.
	{
		var sel bool
		require.NoError(t, migrator.QueryRow(ctx,
			`SELECT has_table_privilege('qp_app', 'app.goose_db_version', 'SELECT')`).Scan(&sel))
		assert.True(t, sel, "qp_app keeps SELECT on app.goose_db_version")
		for _, priv := range []string{"INSERT", "UPDATE", "DELETE"} {
			var allowed bool
			require.NoError(t, migrator.QueryRow(ctx,
				`SELECT has_table_privilege('qp_app', 'app.goose_db_version', $1)`, priv).Scan(&allowed))
			assert.False(t, allowed, "qp_app must not hold %s on app.goose_db_version", priv)
		}
	}

	// Append-only: SELECT/INSERT stay, UPDATE/DELETE are revoked.
	for _, table := range appendOnlyTables {
		for _, priv := range []string{"SELECT", "INSERT"} {
			var allowed bool
			require.NoError(t, migrator.QueryRow(ctx,
				`SELECT has_table_privilege('qp_app', $1, $2)`, "app."+table, priv).Scan(&allowed))
			assert.True(t, allowed, "qp_app must keep %s on append-only app.%s", priv, table)
		}
		for _, priv := range []string{"UPDATE", "DELETE"} {
			var allowed bool
			require.NoError(t, migrator.QueryRow(ctx,
				`SELECT has_table_privilege('qp_app', $1, $2)`, "app."+table, priv).Scan(&allowed))
			assert.False(t, allowed,
				"qp_app must not hold %s on append-only app.%s", priv, table)
		}
	}

	// SECURITY DEFINER functions: EXECUTE granted to qp_app, denied to PUBLIC
	// beyond the explicit grant.
	for _, fn := range platformFunctions {
		var allowed bool
		require.NoError(t, migrator.QueryRow(ctx,
			`SELECT has_function_privilege('qp_app', $1, 'EXECUTE')`, fn).Scan(&allowed))
		assert.True(t, allowed, "qp_app needs EXECUTE on %s", fn)
		var publicExec bool
		require.NoError(t, migrator.QueryRow(ctx,
			`SELECT has_function_privilege('public', $1, 'EXECUTE')`, fn).Scan(&publicExec))
		assert.False(t, publicExec, "PUBLIC must not EXECUTE %s", fn)
	}
}

// TestSchemaLintSnapshotImmutability proves the trigger (not just the grants)
// blocks UPDATE/DELETE: qp_migrator owns the table, so grants cannot explain a
// failure — only block_snapshot_mutation can (plan task 3).
func TestSchemaLintSnapshotImmutability(t *testing.T) {
	testenv.RequirePostgres(t)
	migrator := testenv.MigratorPool(t)
	ctx := context.Background()

	slug := "lint-snap-" + uuid.NewString()
	_, err := migrator.Exec(ctx,
		`INSERT INTO app.connectors (slug, display_name, target_url) VALUES ($1, 'Lint Connector', 'https://example.test/feed')`,
		slug)
	require.NoError(t, err)
	var snapshotID uuid.UUID
	require.NoError(t, migrator.QueryRow(ctx, `
		INSERT INTO app.public_snapshots (connector_id, parser_version, raw_markdown_hash, extracted_rates_json, source_url, scraped_at)
		SELECT id, 'p1', 'sha256:deadbeef', '{}'::jsonb, target_url, now()
		FROM app.connectors WHERE slug = $1
		RETURNING id`, slug).Scan(&snapshotID))

	_, err = migrator.Exec(ctx,
		`UPDATE app.public_snapshots SET confidence_score = 0.5 WHERE id = $1`, snapshotID.String())
	require.Error(t, err, "UPDATE on public_snapshots must raise even as qp_migrator")
	assert.Contains(t, err.Error(), "immutable")

	_, err = migrator.Exec(ctx,
		`DELETE FROM app.public_snapshots WHERE id = $1`, snapshotID.String())
	require.Error(t, err, "DELETE on public_snapshots must raise even as qp_migrator")
	assert.Contains(t, err.Error(), "immutable")
}

// TestSchemaLintApprovedQuoteImmutability exercises block_approved_quote_mutation
// as qp_app: on APPROVED_SENT the only legal write is status -> SUPERSEDED
// alone; every other column change and every DELETE still raises (spec
// section 7.3, rule 9); corrections are new versions.
func TestSchemaLintApprovedQuoteImmutability(t *testing.T) {
	db := testenv.Open(t)
	migrator := testenv.MigratorPool(t)
	ctx := context.Background()

	org := uuid.New()
	seedOrganization(t, ctx, migrator, org, "lint-org-quote")

	var quoteID uuid.UUID
	err := db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		var rfq uuid.UUID
		if err := tx.QueryRow(ctx,
			`INSERT INTO app.rfq_requests (organization_id, resend_message_id, customer_email) VALUES ($1, $2, 'cust@example.test') RETURNING id`,
			org.String(), "lint-rfq-"+org.String()).Scan(&rfq); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO app.quotes (organization_id, rfq_id, quote_number, total_cost_brl, target_margin, final_price_brl, overall_confidence)
			VALUES ($1, $2, 'LINT-2026-0001', 1000.00, 0.1800, 1180.00, 1.0000) RETURNING id`,
			org.String(), rfq.String()).Scan(&quoteID); err != nil {
			return err
		}
		// DRAFT -> APPROVED_SENT is the allowed transition (OLD.status = DRAFT).
		_, err := tx.Exec(ctx,
			`UPDATE app.quotes SET status = 'APPROVED_SENT' WHERE id = $1`, quoteID.String())
		return err
	})
	require.NoError(t, err, "DRAFT -> APPROVED_SENT transition must be allowed")

	// Each denied write runs in its own tenant tx: the trigger exception aborts
	// the transaction, so the error must surface through InTenantTx itself.
	// Field edits are forbidden even when status stays APPROVED_SENT.
	err = db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE app.quotes SET final_price_brl = 1.00 WHERE id = $1`, quoteID.String())
		return err
	})
	require.Error(t, err, "field UPDATE on an APPROVED_SENT quote must raise")
	assert.Contains(t, err.Error(), "APPROVED_SENT")

	// A status change to anything but SUPERSEDED is still a forbidden mutation.
	err = db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE app.quotes SET status = 'DRAFT' WHERE id = $1`, quoteID.String())
		return err
	})
	require.Error(t, err, "APPROVED_SENT -> DRAFT must raise")
	assert.Contains(t, err.Error(), "APPROVED_SENT")

	err = db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`DELETE FROM app.quotes WHERE id = $1`, quoteID.String())
		return err
	})
	require.Error(t, err, "DELETE on an APPROVED_SENT quote must raise")
	assert.Contains(t, err.Error(), "APPROVED_SENT")

	// The one permitted mutation: retire the sent version to SUPERSEDED so the
	// correction (a new version) can take its place.
	err = db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE app.quotes SET status = 'SUPERSEDED' WHERE id = $1`, quoteID.String())
		return err
	})
	require.NoError(t, err, "APPROVED_SENT -> SUPERSEDED is the only legal update")
}

// TestSchemaLintAppendOnlyEnforced proves the revokes as qp_app, not just in
// the catalog: writes on private_tariffs and tenant_audit_logs fail with a
// permission error inside a tenant transaction (spec section 7.3).
func TestSchemaLintAppendOnlyEnforced(t *testing.T) {
	db := testenv.Open(t)
	migrator := testenv.MigratorPool(t)
	ctx := context.Background()

	org := uuid.New()
	seedOrganization(t, ctx, migrator, org, "lint-org-append")

	var tariff uuid.UUID
	require.NoError(t, db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		var batch uuid.UUID
		require.NoError(t, tx.QueryRow(ctx,
			`INSERT INTO app.private_tariff_batches (organization_id, source_type) VALUES ($1, 'CSV_UPLOAD') RETURNING id`,
			org.String()).Scan(&batch))
		require.NoError(t, tx.QueryRow(ctx, `
			INSERT INTO app.private_tariffs (organization_id, batch_id, carrier_or_agent, modal,
				origin_locode, dest_locode, equipment_or_unit, charge_category, charge_code, currency, net_amount,
				valid_from, valid_until, search_description)
			VALUES ($1, $2, 'CARRIER_A', 'FCL', 'BRSSZ', 'CNSHA', '40HC', 'OCEAN_FREIGHT', 'OCEAN', 'USD',
				1000.00, '2026-01-01', '2026-12-31', 'FCL ocean freight BRSSZ-CNSHA') RETURNING id`,
			org.String(), batch.String()).Scan(&tariff))
		_, err := tx.Exec(ctx,
			`INSERT INTO app.tenant_audit_logs (organization_id, action) VALUES ($1, 'TARIFF_BATCH_UPLOADED')`,
			org.String())
		require.NoError(t, err)

		return nil
	}))

	// Each denied write runs in its own tenant tx: the permission error aborts
	// the transaction, so the error must surface through InTenantTx itself.
	err := db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE app.private_tariffs SET carrier_or_agent = 'X' WHERE id = $1`, tariff.String())
		return err
	})
	require.Error(t, err, "qp_app UPDATE on append-only private_tariffs must be denied")
	assert.Contains(t, err.Error(), "permission denied")

	err = db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE app.tenant_audit_logs SET action = 'X' WHERE organization_id = $1`, org.String())
		return err
	})
	require.Error(t, err, "qp_app UPDATE on append-only tenant_audit_logs must be denied")
	assert.Contains(t, err.Error(), "permission denied")

	// DELETE is revoked symmetrically on both append-only tables.
	for _, denied := range []struct{ stmt, arg string }{
		{`DELETE FROM app.private_tariffs WHERE id = $1`, tariff.String()},
		{`DELETE FROM app.tenant_audit_logs WHERE organization_id = $1`, org.String()},
	} {
		err = db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, denied.stmt, denied.arg)
			return err
		})
		require.Error(t, err, "qp_app must not run %q", denied.stmt)
		assert.Contains(t, err.Error(), "permission denied")
	}
}

// TestSchemaLintPlatformFunctions exercises each SECURITY DEFINER function as
// qp_app: they run, return only the documented shape, and the underlying
// tables stay unreadable directly (ADR-005 section 2 hygiene rule 5).
func TestSchemaLintPlatformFunctions(t *testing.T) {
	db := testenv.Open(t)
	migrator := testenv.MigratorPool(t)
	ctx := context.Background()

	org := uuid.New()
	orgSlug := seedOrganization(t, ctx, migrator, org, "lint-org-fn")
	slug := "lint-ptax-" + uuid.NewString()
	_, err := migrator.Exec(ctx,
		`INSERT INTO app.connectors (slug, display_name, target_url) VALUES ($1, 'Lint PTAX', 'https://olinda.bcb.gov.br/ptax')`,
		slug)
	require.NoError(t, err)
	userEmail := "lint-user-" + uuid.NewString() + "@test.dev"
	var userID uuid.UUID
	require.NoError(t, migrator.QueryRow(ctx,
		`INSERT INTO app.users (email, full_name) VALUES ($1, 'Lint User') RETURNING id`,
		userEmail).Scan(&userID))
	_, err = migrator.Exec(ctx,
		`INSERT INTO app.organization_memberships (organization_id, user_id, role) VALUES ($1, $2, 'OWNER')`,
		org.String(), userID.String())
	require.NoError(t, err)

	// Decoy membership in a different org: list_memberships_for_email must
	// filter on the argument. (Regression guard for the LANGUAGE sql
	// column-shadows-parameter trap — an unfiltered body returns this row too.)
	otherOrg := uuid.New()
	seedOrganization(t, ctx, migrator, otherOrg, "lint-org-other")
	var otherUserID uuid.UUID
	require.NoError(t, migrator.QueryRow(ctx,
		`INSERT INTO app.users (email, full_name) VALUES ($1, 'Other User') RETURNING id`,
		"lint-other-"+uuid.NewString()+"@test.dev").Scan(&otherUserID))
	_, err = migrator.Exec(ctx,
		`INSERT INTO app.organization_memberships (organization_id, user_id, role) VALUES ($1, $2, 'PRICING_ANALYST')`,
		otherOrg.String(), otherUserID.String())
	require.NoError(t, err)

	// Alias match, including "Name <local+tag@domain>" normalization.
	var resolved *uuid.UUID
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.resolve_inbound_org($1)`,
		"Lint Org <"+orgSlug+"+spam@in.test>").Scan(&resolved))
	require.NotNil(t, resolved)
	assert.Equal(t, org, *resolved, "alias resolution must normalize case, display name and plus-tag")
	// Slug fallback: no alias registered for this local part, but slug@in.* resolves.
	resolved = nil
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.resolve_inbound_org($1)`,
		orgSlug+"@in.anything.test").Scan(&resolved))
	require.NotNil(t, resolved)
	assert.Equal(t, org, *resolved, "<slug>@in.* fallback must resolve")
	// Unknown recipient resolves to NULL, never an error.
	resolved = nil
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.resolve_inbound_org($1)`, "nobody@elsewhere.test").Scan(&resolved))
	assert.Nil(t, resolved, "unknown recipient must resolve to NULL")
	// Direct table read is denied even though the function works.
	var direct int
	directErr := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM app.organizations`).Scan(&direct)
	require.Error(t, directErr, "qp_app must not SELECT app.organizations directly")
	assert.Contains(t, directErr.Error(), "permission denied")

	// Memberships surface only (organization_id, role); lookup is
	// case-insensitive, so the query uses a differently-cased spelling. The
	// decoy row must not come back for our email.
	var memberOrg uuid.UUID
	var memberRole string
	var memberRows int
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM app.list_memberships_for_email($1)`,
		strings.ToUpper(userEmail)).Scan(&memberRows))
	assert.Equal(t, 1, memberRows, "the function must return only this email's memberships")
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT organization_id, role FROM app.list_memberships_for_email($1)`,
		strings.ToUpper(userEmail)).Scan(&memberOrg, &memberRole))
	assert.Equal(t, org, memberOrg)
	assert.Equal(t, "OWNER", memberRole)

	// get_current_organization inside a tenant tx; NULL outside one.
	require.NoError(t, db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		var got uuid.UUID
		require.NoError(t, tx.QueryRow(ctx,
			`SELECT (app.get_current_organization()).id`).Scan(&got))
		require.Equal(t, org, got)
		return nil
	}))
	var nilOrg *uuid.UUID
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT (app.get_current_organization()).id`).Scan(&nilOrg))
	assert.Nil(t, nilOrg, "no tenant context must yield NULL, not an ''::uuid error")

	// Platform write path: qp_app writes Layer C only through the functions.
	var snapshotID uuid.UUID
	require.NoError(t, db.Pool().QueryRow(ctx, `
		SELECT app.platform_insert_public_snapshot(
			$1, 'p1', 'https://example.test/feed', 'sha256:cafe',
			'platform/snapshots/lint.json', '{}'::jsonb, 0.98, '[]'::jsonb, now())`,
		slug).Scan(&snapshotID))
	require.NotEqual(t, uuid.Nil, snapshotID)
	_, err = db.Pool().Exec(ctx,
		`SELECT app.platform_update_connector_status($1, 'DEGRADED', 0.42)`, slug)
	require.NoError(t, err)
	var sessionID uuid.UUID
	require.NoError(t, db.Pool().QueryRow(ctx, `
		SELECT app.platform_insert_healing_session(
			$1, $2, 'sha256:beef', 'platform/healing/lint.bin',
			'devin-lint-0001', 'https://app.devin.ai/sessions/lint')`,
		slug, snapshotID.String()).Scan(&sessionID))
	require.NotEqual(t, uuid.Nil, sessionID)

	// Sessions leave OPEN only through platform_update_healing_session:
	// progress first, then a terminal status stamps resolved_at.
	_, err = db.Pool().Exec(ctx,
		`SELECT app.platform_update_healing_session($1, 'PR_READY', 'https://github.com/x/pull/1', true, NULL)`,
		sessionID.String())
	require.NoError(t, err)
	var status string
	var resolvedAt *string
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT status::text, resolved_at::text FROM app.healing_sessions WHERE id = $1`,
		sessionID.String()).Scan(&status, &resolvedAt))
	assert.Equal(t, "PR_READY", status)
	assert.Nil(t, resolvedAt, "non-terminal status must not stamp resolved_at")
	_, err = db.Pool().Exec(ctx,
		`SELECT app.platform_update_healing_session($1, 'MERGED', NULL, NULL, NULL)`, sessionID.String())
	require.NoError(t, err)
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT status::text, resolved_at::text FROM app.healing_sessions WHERE id = $1`,
		sessionID.String()).Scan(&status, &resolvedAt))
	assert.Equal(t, "MERGED", status)
	assert.NotNil(t, resolvedAt, "MERGED must stamp resolved_at")

	var lastScrape bool
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT last_scraped_at IS NOT NULL FROM app.connectors WHERE slug = $1`, slug).Scan(&lastScrape))
	assert.True(t, lastScrape, "insert snapshot must refresh connectors.last_scraped_at")
}
