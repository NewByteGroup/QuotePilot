// Package migrate applies the application migrations (goose, schema app) and
// the River job-queue migrations (rivermigrate, schema river) with the
// qp_migrator credential.
//
// Splitting this out of cmd/migrate lets the integration suite reach the same
// code path instead of shelling out to a binary (ADR-002 sections 1-2).
package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/db"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"

	// Registers the pgx stdlib driver for database/sql; goose needs *sql.DB.
	_ "github.com/jackc/pgx/v5/stdlib"
)

// riverSchema is where rivermigrate must create River's tables. qp_migrator's
// search_path starts with `app`, so without the explicit Schema option River's
// objects would land in the wrong schema (init script section 7 comment).
const riverSchema = "river"

// Run applies all pending migrations with the qp_migrator DSN.
//
//	migrate.Run(ctx, os.Getenv("MIGRATOR_DSN"))
func Run(ctx context.Context, dsn string) error {
	if dsn == "" {
		return fmt.Errorf("migrate: MIGRATOR_DSN is empty; expected postgres://qp_migrator:...@host/db")
	}
	if err := runGoose(ctx, dsn); err != nil {
		return err
	}
	if err := runRiver(ctx, dsn); err != nil {
		return err
	}
	return nil
}

// runGoose applies the embedded db/migrations as qp_migrator. The version table
// goose_db_version lands in schema app (first on qp_migrator's search_path);
// it has no organization_id column, so RLS assertions ignore it.
func runGoose(ctx context.Context, dsn string) error {
	sub, err := fs.Sub(db.Migrations, "migrations")
	if err != nil {
		return fmt.Errorf("migrate: sub migrations fs: %w", err)
	}
	goose.SetBaseFS(sub)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("migrate: goose dialect: %w", err)
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("migrate: open sql db: %w", err)
	}
	defer sqlDB.Close()
	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		return fmt.Errorf("migrate: goose up: %w", err)
	}
	return nil
}

// runRiver applies River's own migrations into schema river as qp_migrator.
// A qp_migrator pool (no qp_app startup assertions) comes from tenantdb so
// pgxpool stays confined to one package (ADR-002 section 2).
func runRiver(ctx context.Context, dsn string) error {
	pool, err := tenantdb.OpenPool(ctx, dsn)
	if err != nil {
		return fmt.Errorf("migrate: open migrator pool: %w", err)
	}
	defer pool.Close()
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{Schema: riverSchema})
	if err != nil {
		return fmt.Errorf("migrate: river migrator (schema %q): %w", riverSchema, err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("migrate: river up (schema %q): %w", riverSchema, err)
	}
	return nil
}
