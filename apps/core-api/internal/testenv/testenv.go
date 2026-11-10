// Package testenv provides the shared helpers for the real-Postgres spike gate
// tests: the two DSN env vars, one-shot migration, and an opened tenantdb.DB.
//
// It exists so that DB tests never mock Postgres (AGENTS.md: the isolation
// suite must prove RLS against the real role model) and never re-implement
// migration setup.
package testenv

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/migrate"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
)

var (
	migrateOnce sync.Once
	migrateErr  error
)

// RequirePostgres skips t under -short or without POSTGRES_DSN and returns the
// qp_app DSN. Integration behaviour is skipped only, never faked.
//
//	dsn := testenv.RequirePostgres(t)
func RequirePostgres(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping real-Postgres test under -short")
	}
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("skipping: POSTGRES_DSN is not set")
	}
	return dsn
}

// EnsureMigrated applies all goose and River migrations once per test binary
// with MIGRATOR_DSN (qp_migrator). Tests may run in any order, so migration
// state is a prerequisite, not a test of its own.
func EnsureMigrated(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping real-Postgres test under -short")
	}
	dsn := os.Getenv("MIGRATOR_DSN")
	require.NotEmpty(t, dsn, "MIGRATOR_DSN is not set")
	migrateOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		migrateErr = migrate.Run(ctx, dsn)
	})
	require.NoError(t, migrateErr)
}

// Open returns a tenantdb.DB on the qp_app DSN with migrations applied; the
// pool closes at test cleanup.
//
//	db := testenv.Open(t)
func Open(t *testing.T) *tenantdb.DB {
	t.Helper()
	dsn := RequirePostgres(t)
	EnsureMigrated(t)
	db, err := tenantdb.Open(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(db.Close)
	return db
}

// MigratorPool opens a pool on MIGRATOR_DSN with migrations applied and the
// project types registered; it closes at test cleanup. Use it for catalog
// reads and for seeding Layer A rows (organizations, users, memberships) that
// qp_app must not write directly — REVOKE ALL on Layer A is the spec's point,
// so tests that need those rows go through the owner.
//
//	pool := testenv.MigratorPool(t)
func MigratorPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	EnsureMigrated(t)
	dsn := os.Getenv("MIGRATOR_DSN")
	require.NotEmpty(t, dsn, "MIGRATOR_DSN is not set")
	pool, err := tenantdb.OpenPool(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// SeedOrganization inserts the Layer A registry row as qp_migrator and returns
// the effective slug: slug and inbound_email_alias are the unique columns, so
// a per-run suffix keeps the suite idempotent against a persistent dev
// database. Every tenant-row insert now satisfies the organizations FK, so
// tests must seed the org before writing Camada B rows under it. Callers that
// exercise <slug>@in.quotepilot.app resolution use the return.
//
//	slug := testenv.SeedOrganization(t, org, "my-test")
func SeedOrganization(t *testing.T, org uuid.UUID, base string) string {
	t.Helper()
	pool := MigratorPool(t)
	slug := base + "-" + org.String()[:8]
	_, err := pool.Exec(context.Background(), `
		INSERT INTO app.organizations (id, name, slug, inbound_email_alias, outbound_from_name, outbound_reply_to)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		org.String(), "Test "+slug, slug, slug+"@in.test", "Test "+slug, "ops@"+slug+".test")
	require.NoError(t, err)
	return slug
}
