// Package carrier is the S3a carrier-offer seam (spec
// 2026-11-11-staged-quote-pipeline S3a): the connector interface the
// rfq_fetch_rates worker depends on — owned by core, so the worker never
// names a carrier client — plus the normalized-request digest
// (request_hash), the sanity bounds run on the untrusted payload before
// anything persists (AGENTS.md rule 6), and the two bindings: LIVE over
// the ai-engine transport and the deterministic MIRROR demo fetcher
// (RFC-003 — never bound in production; the APP_ENV guard refuses it).
package carrier

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
)

// Checkpoint-source vocabulary persisted on carrier_offers.source
// (migration 0010's CHECK): which connector produced the row. The UI
// labels MIRROR rows simulated by reading this column — never by looking
// at the environment (RFC-003).
const (
	SourceLive   = "LIVE"
	SourceMirror = "MIRROR"
)

// isoDate is the ready_date contract format (IsoDate) — the same layout
// the extraction contract and the engine's validity windows share.
const isoDate = "2006-01-02"

// Request is the normalized carrier-offer request the worker builds from
// the validated extraction. CorrelationID rides the wire for tracing but
// is deliberately excluded from RequestHash — the digest identifies the
// request CONTENT so a re-run collapses onto the same checkpoint row.
type Request struct {
	CorrelationID   uuid.UUID
	OriginLOCODE    string
	DestLOCODE      string
	EquipmentOrUnit string
	Quantity        int
	ReadyDate       time.Time // effective quote date: extracted ready_date, else the RFQ's received_at day
}

// Result is a fetcher's answer: Offer is the raw payload bytes
// (pricing.CarrierOffer minus id — JSON null or empty means the carrier
// answered with nothing usable). The worker validates Offer through
// ValidateOffer before persisting, whatever binding produced it.
type Result struct {
	Offer          json.RawMessage
	OfferReference *string
}

// Fetcher is the connector interface the S3a worker binds (spec S3a: the
// interface is owned by core — LIVE, the Playwright portal and the mirror
// are bindings behind it; which one is bound is configuration).
type Fetcher interface {
	// Carrier is the checkpoint row's carrier column: the queried
	// carrier's stable name ("CMA_CGM" for the live binding). It is part
	// of the idempotency key, so it must be present on every outcome row —
	// including NO_OFFER and FETCH_FAILED, which carry no offer content.
	Carrier() string
	// Source is the checkpoint row's source column (LIVE or MIRROR).
	Source() string
	// FetchOffer asks the connector for one offer for the normalized
	// request. Errors propagate as fetch failures; a nil Offer on the
	// Result means NO_OFFER.
	FetchOffer(ctx context.Context, req Request) (*Result, error)
}

// OfferPayload is the persisted offer shape (spec S3a + internal/pricing
// CarrierOffer minus id — loadLatestOffer supplies the row id at read
// time). The same struct decodes the wire payload ValidateOffer bounds.
type OfferPayload struct {
	Carrier   string                `json:"carrier"`
	Charges   []pricing.OfferCharge `json:"charges"`
	ExpiresAt time.Time             `json:"expires_at"`
}

// IsNoOfferPayload reports whether the raw offer key is absent, empty or
// a JSON null — the wire's NO_OFFER signal.
func IsNoOfferPayload(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// requestHashDoc is the canonical-JSON document request_hash digests
// (spec S3a + migration 0010): route, equipment, container count and the
// quote date — no tenant content, and it never enters job args. Struct
// field order fixes the key order, so the marshaled bytes are stable.
type requestHashDoc struct {
	OriginLOCODE    string `json:"origin_locode"`
	DestLOCODE      string `json:"dest_locode"`
	EquipmentOrUnit string `json:"equipment_or_unit"`
	Quantity        int    `json:"quantity"`
	ReadyDate       string `json:"ready_date"`
}

// RequestHash is the idempotency half of the checkpoint's unique key:
// sha256 hex over the canonical JSON of the normalized request. A repeated
// fetch for the same request collapses onto the same row; a re-extraction
// that changes route/equipment/count/date yields a new hash and a new
// offer. CorrelationID is excluded on purpose — it differs per run.
func RequestHash(req Request) string {
	// Scalar fields only — Marshal cannot fail.
	raw, _ := json.Marshal(requestHashDoc{
		OriginLOCODE:    req.OriginLOCODE,
		DestLOCODE:      req.DestLOCODE,
		EquipmentOrUnit: req.EquipmentOrUnit,
		Quantity:        req.Quantity,
		ReadyDate:       req.ReadyDate.Format(isoDate),
	})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
