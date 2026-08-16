package ratelimit

import (
	"sync"
	"testing"
	"time"
)

// fakeClock lets the refill be tested without sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestLimiter(capacity int, refill time.Duration) (*Limiter, *fakeClock) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := New(capacity, refill)
	l.now = clock.now
	return l, clock
}

// Checking does not spend a token — only a failed attempt does. A user who
// logs in successfully every minute must never be throttled.
func TestAllowedDoesNotConsume(t *testing.T) {
	l, _ := newTestLimiter(3, time.Minute)

	for i := 0; i < 100; i++ {
		if ok, _ := l.Allowed("bob"); !ok {
			t.Fatalf("Allowed became false after %d checks with no failures", i)
		}
	}
}

func TestPenalizeExhaustsTheBucket(t *testing.T) {
	l, _ := newTestLimiter(3, time.Minute)

	for i := 0; i < 3; i++ {
		if ok, _ := l.Allowed("bob"); !ok {
			t.Fatalf("blocked after %d failures, want 3 allowed", i)
		}
		l.Penalize("bob")
	}

	ok, retry := l.Allowed("bob")
	if ok {
		t.Error("a fourth attempt was allowed from a 3-token bucket")
	}
	if retry <= 0 {
		t.Errorf("retry-after = %s, want a positive wait", retry)
	}
	if retry > time.Minute {
		t.Errorf("retry-after = %s, want no more than the refill interval", retry)
	}
}

func TestBucketRefillsOverTime(t *testing.T) {
	l, clock := newTestLimiter(3, time.Minute)

	for i := 0; i < 3; i++ {
		l.Penalize("bob")
	}
	if ok, _ := l.Allowed("bob"); ok {
		t.Fatal("the bucket was not empty")
	}

	clock.advance(59 * time.Second)
	if ok, _ := l.Allowed("bob"); ok {
		t.Error("a token appeared before the refill interval elapsed")
	}

	clock.advance(2 * time.Second)
	if ok, _ := l.Allowed("bob"); !ok {
		t.Error("no token after the refill interval elapsed")
	}

	// Refill stops at capacity rather than banking credit for an idle year.
	clock.advance(365 * 24 * time.Hour)
	for i := 0; i < 3; i++ {
		l.Penalize("bob")
	}
	if ok, _ := l.Allowed("bob"); ok {
		t.Error("the bucket held more than its capacity after a long idle period")
	}
}

// One key's failures must not throttle another. Rate limiting that leaked
// across accounts would let any attacker lock out the whole server.
func TestKeysAreIndependent(t *testing.T) {
	l, _ := newTestLimiter(2, time.Minute)

	for i := 0; i < 5; i++ {
		l.Penalize("bob")
	}
	if ok, _ := l.Allowed("bob"); ok {
		t.Error("bob was not throttled")
	}
	if ok, _ := l.Allowed("alice"); !ok {
		t.Error("alice was throttled by bob's failures")
	}
}

func TestResetClearsTheBucket(t *testing.T) {
	l, _ := newTestLimiter(2, time.Hour)

	l.Penalize("bob")
	l.Penalize("bob")
	if ok, _ := l.Allowed("bob"); ok {
		t.Fatal("the bucket was not empty")
	}

	l.Reset("bob")
	if ok, _ := l.Allowed("bob"); !ok {
		t.Error("Reset did not refill the bucket")
	}
}

// A full bucket carries no information, so forgetting it is free — and it is
// what stops an attacker cycling through keys from growing the map without
// bound. A bucket still holding a penalty must survive.
func TestCleanupDropsOnlyFullBuckets(t *testing.T) {
	l, clock := newTestLimiter(2, time.Minute)

	l.Penalize("recent")
	l.Penalize("recent")
	l.Penalize("old")
	l.Penalize("old")
	clock.advance(3 * time.Minute) // enough to refill "old" only if we let it

	// Both have refilled by now, so both go.
	l.Cleanup()
	if n := len(l.buckets); n != 0 {
		t.Errorf("%d refilled buckets survived cleanup, want 0", n)
	}

	l.Penalize("active")
	l.Penalize("active")
	l.Cleanup()
	if _, ok := l.buckets["active"]; !ok {
		t.Error("cleanup dropped a bucket that still held a penalty")
	}
}

func TestConcurrentUse(t *testing.T) {
	l := New(100, time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				l.Allowed("shared")
				l.Penalize("shared")
				l.Cleanup()
			}
		}()
	}
	wg.Wait()
}
