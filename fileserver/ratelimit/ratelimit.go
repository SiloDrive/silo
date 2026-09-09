// Package ratelimit provides an in-memory token-bucket limiter, used to put a
// ceiling on password guessing against the login endpoints.
//
// A bucket holds up to Capacity tokens and regains one every RefillInterval.
// Callers take a slot with Reserve before doing the expensive or sensitive work
// and spend a token with Penalize only when the attempt fails, so a user who
// logs in successfully is never throttled no matter how often they do it — only
// failures accumulate.
//
// Reserve rather than a bare check, because a check is not a decision. The work
// a limiter guards is slow — that is why it is guarded — and a check that does
// not hold anything leaves the whole of that work as a window in which every
// other request reads the same unspent bucket. Ten tokens admitted five hundred
// attempts. What Reserve holds is the slot, released when the attempt finishes;
// what Penalize spends is the token, and only a failure spends one.
package ratelimit

import (
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

	// inflight is how many attempts hold a slot on this key right now. It is
	// what makes capacity mean something under concurrency: an attempt that
	// has been admitted but has not yet failed is still occupying one of the
	// key's tokens, because the reason to limit it has not been resolved yet.
	//
	// It is also what keeps the map from growing under an attacker cycling
	// through addresses. A bucket exists while it is either penalised or held;
	// a key that is neither is deleted on release, so the map is bounded by
	// how many attempts are in flight rather than by how many have ever
	// arrived.
	inflight int
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

// Reserve takes a slot for one attempt against key. The attempt may proceed
// when the first return value is true, and must call the returned function when
// it finishes, however it finishes — the slot is held until then.
//
// Reserving is not spending. A slot costs nothing once released, so an account
// logging in successfully all day is never throttled; a token is spent only by
// Penalize, and only a failed attempt calls it. What the slot buys is the
// guarantee that at most capacity attempts are ever inside the guarded work at
// once, which is the whole of what a check-and-then-act cannot promise.
//
// When the attempt may not proceed, the second return value is how long until
// it may, and the returned function is a no-op that is still safe to call.
func (l *Limiter) Reserve(key string) (bool, time.Duration, func()) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if ok, wait := l.allowedLocked(key); !ok {
		return false, wait, func() {}
	}

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.capacity, last: l.now()}
		l.buckets[key] = b
	}
	b.inflight++

	// Once, because a handler that releases twice would hand a slot back it
	// does not hold and let the next attempt past the ceiling.
	var once sync.Once
	return true, 0, func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.release(key)
		})
	}
}

// Spend takes a token for key outright, for the callers whose every request
// costs one rather than only their failures. It is Reserve and Penalize with no
// window between them, which is the point: there is no attempt to wait for.
func (l *Limiter) Spend(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if ok, wait := l.allowedLocked(key); !ok {
		return false, wait
	}
	b := l.refillLocked(key)
	if b.tokens >= 1 {
		b.tokens--
	} else {
		b.tokens = 0
	}
	return true, 0
}

// release gives back a slot, and drops the bucket if nothing is left to
// remember about the key.
func (l *Limiter) release(key string) {
	b, ok := l.buckets[key]
	if !ok {
		return
	}
	if b.inflight > 0 {
		b.inflight--
	}
	l.refillBucket(b)
	if b.inflight == 0 && b.tokens >= l.capacity {
		delete(l.buckets, key)
	}
}

// allowed reports whether key has a token to spend, without spending or holding
// it. It is the probe the reserving calls are built from; handlers want Reserve
// or Spend, because an answer that holds nothing is stale the moment it is
// given.
func (l *Limiter) allowed(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.allowedLocked(key)
}

func (l *Limiter) allowedLocked(key string) (bool, time.Duration) {
	// A key with no bucket has a full one, so answering from the absence costs
	// no allocation. That matters because this runs before the password is
	// checked, on a key derived from the submitted email: allocating for every
	// address ever submitted would let an attacker cycling through usernames
	// grow the map without bound. Reserve does create an entry, but only for
	// the length of one attempt — see the note on bucket.inflight.
	b, ok := l.buckets[key]
	if !ok {
		return true, 0
	}
	l.refillBucket(b)
	if b.tokens >= float64(1+b.inflight) {
		return true, 0
	}
	// The exact wait. Callers that have to report it in coarser units round it
	// up themselves — rounding here as well would mean two layers deciding how
	// long a caller should wait, and only one of them knows why.
	wait := time.Duration((float64(1+b.inflight) - b.tokens) / l.refill * float64(time.Second))
	if wait <= 0 {
		// A caller that is refused is entitled to a wait it can act on.
		wait = 1
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
//
// A key with attempts still in flight keeps its entry rather than losing it:
// the slots those attempts hold are counted there, and forgetting them would
// let the next arrivals past the ceiling.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if b, ok := l.buckets[key]; ok && b.inflight > 0 {
		b.tokens = l.capacity
		b.last = l.now()
		return
	}
	delete(l.buckets, key)
}

// ResetAll empties the whole map, so every key starts from a full bucket
// again.
//
// It is here for tests. A limiter outlives any one of them — it is a
// package-level var, and every test reaches it from the same loopback address
// — so a test that spends a bucket on purpose hands the next one a throttle it
// did not earn. Resetting per key is not enough for that, because the harness
// resetting it does not know which keys the last test touched.
func (l *Limiter) ResetAll() {
	l.mu.Lock()
	defer l.mu.Unlock()

	for key, b := range l.buckets {
		if b.inflight > 0 {
			b.tokens = l.capacity
			b.last = l.now()
			continue
		}
		delete(l.buckets, key)
	}
}

// refillLocked returns key's bucket, creating a full one if it has none.
func (l *Limiter) refillLocked(key string) *bucket {
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.capacity, last: l.now()}
		l.buckets[key] = b
		return b
	}
	l.refillBucket(b)
	return b
}

// refillBucket credits b for the time since it was last touched.
func (l *Limiter) refillBucket(b *bucket) {
	now := l.now()
	b.tokens += now.Sub(b.last).Seconds() * l.refill
	if b.tokens > l.capacity {
		b.tokens = l.capacity
	}
	b.last = now
}

// Cleanup drops buckets that have refilled completely.
func (l *Limiter) Cleanup() {
	l.mu.Lock()
	defer l.mu.Unlock()

	for key := range l.buckets {
		// A held bucket is not idle, whatever its tokens say: the slots its
		// attempts occupy are counted in the entry, and dropping it would
		// release them.
		if b := l.refillLocked(key); b.tokens >= l.capacity && b.inflight == 0 {
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
