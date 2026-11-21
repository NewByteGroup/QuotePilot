package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// AuthMailArgs carries the outbox id only (AGENTS.md rule 7: job args are
// IDs, never content). river_job args are qp_app-readable at rest, so the
// tokened link_url riding here would be a second live-credential copy
// outside the outbox's retention story (migration 0004) — the worker
// re-reads the link instead.
type AuthMailArgs struct {
	OutboxID uuid.UUID `json:"outbox_id"`
}

// Kind returns the River job kind.
func (AuthMailArgs) Kind() string { return "auth_mail_send" }

// AuthMailWorker drains the auth-mail outbox — the read→send→delete
// delivery of migration 0004, replacing /auth/login's old detached
// goroutine, extended by migration 0007 to pick the mail template from
// the token's purpose (login link vs. set-password link). The Store is
// the plain pool Querier, never InTenantTx: auth_mail_outbox is Layer A
// like the rest of the auth family, written before any tenant context
// exists.
//
// Delivery is at-least-once by design: the row only disappears after the
// mail provider accepts, so a send failure leaves it for River's retry and
// a retried send delivers the SAME link. The one residual — a crash
// between send and delete — resends that identical link, harmless because
// the login token is single-use on consume.
type AuthMailWorker struct {
	river.WorkerDefaults[AuthMailArgs]
	Store  *platform.Store
	Mailer platform.MailSender
	// Now is the expiry clock — the worker, not the login transaction, owns
	// the dead-link policy read_mail_outbox exposes through expires_at.
	Now func() time.Time
}

// NewAuthMailWorker builds the worker against the pool Store and the mailer.
//
//	river.AddWorker(workers, jobs.NewAuthMailWorker(store, mailer, time.Now))
func NewAuthMailWorker(store *platform.Store, mailer platform.MailSender, now func() time.Time) *AuthMailWorker {
	return &AuthMailWorker{Store: store, Mailer: mailer, Now: now}
}

// Work is the migration-0004 contract in order: read the pending send, skip
// a missing row (already sent, dead, or never queued — the job is done),
// drop an expired one without mailing a dead link, send, then delete as the
// delivery receipt. Only the send's error retried with the row still in
// place preserves at-least-once delivery.
func (w *AuthMailWorker) Work(ctx context.Context, job *river.Job[AuthMailArgs]) error {
	id := job.Args.OutboxID
	ob, found, err := w.Store.ReadMailOutbox(ctx, id)
	if err != nil {
		return fmt.Errorf("jobs: auth_mail_send: read outbox %s: %w", id, err)
	}
	if !found {
		return nil
	}
	if w.Now().After(ob.ExpiresAt) {
		return w.deleteOutbox(ctx, id)
	}
	if err := w.sendForPurpose(ctx, ob); err != nil {
		return fmt.Errorf("jobs: auth_mail_send: send outbox %s: %w", id, err)
	}
	return w.deleteOutbox(ctx, id)
}

// sendForPurpose picks the template the token's purpose asks for — a
// password_reset row mails the set-password link, never a sign-in link.
// Anything outside the two CHECKed values is a loud error, not a
// defaulted template: a migration that adds a purpose must teach this
// switch, and silently mailing a magic link for an unknown class would
// send the wrong credential mail to a real inbox.
func (w *AuthMailWorker) sendForPurpose(ctx context.Context, ob platform.MailOutbox) error {
	switch ob.Purpose {
	case platform.MailPurposeLogin:
		return w.Mailer.SendMagicLink(ctx, ob.Email, ob.Link)
	case platform.MailPurposePasswordReset:
		return w.Mailer.SendPasswordReset(ctx, ob.Email, ob.Link)
	default:
		return fmt.Errorf("jobs: auth_mail_send: unknown outbox purpose %q", ob.Purpose)
	}
}

// deleteOutbox is the terminal step both finishing branches share — the
// post-send receipt and the dead-link sweep. Its error propagates so River
// retries rather than leak a pending row the next job cannot see as done.
func (w *AuthMailWorker) deleteOutbox(ctx context.Context, id uuid.UUID) error {
	if _, err := w.Store.DeleteMailOutbox(ctx, id); err != nil {
		return fmt.Errorf("jobs: auth_mail_send: delete outbox %s: %w", id, err)
	}
	return nil
}
