package tenantdb_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// TestSpikeRoleModel is gate criterion 2 (ADR-002 section 6): after migrate,
// qp_app is unprivileged and owns nothing, and both tenant tables carry
// ENABLE + FORCE RLS with a tenant_isolation policy. It runs as qp_app on
// purpose: the catalogs it reads are world-readable, so this is exactly what
// the runtime assertions see.
func TestSpikeRoleModel(t *testing.T) {
	db := testenv.Open(t)
	ctx := context.Background()

	var privileged bool
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = 'qp_app'`).Scan(&privileged))
	assert.False(t, privileged, "qp_app must not be superuser or BYPASSRLS")

	var ownedObjects int
	require.NoError(t, db.Pool().QueryRow(ctx, `
		SELECT count(*) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname IN ('app','river')
		  AND pg_get_userbyid(c.relowner) = 'qp_app'`).Scan(&ownedObjects))
	assert.Equal(t, 0, ownedObjects, "qp_app must own no object in app/river")

	rlsFlags := map[string][2]bool{}
	rows, err := db.Pool().Query(ctx, `
		SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'app'
		  AND c.relname IN ('inbound_messages', 'private_tariffs')`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var name string
		var enabled, forced bool
		require.NoError(t, rows.Scan(&name, &enabled, &forced))
		rlsFlags[name] = [2]bool{enabled, forced}
	}
	require.NoError(t, rows.Err())
	for _, table := range []string{"inbound_messages", "private_tariffs"} {
		flags, ok := rlsFlags[table]
		require.True(t, ok, "table app.%s missing after migration", table)
		assert.True(t, flags[0], "app.%s must ENABLE row level security", table)
		assert.True(t, flags[1], "app.%s must FORCE row level security", table)
	}

	// The v1.3 canonical policy name is tenant_isolation_policy; migration 0002
	// renamed the spike's tenant_isolation on inbound_messages to match.
	var policies int
	require.NoError(t, db.Pool().QueryRow(ctx, `
		SELECT count(*) FROM pg_policy p
		JOIN pg_class c ON c.oid = p.polrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE p.polname = 'tenant_isolation_policy'
		  AND n.nspname = 'app'
		  AND c.relname IN ('inbound_messages', 'private_tariffs')`).Scan(&policies))
	assert.Equal(t, 2, policies, "both tenant tables need a tenant_isolation_policy")
}

// TestSpikeIsolation is gate criterion 4. A max-1 pool guarantees org B reuses
// the exact connection org A held, so any leftover session state would be
// visible to B — the strictest form of the pooled-reuse check.
func TestSpikeIsolation(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	testenv.EnsureMigrated(t)
	ctx := context.Background()

	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.MaxConns = 1
	cfg.AfterConnect = tenantdb.RegisterTypesForTest
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	db := tenantdb.NewForTest(pool)
	defer db.Close()

	orgA := uuid.New()
	orgB := uuid.New()
	vec := make([]float32, 1024)
	vec[0], vec[1] = 0.5, -0.25

	// Camada B tables now reference organizations, which qp_app cannot write
	// (Layer A is REVOKE ALL); the seed goes through the migrator pool.
	migrator := testenv.MigratorPool(t)
	seedOrganization(t, ctx, migrator, orgA, "iso-org-a")
	seedOrganization(t, ctx, migrator, orgB, "iso-org-b")

	// Org A writes an inbound message and a tariff embedding on the pooled
	// connection. private_tariffs needs a batch row for the FK (comment 11:
	// the vector column moved here from the dropped private_tariff_embeddings).
	require.NoError(t, db.InTenantTx(ctx, orgA, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO app.inbound_messages (organization_id, resend_message_id, subject) VALUES ($1, $2, $3)`,
			orgA.String(), "iso-a-1", "org a"); err != nil {
			return err
		}
		var batchA uuid.UUID
		if err := tx.QueryRow(ctx,
			`INSERT INTO app.private_tariff_batches (organization_id, source_type) VALUES ($1, 'CSV_UPLOAD') RETURNING id`,
			orgA.String()).Scan(&batchA); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO app.private_tariffs (organization_id, batch_id, carrier_or_agent, modal, origin_locode, dest_locode,
				equipment_or_unit, charge_category, charge_code, currency, net_amount, valid_from, valid_until,
				search_description, embedding, embedding_model)
			VALUES ($1, $2, 'CARRIER_A', 'FCL', 'BRSSZ', 'CNSHA', '40HC', 'OCEAN_FREIGHT', 'OCEAN', 'USD', 1000.00,
				'2026-01-01', '2026-12-31', 'FCL ocean freight BRSSZ-CNSHA', $3, 'test-model')`,
			orgA.String(), batchA.String(), pgvector.NewVector(vec))
		return err
	}))

	// Org B stores the IDENTICAL vector, reusing the same pooled connection.
	require.NoError(t, db.InTenantTx(ctx, orgB, func(ctx context.Context, tx pgx.Tx) error {
		var batchB uuid.UUID
		if err := tx.QueryRow(ctx,
			`INSERT INTO app.private_tariff_batches (organization_id, source_type) VALUES ($1, 'CSV_UPLOAD') RETURNING id`,
			orgB.String()).Scan(&batchB); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO app.private_tariffs (organization_id, batch_id, carrier_or_agent, modal, origin_locode, dest_locode,
				equipment_or_unit, charge_category, charge_code, currency, net_amount, valid_from, valid_until,
				search_description, embedding, embedding_model)
			VALUES ($1, $2, 'CARRIER_A', 'FCL', 'BRSSZ', 'CNSHA', '40HC', 'OCEAN_FREIGHT', 'OCEAN', 'USD', 1000.00,
				'2026-01-01', '2026-12-31', 'FCL ocean freight BRSSZ-CNSHA', $3, 'test-model')`,
			orgB.String(), batchB.String(), pgvector.NewVector(vec))
		return err
	}))

	// Inside B's tenant tx the applied context is B's; an unscoped SELECT
	// returns only B's rows (0 of A's); a WHERE on A yields 0.
	require.NoError(t, db.InTenantTx(ctx, orgB, func(ctx context.Context, tx pgx.Tx) error {
		var setting string
		require.NoError(t, tx.QueryRow(ctx,
			`SELECT current_setting('app.current_organization_id', true)`).Scan(&setting))
		require.Equal(t, orgB.String(), setting, "set_config must apply the tenant inside fn")

		seen := map[string]int{}
		// Deliberately no WHERE clause: RLS is the only filter under test.
		rows, err := tx.Query(ctx, `SELECT organization_id::text FROM app.private_tariffs`)
		require.NoError(t, err)
		for rows.Next() {
			var org string
			require.NoError(t, rows.Scan(&org))
			seen[org]++
		}
		require.NoError(t, rows.Err())
		rows.Close()

		assert.Len(t, seen, 1, "org B sees only its own organization_id")
		assert.Equal(t, 1, seen[orgB.String()])
		assert.Zero(t, seen[orgA.String()], "org B must not see org A embeddings")

		var aMessages int
		require.NoError(t, tx.QueryRow(ctx,
			`SELECT count(*) FROM app.inbound_messages WHERE organization_id = $1`,
			orgA.String()).Scan(&aMessages))
		assert.Equal(t, 0, aMessages, "org B must not see org A messages")
		return nil
	}))

	// A bare pooled query with no tenant context returns zero rows, not an
	// error: NULLIF(current_setting(..., true), '')::uuid is NULL (rule 4).
	var bare int
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM app.private_tariffs`).Scan(&bare))
	assert.Equal(t, 0, bare, "no tenant context must yield zero rows")
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM app.inbound_messages`).Scan(&bare))
	assert.Equal(t, 0, bare, "no tenant context must yield zero rows")
}

// seedOrganization inserts the Layer A registry row as qp_migrator and
// returns the effective slug: slug and inbound_email_alias are the unique
// columns, so a per-run suffix keeps the suite idempotent against a
// persistent dev database. Callers that exercise <slug>@in.* resolution
// use the return.
func seedOrganization(t *testing.T, ctx context.Context, pool *pgxpool.Pool, org uuid.UUID, base string) string {
	t.Helper()
	slug := base + "-" + org.String()[:8]
	_, err := pool.Exec(ctx, `
		INSERT INTO app.organizations (id, name, slug, inbound_email_alias, outbound_from_name, outbound_reply_to)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		org.String(), "Test "+slug, slug, slug+"@in.test", "Test "+slug, "ops@"+slug+".test")
	require.NoError(t, err)
	return slug
}

// TestSpikeNestedTxRejected guards ADR-002 section 3 rule 5: InTenantTx inside
// fn is a programming error, not silently allowed.
func TestSpikeNestedTxRejected(t *testing.T) {
	db := testenv.Open(t)
	err := db.InTenantTx(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		return db.InTenantTx(ctx, uuid.New(), func(context.Context, pgx.Tx) error { return nil })
	})
	assert.ErrorIs(t, err, tenantdb.ErrNestedTenantTx)
}

// TestSpikeNilOrgRejected guards ADR-002 section 3 rule 1: uuid.Nil is refused
// before the database is touched.
func TestSpikeNilOrgRejected(t *testing.T) {
	db := testenv.Open(t)
	called := false
	err := db.InTenantTx(context.Background(), uuid.Nil, func(context.Context, pgx.Tx) error {
		called = true
		return nil
	})
	assert.ErrorIs(t, err, tenantdb.ErrNilOrganization)
	assert.False(t, called, "fn must not run for a nil organization")
}

// TestSpikeReadTxReadOnly exercises InTenantReadTx: the transaction-local
// statement_timeout applies and writes are refused by the read-only tx.
func TestSpikeReadTxReadOnly(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	err := db.InTenantReadTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		var timeout string
		require.NoError(t, tx.QueryRow(ctx,
			`SELECT current_setting('statement_timeout', true)`).Scan(&timeout))
		assert.Equal(t, tenantdb.ReadTxStatementTimeoutForTest, timeout)

		var setting string
		require.NoError(t, tx.QueryRow(ctx,
			`SELECT current_setting('app.current_organization_id', true)`).Scan(&setting))
		assert.Equal(t, org.String(), setting)

		_, err := tx.Exec(ctx,
			`INSERT INTO app.inbound_messages (organization_id, resend_message_id) VALUES ($1, $2)`,
			org.String(), "read-only-write")
		return err
	})
	assert.Error(t, err, "INSERT inside a read-only tenant tx must fail")
}
