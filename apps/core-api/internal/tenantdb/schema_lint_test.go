package tenantdb_test

// Schema lint for the v1.3 data model (spec section 7.1, comment 17a, plan
// task 3): mechanical catalog assertions that fail CI when a table or grant
// drifts from the layer model, so a future migration cannot quietly open a
// hole. Behavioural trigger/function enforcement lives in
// schema_enforcement_test.go.
//
// Catalog reads run on MIGRATOR_DSN; the has_*_privilege functions answer for
// any role regardless of the caller (AGENTS.md, ADR-008 section 3).

import (
	"context"
	"sort"
	"testing"

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
// Layer A identity (including the 0003 auth/session tables) plus Layer C
// globals plus goose's bookkeeping table (no organization_id, nothing for RLS
// to filter). A new unprotected table fails this assertion until the list is
// edited on purpose (ADR-008 section 3).
var platformTablesNoRLS = []string{
	"auth_login_tokens",
	"auth_sessions",
	"connectors",
	"goose_db_version",
	"healing_sessions",
	"organization_memberships",
	"organizations",
	"public_snapshots",
	"users",
}

// layerATables are revoked from qp_app entirely; the platform functions are
// the only path in (spec section 3, tenant-resolution spec section 3.2).
var layerATables = []string{
	"organizations", "users", "organization_memberships",
	"auth_login_tokens", "auth_sessions",
}

// layerCTables keep SELECT but no writes for qp_app (spec section 5).
var layerCTables = []string{"connectors", "public_snapshots", "healing_sessions"}

// appendOnlyTables keep SELECT/INSERT but lose UPDATE/DELETE (comments 11, 14;
// carrier_offers is an audit record the same way — RFC-002).
var appendOnlyTables = []string{"private_tariffs", "tenant_audit_logs", "carrier_offers"}

// tenantFunctions are the tenant-facing SECURITY DEFINER surface; qp_app gets
// EXECUTE on exactly these signatures (spec section 3, tenant-resolution spec
// section 5 — the 0003 auth/session family joins the original three).
var tenantFunctions = []string{
	"app.resolve_inbound_org(text)",
	"app.list_memberships_for_email(text)",
	"app.get_current_organization()",
	"app.find_user_by_email(text)",
	"app.create_login_token(uuid, text, timestamptz)",
	"app.consume_login_token(text)",
	"app.create_session(uuid, text, timestamptz)",
	"app.rotate_session(text, text, timestamptz)",
	"app.revoke_session(uuid)",
	"app.revoke_session_family_by_hash(text)",
	"app.get_membership(uuid, uuid)",
	"app.get_user_profile(uuid)",
}

// platformFunctions are the platform path's SECURITY DEFINER surface;
// qp_admin — never qp_app — gets EXECUTE on exactly these signatures (spec
// section 5, ADR-005). Granting them to qp_app left a single in-function
// scope check between the exposed API role and tenant deletion (Devin
// Review); the catalog diff below proves the split mechanically.
var platformFunctions = []string{
	"app.platform_insert_public_snapshot(text, text, text, text, text, jsonb, numeric, jsonb, timestamptz)",
	"app.platform_update_connector_status(text, app.connector_status, numeric)",
	"app.platform_insert_healing_session(text, uuid, text, text, text, text)",
	"app.platform_update_healing_session(uuid, app.healing_status, text, boolean, text)",
	"app.platform_delete_organization(uuid)",
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

	// private_tariff_batches is not append-only by grant: activation is an
	// UPDATE (enforce_batch_activation narrows what that UPDATE may do). The
	// batch row itself is an audit record, so DELETE is revoked outright.
	for _, priv := range []string{"SELECT", "INSERT", "UPDATE"} {
		var allowed bool
		require.NoError(t, migrator.QueryRow(ctx,
			`SELECT has_table_privilege('qp_app', 'app.private_tariff_batches', $1)`, priv).Scan(&allowed))
		assert.True(t, allowed, "qp_app must keep %s on app.private_tariff_batches", priv)
	}
	var batchDelete bool
	require.NoError(t, migrator.QueryRow(ctx,
		`SELECT has_table_privilege('qp_app', 'app.private_tariff_batches', 'DELETE')`).Scan(&batchDelete))
	assert.False(t, batchDelete, "qp_app must not DELETE app.private_tariff_batches")

	// Tenant-facing SECURITY DEFINER functions: EXECUTE granted to qp_app,
	// denied to PUBLIC beyond the explicit grant.
	for _, fn := range tenantFunctions {
		var allowed bool
		require.NoError(t, migrator.QueryRow(ctx,
			`SELECT has_function_privilege('qp_app', $1, 'EXECUTE')`, fn).Scan(&allowed))
		assert.True(t, allowed, "qp_app needs EXECUTE on %s", fn)
		var publicExec bool
		require.NoError(t, migrator.QueryRow(ctx,
			`SELECT has_function_privilege('public', $1, 'EXECUTE')`, fn).Scan(&publicExec))
		assert.False(t, publicExec, "PUBLIC must not EXECUTE %s", fn)
	}

	// The documented return shape is part of the contract too: a
	// DROP+recreate that widens the column set keeps EXECUTE but drifts the
	// callers' expectations, so pin pg_get_function_result for the
	// TABLE-returning tenant functions. Type names render under this
	// session's search_path (qp_migrator's is app,extensions), so
	// membership_role appears unqualified.
	for sig, want := range map[string]string{
		"app.list_memberships_for_email(text)":        "TABLE(organization_id uuid, name text, slug text, role membership_role)",
		"app.rotate_session(text, text, timestamptz)": "TABLE(session_id uuid, user_id uuid, rotated boolean)",
		"app.get_user_profile(uuid)":                  "TABLE(id uuid, email text, full_name text)",
	} {
		var result string
		require.NoError(t, migrator.QueryRow(ctx,
			`SELECT pg_get_function_result($1::regprocedure)`, sig).Scan(&result))
		assert.Equal(t, want, result, "return shape drifted on %s", sig)
	}

	// The platform split (Devin Review): qp_admin — never qp_app — EXECUTEs
	// the platform_* functions, so the tenant credential physically cannot
	// reach teardown or Layer C writes.
	for _, fn := range platformFunctions {
		var adminExec bool
		require.NoError(t, migrator.QueryRow(ctx,
			`SELECT has_function_privilege('qp_admin', $1, 'EXECUTE')`, fn).Scan(&adminExec))
		assert.True(t, adminExec, "qp_admin needs EXECUTE on %s", fn)
		var appExec bool
		require.NoError(t, migrator.QueryRow(ctx,
			`SELECT has_function_privilege('qp_app', $1, 'EXECUTE')`, fn).Scan(&appExec))
		assert.False(t, appExec, "qp_app must not EXECUTE %s", fn)
		var publicExec bool
		require.NoError(t, migrator.QueryRow(ctx,
			`SELECT has_function_privilege('public', $1, 'EXECUTE')`, fn).Scan(&publicExec))
		assert.False(t, publicExec, "PUBLIC must not EXECUTE %s", fn)
	}

	// The EXECUTE surfaces are catalog diffs like platformTablesNoRLS: the set
	// of app functions each role can EXECUTE must equal its allowlist exactly —
	// a migration that grants EXECUTE on a trigger/helper, or adds a platform
	// entry point without listing it, fails here. Signatures go through
	// regprocedure on this same session so both sides share one rendering:
	// the allowlists use aliases (timestamptz, app.connector_status) that the
	// catalog renders canonically (timestamp with time zone, connector_status).
	canonicalize := func(fns []string) []string {
		canon := make([]string, 0, len(fns))
		for _, fn := range fns {
			var c string
			require.NoError(t, migrator.QueryRow(ctx,
				`SELECT $1::regprocedure::text`, fn).Scan(&c))
			canon = append(canon, c)
		}
		sort.Strings(canon)
		return canon
	}
	wantTenant := canonicalize(tenantFunctions)
	wantPlatform := canonicalize(platformFunctions)

	// COLLATE "C" on both catalog ORDER BYs: Go's sort.Strings is bytewise
	// but the database collation folds '_' before alphanumerics, so
	// "revoke_session(uuid)" and "revoke_session_family_by_hash(text)" order
	// differently on the two sides without it.
	executableBy := func(role string) []string {
		rows, err := migrator.Query(ctx, `
			SELECT p.oid::regprocedure::text
			FROM pg_proc p
			JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = 'app'
			  AND has_function_privilege($1, p.oid, 'EXECUTE')
			ORDER BY p.oid::regprocedure::text COLLATE "C"`, role)
		require.NoError(t, err)
		var fns []string
		for rows.Next() {
			var name string
			require.NoError(t, rows.Scan(&name))
			fns = append(fns, name)
		}
		require.NoError(t, rows.Err())
		rows.Close()
		return fns
	}
	assert.Equal(t, wantTenant, executableBy("qp_app"),
		"qp_app EXECUTE surface must equal the tenant-function allowlist")
	assert.Equal(t, wantPlatform, executableBy("qp_admin"),
		"qp_admin EXECUTE surface must equal the platform-function allowlist")

	// SECURITY DEFINER is the dangerous property (the body runs as
	// qp_migrator): its set must equal the union of both allowlists — no new
	// definer may appear unlisted, and no allowlisted function may lose the
	// flag silently.
	var definers []string
	rows, err := migrator.Query(ctx, `
		SELECT p.oid::regprocedure::text
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'app' AND p.prosecdef
		ORDER BY p.oid::regprocedure::text COLLATE "C"`)
	require.NoError(t, err)
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		definers = append(definers, name)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	wantDefiners := append(append([]string{}, wantTenant...), wantPlatform...)
	sort.Strings(wantDefiners)
	assert.Equal(t, wantDefiners, definers,
		"SECURITY DEFINER functions in app must equal the tenant + platform allowlists")
}
