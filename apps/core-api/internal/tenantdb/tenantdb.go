// Package tenantdb is the only tenant path to Postgres (ADR-002 section 2):
// it owns the pgxpool and implements the ADR-002 section 3 InTenantTx
// contract. Every other package sees TenantRunner, so nothing else can open a
// connection.
package tenantdb

import (
	"context"
	"fmt"

	pgxdecimal "github.com/jackc/pgx-shopspring-decimal"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvector "github.com/pgvector/pgvector-go/pgx"
)

// DB wraps the application's connection pool. It is the only type allowed to
// hold a *pgxpool.Pool outside of tests (ADR-002 section 2, depguard rule).
type DB struct {
	pool *pgxpool.Pool
}

// Open connects and refuses to return unless the ADR-002 section 4 startup
// assertions hold (role not privileged, owns nothing, RLS enforced). The DSN
// names the role: qp_app on the tenant request path, qp_admin on the platform
// path — the assertions are role-agnostic and qp_admin passes them too.
//
//	db, err := tenantdb.Open(ctx, os.Getenv("POSTGRES_DSN"))
func Open(ctx context.Context, dsn string) (*DB, error) {
	pool, err := OpenPool(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := assertRoleModel(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &DB{pool: pool}, nil
}

// OpenPool connects without the qp_app startup assertions. It exists for
// cmd/migrate, whose DSN is qp_migrator (assertions target qp_app) — the
// credential split of ADR-003 section 1 makes the migrator fail them by design.
func OpenPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("tenantdb: parse dsn (expected postgres://role:...@host/db): %w", err)
	}
	cfg.AfterConnect = registerTypes
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("tenantdb: connect: %w", err)
	}
	return pool, nil
}

// registerTypes runs on every pooled connection: NUMERIC decodes to
// decimal.Decimal (AGENTS.md: no floats near money) and pgvector types map.
func registerTypes(ctx context.Context, conn *pgx.Conn) error {
	pgxdecimal.Register(conn.TypeMap())
	if err := pgxvector.RegisterTypes(ctx, conn); err != nil {
		return fmt.Errorf("tenantdb: register vector types: %w", err)
	}
	return nil
}

// Close releases the pool.
func (d *DB) Close() { d.pool.Close() }

// Pool exposes the raw pool for the River driver (riverpgxv5.New takes a
// *pgxpool.Pool concretely) and for the no-tenant-context isolation probe.
// Callers pass the value through; only this package names the pgxpool type.
func (d *DB) Pool() *pgxpool.Pool { return d.pool }

// assertRoleModel runs the three ADR-002 section 4 startup assertions.
func assertRoleModel(ctx context.Context, pool *pgxpool.Pool) error {
	if err := assertNotPrivileged(ctx, pool); err != nil {
		return err
	}
	if err := assertOwnsNothing(ctx, pool); err != nil {
		return err
	}
	return assertRLSForced(ctx, pool)
}

// assertNotPrivileged requires the connected role to be neither superuser nor
// BYPASSRLS (ADR-003 section 1): either would silently defeat RLS.
func assertNotPrivileged(ctx context.Context, pool *pgxpool.Pool) error {
	var privileged bool
	err := pool.QueryRow(ctx,
		`SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&privileged)
	if err != nil {
		return fmt.Errorf("tenantdb: role attribute check: %w", err)
	}
	if privileged {
		return fmt.Errorf("tenantdb: connected role must not be SUPERUSER or BYPASSRLS (ADR-003 section 1)")
	}
	return nil
}

// assertOwnsNothing requires the role to own no schema or relation in
// app/river: an owner bypasses FORCE RLS for its own objects.
func assertOwnsNothing(ctx context.Context, pool *pgxpool.Pool) error {
	var owned int
	err := pool.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT 1 FROM pg_namespace
			WHERE nspname IN ('app','river')
			  AND nspowner = (SELECT oid FROM pg_roles WHERE rolname = current_user)
			UNION ALL
			SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname IN ('app','river')
			  AND c.relkind IN ('r','p','v','m','S','f')
			  AND c.relowner = (SELECT oid FROM pg_roles WHERE rolname = current_user)
		) offenders`).Scan(&owned)
	if err != nil {
		return fmt.Errorf("tenantdb: ownership check: %w", err)
	}
	if owned > 0 {
		return fmt.Errorf("tenantdb: connected role owns %d object(s) in app/river; expected 0 (ADR-002 section 4)", owned)
	}
	return nil
}

// platformRLSAllowlist names the Layer A tables that legitimately carry an
// organization_id column without tenant RLS: they are read before a tenant
// context exists and are reached only through SECURITY DEFINER functions
// (spec 2026-11-10-data-model-v1.3 section 3, ADR-005 section 1, comment 9).
// The same allowlist is enforced by roles.sql assertion [M] and by the
// schema lint test — a new entry must land in all three on purpose.
var platformRLSAllowlist = []string{"organization_memberships"}

// assertRLSForced requires every app table carrying an organization_id column
// to have RLS enabled AND forced (AGENTS.md rule 4), minus the explicit
// Layer A allowlist above.
func assertRLSForced(ctx context.Context, pool *pgxpool.Pool) error {
	var joined string
	err := pool.QueryRow(ctx, `
		SELECT coalesce(string_agg(n.nspname || '.' || c.relname, ', '), '')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'app' AND c.relkind IN ('r','p')
		  AND EXISTS (SELECT 1 FROM pg_attribute a
		              WHERE a.attrelid = c.oid AND a.attname = 'organization_id'
		                AND a.attnum > 0 AND NOT a.attisdropped)
		  AND NOT (c.relrowsecurity AND c.relforcerowsecurity)
		  AND c.relname::text <> ALL($1::text[])`, platformRLSAllowlist).Scan(&joined)
	if err != nil {
		return fmt.Errorf("tenantdb: RLS enforcement check: %w", err)
	}
	if joined != "" {
		return fmt.Errorf("tenantdb: ENABLE+FORCE RLS missing on tenant tables: %s (AGENTS.md rule 4)", joined)
	}
	return nil
}
