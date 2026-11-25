package jobs

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/riverqueue/river"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// AuthMailSweepArgs is deliberately empty: the sweep is whole-table
// platform maintenance, so there is nothing to carry — AGENTS.md rule 7
// holds trivially, no tenant content and no IDs exist to send.
type AuthMailSweepArgs struct{}

// Kind returns the River job kind.
func (AuthMailSweepArgs) Kind() string { return "auth_mail_sweep" }

// AuthMailSweepWorker is the follow-up adr-rfc-plan.md's durable-send
// entry contemplated: a send that exhausts River's retries discards the
// job while its outbox row persists as dead weight, and a consumed
// token's row is dead by construction — read_mail_outbox filters it
// before the worker could receipt it, so the per-job path never deletes
// one (migration 0014). The Store is the plain pool Querier like
// AuthMailWorker's: auth_mail_outbox is Layer A, outside every tenant
// context.
type AuthMailSweepWorker struct {
	river.WorkerDefaults[AuthMailSweepArgs]
	Store *platform.Store
}

// NewAuthMailSweepWorker builds the worker against the pool Store.
//
//	river.AddWorker(workers, jobs.NewAuthMailSweepWorker(store))
func NewAuthMailSweepWorker(store *platform.Store) *AuthMailSweepWorker {
	return &AuthMailSweepWorker{Store: store}
}

// Work runs one sweep pass and logs the deleted count — the run's only
// observable output. A failure propagates for River's retry: an undone
// sweep is cheap to redo, the DELETE is one statement.
func (w *AuthMailSweepWorker) Work(ctx context.Context, _ *river.Job[AuthMailSweepArgs]) error {
	deleted, err := w.Store.SweepAuthMailOutbox(ctx)
	if err != nil {
		return fmt.Errorf("jobs: auth_mail_sweep: %w", err)
	}
	slog.InfoContext(ctx, "jobs: auth_mail_sweep: swept dead outbox rows", "deleted", deleted)
	return nil
}
