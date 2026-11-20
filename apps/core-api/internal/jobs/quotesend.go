package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/resend"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
)

// Quote status strings as the app.quote_status enum stores them — the
// worker's transitions (DRAFT→APPROVED_SENT) and no-op check read these.
const (
	quoteDraft        = "DRAFT"
	quoteSuperseded   = "SUPERSEDED"
	quoteApprovedSent = "APPROVED_SENT"
)

// send_refused metadata reasons — the vocabulary of why a staged send died
// without calling the provider. Every refusal also writes the audit row;
// the reason is what the audit view groups on.
const (
	refuseUnapproved     = "unapproved"
	refuseNegativeMargin = "negative_margin"
	refuseMissingRender  = "missing_render"
	refuseSuperseded     = "superseded"
	refuseNoRecipient    = "no_recipient"
	refuseQuoteMissing   = "quote_missing"
	// refuseProviderPermanent marks a provider's deterministic rejection
	// (4xx other than 408/429, or an unconfigured client) — audited in a
	// fresh tx because the send's own tx already rolled back.
	refuseProviderPermanent = "provider_permanent"
)

// QuoteSendWorker is S5 quote_send (spec section 4): the last pipeline
// stage and the only path that can put a quote in a customer's inbox. The
// first database action is InTenantTx(organization_id); the whole load →
// send → bookkeeping sequence runs inside it, so a crash between the
// provider call and the commit rolls back the bookkeeping and the retry
// collapses on the Resend idempotency key ({quote_id}:{version}).
type QuoteSendWorker struct {
	river.WorkerDefaults[SendArgs]
	// Runner is the only DB path a worker may use (ADR-002).
	Runner tenantdb.TenantRunner
	// Sender is the Resend sending seam — FakeSender in tests,
	// UnconfiguredSender when credentials are absent.
	Sender resend.Sender
	// Storage fetches the rendered PDF by pdf_storage_key.
	Storage platform.ObjectStorage
	// OutboundFrom is the verified platform mailbox Resend accepts as the
	// From: domain (QP_OUTBOUND_FROM) — the tenant's outbound_reply_to
	// rides as Reply-To instead, so customer answers still land on the
	// tenant while the send domain stays platform-verified.
	OutboundFrom string
}

// NewQuoteSendWorker builds the worker.
//
//	w := jobs.NewQuoteSendWorker(db, sender, storage, cfg.OutboundFrom)
func NewQuoteSendWorker(runner tenantdb.TenantRunner, sender resend.Sender, store platform.ObjectStorage, outboundFrom string) *QuoteSendWorker {
	return &QuoteSendWorker{Runner: runner, Sender: sender, Storage: store, OutboundFrom: outboundFrom}
}

// Work runs S5 for one approved quote version. Deterministic failures —
// the refusal set plus permanent provider errors — cancel via JobCancel;
// transient errors propagate for River's retry; a transient error on the
// final attempt additionally parks the RFQ FAILED (spec section 3:
// "exhausted ⇒ FAILED, no automatic resend").
func (w *QuoteSendWorker) Work(ctx context.Context, job *river.Job[SendArgs]) error {
	args := job.Args
	cancelCause, err := w.run(ctx, args)
	if err != nil {
		if resend.IsPermanent(err) {
			// A deterministic provider rejection cancels — but not
			// invisibly: the tenant audit row lands in a fresh tx (the
			// send's own tx rolled back). The audit write failing must
			// not mask the original error — join, like writeSendFailed.
			if aerr := w.writeProviderRefusal(ctx, args); aerr != nil {
				return errors.Join(err, aerr)
			}
			return river.JobCancel(err)
		}
		if job.JobRow != nil && job.MaxAttempts > 0 && job.Attempt >= job.MaxAttempts {
			if ferr := w.writeSendFailed(ctx, args, job.MaxAttempts, err); ferr != nil {
				return errors.Join(err, ferr)
			}
		}
		return err
	}
	if cancelCause != nil {
		return river.JobCancel(cancelCause)
	}
	return nil
}

// run is the InTenantTx boundary: the tenant tx commits whatever send
// decided — a refusal's audit/RFQ state or the sent bookkeeping — and the
// returned cancelCause becomes the JobCancel Work wraps afterwards. A real
// error rolls the whole decision back instead.
func (w *QuoteSendWorker) run(ctx context.Context, args SendArgs) (cancelCause error, err error) {
	err = w.Runner.InTenantTx(ctx, args.OrganizationID, func(ctx context.Context, tx pgx.Tx) error {
		cancelCause, err = w.send(ctx, tx, args)
		return err
	})
	return cancelCause, err
}

// send is the full S5 sequence inside the tenant transaction:
// lock → refuse → load → deliver → bookkeeping.
func (w *QuoteSendWorker) send(ctx context.Context, tx pgx.Tx, args SendArgs) (cancelCause error, err error) {
	q, found, err := lockSendQuote(ctx, tx, args.QuoteID)
	if err != nil {
		return nil, fmt.Errorf("jobs: quote_send: lock %s: %w", args.QuoteID, err)
	}
	if !found {
		// entity_id carries no FK, so the refusal is still auditable —
		// a send job for a deleted quote is drift, not a silent skip.
		return w.refuse(ctx, tx, args, refuseQuoteMissing,
			fmt.Errorf("jobs: quote_send: quote %s does not exist", args.QuoteID))
	}
	if q.Status == quoteApprovedSent {
		return nil, nil // replay: the row already flipped — no second send
	}
	if q.Status == quoteSuperseded {
		return w.refuse(ctx, tx, args, refuseSuperseded,
			fmt.Errorf("jobs: quote_send: quote %s is superseded", q.ID))
	}
	if q.ApprovedAt == nil {
		return w.refuse(ctx, tx, args, refuseUnapproved,
			fmt.Errorf("jobs: quote_send: quote %s was never approved", q.ID))
	}
	return w.deliver(ctx, tx, args, q)
}

// deliver is the second half: the margin re-check, the render lookup, the
// provider call and the sent bookkeeping.
func (w *QuoteSendWorker) deliver(ctx context.Context, tx pgx.Tx, args SendArgs, q sendQuote) (cancelCause error, err error) {
	negative, err := recheckMargin(q.PricingJSON)
	if err != nil {
		return nil, fmt.Errorf("jobs: quote_send: pricing_result_json on %s: %w", q.ID, err)
	}
	if negative {
		return w.refuseMargin(ctx, tx, args, q)
	}
	if q.PdfKey == nil || *q.PdfKey == "" {
		return w.refuse(ctx, tx, args, refuseMissingRender,
			fmt.Errorf("jobs: quote_send: quote %s has no pdf_storage_key", q.ID))
	}
	pdf, err := w.Storage.Get(ctx, *q.PdfKey)
	if errors.Is(err, platform.ErrObjectNotFound) {
		return w.refuse(ctx, tx, args, refuseMissingRender,
			fmt.Errorf("jobs: quote_send: render artifact %q is gone", *q.PdfKey))
	}
	if err != nil {
		return nil, fmt.Errorf("jobs: quote_send: storage get %q: %w", *q.PdfKey, err)
	}
	rfq, err := loadSendRfq(ctx, tx, q.RfqID)
	if err != nil {
		return nil, fmt.Errorf("jobs: quote_send: load rfq %s: %w", q.RfqID, err)
	}
	branding, err := loadBranding(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("jobs: quote_send: load org branding: %w", err)
	}
	to, err := recipient(rfq.CustomerEmail)
	if err != nil {
		return w.refuse(ctx, tx, args, refuseNoRecipient, err)
	}
	req, err := w.buildSendRequest(q, rfq, branding, pdf, args.QuoteID, to)
	if err != nil {
		// Sender/configuration faults are not recipient refusals: they
		// propagate as job errors — loud, retried, then dead-lettered.
		return nil, err
	}
	messageID, err := w.Sender.Send(ctx, *req)
	if err != nil {
		return nil, err // classified by Work: permanent cancels, transient retries
	}
	return nil, w.recordSent(ctx, tx, args, q, messageID)
}

// refuse writes the quote.send_refused audit row and returns the cause the
// tx commits — Work turns it into the JobCancel afterwards.
func (w *QuoteSendWorker) refuse(ctx context.Context, tx pgx.Tx, args SendArgs, reason string, cause error) (error, error) {
	if err := auditSend(ctx, tx, args.OrganizationID, "quote.send_refused", args.QuoteID,
		map[string]any{"reason": reason}); err != nil {
		return nil, err
	}
	return cause, nil
}

// refuseMargin is the defense-in-depth refusal (spec: the check fires at
// approval AND at send): the RFQ parks NEEDS_REVIEW with
// NEGATIVE_MARGIN alongside the send_refused audit row.
func (w *QuoteSendWorker) refuseMargin(ctx context.Context, tx pgx.Tx, args SendArgs, q sendQuote) (error, error) {
	if _, err := tx.Exec(ctx, `
		UPDATE app.rfq_requests
		SET status = 'NEEDS_REVIEW', review_reason = $2, updated_at = now()
		WHERE id = $1`, q.RfqID, string(ReviewReasonNegativeMargin)); err != nil {
		return nil, fmt.Errorf("jobs: quote_send: park rfq %s: %w", q.RfqID, err)
	}
	return w.refuse(ctx, tx, args, refuseNegativeMargin,
		fmt.Errorf("jobs: quote_send: quote %s is priced below cost", q.ID))
}

// recordSent is the success bookkeeping (spec S5): quote and RFQ flip to
// APPROVED_SENT, the provider message id lands on
// sent_outbound_message_id (migration 0008's partial unique index is the
// final double-send guard) and quote.sent audits the delivery.
func (w *QuoteSendWorker) recordSent(ctx context.Context, tx pgx.Tx, args SendArgs, q sendQuote, messageID string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE app.quotes
		SET status = 'APPROVED_SENT', sent_outbound_message_id = $2, updated_at = now()
		WHERE id = $1`, q.ID, messageID); err != nil {
		return fmt.Errorf("jobs: quote_send: flip quote %s: %w", q.ID, err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE app.rfq_requests
		SET status = 'APPROVED_SENT', updated_at = now()
		WHERE id = $1`, q.RfqID); err != nil {
		return fmt.Errorf("jobs: quote_send: flip rfq %s: %w", q.RfqID, err)
	}
	return auditSend(ctx, tx, args.OrganizationID, "quote.sent", q.ID,
		map[string]any{"message_id": messageID})
}

// writeProviderRefusal audits a permanent provider rejection in a fresh
// tenant transaction — the send's own tx rolled back, so without this row
// the cancelled delivery would be invisible to the tenant.
func (w *QuoteSendWorker) writeProviderRefusal(ctx context.Context, args SendArgs) error {
	return w.Runner.InTenantTx(ctx, args.OrganizationID, func(ctx context.Context, tx pgx.Tx) error {
		return auditSend(ctx, tx, args.OrganizationID, "quote.send_refused", args.QuoteID,
			map[string]any{"reason": refuseProviderPermanent})
	})
}

// writeSendFailed is the dead-letter half of S5: retries exhausted on a
// transient error parks the RFQ FAILED with the reason — a fresh tenant tx
// because the failed one already rolled back (mirrors writeFailedShell).
func (w *QuoteSendWorker) writeSendFailed(ctx context.Context, args SendArgs, attempts int, cause error) error {
	return w.Runner.InTenantTx(ctx, args.OrganizationID, func(ctx context.Context, tx pgx.Tx) error {
		reason := truncate(fmt.Sprintf("quote_send: exhausted %d attempts, last error: %s", attempts, cause), maxFailureReason)
		_, err := tx.Exec(ctx, `
			UPDATE app.rfq_requests AS r
			SET status = 'FAILED', failure_reason = $2, updated_at = now()
			FROM app.quotes AS q
			WHERE q.id = $1 AND r.id = q.rfq_id`,
			args.QuoteID, reason)
		return err
	})
}

// sendQuote is the locked slice of app.quotes the decision reads.
type sendQuote struct {
	ID          uuid.UUID
	RfqID       uuid.UUID
	Status      string
	QuoteNumber string
	Version     int
	ApprovedAt  *time.Time
	PdfKey      *string
	PricingJSON []byte
}

// lockSendQuote SELECTs the version FOR UPDATE — the lock is what makes
// "already APPROVED_SENT ⇒ no second send" airtight against a racing
// worker attempt.
func lockSendQuote(ctx context.Context, tx pgx.Tx, quoteID uuid.UUID) (sendQuote, bool, error) {
	var q sendQuote
	err := tx.QueryRow(ctx, `
		SELECT id, rfq_id, status::text, quote_number, version,
		       approved_at, pdf_storage_key, pricing_result_json
		FROM app.quotes WHERE id = $1 FOR UPDATE`, quoteID).
		Scan(&q.ID, &q.RfqID, &q.Status, &q.QuoteNumber, &q.Version,
			&q.ApprovedAt, &q.PdfKey, &q.PricingJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return sendQuote{}, false, nil
	}
	return q, err == nil, err
}

// sendRfq is the recipient + thread data the RFQ row carries.
type sendRfq struct {
	CustomerEmail string
	Subject       string
	ThreadHeaders []byte
}

// loadSendRfq reads the RFQ the quote hangs off — always present (FK), so a
// miss is contract drift and an error, never a quiet skip.
func loadSendRfq(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID) (sendRfq, error) {
	var r sendRfq
	var subject *string
	err := tx.QueryRow(ctx, `
		SELECT customer_email, subject, thread_headers_json::text
		FROM app.rfq_requests WHERE id = $1`, rfqID).
		Scan(&r.CustomerEmail, &subject, &r.ThreadHeaders)
	if err != nil {
		return sendRfq{}, err
	}
	if subject != nil {
		r.Subject = *subject
	}
	return r, nil
}

// sendBranding is the org's outbound pair (spec S5): the display name on
// From and the reply-to address customer answers land on.
type sendBranding struct {
	FromName string
	ReplyTo  string
}

// loadBranding reads outbound_from_name/outbound_reply_to through
// app.get_current_organization() — qp_app holds no direct grant on
// organizations, so the SECURITY DEFINER accessor is the only read path;
// inside the tenant tx it scopes to the resolved org by construction.
func loadBranding(ctx context.Context, tx pgx.Tx) (sendBranding, error) {
	var b sendBranding
	err := tx.QueryRow(ctx, `
		SELECT outbound_from_name, outbound_reply_to
		FROM app.get_current_organization()`).
		Scan(&b.FromName, &b.ReplyTo)
	return b, err
}

// recheckMargin decodes the pinned engine output — the same guard the
// approve endpoint runs, repeated here because data may have changed
// between approval and send (defense in depth, spec S5).
func recheckMargin(raw []byte) (bool, error) {
	if len(raw) == 0 {
		return false, errors.New("pricing_result_json is NULL")
	}
	var out pricing.Output
	if err := json.Unmarshal(raw, &out); err != nil {
		return false, fmt.Errorf("decode: %w", err)
	}
	return out.NegativeMargin(), nil
}

// buildSendRequest composes the minimal send (spec S5: the PDF, the quote
// number in the text, thread headers so the reply lands in the customer's
// existing conversation — S4's render owns the real HTML later).
func (w *QuoteSendWorker) buildSendRequest(q sendQuote, rfq sendRfq, b sendBranding, pdf []byte, quoteID uuid.UUID, to string) (*resend.SendRequest, error) {
	from, err := fromAddress(b, w.OutboundFrom)
	if err != nil {
		return nil, err
	}
	return &resend.SendRequest{
		To:       []string{to},
		From:     from,
		ReplyTo:  b.ReplyTo,
		Subject:  replySubject(rfq.Subject, q.QuoteNumber),
		TextBody: textBody(q.QuoteNumber, b.FromName),
		Attachments: []resend.SendAttachment{{
			Filename: q.QuoteNumber + ".pdf",
			Content:  pdf,
		}},
		Headers:        replyHeaders(rfq.ThreadHeaders),
		IdempotencyKey: fmt.Sprintf("%s:%d", quoteID, q.Version),
	}, nil
}

// recipient validates the RFQ's customer address — an empty or malformed
// address is deterministic drift (the row was written from a verified
// inbound From), so it refuses rather than retrying.
func recipient(email string) (string, error) {
	addr, err := mail.ParseAddress(strings.TrimSpace(email))
	if err != nil {
		return "", fmt.Errorf("jobs: quote_send: rfq customer_email %q is not a usable address", email)
	}
	return addr.Address, nil
}

// fromAddress builds "Name <platform-addr>" — From must be a Resend-
// verified domain, and the tenant's reply_to is not one (sending From an
// unverified domain is a permanent 4xx). The tenant's name still fronts
// the message and its outbound_reply_to rides as Reply-To, so the customer
// sees and answers the tenant's address either way. An empty outboundFrom
// is a wiring bug — a plain error, not a refusal, so it surfaces as a
// retryable job failure instead of a silent cancel.
func fromAddress(b sendBranding, outboundFrom string) (string, error) {
	addr := strings.TrimSpace(outboundFrom)
	if addr == "" {
		return "", errors.New("jobs: quote_send: outbound From address not configured (QP_OUTBOUND_FROM)")
	}
	name := strings.TrimSpace(b.FromName)
	if name == "" {
		return addr, nil
	}
	return fmt.Sprintf("%s <%s>", name, addr), nil
}

// replyHeaders turns thread_headers_json into the outbound threading pair:
// In-Reply-To anchors on the RFQ's own Message-ID (the message the quote
// answers), References chains the inbound References plus that Message-ID
// per RFC 5322. With no Message-ID the inbound pair passes through so the
// mail still threads onto whatever the customer replied to.
func replyHeaders(raw []byte) map[string]string {
	var t map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &t) != nil {
		return nil
	}
	str := func(k string) string {
		if v, ok := t[k].(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	messageID := str("message_id")
	references := strings.TrimSpace(str("references") + " " + messageID)
	inReplyTo := messageID
	if inReplyTo == "" {
		inReplyTo = str("in-reply-to")
	}
	out := map[string]string{}
	if inReplyTo != "" {
		out["In-Reply-To"] = inReplyTo
	}
	if references != "" {
		out["References"] = references
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// replySubject prefixes the RFQ subject with Re: once; an empty subject
// falls back to the quote number so the mail is never subjectless.
func replySubject(subject, quoteNumber string) string {
	s := strings.TrimSpace(subject)
	if s == "" {
		return "Quote " + quoteNumber
	}
	if len(s) >= 3 && strings.EqualFold(s[:3], "re:") {
		return s
	}
	return "Re: " + s
}

// textBody is the minimal body spec S5 asks for — the quote number is the
// reference every support conversation starts from.
func textBody(quoteNumber, fromName string) string {
	return fmt.Sprintf("Hello,\n\nPlease find attached your quotation %s.\n\nReply to this email with any questions.\n\n%s", quoteNumber, fromName)
}

// auditSend appends the tenant_audit_logs row — worker actions carry no
// actor (ActorUserID nil; a machine did this, not a user).
func auditSend(ctx context.Context, tx pgx.Tx, org uuid.UUID, action string, quoteID uuid.UUID, meta map[string]any) error {
	if _, err := platform.NewStore(tx).InsertAuditLog(ctx, platform.AuditEntry{
		OrganizationID: org,
		Action:         action,
		EntityType:     "quote",
		EntityID:       quoteID,
		Metadata:       meta,
	}); err != nil {
		return fmt.Errorf("jobs: quote_send: audit %q on %s: %w", action, quoteID, err)
	}
	return nil
}
