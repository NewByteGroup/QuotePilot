package jobs_test

// ADR-004 consequence: river_job sits outside tenant RLS, so job args must
// carry identifiers only. These tests enforce both halves — the serialized
// JSON keys are limited to ID fields, and every Go field is a uuid.UUID so
// no content type can ever be smuggled in.

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/jobs"
)

// pipelineArgs is the full envelope set from spec section 3.
func pipelineArgs() []river.JobArgs {
	return []river.JobArgs{
		jobs.IngestArgs{},
		jobs.ExtractArgs{},
		jobs.RateArgs{},
		jobs.PriceArgs{},
		jobs.RenderArgs{},
		jobs.SendArgs{},
	}
}

// allowedJSONKeys is the closed set of key names a job payload may carry:
// organization/correlation ids plus per-entity ids — nothing else.
var allowedJSONKeys = map[string]bool{
	"organization_id":    true,
	"correlation_id":     true,
	"inbound_message_id": true,
	"rfq_id":             true,
	"quote_id":           true,
}

// TestJobArgsKinds pins the spec section 3 kind strings.
func TestJobArgsKinds(t *testing.T) {
	want := map[river.JobArgs]string{
		jobs.IngestArgs{}:  "rfq_ingest",
		jobs.ExtractArgs{}: "rfq_extract",
		jobs.RateArgs{}:    "rfq_fetch_rates",
		jobs.PriceArgs{}:   "rfq_price",
		jobs.RenderArgs{}:  "rfq_render",
		jobs.SendArgs{}:    "quote_send",
	}
	for args, kind := range want {
		assert.Equal(t, kind, args.Kind())
	}
}

// TestJobArgsNoForbiddenFields is the arg-scan rule: every serialized key
// must be in the ID allowlist, and every field must be a uuid.UUID — a
// string or []byte field could carry tenant content.
func TestJobArgsNoForbiddenFields(t *testing.T) {
	for _, args := range pipelineArgs() {
		rt := reflect.TypeOf(args)
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			assert.Equal(t, reflect.TypeOf(uuid.UUID{}), f.Type,
				"%s.%s must be uuid.UUID (IDs only, ADR-004)", rt.Name(), f.Name)
		}
		raw, err := json.Marshal(args)
		require.NoError(t, err)
		var payload map[string]any
		require.NoError(t, json.Unmarshal(raw, &payload))
		for key := range payload {
			assert.True(t, allowedJSONKeys[key],
				"%s serializes forbidden key %q (tenant content must never ride job args)", rt.Name(), key)
		}
	}
}

// TestJobArgsUniqueTags pins the river:"unique" tag to exactly the entity
// field on every args struct: UniqueOpts.ByArgs hashes the tagged fields
// only, so a dropped tag silently widens the dedupe hash to every field
// (a re-enqueue with a fresh correlation_id would never collapse) and an
// extra tag narrows it. Both corrupt replay dedupe.
func TestJobArgsUniqueTags(t *testing.T) {
	want := map[reflect.Type][]string{
		reflect.TypeOf(jobs.IngestArgs{}):  {"InboundMessageID"},
		reflect.TypeOf(jobs.ExtractArgs{}): {"RfqID"},
		reflect.TypeOf(jobs.RateArgs{}):    {"RfqID"},
		reflect.TypeOf(jobs.PriceArgs{}):   {"RfqID"},
		reflect.TypeOf(jobs.RenderArgs{}):  {"QuoteID"},
		reflect.TypeOf(jobs.SendArgs{}):    {"QuoteID"},
	}
	for _, args := range pipelineArgs() {
		rt := reflect.TypeOf(args)
		var tagged []string
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			tag, ok := f.Tag.Lookup("river")
			if !ok {
				continue
			}
			assert.Equal(t, "unique", tag, "%s.%s river tag value", rt.Name(), f.Name)
			tagged = append(tagged, f.Name)
		}
		assert.Equal(t, want[rt], tagged, "%s unique-tagged fields", rt.Name())
	}
}

// TestJobArgsInsertOpts checks queue routing and the (kind, entity id)
// uniqueness contract on the two kinds this slice enqueues.
func TestJobArgsInsertOpts(t *testing.T) {
	ing := jobs.IngestArgs{
		OrganizationID:   uuid.New(),
		CorrelationID:    uuid.New(),
		InboundMessageID: uuid.New(),
	}
	opts := ing.InsertOpts()
	assert.Equal(t, jobs.QueueIngest, opts.Queue)
	assert.True(t, opts.UniqueOpts.ByArgs)

	ext := jobs.ExtractArgs{OrganizationID: uuid.New(), CorrelationID: uuid.New(), RfqID: uuid.New()}
	opts = ext.InsertOpts()
	assert.Equal(t, jobs.QueueExtract, opts.Queue)
	assert.True(t, opts.UniqueOpts.ByArgs)
}

// TestJobArgsUniqueStates pins the dedupe window to live states only:
// terminal jobs (completed/cancelled/discarded) must not block a re-enqueue
// — the spec's "reclassify as RFQ" action inserts rfq_extract by rfq_id
// after its first job finished, and every worker is idempotent on entity
// state so a re-run is safe.
func TestJobArgsUniqueStates(t *testing.T) {
	live := []rivertype.JobState{
		rivertype.JobStateAvailable,
		rivertype.JobStatePending,
		rivertype.JobStateRetryable,
		rivertype.JobStateRunning,
		rivertype.JobStateScheduled,
	}
	for _, args := range pipelineArgs() {
		withOpts, ok := args.(river.JobArgsWithInsertOpts)
		require.True(t, ok, "%T must implement JobArgsWithInsertOpts", args)
		states := withOpts.InsertOpts().UniqueOpts.ByState
		assert.ElementsMatch(t, live, states, "%T unique states", args)
		for _, terminal := range []rivertype.JobState{
			rivertype.JobStateCompleted,
			rivertype.JobStateCancelled,
			rivertype.JobStateDiscarded,
		} {
			assert.NotContains(t, states, terminal,
				"%T dedupe must not cover terminal jobs (reclassify needs re-enqueue)", args)
		}
	}
}
