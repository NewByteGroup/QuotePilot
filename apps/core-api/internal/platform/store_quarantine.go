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
