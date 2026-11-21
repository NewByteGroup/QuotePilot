package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/resend"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
)

// maxFailureReason bounds the text written into rfq_requests.failure_reason.
const maxFailureReason = 500

// JobInserter is the slice of *river.Client[pgx.Tx] the worker uses to chain
// the next stage inside the same tenant transaction — checkpoint row and
// job row commit or roll back together (spec section 3, ADR-004).
type JobInserter interface {
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// RfqIngestWorker is S1 rfq_ingest (spec section 4): it promotes one
// inbound_messages receipt into an rfq_requests row carrying the full
// message — bodies, RFC headers, attachments stored under the tenant key —
// then chains rfq_extract. Everything happens inside one InTenantTx so a
// failure anywhere rolls the whole checkpoint back and a retry redoes it.
type RfqIngestWorker struct {
	river.WorkerDefaults[IngestArgs]
	// Runner is the only DB path a worker may use (ADR-002).
	Runner   tenantdb.TenantRunner
	Receiver resend.Receiver
	Storage  platform.ObjectStorage
	// Inserter is the River client; set before client.Start because the
	// registry must exist before the client can be built.
	Inserter JobInserter
}

// NewRfqIngestWorker builds the worker. main assigns Inserter once the
// River client exists.
//
//	w := jobs.NewRfqIngestWorker(db, receiver, storage)
//	... client := river.NewClient(...); w.Inserter = client
func NewRfqIngestWorker(runner tenantdb.TenantRunner, recv resend.Receiver, store platform.ObjectStorage) *RfqIngestWorker {
	return &RfqIngestWorker{Runner: runner, Receiver: recv, Storage: store}
}

// Work runs the S1 pipeline for one receipt. A transient error propagates
// for River's retry; on the final attempt the failure is also written as a
// FAILED shell in a fresh tenant tx (spec section 1: a dead-lettered job
// flips the RFQ to FAILED — nothing waits silently).
func (w *RfqIngestWorker) Work(ctx context.Context, job *river.Job[IngestArgs]) error {
	args := job.Args
	err := w.Runner.InTenantTx(ctx, args.OrganizationID, func(ctx context.Context, tx pgx.Tx) error {
		return w.ingest(ctx, tx, args)
	})
	if err != nil && job.JobRow != nil && job.MaxAttempts > 0 && job.Attempt >= job.MaxAttempts {
		if ferr := w.writeFailedShell(ctx, args, job.MaxAttempts, err); ferr != nil {
			return errors.Join(err, ferr)
		}
	}
	return err
}

// ingest is the whole S1 sequence inside the tenant transaction:
// resolve → fetch → promote → chain.
func (w *RfqIngestWorker) ingest(ctx context.Context, tx pgx.Tx, args IngestArgs) error {
	receipt, skip, err := w.findReceipt(ctx, tx, args)
	if err != nil || skip {
		return err
	}
	email, err := w.fetchOrShell(ctx, tx, args, receipt)
	if err != nil || email == nil {
		return err // nil email = the FAILED shell was written; job is done
	}
	return w.promote(ctx, tx, args, receipt, email)
}

// findReceipt loads the receipt and applies the replay check: skip=true
// means the job is done — the receipt is gone, or an rfq_requests row
// already holds this resend_message_id (UNIQUE collapses replays, spec S1).
func (w *RfqIngestWorker) findReceipt(ctx context.Context, tx pgx.Tx, args IngestArgs) (inboundReceipt, bool, error) {
	receipt, found, err := readReceipt(ctx, tx, args.InboundMessageID)
	if err != nil {
		return inboundReceipt{}, false, fmt.Errorf("jobs: rfq_ingest: read receipt %s: %w", args.InboundMessageID, err)
	}
	if !found {
		return inboundReceipt{}, true, nil
	}
	exists, err := rfqExists(ctx, tx, receipt.ResendMessageID)
	if err != nil {
		return inboundReceipt{}, false, fmt.Errorf("jobs: rfq_ingest: rfq lookup %q: %w", receipt.ResendMessageID, err)
	}
	return receipt, exists, nil
}

// fetchOrShell returns the fetched message; on a permanent fetch failure it
// writes the FAILED shell (spec S1: nothing is invisible) and returns nil.
// Transient errors propagate for River retry.
func (w *RfqIngestWorker) fetchOrShell(ctx context.Context, tx pgx.Tx, args IngestArgs, rcpt inboundReceipt) (*resend.InboundEmail, error) {
	email, err := w.Receiver.GetInboundEmail(ctx, rcpt.ResendMessageID)
	if err == nil {
		return email, nil
	}
	if resend.IsPermanent(err) {
		return nil, insertFailedShell(ctx, tx, args, rcpt, err)
	}
	return nil, fmt.Errorf("jobs: rfq_ingest: fetch %q: %w", rcpt.ResendMessageID, err)
}

// promote writes the RECEIVED row, stores the attachments, advances to
// PARSING and chains rfq_extract — one commit behind the checkpoint.
func (w *RfqIngestWorker) promote(ctx context.Context, tx pgx.Tx, args IngestArgs, rcpt inboundReceipt, email *resend.InboundEmail) error {
	rfqID, created, err := insertRfq(ctx, tx, args, rcpt, email)
	if err != nil {
		return err
	}
	if !created {
		return nil // lost the insert race — the other copy owns the pipeline
	}
	if err := w.storeAttachments(ctx, tx, args.OrganizationID, rfqID, email.Attachments); err != nil {
		return err
	}
	if err := setStatusParsing(ctx, tx, rfqID); err != nil {
		return err
	}
	return w.enqueueExtract(ctx, tx, args, rfqID)
}

// inboundReceipt is the slice of app.inbound_messages the worker needs.
type inboundReceipt struct {
	ResendMessageID string
	Subject         string
	ReceivedAt      time.Time
}

// readReceipt loads the receipt by id; RLS scopes it to the tenant org, so
// a receipt id belonging to another org is simply found=false.
func readReceipt(ctx context.Context, tx pgx.Tx, id uuid.UUID) (inboundReceipt, bool, error) {
	var r inboundReceipt
	err := tx.QueryRow(ctx,
		`SELECT resend_message_id, subject, received_at FROM app.inbound_messages WHERE id = $1`, id).
		Scan(&r.ResendMessageID, &r.Subject, &r.ReceivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return inboundReceipt{}, false, nil
	}
	return r, err == nil, err
}

// rfqExists reports whether an rfq_requests row already holds this Resend id.
func rfqExists(ctx context.Context, tx pgx.Tx, resendID string) (bool, error) {
	var one int
	err := tx.QueryRow(ctx,
		`SELECT 1 FROM app.rfq_requests WHERE resend_message_id = $1 LIMIT 1`, resendID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// allocRfqNumber is the org_rfq_counters upsert every rfq_requests insert
// runs as its CTE (migration 0009): one row-lock inside the tenant
// transaction serializes allocation per org, so concurrent ingests get
// sequential numbers. A replay that loses the INSERT race still burns its
// allocated number — gaps in rfq_number are the documented cost of the
// conflict-tolerant promotion path, exactly like quote_number.
const allocRfqNumber = `
	WITH alloc AS (
		INSERT INTO app.org_rfq_counters (organization_id, next_seq)
		VALUES ($1, 2)
		ON CONFLICT (organization_id) DO UPDATE
			SET next_seq = org_rfq_counters.next_seq + 1
		RETURNING next_seq - 1 AS n
	)`

// insertRfq writes the RECEIVED checkpoint row; ON CONFLICT DO NOTHING keeps
// the (org, resend_message_id) race idempotent — created=false means replay.
func insertRfq(ctx context.Context, tx pgx.Tx, args IngestArgs, rcpt inboundReceipt, em *resend.InboundEmail) (uuid.UUID, bool, error) {
	customerEmail, customerName := parseFrom(em.From)
	threadJSON, err := threadHeaders(em)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("jobs: rfq_ingest: thread headers: %w", err)
	}
	var id uuid.UUID
	err = tx.QueryRow(ctx, allocRfqNumber+`
		INSERT INTO app.rfq_requests
			(organization_id, rfq_number, resend_message_id, rfc_message_id, thread_headers_json,
			 customer_email, customer_name, subject, received_at, raw_text_body, raw_html_body)
		SELECT $1, alloc.n, $2, NULLIF($3,''), $4, $5, NULLIF($6,''), $7, $8, NULLIF($9,''), NULLIF($10,'')
		FROM alloc
		ON CONFLICT (organization_id, resend_message_id) DO NOTHING
		RETURNING id`,
		args.OrganizationID, rcpt.ResendMessageID, rfcMessageID(em), threadJSON,
		customerEmail, customerName, rcpt.Subject, rcpt.ReceivedAt, em.Text, em.HTML,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("jobs: rfq_ingest: insert rfq: %w", err)
	}
	return id, true, nil
}

// insertFailedShell writes the spec-S1 shell: a FAILED rfq_requests row with
// failure_reason so a permanently-unfetchable message stays visible.
func insertFailedShell(ctx context.Context, tx pgx.Tx, args IngestArgs, rcpt inboundReceipt, cause error) error {
	reason := truncate("rfq_ingest: "+cause.Error(), maxFailureReason)
	if _, err := tx.Exec(ctx, allocRfqNumber+`
		INSERT INTO app.rfq_requests
			(organization_id, rfq_number, resend_message_id, customer_email, subject, received_at, status, failure_reason)
		SELECT $1, alloc.n, $2, '', $3, $4, 'FAILED', $5
		FROM alloc
		ON CONFLICT (organization_id, resend_message_id) DO NOTHING`,
		args.OrganizationID, rcpt.ResendMessageID, rcpt.Subject, rcpt.ReceivedAt, reason); err != nil {
		return fmt.Errorf("jobs: rfq_ingest: insert failed shell: %w", err)
	}
	return nil
}

// storeAttachments downloads each attachment, pushes it to storage under
// the tenant key and writes the rfq_attachments row — all inside the tx.
func (w *RfqIngestWorker) storeAttachments(ctx context.Context, tx pgx.Tx, org, rfqID uuid.UUID, atts []resend.InboundAttachment) error {
	for i := range atts {
		if err := w.storeAttachment(ctx, tx, org, rfqID, atts[i], i); err != nil {
			return err
		}
	}
	return nil
}

// storeAttachment handles one attachment: download → storage.Put → row.
func (w *RfqIngestWorker) storeAttachment(ctx context.Context, tx pgx.Tx, org, rfqID uuid.UUID, att resend.InboundAttachment, index int) error {
	if att.DownloadURL == "" {
		return fmt.Errorf("jobs: rfq_ingest: attachment %q (%s) has no download_url", att.Filename, att.ID)
	}
	data, err := w.Receiver.DownloadAttachment(ctx, att.DownloadURL)
	if err != nil {
		return fmt.Errorf("jobs: rfq_ingest: download %q: %w", att.Filename, err)
	}
	filename := attachmentFilename(att)
	key := attachmentKey(org, rfqID, att, index, filename)
	if err := w.Storage.Put(ctx, key, data); err != nil {
		return fmt.Errorf("jobs: rfq_ingest: storage put %q: %w", key, err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO app.rfq_attachments (organization_id, rfq_id, storage_key, filename, mime_type, size_bytes)
		VALUES ($1, $2, $3, $4, NULLIF($5,''), $6)`,
		org, rfqID, key, filename, att.ContentType, int64(len(data)))
	if err != nil {
		return fmt.Errorf("jobs: rfq_ingest: insert attachment %q: %w", filename, err)
	}
	return nil
}

// setStatusParsing advances RECEIVED → PARSING (spec section 5) once the
// raw content and attachments are durably written.
func setStatusParsing(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID) error {
	_, err := tx.Exec(ctx,
		`UPDATE app.rfq_requests SET status = 'PARSING', updated_at = now() WHERE id = $1`, rfqID)
	if err != nil {
		return fmt.Errorf("jobs: rfq_ingest: set PARSING %s: %w", rfqID, err)
	}
	return nil
}

// enqueueExtract chains S2 in the same transaction; queue and uniqueness
// come from ExtractArgs.InsertOpts().
func (w *RfqIngestWorker) enqueueExtract(ctx context.Context, tx pgx.Tx, args IngestArgs, rfqID uuid.UUID) error {
	if w.Inserter == nil {
		return errors.New("jobs: rfq_ingest: Inserter not configured")
	}
	_, err := w.Inserter.InsertTx(ctx, tx, ExtractArgs{
		OrganizationID: args.OrganizationID,
		CorrelationID:  args.CorrelationID,
		RfqID:          rfqID,
	}, nil)
	if err != nil {
		return fmt.Errorf("jobs: rfq_ingest: enqueue rfq_extract %s: %w", rfqID, err)
	}
	return nil
}

// writeFailedShell re-reads the receipt in a fresh tenant tx and writes the
// FAILED shell — the dead-letter visibility rule for transient exhaustion.
//
// Known uncovered edges (accepted for this slice):
//   - a panic inside the tenant tx on the final attempt propagates past the
//     error path, so no shell is written — River's executor still discards
//     the job and the panic surfaces in job errors/logs;
//   - a dead ctx (shutdown, client cancel) makes the fresh InTenantTx fail
//     too, so the shell cannot be written — the joined error still reports
//     both failures rather than swallowing either.
func (w *RfqIngestWorker) writeFailedShell(ctx context.Context, args IngestArgs, attempts int, cause error) error {
	return w.Runner.InTenantTx(ctx, args.OrganizationID, func(ctx context.Context, tx pgx.Tx) error {
		rcpt, found, err := readReceipt(ctx, tx, args.InboundMessageID)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		return insertFailedShell(ctx, tx, args, rcpt,
			fmt.Errorf("exhausted %d attempts, last error: %w", attempts, cause))
	})
}

// parseFrom splits "Name <addr@x>" into (address, name) via net/mail;
// unparseable input degrades to the raw string and no name (tolerant).
func parseFrom(from string) (emailAddr, name string) {
	addr, err := mail.ParseAddress(from)
	if err != nil {
		return from, ""
	}
	return addr.Address, addr.Name
}

// rfcMessageID prefers the response's message_id (RFC 5322 Message-ID) and
// falls back to the headers map (Resend normalizes header names lowercase).
func rfcMessageID(em *resend.InboundEmail) string {
	if em.MessageID != "" {
		return em.MessageID
	}
	return em.Headers["message-id"]
}

// threadHeaders builds thread_headers_json (spec section 6): the RFC thread
// fields plus the full headers map — which preserves Authentication-Results
// and any auth headers — and Resend's server-side SPF/DKIM/DMARC verdict.
func threadHeaders(em *resend.InboundEmail) (json.RawMessage, error) {
	t := map[string]any{}
	if id := rfcMessageID(em); id != "" {
		t["message_id"] = id
	}
	for _, k := range []string{"in-reply-to", "references", "authentication-results"} {
		if v := em.Headers[k]; v != "" {
			t[k] = v
		}
	}
	if len(em.Headers) > 0 {
		t["headers"] = em.Headers
	}
	if em.Authentication != nil {
		t["authentication"] = em.Authentication
	}
	return json.Marshal(t)
}

// attachmentFilename derives a safe object-key segment: directories and
// backslashes are stripped so the key always stays under the tenant prefix,
// and an empty name falls back to a fixed "attachment" (uniqueness comes
// from the id segment attachmentKey adds in front).
func attachmentFilename(att resend.InboundAttachment) string {
	name := path.Base(strings.ReplaceAll(strings.TrimSpace(att.Filename), "\\", "/"))
	if name == "." || name == ".." || name == "/" || name == "" {
		name = "attachment"
	}
	return name
}

// attachmentKey builds tenants/{org}/rfqs/{rfq}/{id}-{filename}. The id
// segment — the Resend attachment id, or the slice index when absent —
// keeps every attachment's key unique even for duplicate or missing
// filenames, while the sanitized filename stays readable in the key.
func attachmentKey(org, rfqID uuid.UUID, att resend.InboundAttachment, index int, filename string) string {
	seg := att.ID
	if seg == "" {
		seg = "att-" + strconv.Itoa(index)
	} else {
		seg = path.Base(strings.ReplaceAll(seg, "\\", "/"))
	}
	return fmt.Sprintf("tenants/%s/rfqs/%s/%s-%s", org, rfqID, seg, filename)
}

// truncate bounds s to n runes for failure_reason.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
