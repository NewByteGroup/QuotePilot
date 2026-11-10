// Package inbox ingests Resend inbound webhooks: Svix-verified POSTs become
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

// Server serves the inbox HTTP surface on a stdlib ServeMux (ADR-002: no router).
type Server struct {
	mux      *http.ServeMux
	runner   tenantdb.TenantRunner
	verifier SignatureVerifier
	logger   *slog.Logger
}

// NewServer wires the inbox endpoints. Dependencies are injected so tests can
// supply a fake verifier or runner.
//
//	http.ListenAndServe(addr, inbox.NewServer(db, verifier, logger))
func NewServer(runner tenantdb.TenantRunner, verifier SignatureVerifier, logger *slog.Logger) *Server {
	s := &Server{mux: http.NewServeMux(), runner: runner, verifier: verifier, logger: logger}
	s.mux.HandleFunc("POST /webhooks/resend/inbound", s.handleResendInbound)
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// inboundPayload is the spike's minimal webhook body. The real contract comes
// from packages/contracts later; this exists to prove the verify→tx→insert
// path. Spike-only shortcut: organization_id rides in the payload — production
// resolves the tenant through the ADR-005 platform path (alias lookup), never
// trusting caller-supplied tenant ids.
type inboundPayload struct {
	OrganizationID   uuid.UUID       `json:"organization_id"`
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
	inserted, err := s.storeInbound(r.Context(), payload)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "inbound store failed",
			"correlation_id", payload.ResendMessageID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := fmt.Fprintf(w, `{"inserted":%t}`, inserted); err != nil {
		s.logger.ErrorContext(r.Context(), "write response failed",
			"correlation_id", payload.ResendMessageID, "error", err)
	}
}

// parseInboundPayload validates the decoded body: a nil org or empty message
// id is a 400, never a DB call.
func parseInboundPayload(body []byte) (inboundPayload, error) {
	var p inboundPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return p, fmt.Errorf("invalid json payload: %w", err)
	}
	if p.OrganizationID == uuid.Nil {
		return p, fmt.Errorf("organization_id must be a non-nil uuid, got %q", p.OrganizationID)
	}
	if p.ResendMessageID == "" {
		return p, errors.New("resend_message_id must be a non-empty string")
	}
	if p.DeclaredValueUSD.IsNegative() {
		return p, fmt.Errorf("declared_value_usd must be >= 0, got %s", p.DeclaredValueUSD)
	}
	return p, nil
}

// storeInbound performs the idempotent insert inside the tenant transaction.
// The unique key (organization_id, resend_message_id) makes webhook replays
// free; rows=0 means a replay, rows=1 a first insert.
func (s *Server) storeInbound(ctx context.Context, p inboundPayload) (bool, error) {
	var rows int64
	err := s.runner.InTenantTx(ctx, p.OrganizationID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = inboxdb.New(tx).InsertInboundMessage(ctx, inboxdb.InsertInboundMessageParams{
			OrganizationID:   pgtype.UUID{Bytes: p.OrganizationID, Valid: true},
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
