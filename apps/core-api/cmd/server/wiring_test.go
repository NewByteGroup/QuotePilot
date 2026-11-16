package main

// Unit tests for the run() wiring seams the S1 slice added: the
// enqueueIngest adapter, the receiver binding and the storage binding.
// enqueue_test.go proves the auth adapter's transaction contract end to
// end on real Postgres; these pin the S1 pieces at the seam — a fake
// JobInserter suffices because InsertTx itself is proven there and in
// inbox's webhook-chain test.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/jobs"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/resend"
)

// spyInserter records what InsertTx was asked to stage; fail makes the
// insert error so the adapter's wrap is observable.
type spyInserter struct {
	calls int
	args  river.JobArgs
	fail  error
}

func (s *spyInserter) InsertTx(_ context.Context, _ pgx.Tx, args river.JobArgs, _ *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	s.calls++
	s.args = args
	return nil, s.fail
}

// TestEnqueueIngestStagesJob pins the adapter contract: the webhook's three
// ids land in an IngestArgs — queue and uniqueness come from its
// InsertOpts, so the adapter passes no explicit opts.
func TestEnqueueIngestStagesJob(t *testing.T) {
	sp := &spyInserter{}
	org, corr, msg := uuid.New(), uuid.New(), uuid.New()
	require.NoError(t, enqueueIngest(sp)(context.Background(), nil, org, corr, msg))
	require.Equal(t, 1, sp.calls)
	got, ok := sp.args.(jobs.IngestArgs)
	require.True(t, ok, "the adapter must stage IngestArgs, got %T", sp.args)
	assert.Equal(t, org, got.OrganizationID)
	assert.Equal(t, corr, got.CorrelationID)
	assert.Equal(t, msg, got.InboundMessageID)
}

// TestEnqueueIngestErrorWraps proves the adapter names the failed staging:
// "server: enqueue rfq_ingest <id>: …" so the webhook's 500 log can name
// the receipt whose job was stranded by the rollback.
func TestEnqueueIngestErrorWraps(t *testing.T) {
	msg := uuid.New()
	sp := &spyInserter{fail: errors.New("insert refused")}
	err := enqueueIngest(sp)(context.Background(), nil, uuid.New(), uuid.New(), msg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "enqueue rfq_ingest")
	assert.Contains(t, err.Error(), msg.String())
}

// TestNewReceiver pins the two receiver bindings: the real ReceivingClient
// only when RESEND_API_KEY exists, else the unconfigured stub that fails
// loudly (permanent) instead of silently disabling ingest.
func TestNewReceiver(t *testing.T) {
	require.IsType(t, &resend.ReceivingClient{}, newReceiver(&platform.Config{ResendAPIKey: "re_x"}))
	require.IsType(t, resend.UnconfiguredReceiver{}, newReceiver(&platform.Config{}))
}

// TestNewStorage pins the dev-storage binding: DirStorage at QP_STORAGE_DIR
// when set, the repo-local .dev-storage default otherwise — bytes survive a
// restart, which the in-memory fake could not promise.
func TestNewStorage(t *testing.T) {
	want := t.TempDir() + "/nested"
	t.Setenv("QP_STORAGE_DIR", want)
	s := newStorage()
	dir, ok := s.(*platform.DirStorage)
	require.True(t, ok, "newStorage must bind DirStorage, got %T", s)
	assert.Equal(t, want, dir.Root())

	t.Setenv("QP_STORAGE_DIR", "")
	s = newStorage()
	dir, ok = s.(*platform.DirStorage)
	require.True(t, ok)
	assert.Equal(t, ".dev-storage", dir.Root())
}
