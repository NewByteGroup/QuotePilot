// Package inbox ingests Resend inbound webhooks: Svix-verified POSTs are
// resolved to their tenant through resolve_inbound_org and become
// idempotent app.inbound_messages rows inside the tenant transaction.
package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/inbox/inboxdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
)

// maxWebhookBody caps the request body; webhooks carry metadata only and a
// bigger body is an attack or a bug, not a legitimate payload.
const maxWebhookBody = 1 << 20 // 1 MiB

// maxRecipients bounds the per-recipient resolution loop: each entry costs
// one platform call, so a signed pathological body must not turn into tens
// of thousands of sequential queries.
const maxRecipients = 50

// abortDeliveryHeader is the response header Svix documents for telling the
// sender to stop retrying a permanently undeliverable message (ADR-005
// section 7). Header name, then its only value.
const (
	abortDeliveryHeader = "webhook-delivery"
	abortDeliveryValue  = "abort-message"
)

// OrgResolver is the seam through which the handler asks the platform path
// who owns an inbound recipient — platform.Store in production, a fake in
// tests. A miss is ok=false, never an error.
type OrgResolver interface {
	ResolveInboundOrg(ctx context.Context, recipient string) (uuid.UUID, bool, error)
}

// Server serves the inbox HTTP surface on a stdlib ServeMux (ADR-002: no router).
type Server struct {
	mux      *http.ServeMux
	runner   tenantdb.TenantRunner
	resolver OrgResolver
	verifier SignatureVerifier
	logger   *slog.Logger
}

// NewServer wires the inbox endpoints. Dependencies are injected so tests can
// supply a fake verifier, resolver or runner.
//
//	http.ListenAndServe(addr, inbox.NewServer(db, store, verifier, logger))
func NewServer(runner tenantdb.TenantRunner, resolver OrgResolver, verifier SignatureVerifier, logger *slog.Logger) *Server {
	s := &Server{mux: http.NewServeMux(), runner: runner, resolver: resolver, verifier: verifier, logger: logger}
	s.mux.HandleFunc("POST /webhooks/resend/inbound", s.handleResendInbound)
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// The webhook body decodes into authapi.ResendEmailReceived — Resend's
// documented email.received envelope, fixed in
// packages/contracts/openapi/core-api.yaml (schemas only: the route stays
// hand-mounted because Svix verifies the RAW body before any binding). The
// tenant never arrives in it: `data.to` is resolved through
// resolve_inbound_org (ADR-005 section 7), so a signed or forged caller
// cannot pick an organization. MVP gaps tracked in docs/adr-rfc-plan.md
// under RFC-002: unmatched and multi-org messages are logged, not yet
// persisted in a platform quarantine/counter; body, headers and attachment
// bytes are fetched through the Receiving/Attachments APIs by the ingest
// job (the webhook is metadata only). `data.created_at` persists as
// inbound_messages.received_at — the audit trail records Resend's receipt
// time, not our processing time.

// handleResendInbound runs the ADR-005 section 7 pipeline: verify, resolve
// tenants from the recipients, then either store under the single matched
// org or acknowledge-and-drop.
func (s *Server) handleResendInbound(w http.ResponseWriter, r *http.Request) {
	payload, ok := s.verifiedPayload(w, r)
	if !ok {
		return
	}
	orgs, err := s.resolveOrgs(r.Context(), payload.Data.To)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "inbound tenant resolution failed",
			"correlation_id", payload.Data.EmailID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(orgs) != 1 {
		s.drop(w, r, payload, orgs)
		return
	}
	s.store(w, r, payload, orgs[0])
}

// verifiedPayload is the unauthenticated front of the pipeline: body cap,
// Svix signature before anything else (an unauthenticated caller must not
// reach the database — gate criterion 5), then body validation. false means
// the denial is already written.
func (s *Server) verifiedPayload(w http.ResponseWriter, r *http.Request) (authapi.ResendEmailReceived, bool) {
	body, ok := readBody(w, r)
	if !ok {
		return authapi.ResendEmailReceived{}, false
	}
	if err := s.verifier.Verify(body, r.Header); err != nil {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return authapi.ResendEmailReceived{}, false
	}
	payload, err := parseInboundPayload(body)
	if err != nil {
		// Every validation failure is permanent for THIS body — retries
		// replay it unchanged, so abort instead of burning Svix's ~27h
		// retry window (a misconfigured endpoint is the expected case).
		w.Header().Set(abortDeliveryHeader, abortDeliveryValue)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return authapi.ResendEmailReceived{}, false
	}
	return payload, true
}

// readBody caps the request at 1 MiB — an unauthenticated caller never
// gets to stream unbounded bytes into memory. false means the 400 is
// already written.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "unreadable or oversized body (max 1 MiB)", http.StatusBadRequest)
		return nil, false
	}
	return body, true
}

// resolveOrgs maps every recipient through the platform function and
// dedupes the organizations: a message addressed to two tenants yields two
// orgs — and that message must not be guessed (ADR-005 section 7).
func (s *Server) resolveOrgs(ctx context.Context, recipients []string) ([]uuid.UUID, error) {
	seen := map[uuid.UUID]struct{}{}
	orgs := []uuid.UUID{}
	for _, to := range recipients {
		org, ok, err := s.resolver.ResolveInboundOrg(ctx, to)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[org]; ok && !dup {
			seen[org] = struct{}{}
			orgs = append(orgs, org)
		}
	}
	return orgs, nil
}

// drop acknowledges delivery without storing: a message that resolves to
// no tenant, or to several, is undeliverable here — a non-2xx would make
// Svix replay it eight times over ~27h for nothing, so the documented
// abort-message response header marks it dead at once (ADR-005 section 7).
// Recipients and match count are logged for ops; the quarantine row and
// unmatched counter the ADR reserves need schema and are tracked under
// RFC-002.
func (s *Server) drop(w http.ResponseWriter, r *http.Request, p authapi.ResendEmailReceived, orgs []uuid.UUID) {
	s.logger.WarnContext(r.Context(), "inbound message dropped — no unique tenant match",
		"correlation_id", p.Data.EmailID,
		"recipients", p.Data.To,
		"matched_orgs", len(orgs))
	w.Header().Set(abortDeliveryHeader, abortDeliveryValue)
	s.writeResult(w, r, p, `{"inserted":false,"dropped":true}`)
}

// store performs the idempotent insert inside the tenant transaction and
// answers the result envelope.
func (s *Server) store(w http.ResponseWriter, r *http.Request, p authapi.ResendEmailReceived, org uuid.UUID) {
	inserted, err := s.storeInbound(r.Context(), org, p)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "inbound store failed",
			"correlation_id", p.Data.EmailID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.writeResult(w, r, p, fmt.Sprintf(`{"inserted":%t}`, inserted))
}

// writeResult emits the JSON result envelope; a write failure is logged,
// never retried — the delivery is already decided.
func (s *Server) writeResult(w http.ResponseWriter, r *http.Request, p authapi.ResendEmailReceived, body string) {
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte(body)); err != nil {
		s.logger.ErrorContext(r.Context(), "write response failed",
			"correlation_id", p.Data.EmailID, "error", err)
	}
}

// parseInboundPayload validates the decoded body; every failure maps to a
// 400, never a DB call.
func parseInboundPayload(body []byte) (authapi.ResendEmailReceived, error) {
	var p authapi.ResendEmailReceived
	if err := json.Unmarshal(body, &p); err != nil {
		return p, fmt.Errorf("invalid json payload: %w", err)
	}
	return p, validateInboundPayload(p)
}

// validateInboundPayload enforces the fields the handler depends on: the
// email.received discriminator (anything else means a misconfigured
// endpoint, so 400 rather than a silent store or a retry loop), at least
// one recipient to resolve (bounded by maxRecipients), an email_id to
// dedupe on, and created_at for the received_at audit column. Other
// schema-required fields (from, subject) are tolerated absent — the
// handler never reads them.
func validateInboundPayload(p authapi.ResendEmailReceived) error {
	if p.Type != authapi.EmailReceived {
		return fmt.Errorf("type must be %q, got %q", authapi.EmailReceived, p.Type)
	}
	if p.Data.CreatedAt.IsZero() {
		return fmt.Errorf("data.created_at got %v, expected a non-empty RFC 3339 timestamp", p.Data.CreatedAt)
	}
	if err := validateRecipients(p.Data.To); err != nil {
		return err
	}
	if p.Data.EmailID == "" {
		return errors.New("data.email_id must be a non-empty string")
	}
	return nil
}

// validateRecipients enforces the recipient list's shape: at least one
// address to resolve, bounded so a hostile or misconfigured sender cannot
// fan the resolver out, and no blank entries (each is a resolver lookup).
func validateRecipients(to []string) error {
	if len(to) == 0 {
		return fmt.Errorf("data.to must list at least one recipient address, got %v", to)
	}
	if len(to) > maxRecipients {
		return fmt.Errorf("data.to lists %d recipients, max %d", len(to), maxRecipients)
	}
	for _, addr := range to {
		if strings.TrimSpace(addr) == "" {
			return fmt.Errorf("data.to entries must be non-empty addresses, got %q", addr)
		}
	}
	return nil
}

// storeInbound performs the idempotent insert inside the tenant transaction.
// The unique key (organization_id, resend_message_id) makes webhook replays
// free; rows=0 means a replay, rows=1 a first insert.
func (s *Server) storeInbound(ctx context.Context, org uuid.UUID, p authapi.ResendEmailReceived) (bool, error) {
	var rows int64
	err := s.runner.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = inboxdb.New(tx).InsertInboundMessage(ctx, inboundInsertParams(org, p))
		return err
	})
	if err != nil {
		return false, fmt.Errorf("inbox: store inbound message %q: %w", p.Data.EmailID, err)
	}
	return rows == 1, nil
}

// inboundInsertParams maps the webhook payload to the row: email_id is the
// idempotency key, created_at is Resend's receipt timestamp (the audit
// trail orders on when the RFQ arrived, not our processing time), and the
// declared value stays zero — the Resend payload carries none, so the
// NOT NULL DEFAULT 0 column gets an explicit zero until the data-model
// change decides where the value comes from (RFC-002).
func inboundInsertParams(org uuid.UUID, p authapi.ResendEmailReceived) inboxdb.InsertInboundMessageParams {
	return inboxdb.InsertInboundMessageParams{
		OrganizationID:   pgtype.UUID{Bytes: org, Valid: true},
		ResendMessageID:  p.Data.EmailID,
		Subject:          p.Data.Subject,
		ReceivedAt:       pgtype.Timestamptz{Time: p.Data.CreatedAt, Valid: true},
		DeclaredValueUsd: decimal.Zero,
	}
}
