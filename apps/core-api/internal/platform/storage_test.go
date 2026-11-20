package platform_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// TestFakeStorageCapturesKeyAndBytes is the dev/test seam's contract: Put
// records key+bytes in memory so worker tests can assert uploads with no
// network, and the stored copy is immune to later mutation of the caller's
// slice.
func TestFakeStorageCapturesKeyAndBytes(t *testing.T) {
	store := platform.NewFakeStorage()
	body := []byte("attachment-bytes")
	require.NoError(t, store.Put(context.Background(), "tenants/org1/rfqs/rfq1/a.pdf", body))
	body[0] = 'X' // caller mutation must not reach the stored copy

	got, err := store.Get(context.Background(), "tenants/org1/rfqs/rfq1/a.pdf")
	require.NoError(t, err)
	assert.Equal(t, []byte("attachment-bytes"), got)
	assert.Equal(t, []string{"tenants/org1/rfqs/rfq1/a.pdf"}, store.Keys())

	_, err = store.Get(context.Background(), "tenants/org1/rfqs/rfq1/missing")
	assert.ErrorIs(t, err, platform.ErrObjectNotFound)
}

// TestDirStorageGet exercises the dev binding against a real temp dir:
// bytes round-trip through the filesystem, a missing key is
// ErrObjectNotFound (not a wrapped fs error), and a ".." key is refused by
// the same guard Put applies — Get must not climb out of the root either.
func TestDirStorageGet(t *testing.T) {
	root := t.TempDir()
	store := platform.NewDirStorage(root)
	ctx := context.Background()

	require.NoError(t, store.Put(ctx, "tenants/org1/quotes/q.pdf", []byte("%PDF-1")))
	got, err := store.Get(ctx, "tenants/org1/quotes/q.pdf")
	require.NoError(t, err)
	assert.Equal(t, []byte("%PDF-1"), got)

	_, err = store.Get(ctx, "tenants/org1/quotes/none.pdf")
	assert.ErrorIs(t, err, platform.ErrObjectNotFound)

	_, err = store.Get(ctx, "../outside.txt")
	assert.Error(t, err)
	assert.NotErrorIs(t, err, platform.ErrObjectNotFound, "traversal is a guard refusal, not a miss")

	assert.Error(t, store.Put(ctx, "tenants/../escape", []byte("x")),
		"Put applies the same '..' guard")
}
