package ratelimit

import (
	"strconv"
	"sync"
	"sync/atomic"
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
		if ok, _ := l.allowed("bob"); !ok {
			t.Fatalf("Allowed became false after %d checks with no failures", i)
		}
	}
}

func TestPenalizeExhaustsTheBucket(t *testing.T) {
	l, _ := newTestLimiter(3, time.Minute)

	for i := 0; i < 3; i++ {
		if ok, _ := l.allowed("bob"); !ok {
			t.Fatalf("blocked after %d failures, want 3 allowed", i)
		}
		l.Penalize("bob")
	}

	ok, retry := l.allowed("bob")
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
	if ok, _ := l.allowed("bob"); ok {
		t.Fatal("the bucket was not empty")
	}

	clock.advance(59 * time.Second)
	if ok, _ := l.allowed("bob"); ok {
		t.Error("a token appeared before the refill interval elapsed")
	}

	clock.advance(2 * time.Second)
	if ok, _ := l.allowed("bob"); !ok {
		t.Error("no token after the refill interval elapsed")
	}

	// Refill stops at capacity rather than banking credit for an idle year.
	clock.advance(365 * 24 * time.Hour)
	for i := 0; i < 3; i++ {
		l.Penalize("bob")
	}
	if ok, _ := l.allowed("bob"); ok {
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
	if ok, _ := l.allowed("bob"); ok {
		t.Error("bob was not throttled")
	}
	if ok, _ := l.allowed("alice"); !ok {
		t.Error("alice was throttled by bob's failures")
	}
}

func TestResetClearsTheBucket(t *testing.T) {
	l, _ := newTestLimiter(2, time.Hour)

	l.Penalize("bob")
	l.Penalize("bob")
	if ok, _ := l.allowed("bob"); ok {
		t.Fatal("the bucket was not empty")
	}

	l.Reset("bob")
	if ok, _ := l.allowed("bob"); !ok {
		t.Error("Reset did not refill the bucket")
	}
}

// ResetAll is what a test harness reaches for, so it has to clear keys the
// caller cannot name — Reset takes one key, and a harness resetting between
// tests does not know which keys the last test spent.
func TestResetAllClearsEveryBucket(t *testing.T) {
	l, _ := newTestLimiter(2, time.Hour)

	for _, who := range []string{"alice", "bob"} {
		l.Penalize(who)
		l.Penalize(who)
		if ok, _ := l.allowed(who); ok {
			t.Fatalf("%s's bucket was not empty", who)
		}
	}

	l.ResetAll()
	for _, who := range []string{"alice", "bob"} {
		if ok, _ := l.allowed(who); !ok {
			t.Errorf("ResetAll left %s throttled", who)
		}
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
				l.allowed("shared")
				l.Penalize("shared")
				l.Cleanup()
			}
		}()
	}
	wg.Wait()
}

// The window between the check and the charge is the whole password hash —
// 600k PBKDF2 iterations, some 80ms of it. Every request that arrives inside
// that window reads the same unspent bucket and is admitted, so a limiter with
// a capacity of ten admits as many attempts as arrive. Capacity has to mean
// something under concurrency or it means nothing at all.
func TestConcurrentAttemptsCannotExceedCapacity(t *testing.T) {
	const capacity = 10
	const attempts = 500

	l := New(capacity, time.Minute)

	var admitted atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, _, release := l.Reserve("bob")
			if !ok {
				return
			}
			defer release()
			admitted.Add(1)
			// The hash the limiter exists to keep out of reach.
			time.Sleep(20 * time.Millisecond)
			l.Penalize("bob")
		}()
	}
	close(start)
	wg.Wait()

	if n := admitted.Load(); n > capacity {
		t.Errorf("%d of %d concurrent attempts reached the hash, against a capacity of %d", n, attempts, capacity)
	}
}

// A released slot is a slot again. Nothing is spent by an attempt that finishes
// without failing, so an account logging in all day is never throttled.
func TestReleasedSlotsAreReusable(t *testing.T) {
	l, _ := newTestLimiter(2, time.Hour)

	for i := 0; i < 100; i++ {
		ok, _, release := l.Reserve("bob")
		if !ok {
			t.Fatalf("Reserve refused after %d released attempts", i)
		}
		release()
	}
}

// Releasing twice must not hand back a slot that was never held: the second
// release would let one more attempt past the ceiling than the ceiling allows.
func TestReleaseIsIdempotent(t *testing.T) {
	l, _ := newTestLimiter(1, time.Hour)

	ok, _, release := l.Reserve("bob")
	if !ok {
		t.Fatal("the first attempt was refused")
	}
	held, _, _ := l.Reserve("bob")
	if held {
		t.Fatal("a second attempt was admitted against a capacity of 1")
	}
	release()
	release()

	first, _, releaseFirst := l.Reserve("bob")
	second, _, _ := l.Reserve("bob")
	if !first {
		t.Fatal("the slot did not come back")
	}
	if second {
		t.Error("the double release handed back a slot that was never held")
	}
	releaseFirst()
}

// The map is bounded by what is in flight, not by what has ever arrived. That
// is the property the read-only check used to buy, and reserving must not
// spend it: an attacker cycling through addresses gets one entry at a time.
func TestReserveLeavesNoEntryBehind(t *testing.T) {
	l, _ := newTestLimiter(5, time.Hour)

	for i := 0; i < 1000; i++ {
		_, _, release := l.Reserve("nobody-" + strconv.Itoa(i) + "@example.com")
		release()
	}

	l.mu.Lock()
	n := len(l.buckets)
	l.mu.Unlock()
	if n != 0 {
		t.Errorf("%d buckets survived 1000 reserved-and-released attempts, want 0", n)
	}
}

// Spend is Reserve and Penalize with nothing between them, for the endpoint
// whose every request costs a token rather than only its failures.
func TestSpendChargesEveryCall(t *testing.T) {
	l, _ := newTestLimiter(3, time.Minute)

	for i := 0; i < 3; i++ {
		if ok, _ := l.Spend("bob"); !ok {
			t.Fatalf("Spend refused call %d of a 3-token bucket", i)
		}
	}
	ok, retry := l.Spend("bob")
	if ok {
		t.Error("a fourth call was allowed from a 3-token bucket")
	}
	if retry <= 0 {
		t.Errorf("retry-after = %s, want a positive wait", retry)
	}
}

// Cleanup must not drop a bucket whose slots are held: the count lives in the
// entry, and deleting it releases every attempt still inside the guarded work.
func TestCleanupKeepsHeldBuckets(t *testing.T) {
	l, _ := newTestLimiter(2, time.Minute)

	ok, _, release := l.Reserve("bob")
	if !ok {
		t.Fatal("Reserve refused a full bucket")
	}
	defer release()

	l.Cleanup()
	if _, ok := l.buckets["bob"]; !ok {
		t.Fatal("cleanup dropped a bucket with an attempt in flight")
	}
	if held, _, _ := l.Reserve("bob"); !held {
		t.Fatal("the second slot was refused")
	}
	if third, _, _ := l.Reserve("bob"); third {
		t.Error("a third attempt was admitted against a capacity of 2")
	}
}

// Reset refills on success. A key with attempts still in flight keeps its
// entry, because the slots they hold are counted there.
func TestResetKeepsHeldSlots(t *testing.T) {
	l, _ := newTestLimiter(2, time.Hour)

	ok, _, release := l.Reserve("bob")
	if !ok {
		t.Fatal("Reserve refused a full bucket")
	}
	defer release()
	l.Penalize("bob")
	l.Reset("bob")

	if held, _, _ := l.Reserve("bob"); !held {
		t.Fatal("Reset did not refill the bucket")
	}
	if third, _, _ := l.Reserve("bob"); third {
		t.Error("Reset forgot a slot that was still held")
	}
}
