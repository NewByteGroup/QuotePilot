package tenantdb

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TenantRunner is the only interface through which tenant SQL runs
// (ADR-002 section 3). Implemented by *DB; every other package depends on this.
//
//	runner.InTenantTx(ctx, orgID, func(ctx context.Context, tx pgx.Tx) error {
//		return queries.WithTx(tx).InsertInboundMessage(ctx, params)
//	})
type TenantRunner interface {
	InTenantTx(ctx context.Context, org uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) error
	InTenantReadTx(ctx context.Context, org uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) error
}

var _ TenantRunner = (*DB)(nil)

var (
	// ErrNilOrganization is returned before touching the DB for uuid.Nil
	// (ADR-002 section 3 rule 1).
	ErrNilOrganization = errors.New("tenantdb: organization id must be a non-nil uuid")
	// ErrNestedTenantTx is returned when fn calls back into a tenant tx on the
	// same context (ADR-002 section 3 rule 5: nesting is a programming error).
	ErrNestedTenantTx = errors.New("tenantdb: nested tenant transaction is forbidden")
)

// tenantCtxKey marks a context already inside a tenant transaction. Unexported
// per golang-context rule 8 (context keys must be private types).
type tenantCtxKey struct{}

// readTxStatementTimeout bounds long-running reads inside InTenantReadTx; set
// transaction-locally so it never leaks to other borrowers of the connection.
const readTxStatementTimeout = "10s"

// InTenantTx runs fn in a transaction with RLS tenant context applied.
func (d *DB) InTenantTx(ctx context.Context, org uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return d.tenantTx(ctx, org, pgx.TxOptions{}, "", fn)
}

// InTenantReadTx is InTenantTx in a read-only transaction plus a
// transaction-local statement_timeout (ADR-002 section 3 rule 4).
func (d *DB) InTenantReadTx(ctx context.Context, org uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return d.tenantTx(ctx, org, pgx.TxOptions{AccessMode: pgx.ReadOnly}, readTxStatementTimeout, fn)
}

// tenantTx implements the normative InTenantTx sequence of ADR-002 section 3:
// nil check, nesting check, begin, set_config as first statement, fn, commit.
func (d *DB) tenantTx(
	ctx context.Context,
	org uuid.UUID,
	opts pgx.TxOptions,
	statementTimeout string,
	fn func(ctx context.Context, tx pgx.Tx) error,
) error {
	if org == uuid.Nil {
		return ErrNilOrganization
	}
	if ctx.Value(tenantCtxKey{}) != nil {
		return ErrNestedTenantTx
	}
	ctx = context.WithValue(ctx, tenantCtxKey{}, org)
	tx, err := d.pool.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("tenantdb: begin: %w", err)
	}
	// Unconditional rollback: covers panic AND runtime.Goexit (recover()==nil on
	// Goexit, so a recover-only defer would leak the tx on a pooled connection).
	// After a successful Commit this is ErrTxClosed and ignored. The rollback
	// uses a cancel-free context so a dead caller context cannot abort it.
	defer func() {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		if p := recover(); p != nil {
			panic(p)
		}
	}()
	if err := applyTenantContext(ctx, tx, org, statementTimeout); err != nil {
		return err
	}
	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("tenantdb: commit: %w", err)
	}
	return nil
}

// applyTenantContext sets the RLS tenant context as the first statement. The
// org id is a bind parameter and is never formatted into SQL (AGENTS.md rule
// 3); `true` makes the setting transaction-local so nothing leaks to the next
// borrower of a pooled connection (ADR-002 section 3 rule 2).
func applyTenantContext(ctx context.Context, tx pgx.Tx, org uuid.UUID, statementTimeout string) error {
	if _, err := tx.Exec(ctx,
		`SELECT set_config('app.current_organization_id', $1, true)`, org.String()); err != nil {
		return fmt.Errorf("tenantdb: set tenant context: %w", err)
	}
	if statementTimeout == "" {
		return nil
	}
	if _, err := tx.Exec(ctx,
		`SELECT set_config('statement_timeout', $1, true)`, statementTimeout); err != nil {
		return fmt.Errorf("tenantdb: set statement_timeout %q: %w", statementTimeout, err)
	}
	return nil
}
