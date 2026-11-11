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

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/inbox/inboxdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
)

// maxWebhookBody caps the request body; webhooks carry metadata only and a
// bigger body is an attack or a bug, not a legitimate payload.
const maxWebhookBody = 1 << 20 // 1 MiB

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

// inboundPayload is the webhook body. The tenant never arrives in it: `to`
// is resolved through resolve_inbound_org (ADR-005 section 7), so a signed
// or forged caller cannot pick an organization. The real Resend
// email.received contract comes from packages/contracts later; this minimal
// shape proves verify→resolve→tx→insert. MVP gaps tracked in
// docs/adr-rfc-plan.md under RFC-002: unmatched and multi-org messages are
// logged, not yet persisted in a platform quarantine/counter.
type inboundPayload struct {
	To               []string        `json:"to"`
	ResendMessageID  string          `json:"resend_message_id"`
	Subject          string          `json:"subject"`
	DeclaredValueUSD decimal.Decimal `json:"declared_value_usd"`
}

// handleResendInbound verifies the signature before anything else: an
// unauthenticated caller must not reach the database (gate criterion 5).
func (s *Server) handleResendInbound(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "unreadable or oversized body (max 1 MiB)", http.StatusBadRequest)
		return
	}
	if err := s.verifier.Verify(body, r.Header); err != nil {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	payload, err := parseInboundPayload(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	orgs, err := s.resolveOrgs(r.Context(), payload.To)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "inbound tenant resolution failed",
			"correlation_id", payload.ResendMessageID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(orgs) != 1 {
		s.drop(w, r, payload, orgs)
		return
	}
	s.store(w, r, payload, orgs[0])
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
func (s *Server) drop(w http.ResponseWriter, r *http.Request, p inboundPayload, orgs []uuid.UUID) {
	s.logger.WarnContext(r.Context(), "inbound message dropped — no unique tenant match",
		"correlation_id", p.ResendMessageID,
		"recipients", p.To,
		"matched_orgs", len(orgs))
	w.Header().Set("webhook-delivery", "abort-message")
	s.writeResult(w, r, p, `{"inserted":false,"dropped":true}`)
}

// store performs the idempotent insert inside the tenant transaction and
// answers the result envelope.
func (s *Server) store(w http.ResponseWriter, r *http.Request, p inboundPayload, org uuid.UUID) {
	inserted, err := s.storeInbound(r.Context(), org, p)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "inbound store failed",
			"correlation_id", p.ResendMessageID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.writeResult(w, r, p, fmt.Sprintf(`{"inserted":%t}`, inserted))
}

// writeResult emits the JSON result envelope; a write failure is logged,
// never retried — the delivery is already decided.
func (s *Server) writeResult(w http.ResponseWriter, r *http.Request, p inboundPayload, body string) {
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte(body)); err != nil {
		s.logger.ErrorContext(r.Context(), "write response failed",
			"correlation_id", p.ResendMessageID, "error", err)
	}
}

// parseInboundPayload validates the decoded body; every failure maps to a
// 400, never a DB call.
func parseInboundPayload(body []byte) (inboundPayload, error) {
	var p inboundPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return p, fmt.Errorf("invalid json payload: %w", err)
	}
	return p, p.validate()
}

// validate enforces the fields the handler depends on: at least one
// recipient to resolve, a message id to dedupe on, a non-negative amount.
func (p inboundPayload) validate() error {
	if len(p.To) == 0 {
		return errors.New("to must list at least one recipient address")
	}
	for _, to := range p.To {
		if strings.TrimSpace(to) == "" {
			return fmt.Errorf("to entries must be non-empty addresses, got %q", to)
		}
	}
	if p.ResendMessageID == "" {
		return errors.New("resend_message_id must be a non-empty string")
	}
	if p.DeclaredValueUSD.IsNegative() {
		return fmt.Errorf("declared_value_usd must be >= 0, got %s", p.DeclaredValueUSD)
	}
	return nil
}

// storeInbound performs the idempotent insert inside the tenant transaction.
// The unique key (organization_id, resend_message_id) makes webhook replays
// free; rows=0 means a replay, rows=1 a first insert.
func (s *Server) storeInbound(ctx context.Context, org uuid.UUID, p inboundPayload) (bool, error) {
	var rows int64
	err := s.runner.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = inboxdb.New(tx).InsertInboundMessage(ctx, inboxdb.InsertInboundMessageParams{
			OrganizationID:   pgtype.UUID{Bytes: org, Valid: true},
			ResendMessageID:  p.ResendMessageID,
			Subject:          p.Subject,
			DeclaredValueUsd: p.DeclaredValueUSD,
		})
		return err
	})
	if err != nil {
		return false, fmt.Errorf("inbox: store inbound message %q: %w", p.ResendMessageID, err)
	}
	return rows == 1, nil
}
