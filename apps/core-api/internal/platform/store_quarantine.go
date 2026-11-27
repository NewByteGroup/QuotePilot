package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// InboundQuarantineDrop is one recorded inbound drop as
// app.record_inbound_quarantine persists it (migration 0016): the Resend
// message id is the dedupe key (a lost 2xx makes Svix redeliver, and the
// redelivery must not double-row or double-count), Reason is 'no_match' or
// 'multi_match', Recipients carries the `to` array exactly as received,
// MatchedOrgIDs is empty for no_match and the candidates for multi_match,
// and ReceivedAt is Resend's receipt time (envelope data.created_at).
type InboundQuarantineDrop struct {
	ResendMessageID string
	Reason          string
	Recipients      []string
	MatchedOrgIDs   []uuid.UUID
	Subject         string
	FromAddr        string
	ReceivedAt      time.Time
}

// RecordInboundQuarantine persists one dropped inbound through
// app.record_inbound_quarantine (migration 0016, ADR-005 section 7): a
// message whose recipients resolved to no unique tenant is undeliverable,
// and the quarantine row plus the inbound_unmatched_total counter are the
// audit trail the drop used to lack. recorded=false means the
// resend_message_id was already quarantined — a Svix redelivery, which is
// the expected replay and not an error. The caller answers the webhook the
// same either way; only a driver failure propagates.
//
//	recorded, err := store.RecordInboundQuarantine(ctx, platform.InboundQuarantineDrop{...})
func (s *Store) RecordInboundQuarantine(ctx context.Context, d InboundQuarantineDrop) (bool, error) {
	recipients, err := json.Marshal(d.Recipients)
	if err != nil {
		return false, fmt.Errorf("platform: record_inbound_quarantine recipients: %w", err)
	}
	matched := make([]string, 0, len(d.MatchedOrgIDs))
	for _, id := range d.MatchedOrgIDs {
		matched = append(matched, id.String())
	}
	var recorded bool
	err = s.q.QueryRow(ctx,
		`SELECT app.record_inbound_quarantine($1, $2, $3, $4, $5, $6, $7)`,
		d.ResendMessageID, d.Reason, recipients, matched, d.Subject, d.FromAddr, d.ReceivedAt).
		Scan(&recorded)
	if err != nil {
		return false, fmt.Errorf("platform: record_inbound_quarantine: %w", err)
	}
	return recorded, nil
}

// InboundQuarantineRow is one page row as app.list_inbound_quarantine
// returns it (migration 0018): the auditable columns verbatim plus the
// page-window total the function computes over the whole table. Recipients
// is the decoded `to` array (jsonb); MatchedOrgIDs is empty for no_match.
type InboundQuarantineRow struct {
	ID              uuid.UUID
	ResendMessageID string
	Reason          string
	Recipients      []string
	MatchedOrgIDs   []uuid.UUID
	Subject         string
	FromAddr        string
	ReceivedAt      time.Time
	QuarantinedAt   time.Time
}

// ListInboundQuarantine reads one page through app.list_inbound_quarantine
// — the only SELECT into app.inbound_quarantine qp_app holds, behind the
// RequirePlatformAdmin gate (the function is the table's whole read
// surface; migration 0018 explains the accepted residual). Page bounds are
// clamped in the function; the handler bounds the wire parameter too.
// total is the full quarantine size — it rides every row's window count,
// so an empty page reports 0 rows AND total 0.
//
//	rows, total, err := store.ListInboundQuarantine(ctx, 50, 0)
func (s *Store) ListInboundQuarantine(ctx context.Context, limit, offset int) ([]InboundQuarantineRow, int64, error) {
	rows, err := s.q.Query(ctx, `
		SELECT id, resend_message_id, reason, recipients, matched_org_ids,
		       subject, from_addr, received_at, quarantined_at, total
		FROM app.list_inbound_quarantine($1, $2)`, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("platform: list_inbound_quarantine: %w", err)
	}
	defer rows.Close()
	out := []InboundQuarantineRow{}
	var total int64
	for rows.Next() {
		var r InboundQuarantineRow
		var raw []byte
		if err := rows.Scan(&r.ID, &r.ResendMessageID, &r.Reason, &raw,
			&r.MatchedOrgIDs, &r.Subject, &r.FromAddr,
			&r.ReceivedAt, &r.QuarantinedAt, &total); err != nil {
			return nil, 0, fmt.Errorf("platform: list_inbound_quarantine scan: %w", err)
		}
		if err := json.Unmarshal(raw, &r.Recipients); err != nil {
			return nil, 0, fmt.Errorf("platform: list_inbound_quarantine recipients: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("platform: list_inbound_quarantine rows: %w", err)
	}
	return out, total, nil
}
