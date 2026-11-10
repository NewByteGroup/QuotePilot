package tenantdb

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Test-only exports (the export_test.go convention): visible only to this
// package's test binary. They let the isolation suite pin MaxConns=1 and reuse
// the same type registration without widening the production API.

// NewForTest wraps an externally configured pool in a DB.
func NewForTest(pool *pgxpool.Pool) *DB { return &DB{pool: pool} }

// RegisterTypesForTest is registerTypes for pools built inside tests.
func RegisterTypesForTest(ctx context.Context, conn *pgx.Conn) error {
	return registerTypes(ctx, conn)
}

// ReadTxStatementTimeoutForTest exposes the read-tx timeout literal.
const ReadTxStatementTimeoutForTest = readTxStatementTimeout
