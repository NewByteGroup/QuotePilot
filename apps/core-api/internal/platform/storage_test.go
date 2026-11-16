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

	got, ok := store.Get("tenants/org1/rfqs/rfq1/a.pdf")
	require.True(t, ok)
	assert.Equal(t, []byte("attachment-bytes"), got)
	assert.Equal(t, []string{"tenants/org1/rfqs/rfq1/a.pdf"}, store.Keys())

	_, ok = store.Get("tenants/org1/rfqs/rfq1/missing")
	assert.False(t, ok)
}
