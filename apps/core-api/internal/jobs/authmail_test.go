package jobs_test

// Unit proofs of the auth_mail_send worker's read→send→delete contract
// (migration 0004): a pending row earns one send plus the delete receipt,
// an absent row is done (already sent, dead, or never queued), an expired
// one is dropped without mailing a dead link, a send error propagates for
// River's retry with the row kept, and a delete error propagates the same
// way. The real platform.Store runs over the in-memory Querier, so the SQL
// text and NULL handling are production's — only the transport is faked.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/jobs"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// stubRow is a one-row pgx.Row whose Scan the test controls — the jobs
// package's twin of the auth rigs' stub.
type stubRow struct{ scan func(dest ...any) error }

func (r stubRow) Scan(dest ...any) error { return r.scan(dest...) }

// outboxCreate records one create_mail_outbox call — covered so an
// accidental call on the worker path fails loudly rather than slipping a
// write into a read→send→delete contract that never creates.
type outboxCreate struct {
	tokenID  uuid.UUID
	link     string
	outboxID uuid.UUID
}

// outboxQuerier is the in-memory platform.Querier for the three
// migration-0004 outbox functions: pending sends keyed by outbox id, call
// records, and per-function driver-error injection.
type outboxQuerier struct {
	rows    map[uuid.UUID]platform.MailOutbox
	fail    map[string]error
	creates []outboxCreate
	reads   []uuid.UUID
	deletes []uuid.UUID
}

var _ platform.Querier = (*outboxQuerier)(nil)

func (q *outboxQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "create_mail_outbox"):
		return q.createRow(args)
	case strings.Contains(sql, "read_mail_outbox"):
		return q.readRow(args)
	case strings.Contains(sql, "delete_mail_outbox"):
		return q.deleteRow(args)
	default:
		return stubRow{scan: func(...any) error {
			return fmt.Errorf("outboxQuerier: unexpected sql %q", sql)
		}}
	}
}

func (q *outboxQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("outboxQuerier: Query is not on the outbox path")
}

// createRow answers SELECT app.create_mail_outbox($1, $2): like migration
// 0004's function it refuses a blank link — it would queue a send that can
// never deliver.
func (q *outboxQuerier) createRow(args []any) pgx.Row {
	return stubRow{scan: func(dest ...any) error {
		if err := q.fail["create_mail_outbox"]; err != nil {
			return err
		}
		link := args[1].(string)
		if strings.TrimSpace(link) == "" {
			return errors.New("create_mail_outbox: link_url must be a non-empty string")
		}
		call := outboxCreate{tokenID: args[0].(uuid.UUID), link: link, outboxID: uuid.New()}
		q.creates = append(q.creates, call)
		q.rows[call.outboxID] = platform.MailOutbox{Link: link}
		*(dest[0].(*uuid.UUID)) = call.outboxID
		return nil
	}}
}

// readRow answers SELECT * FROM app.read_mail_outbox($1): a known id scans
// the row, an unknown one the pgx.ErrNoRows the Store maps to found=false —
// the job is done.
func (q *outboxQuerier) readRow(args []any) pgx.Row {
	return stubRow{scan: func(dest ...any) error {
		if err := q.fail["read_mail_outbox"]; err != nil {
			return err
		}
		id := args[0].(uuid.UUID)
		q.reads = append(q.reads, id)
		row, ok := q.rows[id]
		if !ok {
			return pgx.ErrNoRows
		}
		*(dest[0].(*string)) = row.Link
		*(dest[1].(*string)) = row.Email
		*(dest[2].(*time.Time)) = row.ExpiresAt
		return nil
	}}
}

// deleteRow answers SELECT app.delete_mail_outbox($1): the delivery
// receipt — true when a row existed, false when it was already gone,
// scanned through the **bool destination the Store's NULL handling uses.
func (q *outboxQuerier) deleteRow(args []any) pgx.Row {
	return stubRow{scan: func(dest ...any) error {
		if err := q.fail["delete_mail_outbox"]; err != nil {
			return err
		}
		id := args[0].(uuid.UUID)
		q.deletes = append(q.deletes, id)
		_, ok := q.rows[id]
		delete(q.rows, id)
		*(dest[0].(**bool)) = &ok
		return nil
	}}
}

// outboxJob wraps the outbox id in the job shape Work consumes.
func outboxJob(id uuid.UUID) *river.Job[jobs.AuthMailArgs] {
	return &river.Job[jobs.AuthMailArgs]{Args: jobs.AuthMailArgs{OutboxID: id}}
}

func newOutboxQuerier() *outboxQuerier {
	return &outboxQuerier{rows: map[uuid.UUID]platform.MailOutbox{}, fail: map[string]error{}}
}

// TestAuthMailSendsThenDeletes is the happy path: a live pending row earns
// exactly one send — recipient and link as read_mail_outbox resolved them —
// then the delete receipt takes the row off the pending set.
func TestAuthMailSendsThenDeletes(t *testing.T) {
	q := newOutboxQuerier()
	mailer := &platform.FakeMailSender{}
	worker := jobs.NewAuthMailWorker(platform.NewStore(q), mailer, time.Now)
	id := uuid.New()
	q.rows[id] = platform.MailOutbox{
		Link:      "https://web.test/auth/exchange?token=abc",
		Email:     "ada@test.dev",
		ExpiresAt: time.Now().Add(15 * time.Minute),
	}

	require.NoError(t, worker.Work(context.Background(), outboxJob(id)))

	sent := mailer.Sent()
	require.Len(t, sent, 1, "one pending send delivers exactly one mail")
	assert.Equal(t, "ada@test.dev", sent[0].To)
	assert.Equal(t, "https://web.test/auth/exchange?token=abc", sent[0].URL)
	assert.Equal(t, []uuid.UUID{id}, q.deletes, "the send earns the delete receipt")
	assert.NotContains(t, q.rows, id, "the row leaves the pending set")
	assert.Empty(t, q.creates, "the worker drains the outbox — it never writes to it")
}

// TestAuthMailAbsentRowIsDone covers the at-least-once read: an absent row
// means the send already went out, the token died, or the job was never
// staged — all complete, none an error, and nothing is mailed or deleted.
func TestAuthMailAbsentRowIsDone(t *testing.T) {
	q := newOutboxQuerier()
	mailer := &platform.FakeMailSender{}
	worker := jobs.NewAuthMailWorker(platform.NewStore(q), mailer, time.Now)

	require.NoError(t, worker.Work(context.Background(), outboxJob(uuid.New())))
	assert.Empty(t, mailer.Sent(), "a missing row is done, not a send")
	assert.Empty(t, q.deletes, "there is no row to receipt")
}

// TestAuthMailExpiredRowDropsUnsent is the worker-owned expiry policy: a
// row past the token's TTL is a dead link — deleted, never mailed, and the
// job still completes.
func TestAuthMailExpiredRowDropsUnsent(t *testing.T) {
	q := newOutboxQuerier()
	mailer := &platform.FakeMailSender{}
	now := time.Now()
	worker := jobs.NewAuthMailWorker(platform.NewStore(q), mailer, func() time.Time { return now })
	id := uuid.New()
	q.rows[id] = platform.MailOutbox{
		Link:      "https://web.test/auth/exchange?token=abc",
		Email:     "ada@test.dev",
		ExpiresAt: now.Add(-time.Minute),
	}

	require.NoError(t, worker.Work(context.Background(), outboxJob(id)))
	assert.Empty(t, mailer.Sent(), "a dead link is dropped, not mailed")
	assert.Equal(t, []uuid.UUID{id}, q.deletes)
	assert.NotContains(t, q.rows, id)
}

// TestAuthMailSendErrorKeepsRow is the retry contract: a provider failure
// propagates so River retries, and the row stays pending for the next
// attempt — at-least-once by construction.
func TestAuthMailSendErrorKeepsRow(t *testing.T) {
	q := newOutboxQuerier()
	worker := jobs.NewAuthMailWorker(platform.NewStore(q), &failMailer{err: errBoom}, time.Now)
	id := uuid.New()
	q.rows[id] = platform.MailOutbox{
		Link:      "https://web.test/auth/exchange?token=abc",
		Email:     "ada@test.dev",
		ExpiresAt: time.Now().Add(15 * time.Minute),
	}

	err := worker.Work(context.Background(), outboxJob(id))
	require.ErrorIs(t, err, errBoom)
	assert.Empty(t, q.deletes, "no receipt without a send")
	assert.Contains(t, q.rows, id, "the row survives for the retry")
}

// TestAuthMailDeleteErrorPropagates covers both terminal branches' delete
// leg: the receipt's own failure propagates (the duplicate a retry resends
// is the SAME link — harmless, the token is single-use).
func TestAuthMailDeleteErrorPropagates(t *testing.T) {
	q := newOutboxQuerier()
	mailer := &platform.FakeMailSender{}
	worker := jobs.NewAuthMailWorker(platform.NewStore(q), mailer, time.Now)
	id := uuid.New()
	q.rows[id] = platform.MailOutbox{
		Link:      "https://web.test/auth/exchange?token=abc",
		Email:     "ada@test.dev",
		ExpiresAt: time.Now().Add(15 * time.Minute),
	}
	q.fail["delete_mail_outbox"] = errBoom

	err := worker.Work(context.Background(), outboxJob(id))
	require.ErrorIs(t, err, errBoom)
	assert.Len(t, mailer.Sent(), 1, "the send already went out")
	assert.Contains(t, q.rows, id, "a failed receipt leaves the row pending")
}

// TestAuthMailReadErrorPropagates covers the claim leg: a driver error on
// the read itself is an error for River, not a done job.
func TestAuthMailReadErrorPropagates(t *testing.T) {
	q := newOutboxQuerier()
	mailer := &platform.FakeMailSender{}
	worker := jobs.NewAuthMailWorker(platform.NewStore(q), mailer, time.Now)
	q.fail["read_mail_outbox"] = errBoom

	err := worker.Work(context.Background(), outboxJob(uuid.New()))
	require.ErrorIs(t, err, errBoom)
	assert.Empty(t, mailer.Sent())
	assert.Empty(t, q.deletes)
}

// errBoom is the shared driver/provider failure the tests inject.
var errBoom = errors.New("boom")

// failMailer is a MailSender that always errors — FakeMailSender never
// fails, so the send-error branch needs its own double.
type failMailer struct{ err error }

func (f *failMailer) SendMagicLink(context.Context, string, string) error { return f.err }
