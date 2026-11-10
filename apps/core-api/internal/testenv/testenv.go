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
