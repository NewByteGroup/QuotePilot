// Package aiengine is the core → ai-engine seam (ADR-001/ADR-006): a
// narrow interface, its HTTP adapter and the contract validation core-api
// runs on the untrusted responses before persisting them (AGENTS.md rule 6).
//
// ai-engine never receives organization_id — requests carry correlation_id
// plus tenant content only (rule 2). The transport contract is PROVISIONAL
// pending the ai-engine implementation: everything lives behind Client so
// only the HTTP adapter changes when the final API lands.
package aiengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
)

// Client is the ai-engine surface the pipeline workers use. Tests bind a
// named fake (repo rule: named fakes, not inline stubs).
type Client interface {
	// Classify runs stage-2 routing on one inbound message.
	Classify(ctx context.Context, req ClassifyRequest) (*ClassifyResult, error)
	// Extract runs stage-3 structured extraction; the raw JSON body is
	// returned unvalidated — callers must run ValidateExtraction before
	// persisting it (rule 6).
	Extract(ctx context.Context, req ExtractRequest) (json.RawMessage, error)
	// ParseAttachment converts one attachment's bytes to markdown.
	ParseAttachment(ctx context.Context, req ParseAttachmentRequest) (string, error)
}

// AttachmentMeta is the filename/content_type pair classify receives.
type AttachmentMeta struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
}

// ClassifyRequest is POST /v1/classify (provisional transport, confirmed
// by the engine owner). There is no body_html: raw markup never crosses
// the wire — core reduces HTML-only emails to text before truncating.
type ClassifyRequest struct {
	CorrelationID   uuid.UUID        `json:"correlation_id"`
	Subject         string           `json:"subject"`
	Sender          string           `json:"sender"`
	BodyText        string           `json:"body_text"`
	AttachmentMetas []AttachmentMeta `json:"attachment_metas"`
}

// ClassifyResult is the classifier's routing answer: a label mirroring
// app.rfq_kind plus the calibrated probabilities that justify it (they go
// into the audit row — the reason a human later sees).
type ClassifyResult struct {
	Label         string             `json:"label"`
	Probabilities map[string]float64 `json:"probabilities"`
	Model         string             `json:"model"`
}

// ParsedMarkdown is one attachment's parse result on the extract wire —
// the real filename rides along so evidence can cite ATTACHMENT:<filename>.
// Only rows whose parse succeeded (parsed_markdown IS NOT NULL) are sent.
type ParsedMarkdown struct {
	Filename string `json:"filename"`
	Markdown string `json:"markdown"`
}

// ExtractRequest is POST /v1/extract: the single textual body (HTML-only
// emails arrive already reduced to text, like classify), the parsed
// markdown of every attachment that yielded one, and the classification
// the engine echoes back in the ExtractionResult — a second classify call
// would waste money and could disagree.
type ExtractRequest struct {
	CorrelationID   uuid.UUID        `json:"correlation_id"`
	Subject         string           `json:"subject"`
	Sender          string           `json:"sender"`
	BodyText        string           `json:"body_text"`
	ParsedMarkdowns []ParsedMarkdown `json:"parsed_markdown"`
	Classification  ClassifyResult   `json:"classification"`
}

// ParseAttachmentRequest is POST /v1/attachments/parse; the bytes travel
// base64 inside JSON (provisional transport). Bytes is excluded from JSON
// tags on purpose — the adapter encodes it explicitly.
type ParseAttachmentRequest struct {
	CorrelationID uuid.UUID `json:"correlation_id"`
	Filename      string    `json:"filename"`
	ContentType   string    `json:"content_type"`
	Bytes         []byte    `json:"-"`
}

// CarrierOfferSearchRequest is POST /v1/carrier-offers/search — the S3a
// normalized request (spec S3a). PROVISIONAL route, pending engine-owner
// alignment — the same process classify/extract/parse went through. The
// request carries correlation_id plus the normalized shipment slice only;
// organization_id never crosses the wire (rule 2).
type CarrierOfferSearchRequest struct {
	CorrelationID   uuid.UUID `json:"correlation_id"`
	OriginLOCODE    string    `json:"origin_locode"`
	DestLOCODE      string    `json:"dest_locode"`
	EquipmentOrUnit string    `json:"equipment_or_unit"`
	Quantity        int       `json:"quantity"`
	ReadyDate       string    `json:"ready_date"`
}

// CarrierOfferSearchResult is the decoded 200 body of the offer search:
// offer is the raw payload (pricing.CarrierOffer minus id — validated by
// the caller before persisting, rule 6); a JSON null (or absent key) means
// the carrier answered with nothing usable — NO_OFFER. offer_reference is
// the carrier-side id when one was issued.
type CarrierOfferSearchResult struct {
	Offer          json.RawMessage `json:"offer"`
	OfferReference *string         `json:"offer_reference"`
}

// EngineError reports a failed ai-engine call: an HTTP status (when the
// engine answered), a transport failure (StatusCode 0) or a
// contract-violating response body (Contract=true). The classification
// rule mirrors spec section 3: transient retries, violations at most once.
type EngineError struct {
	Op         string // operation: "classify" | "extract" | "parse_attachment"
	StatusCode int    // HTTP status when the engine answered; 0 otherwise
	Detail     string
	Contract   bool // the response violated the contract (never transient)
}

// Error implements error.
func (e *EngineError) Error() string {
	if e.StatusCode == 0 {
		return fmt.Sprintf("aiengine: %s: %s", e.Op, e.Detail)
	}
	return fmt.Sprintf("aiengine: %s: status %d: %s", e.Op, e.StatusCode, e.Detail)
}

// Transient reports whether retrying may succeed: transport errors, 408
// (request timeout), 425 (too early), 429 (rate limited) and 5xx. Contract
// violations are never transient.
func (e *EngineError) Transient() bool {
	if e.Contract {
		return false
	}
	switch e.StatusCode {
	case 0:
		return true
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	}
	return e.StatusCode >= 500
}

// IsTransient unwraps err and reports whether the ai-engine call is worth
// retrying. An untyped error (a raw transport failure that escaped the
// adapter) stays transient — the safe default for a retryable worker.
func IsTransient(err error) bool {
	var t interface{ Transient() bool }
	if errors.As(err, &t) {
		return t.Transient()
	}
	return true
}

// IsContractViolation unwraps err and reports whether the engine answered
// with something core must never persist — a non-transient 4xx or a
// schema-invalid body.
func IsContractViolation(err error) bool {
	var e *EngineError
	return errors.As(err, &e) && e.Contract
}

// UnconfiguredClient is the fail-soft binding used when AI_ENGINE_URL is
// not set (mirrors resend.UnconfiguredReceiver): calls fail loudly as
// contract violations so a missing env never looks transient — nothing
// silently waits or retries forever on a credential that cannot appear.
type UnconfiguredClient struct{}

// Classify fails loudly: the engine is not configured.
func (UnconfiguredClient) Classify(context.Context, ClassifyRequest) (*ClassifyResult, error) {
	return nil, &EngineError{Op: "classify", Contract: true, Detail: "AI_ENGINE_URL is not configured"}
}

// Extract fails loudly: the engine is not configured.
func (UnconfiguredClient) Extract(context.Context, ExtractRequest) (json.RawMessage, error) {
	return nil, &EngineError{Op: "extract", Contract: true, Detail: "AI_ENGINE_URL is not configured"}
}

// ParseAttachment fails loudly: the engine is not configured.
func (UnconfiguredClient) ParseAttachment(context.Context, ParseAttachmentRequest) (string, error) {
	return "", &EngineError{Op: "parse_attachment", Contract: true, Detail: "AI_ENGINE_URL is not configured"}
}

// SearchCarrierOffer fails loudly: the engine is not configured.
func (UnconfiguredClient) SearchCarrierOffer(context.Context, CarrierOfferSearchRequest) (*CarrierOfferSearchResult, error) {
	return nil, &EngineError{Op: "carrier_offer_search", Contract: true, Detail: "AI_ENGINE_URL is not configured"}
}
