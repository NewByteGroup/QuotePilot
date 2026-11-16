// Package jobs hosts River job workers (ADR-004): the spike's echo worker,
// which proved a job runs under the RLS tenant context, and the
// auth_mail_send worker draining the magic-link outbox (migration 0004).
package jobs

import (
	"context"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
)

// EchoTenantArgs carries IDs only (AGENTS.md rule 7: never tenant content).
type EchoTenantArgs struct {
	OrganizationID uuid.UUID `json:"organization_id"`
}

// Kind returns the River job kind.
func (EchoTenantArgs) Kind() string { return "echo_tenant" }

// EchoTenantWorker writes one app.inbound_messages row through InTenantTx,
// proving the worker runs under the tenant's RLS context (gate criterion 6).
type EchoTenantWorker struct {
	river.WorkerDefaults[EchoTenantArgs]
	// Runner is the only DB path a worker may use (ADR-002 section 3 rule 6).
	Runner tenantdb.TenantRunner
}

// NewEchoTenantWorker builds the worker.
//
//	river.AddWorker(workers, jobs.NewEchoTenantWorker(db))
func NewEchoTenantWorker(runner tenantdb.TenantRunner) *EchoTenantWorker {
	return &EchoTenantWorker{Runner: runner}
}

// Work inserts a deterministic row (resend_message_id "job:<river job id>")
// inside the tenant transaction of the org from the job args.
func (w *EchoTenantWorker) Work(ctx context.Context, job *river.Job[EchoTenantArgs]) error {
	org := job.Args.OrganizationID
	resendID := "job:" + strconv.FormatInt(job.ID, 10)
	err := w.Runner.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO app.inbound_messages (organization_id, resend_message_id, subject) VALUES ($1, $2, $3)`,
			org.String(), resendID, "river echo")
		return err
	})
	if err != nil {
		return fmt.Errorf("jobs: echo_tenant insert for org %q: %w", org, err)
	}
	return nil
}
