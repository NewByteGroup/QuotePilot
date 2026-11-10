package platform

import (
	"sync"
	"time"
)

// Login rate-limit defaults (spec section 8.1): /auth/login gets one
// limiter per dimension — recipient email and caller IP. The other auth
// endpoints are poor abuse targets (single-use tokens, uniform responses),
// so they stay unthrottled in the MVP.
const (
	// LoginEmailCapacity is the magic-link budget per recipient per window.
	LoginEmailCapacity = 5
	// LoginIPCapacity is the /auth/login budget per caller IP per window.
	LoginIPCapacity = 30
	// LimitWindow is the refill window both login limiters share.
	LimitWindow = time.Hour
)

// Limiter is a hand-rolled in-process token bucket (spec section 8.1 —
// golang.org/x/time/rate is not on the ADR-002 allow-list, so the bucket
// ships here). Balances are kept in nanoseconds (each token costs
// per/capacity of refill time), so boundary cases are exact integer math
// with no float drift. State is per-process and per-key maps grow with
// traffic: both are the spec section 11 single-instance open item,
// deliberate for the MVP.
type Limiter struct {
	mu           sync.Mutex
	buckets      map[string]*bucket
	costPerToken time.Duration // refill time one token costs: per / capacity
	maxBalance   time.Duration // a full bucket: capacity * costPerToken
	now          func() time.Time
}

// bucket tracks one key's spendable balance and when it was last refilled.
type bucket struct {
	balance time.Duration
	last    time.Time
}

// NewLimiter starts every unseen key with a full bucket refilling over
// `per`: one token per per/capacity elapsed, capped at capacity. now is
// injected so tests drive the clock.
//
//	limiter := platform.NewLimiter(platform.LoginIPCapacity, platform.LimitWindow, time.Now)
func NewLimiter(capacity int, per time.Duration, now func() time.Time) *Limiter {
	if capacity <= 0 {
		panic("platform: NewLimiter capacity must be positive")
	}
	cost := per / time.Duration(capacity)
	return &Limiter{
		buckets:      make(map[string]*bucket),
		costPerToken: cost,
		maxBalance:   cost * time.Duration(capacity),
		now:          now,
	}
}

// Allow spends one token for key: true while the bucket can pay for one,
// false once it is drained. A denied call consumes nothing — it only
// observes (and banks the elapsed refill).
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{balance: l.maxBalance, last: now}
		l.buckets[key] = b
	}
	b.balance = min(l.maxBalance, b.balance+now.Sub(b.last))
	b.last = now
	if b.balance < l.costPerToken {
		return false
	}
	b.balance -= l.costPerToken
	return true
}
