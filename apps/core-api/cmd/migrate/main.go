// Command migrate applies goose and River migrations with the qp_migrator
// credential only. The API server never holds this DSN (ADR-002 section 1).
//
// Usage:
//
//	MIGRATOR_DSN=postgres://qp_migrator:...@host/db go run ./cmd/migrate
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/migrate"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
	fmt.Println("migrate: all migrations applied")
}

func run() error {
	return migrate.Run(context.Background(), os.Getenv("MIGRATOR_DSN"))
}
