package auth

// Internal-package tests: the retry spacing and budget-aware sleep are
// unexported, so they are pinned here rather than through the external
// rig.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestMagicSendBackoff pins the linear retry spacing — 1s, 2s — the detached
// sender waits between attempts inside its total budget.
func TestMagicSendBackoff(t *testing.T) {
	assert.Equal(t, time.Second, magicSendBackoff(1))
	assert.Equal(t, 2*time.Second, magicSendBackoff(2))
	assert.Equal(t, 3*time.Second, magicSendBackoff(3))
}

// TestSleepCtx proves the retry wait yields to the send budget: an expired
// context stops the wait immediately and reports false.
func TestSleepCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.False(t, sleepCtx(ctx, time.Hour), "a dead context stops the wait")
	assert.True(t, sleepCtx(context.Background(), time.Millisecond))
}
