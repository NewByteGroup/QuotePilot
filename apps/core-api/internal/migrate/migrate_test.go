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

	dbName := "qp_v1guard_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	super := openDB(t, superDSN)
	// Runs last: cleanups are LIFO, so every handle opened below closes
	// before the drop; WITH (FORCE) covers any straggler anyway.
	t.Cleanup(func() { dropDatabase(t, superDSN, dbName) })
	execDB(t, ctx, super, fmt.Sprintf(`CREATE DATABASE %q`, dbName))
	defer func() { _ = super.Close() }()

	provisionV1Database(t, ctx, superDSN, dbName)
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
	// Head version: bump when a migration lands (0003 added the auth/session
	// schema).
	assert.Equal(t, int64(3), version)
	assert.True(t, relationExists(t, ctx, migDB, "app.organizations"))
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
