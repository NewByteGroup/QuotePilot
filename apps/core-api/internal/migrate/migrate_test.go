package migrate_test

// Migration-level test for the 0001 -> 0002 upgrade guard (Devin Review):
// a v1 database that already holds inbound_messages receipts whose
// organization_id matches no organization must refuse the 0002 upgrade with
// the deliberate guard error — not a raw FK violation — and must not drop
// the rows. Reconciling them is the operator's call; once they are gone the
// same upgrade applies cleanly.
//
// The shared test database cannot be taken down to version 1 without racing
// the other integration test binaries that run against it in parallel, so
// this test builds a dedicated throwaway database instead. qp_migrator is
// NOCREATEDB (init section 2), so provisioning goes through PG_SUPER_DSN —
// the same bootstrap superuser the container init script ran as.

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/db"

	// Registers the pgx stdlib driver for database/sql; goose needs *sql.DB.
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestUpgradeV1OrphanReceiptsRefused exercises both sides of the guard:
// upgrade refused while an orphan receipt exists (rolled back to version 1,
// organizations table never materialized), then upgrade succeeding after the
// operator reconciles the receipt away.
func TestUpgradeV1OrphanReceiptsRefused(t *testing.T) {
	superDSN, migratorDSN, appDSN := requireUpgradeEnv(t)
	ctx := context.Background()

	dbName := scratchDB(t, ctx, superDSN, "qp_v1guard_")
	setupGoose(t)

	migDB := openDB(t, dsnFor(t, migratorDSN, dbName))
	defer func() { _ = migDB.Close() }()
	require.NoError(t, goose.UpToContext(ctx, migDB, ".", 1), "throwaway DB must reach version 1")

	// A v1 receipt for an org that will never exist: under v1,
	// organization_id is a bare uuid with no registry to check it against.
	// Inserted as qp_app through the same GUC-pinned path InTenantTx uses.
	orphanOrg := uuid.New()
	msgID := "orphan-" + uuid.NewString()
	appDB := openDB(t, dsnFor(t, appDSN, dbName))
	defer func() { _ = appDB.Close() }()
	writeOrphanReceipt(t, ctx, appDB, orphanOrg,
		`INSERT INTO app.inbound_messages (organization_id, resend_message_id) VALUES ($1, $2)`,
		msgID)

	// The upgrade must refuse with the deliberate error — and roll the whole
	// migration back: still version 1, no organizations table left behind.
	err := goose.UpContext(ctx, migDB, ".")
	require.Error(t, err, "populated-v1 upgrade must refuse orphan receipts")
	assert.Contains(t, err.Error(), "inbound_messages holds receipts for organizations that do not exist")
	version, err := goose.GetDBVersionContext(ctx, migDB)
	require.NoError(t, err)
	assert.Equal(t, int64(1), version, "a refused migration must leave the DB at version 1")
	assert.False(t, relationExists(t, ctx, migDB, "app.organizations"),
		"a rolled-back 0002 must leave no app.organizations behind")

	// Reconcile as an operator would — here by quarantining (deleting) the
	// receipt — and the same upgrade applies cleanly.
	writeOrphanReceipt(t, ctx, appDB, orphanOrg,
		`DELETE FROM app.inbound_messages WHERE organization_id = $1 AND resend_message_id = $2`,
		msgID)
	require.NoError(t, goose.UpContext(ctx, migDB, "."), "a reconciled v1 database must upgrade")
	version, err = goose.GetDBVersionContext(ctx, migDB)
	require.NoError(t, err)
	// Head version: bump when a migration lands (0014 adds
	// app.sweep_auth_mail_outbox, the outbox dead-row sweeper).
	assert.Equal(t, int64(14), version)
	assert.True(t, relationExists(t, ctx, migDB, "app.organizations"))
}

// TestBrandingJsonObjectCheck proves 0013 both ways: a non-object
// branding_json (array here; the read path decodes an object) is refused
// at write time under 13, allowed again after Down to 12, refused on
// re-Up. The column is NOT NULL so a scalar probe covers the shape.
func TestBrandingJsonObjectCheck(t *testing.T) {
	superDSN, migratorDSN, _ := requireUpgradeEnv(t)
	ctx := context.Background()

	dbName := scratchDB(t, ctx, superDSN, "qp_branding_")
	setupGoose(t)

	migDB := openDB(t, dsnFor(t, migratorDSN, dbName))
	defer func() { _ = migDB.Close() }()
	require.NoError(t, goose.UpToContext(ctx, migDB, ".", 13))
	orgID := seedOrganization(t, ctx, migDB)

	write := `UPDATE app.organizations SET branding_json = '[]'::jsonb WHERE id = $1`
	assertCheckViolation(t, ctx, migDB, write, orgID)

	require.NoError(t, goose.DownToContext(ctx, migDB, ".", 12))
	_, err := migDB.ExecContext(ctx, write, orgID)
	require.NoError(t, err, "the Down must restore the unconstrained column")

	// The Up validates existing rows: a database carrying a non-object
	// value refuses the upgrade loudly (SQLSTATE 23514) until an operator
	// reconciles it — same posture as the 0002 orphan-receipt guard.
	err = goose.UpToContext(ctx, migDB, ".", 13)
	require.Error(t, err, "re-Up over a violating row must refuse")
	assert.Contains(t, err.Error(), "organizations_branding_json_object")
	execDB(t, ctx, migDB,
		`UPDATE app.organizations SET branding_json = '{}'::jsonb WHERE id = '`+orgID+`'`)

	require.NoError(t, goose.UpToContext(ctx, migDB, ".", 13))
	assertCheckViolation(t, ctx, migDB, write, orgID)
}

// assertCheckViolation requires stmt to fail under the 0013 CHECK —
// pgx surfaces the constraint name in the error, so the assertion names
// it rather than matching on SQLSTATE alone.
func assertCheckViolation(t *testing.T, ctx context.Context, db *sql.DB, stmt string, orgID string) {
	t.Helper()
	_, err := db.ExecContext(ctx, stmt, orgID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "organizations_branding_json_object")
}

// seedOrganization inserts the minimal organization row the check test
// updates — qp_migrator owns the table so no tenant GUC is needed.
func seedOrganization(t *testing.T, ctx context.Context, db *sql.DB) string {
	t.Helper()
	var id string
	require.NoError(t, db.QueryRowContext(ctx, `
		INSERT INTO app.organizations
			(name, slug, inbound_email_alias, outbound_from_name, outbound_reply_to)
		VALUES ('Check Test', 'check-test', 'check-test', 'T', 't@example.com')
		RETURNING id`).Scan(&id))
	return id
}

// TestSearchPathPinRoundTrip proves 0012 both ways on a throwaway database:
// version 11 leaves the 17 survivor definers on `= app`, 12 pins the whole
// surface, Down restores `= app` without touching the qp_app EXECUTE grant
// (ALTER FUNCTION leaves grants alone — a drop/recreate sequence would
// not), and re-Up pins again. Assertions are whole-catalog counts, not a
// single probe function, so a missed ALTER fails the test.
func TestSearchPathPinRoundTrip(t *testing.T) {
	superDSN, migratorDSN, _ := requireUpgradeEnv(t)
	ctx := context.Background()

	dbName := scratchDB(t, ctx, superDSN, "qp_roundtrip_")
	setupGoose(t)

	migDB := openDB(t, dsnFor(t, migratorDSN, dbName))
	defer func() { _ = migDB.Close() }()

	// The 0006/0007 family ships pinned (9 functions: its 8 plus
	// get_user_auth_by_email); 0012 owns the 17 `= app` survivors. The
	// goose calls pin to 11/12 exactly — a later migration must not move
	// this test's assertions.
	pre := map[string]int{"search_path=app": 17, "search_path=pg_catalog, pg_temp": 9}
	post := map[string]int{"search_path=pg_catalog, pg_temp": 26}

	require.NoError(t, goose.UpToContext(ctx, migDB, ".", 11))
	assert.Equal(t, pre, definerPathCounts(t, ctx, migDB))
	assertDefinerPaths(t, ctx, migDB, revertedSignatures, "search_path=app")
	// Recreate the legacy drift 0012 exists to repair: a database that
	// applied 0006's pre-amendment `= app` text (d1b5fa6 landed after
	// those databases migrated, so goose never re-ran it). Without this
	// the convergence ALTER is a no-op the test cannot distinguish from
	// an omitted statement.
	execDB(t, ctx, migDB,
		`ALTER FUNCTION app.get_user_auth_by_email(text) SET search_path = app`)

	require.NoError(t, goose.UpToContext(ctx, migDB, ".", 12))
	assert.Equal(t, post, definerPathCounts(t, ctx, migDB))
	assertDefinerPaths(t, ctx, migDB, hardenedSignatures, "search_path=pg_catalog, pg_temp")

	require.NoError(t, goose.DownToContext(ctx, migDB, ".", 11))
	assert.Equal(t, pre, definerPathCounts(t, ctx, migDB))
	// Per-signature too: counts alone cannot see a rollback that swaps
	// paths between two functions. The convergence pin stays pinned —
	// its canonical pre-0012 state is the hardened path.
	assertDefinerPaths(t, ctx, migDB, revertedSignatures, "search_path=app")
	assertDefinerPaths(t, ctx, migDB, []string{"app.get_user_auth_by_email(text)"}, "search_path=pg_catalog, pg_temp")
	assert.True(t, qpAppCanExecute(t, ctx, migDB, "app.resolve_inbound_org(text)"),
		"the Down must keep qp_app's EXECUTE grant")

	require.NoError(t, goose.UpToContext(ctx, migDB, ".", 12))
	assert.Equal(t, post, definerPathCounts(t, ctx, migDB))
}

// hardenedSignatures is 0012's whole ALTER list — 17 reverts plus the
// get_user_auth_by_email convergence pin.
var hardenedSignatures = []string{
	"app.resolve_inbound_org(text)",
	"app.get_current_organization()",
	"app.platform_insert_public_snapshot(text, text, text, text, text, jsonb, numeric, jsonb, timestamp with time zone)",
	"app.platform_update_connector_status(text, app.connector_status, numeric)",
	"app.platform_insert_healing_session(text, uuid, text, text, text, text)",
	"app.platform_update_healing_session(uuid, app.healing_status, text, boolean, text)",
	"app.platform_delete_organization(uuid)",
	"app.find_user_by_email(text)",
	"app.create_login_token(uuid, text, timestamp with time zone)",
	"app.create_session(uuid, text, timestamp with time zone)",
	"app.revoke_session(uuid)",
	"app.revoke_session_family_by_hash(text)",
	"app.get_membership(uuid, uuid)",
	"app.get_user_profile(uuid)",
	"app.create_mail_outbox(uuid, text)",
	"app.delete_mail_outbox(uuid)",
	"app.list_memberships_for_email(text)",
	"app.get_user_auth_by_email(text)",
}

// revertedSignatures is hardenedSignatures minus the convergence pin —
// the Down restores `= app` on exactly these.
var revertedSignatures = hardenedSignatures[:17]

// assertDefinerPaths checks each signature's proconfig carries want —
// the per-function complement to the catalog counts.
func assertDefinerPaths(t *testing.T, ctx context.Context, db *sql.DB, sigs []string, want string) {
	t.Helper()
	for _, sig := range sigs {
		assert.Equal(t, want, definerSearchPath(t, ctx, db, sig), sig)
	}
}

// definerSearchPath reports the search_path entry of the named function's
// proconfig — the only state 0012 changes.
func definerSearchPath(t *testing.T, ctx context.Context, db *sql.DB, sig string) string {
	t.Helper()
	var path string
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT c
		FROM pg_proc p, unnest(p.proconfig) AS c
		WHERE p.oid = $1::regprocedure
		  AND c LIKE 'search_path=%'`, sig).Scan(&path))
	return path
}

// definerPathCounts tallies search_path pins across the app-schema
// SECURITY DEFINER surface — the whole-catalog check that catches a
// missing ALTER anywhere, not just at one probe signature.
func definerPathCounts(t *testing.T, ctx context.Context, db *sql.DB) map[string]int {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		SELECT c, count(*)
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace,
		unnest(p.proconfig) AS c
		WHERE n.nspname = 'app' AND p.prosecdef AND c LIKE 'search_path=%'
		GROUP BY c`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	counts := map[string]int{}
	for rows.Next() {
		var path string
		var n int
		require.NoError(t, rows.Scan(&path, &n))
		counts[path] = n
	}
	require.NoError(t, rows.Err())
	return counts
}

// scratchDB provisions a throwaway database for goose — created, seeded
// with the init prerequisites, and registered for drop on cleanup.
func scratchDB(t *testing.T, ctx context.Context, superDSN, prefix string) string {
	t.Helper()
	dbName := prefix + strings.ReplaceAll(uuid.NewString(), "-", "")
	super := openDB(t, superDSN)
	defer func() { _ = super.Close() }()
	// Runs last: cleanups are LIFO, so every handle opened below closes
	// before the drop; WITH (FORCE) covers any straggler anyway.
	t.Cleanup(func() { dropDatabase(t, superDSN, dbName) })
	execDB(t, ctx, super, fmt.Sprintf(`CREATE DATABASE %q`, dbName))
	provisionV1Database(t, ctx, superDSN, dbName)
	return dbName
}

// qpAppCanExecute checks the grant ALTER FUNCTION must not disturb.
func qpAppCanExecute(t *testing.T, ctx context.Context, db *sql.DB, sig string) bool {
	t.Helper()
	var ok bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT has_function_privilege('qp_app', $1::regprocedure, 'EXECUTE')`, sig).Scan(&ok))
	return ok
}

// requireUpgradeEnv skips under -short or without the three DSNs this test
// needs, mirroring testenv.RequirePostgres. PG_SUPER_DSN is used only here:
// qp_migrator cannot CREATE DATABASE by design.
func requireUpgradeEnv(t *testing.T) (super, migrator, app string) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping real-Postgres test under -short")
	}
	super, migrator, app = os.Getenv("PG_SUPER_DSN"), os.Getenv("MIGRATOR_DSN"), os.Getenv("POSTGRES_DSN")
	if super == "" || migrator == "" || app == "" {
		t.Skip("skipping: PG_SUPER_DSN, MIGRATOR_DSN and POSTGRES_DSN are all required")
	}
	return super, migrator, app
}

// provisionV1Database replays the pieces of infra/postgres/init that the
// migrations depend on, inside the throwaway DB: the extensions schema and
// the app schema owned by qp_migrator, plus the default privileges that give
// qp_app DML on migrator-owned tables. Roles and the per-role search_path
// are cluster-wide, so they already exist; the river schema is skipped
// because this test runs goose only.
func provisionV1Database(t *testing.T, ctx context.Context, superDSN, dbName string) {
	t.Helper()
	super := openDB(t, superDSN)
	execDB(t, ctx, super, fmt.Sprintf(`GRANT CONNECT ON DATABASE %q TO qp_migrator, qp_app`, dbName))
	_ = super.Close()

	newDB := openDB(t, dsnFor(t, superDSN, dbName))
	defer func() { _ = newDB.Close() }()
	for _, stmt := range []string{
		`CREATE SCHEMA extensions`,
		`CREATE EXTENSION IF NOT EXISTS vector SCHEMA extensions`,
		`GRANT USAGE ON SCHEMA extensions TO qp_migrator, qp_app`,
		`CREATE SCHEMA app AUTHORIZATION qp_migrator`,
		`REVOKE ALL ON SCHEMA app FROM PUBLIC`,
		`GRANT USAGE ON SCHEMA app TO qp_app`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE qp_migrator IN SCHEMA app
			GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO qp_app`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE qp_migrator IN SCHEMA app
			GRANT USAGE, SELECT ON SEQUENCES TO qp_app`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE qp_migrator REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC`,
	} {
		execDB(t, ctx, newDB, stmt)
	}
}

// setupGoose points goose at the embedded migrations, mirroring runGoose.
func setupGoose(t *testing.T) {
	t.Helper()
	sub, err := fs.Sub(db.Migrations, "migrations")
	require.NoError(t, err)
	goose.SetBaseFS(sub)
	require.NoError(t, goose.SetDialect("postgres"))
}

// writeOrphanReceipt runs stmt inside one qp_app transaction with the tenant
// GUC pinned to the (unverifiable) org — under v1 RLS that context is what
// makes the row visible to its own writer. stmt takes ($1, $2) = (org, key).
func writeOrphanReceipt(t *testing.T, ctx context.Context, appDB *sql.DB, org uuid.UUID, stmt string, key any) {
	t.Helper()
	tx, err := appDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `SELECT set_config('app.current_organization_id', $1, true)`, org.String())
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, stmt, org.String(), key)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}

// relationExists reports whether regclass name resolves — false after a
// rolled-back migration created the object part-way.
func relationExists(t *testing.T, ctx context.Context, db *sql.DB, name string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists))
	return exists
}

// dsnFor returns a copy of a URL-shaped DSN with the database path swapped —
// every role reaches the throwaway DB through its own credential.
func dsnFor(t *testing.T, dsn, database string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	require.NoError(t, err, "DSN must be URL-shaped (see .env.example)")
	u.Path = "/" + database
	return u.String()
}

func openDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	return db
}

func execDB(t *testing.T, ctx context.Context, db *sql.DB, stmt string) {
	t.Helper()
	_, err := db.ExecContext(ctx, stmt)
	require.NoError(t, err)
}

// dropDatabase removes the throwaway DB from a fresh superuser connection on
// the maintenance database (the test's own handles are closed by then).
func dropDatabase(t *testing.T, superDSN, dbName string) {
	t.Helper()
	super, err := sql.Open("pgx", superDSN)
	if err != nil {
		t.Logf("dropDatabase: open super dsn: %v", err)
		return
	}
	defer func() { _ = super.Close() }()
	if _, err := super.ExecContext(context.Background(),
		fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, dbName)); err != nil {
		t.Logf("dropDatabase %s: %v", dbName, err)
	}
}
