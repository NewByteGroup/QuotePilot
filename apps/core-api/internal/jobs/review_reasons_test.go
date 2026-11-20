package jobs_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/jobs"
)

// TestReviewReasonsClosedList pins the controlled vocabulary: every entry
// is non-empty and unique, so the real-Postgres test can trust that a
// stored value outside this list is a bug, not a missing constant.
func TestReviewReasonsClosedList(t *testing.T) {
	seen := map[jobs.ReviewReason]bool{}
	for _, r := range jobs.ReviewReasons {
		require.NotEmpty(t, string(r), "review reasons must be non-empty codes")
		require.False(t, seen[r], "duplicate review reason %q", r)
		seen[r] = true
	}
	assert.Contains(t, jobs.ReviewReasons, jobs.ReviewReasonExportMatrixOpen)
	assert.Contains(t, jobs.ReviewReasons, jobs.ReviewReasonMissingTariff)
	assert.Contains(t, jobs.ReviewReasons, jobs.ReviewReasonStaleTariff)
}
