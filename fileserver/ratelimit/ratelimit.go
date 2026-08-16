// Package ratelimit provides an in-memory token-bucket limiter, used to put a
// ceiling on password guessing against the login endpoints.
//
// A bucket holds up to Capacity tokens and regains one every RefillInterval.
// Callers check with Allowed before doing the expensive or sensitive work and
// spend a token with Penalize only when the attempt fails, so a user who logs
// in successfully is never throttled no matter how often they do it — only
// failures accumulate.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// cleanupInterval is how often full buckets are dropped. A bucket that has
// refilled completely carries no information, so forgetting it is free — and
// it is what stops an attacker cycling through keys from growing the map
// without bound.
const cleanupInterval = 10 * time.Minute

type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter is safe for concurrent use.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket

	capacity float64
	// refill is tokens gained per second.
	refill float64

	// now is time.Now, replaced in tests.
	now func() time.Time
}

// New returns a limiter whose buckets hold capacity tokens and regain one per
// refillInterval.
func New(capacity int, refillInterval time.Duration) *Limiter {
	if capacity < 1 {
		capacity = 1
	}
	if refillInterval <= 0 {
		refillInterval = time.Second
	}
	return &Limiter{
		buckets:  make(map[string]*bucket),
		capacity: float64(capacity),
		refill:   1 / refillInterval.Seconds(),
		now:      time.Now,
	}
}

// Allowed reports whether key has a token to spend, without spending it. When
// it does not, the second return value is how long until it will.
func (l *Limiter) Allowed(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.refillLocked(key)
	if b.tokens >= 1 {
		return true, 0
	}
	// Rounded up to a whole second: Retry-After carries seconds, and reporting
	// a truncated wait would invite a retry that is still too early.
	wait := time.Duration(math.Ceil((1-b.tokens)/l.refill)) * time.Second
	if wait < time.Second {
		wait = time.Second
	}
	return false, wait
}

// Penalize spends a token for key, to be called when an attempt fails.
func (l *Limiter) Penalize(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.refillLocked(key)
	if b.tokens >= 1 {
		b.tokens--
	} else {
		b.tokens = 0
	}
}

// Reset refills key's bucket, called when an attempt succeeds so that a user
// who mistyped a password twice before getting it right is not left one
// mistake away from being throttled.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, key)
}

func (l *Limiter) refillLocked(key string) *bucket {
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.capacity, last: now}
		l.buckets[key] = b
		return b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.refill
	if b.tokens > l.capacity {
		b.tokens = l.capacity
	}
	b.last = now
	return b
}

// Cleanup drops buckets that have refilled completely.
func (l *Limiter) Cleanup() {
	l.mu.Lock()
	defer l.mu.Unlock()

	for key := range l.buckets {
		if b := l.refillLocked(key); b.tokens >= l.capacity {
			delete(l.buckets, key)
		}
	}
}

// StartCleanup sweeps full buckets for the life of the process.
func (l *Limiter) StartCleanup() {
	go func() {
		ticker := time.NewTicker(cleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			l.Cleanup()
		}
	}()
}
