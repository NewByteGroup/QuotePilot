package platform_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// fakeClock is the injected time source: nothing in Limiter reads
// time.Now() itself.
type fakeClock struct{ now time.Time }

func (f *fakeClock) Now() time.Time          { return f.now }
func (f *fakeClock) Advance(d time.Duration) { f.now = f.now.Add(d) }

// TestLimiterDrains proves a fresh key holds exactly `capacity` tokens: the
// capacity-th Allow succeeds, the next one fails, and failed calls consume
// nothing (repeated denials don't dig the bucket deeper).
func TestLimiterDrains(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	lim := platform.NewLimiter(3, time.Hour, clock.Now)

	for i := 0; i < 3; i++ {
		assert.True(t, lim.Allow("k"), "token %d of a fresh bucket must be allowed", i+1)
	}
	assert.False(t, lim.Allow("k"), "a drained bucket must deny")
	assert.False(t, lim.Allow("k"), "denials must not consume future tokens")
}

// TestLimiterRefills proves linear refill: at 3 tokens/hour a drained
// bucket earns one token per 20 minutes, capped at capacity.
func TestLimiterRefills(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	lim := platform.NewLimiter(3, time.Hour, clock.Now)
	for i := 0; i < 3; i++ {
		lim.Allow("k")
	}

	clock.Advance(20 * time.Minute)
	assert.True(t, lim.Allow("k"), "one token must have refilled after 20m")
	assert.False(t, lim.Allow("k"), "only one token refills in 20m")

	clock.Advance(10 * time.Hour)
	for i := 0; i < 3; i++ {
		assert.True(t, lim.Allow("k"), "bucket must cap at capacity after a long idle, token %d", i+1)
	}
	assert.False(t, lim.Allow("k"), "refill is capped at capacity")
}

// TestLimiterIndependentKeys proves buckets are per-key.
func TestLimiterIndependentKeys(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	lim := platform.NewLimiter(1, time.Hour, clock.Now)

	assert.True(t, lim.Allow("a"))
	assert.False(t, lim.Allow("a"), "a's bucket is drained")
	assert.True(t, lim.Allow("b"), "b's bucket is independent of a's")
}

// TestLimiterBoundaryAtZero pins the edge: a bucket holding strictly less
// than one token denies, and a bucket landing on exactly one allows.
func TestLimiterBoundaryAtZero(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	lim := platform.NewLimiter(4, time.Hour, clock.Now)
	for i := 0; i < 4; i++ {
		lim.Allow("k")
	}
	assert.False(t, lim.Allow("k"), "zero balance must deny")

	// 15 minutes at 4/hour is exactly one token.
	clock.Advance(15 * time.Minute)
	assert.True(t, lim.Allow("k"), "a balance of exactly one must allow")
	assert.False(t, lim.Allow("k"), "the boundary token is consumed")
}

// TestLoginDefaultsExist pins the spec section 8.1 numbers so a drift in
// the constants is a test failure, not a silent behaviour change.
func TestLoginDefaultsExist(t *testing.T) {
	assert.Equal(t, 5, platform.LoginEmailCapacity)
	assert.Equal(t, 30, platform.LoginIPCapacity)
	assert.Equal(t, time.Hour, platform.LimitWindow)
}
