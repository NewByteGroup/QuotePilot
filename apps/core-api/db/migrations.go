// Package db embeds the SQL migrations so cmd/migrate (and the integration
// suite through internal/migrate) works regardless of the caller's working
// directory.
package db

import "embed"

// Migrations holds every *.sql file under db/migrations.
//
//	goose.SetBaseFS(migrationsFS)
//
//go:embed migrations/*.sql
var Migrations embed.FS
