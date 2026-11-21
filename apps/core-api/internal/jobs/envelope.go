// Job envelope for the staged quote pipeline (spec
// docs/specs/2026-11-11-staged-quote-pipeline.md section 3, ADR-004).
//
// Every stage's args carry identifiers only — organization_id,
// correlation_id and the entity id (AGENTS.md rule 7). river_job is not
// RLS-scoped, so no email bodies, rates or customer data may ever appear in
// these structs; the arg-scan test in envelope_test.go enforces that.
//
// Kind names and queue routing follow the spec's stage table. Each args
// type implements JobArgsWithInsertOpts so every enqueue site inherits the
// correct queue and the (kind, entity id) uniqueness for free; the
// `river:"unique"` tag pins uniqueness to the entity field only, keeping
// organization_id/correlation_id out of the dedupe hash (a re-enqueue with
// a fresh correlation id still collapses onto the same job).
package jobs

import (
	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// Pipeline queue names (ADR-004: queue limits are configuration, not code —
// only the routing keys live here).
const (
	QueueIngest  = "ingest"
	QueueExtract = "extract"
	QueuePrice   = "price"
	QueueRender  = "render"
	QueueSend    = "send"
)

// entityUnique is the shared insert policy: unique on kind + the tagged
// entity field, so a double enqueue collapses (spec section 3). ByState
// restricts the dedupe window to live states — a terminal job
// (completed/cancelled/discarded) must not block the re-enqueue that the
// spec's "reclassify as RFQ" and reprocess actions issue by entity id.
// Every worker is idempotent on entity state, so a re-run is always safe.
func entityUnique(queue string) river.InsertOpts {
	return river.InsertOpts{Queue: queue, UniqueOpts: river.UniqueOpts{
		ByArgs: true,
		ByState: []rivertype.JobState{
			rivertype.JobStateAvailable,
			rivertype.JobStatePending,
			rivertype.JobStateRetryable,
			rivertype.JobStateRunning,
			rivertype.JobStateScheduled,
		},
	}}
}

// IngestArgs is S1 rfq_ingest: promote one inbound_messages receipt to an
// rfq_requests row plus raw content and stored attachments.
type IngestArgs struct {
	OrganizationID   uuid.UUID `json:"organization_id"`
	CorrelationID    uuid.UUID `json:"correlation_id"`
	InboundMessageID uuid.UUID `json:"inbound_message_id" river:"unique"`
}

// Kind returns the River job kind.
func (IngestArgs) Kind() string { return "rfq_ingest" }

// InsertOpts routes the job to the ingest queue with entity uniqueness.
func (IngestArgs) InsertOpts() river.InsertOpts { return entityUnique(QueueIngest) }

// ExtractArgs is S2 rfq_extract: classify + extract + confidence gate.
type ExtractArgs struct {
	OrganizationID uuid.UUID `json:"organization_id"`
	CorrelationID  uuid.UUID `json:"correlation_id"`
	RfqID          uuid.UUID `json:"rfq_id" river:"unique"`
}

// Kind returns the River job kind.
func (ExtractArgs) Kind() string { return "rfq_extract" }

// extractMaxAttempts pins the spec-§3 row: S2 gets three attempts,
// transient errors only — contract-violating output gives up earlier
// inside the worker (each attempt costs money).
const extractMaxAttempts = 3

// InsertOpts routes the job to the extract queue with entity uniqueness.
func (ExtractArgs) InsertOpts() river.InsertOpts {
	opts := entityUnique(QueueExtract)
	opts.MaxAttempts = extractMaxAttempts
	return opts
}

// RateArgs is S3a rfq_fetch_rates: the conditional carrier-offer fetch.
type RateArgs struct {
	OrganizationID uuid.UUID `json:"organization_id"`
	CorrelationID  uuid.UUID `json:"correlation_id"`
	RfqID          uuid.UUID `json:"rfq_id" river:"unique"`
}

// Kind returns the River job kind.
func (RateArgs) Kind() string { return "rfq_fetch_rates" }

// InsertOpts routes the job to the price queue with entity uniqueness.
func (RateArgs) InsertOpts() river.InsertOpts { return entityUnique(QueuePrice) }

// PriceArgs is S3b rfq_price: deterministic pricing.
type PriceArgs struct {
	OrganizationID uuid.UUID `json:"organization_id"`
	CorrelationID  uuid.UUID `json:"correlation_id"`
	RfqID          uuid.UUID `json:"rfq_id" river:"unique"`
}

// Kind returns the River job kind.
func (PriceArgs) Kind() string { return "rfq_price" }

// priceMaxAttempts pins the spec-§3 row: S3b gets five attempts, transient
// DB errors only — every deterministic failure cancels long before that.
const priceMaxAttempts = 5

// InsertOpts routes the job to the price queue with entity uniqueness and
// the stage's five-attempt budget.
func (PriceArgs) InsertOpts() river.InsertOpts {
	opts := entityUnique(QueuePrice)
	opts.MaxAttempts = priceMaxAttempts
	return opts
}

// RenderArgs is S4 rfq_render: the draft quote document for one quote version.
type RenderArgs struct {
	OrganizationID uuid.UUID `json:"organization_id"`
	CorrelationID  uuid.UUID `json:"correlation_id"`
	RfqID          uuid.UUID `json:"rfq_id"`
	QuoteID        uuid.UUID `json:"quote_id" river:"unique"`
}

// Kind returns the River job kind.
func (RenderArgs) Kind() string { return "rfq_render" }

// InsertOpts routes the job to the render queue with entity uniqueness.
func (RenderArgs) InsertOpts() river.InsertOpts { return entityUnique(QueueRender) }

// SendArgs is S5 quote_send: deliver one approved quote version.
type SendArgs struct {
	OrganizationID uuid.UUID `json:"organization_id"`
	CorrelationID  uuid.UUID `json:"correlation_id"`
	QuoteID        uuid.UUID `json:"quote_id" river:"unique"`
}

// Kind returns the River job kind.
func (SendArgs) Kind() string { return "quote_send" }

// sendMaxAttempts pins the spec-§3 row: S5 gets five attempts, transient
// errors only — a permanent refusal cancels long before that.
const sendMaxAttempts = 5

// InsertOpts routes the job to the send queue with entity uniqueness and
// the stage's five-attempt budget.
func (SendArgs) InsertOpts() river.InsertOpts {
	opts := entityUnique(QueueSend)
	opts.MaxAttempts = sendMaxAttempts
	return opts
}
