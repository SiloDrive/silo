package silod

import (
	"net/http"
	"sync"

	"github.com/dkam/silo/fileserver/option"
)

// The object lanes hand whole objects to memory, and until this there was no
// arithmetic anybody could do about it.
//
// AES-GCM is one-shot: a sealed object is opened in full or not at all, so an
// object is buffered entire on the way in and on the way out. That is a
// property of the format rather than a shortcut, and it is fine for the
// objects the server actually handles -- a directory is kilobytes, a commit is
// a few hundred bytes.
//
// A manifest is the exception, and it is the one a client chooses the size of.
// The ceiling was store.MaxManifestBytes plus framing, a gibibyte and a
// megabyte, and nothing bounded how many requests were inside that ceiling at
// the same time. So the number that mattered was never the size of one object;
// it was that size multiplied by however many requests an attacker cared to
// open, and eight of them asked for forty gigabytes.
//
// Two answers, and they are different in kind. maxObjectBody is smaller now,
// which bounds one request. This bounds all of them together: every lane that
// is about to hold a whole object in memory takes its size out of a shared
// budget first and puts it back when it is done, so the server's exposure is a
// figure an operator can set rather than one an attacker can choose.
//
// It refuses rather than waits. Parking the request until room appears turns
// memory pressure into an unbounded queue of goroutines, each still holding
// its connection, which is the same failure arriving more slowly. 503 with a
// Retry-After is the honest answer: the server cannot do this now, and asking
// again shortly is the right response.
var objectBudget = &budget{}

type budget struct {
	mu   sync.Mutex
	used int64
}

// limit is read per call rather than captured, so a test -- or a config
// reload -- can move it without rebuilding the budget underneath the requests
// already holding part of it.
func (b *budget) limit() int64 {
	if option.MaxBufferedObjectBytes > 0 {
		return option.MaxBufferedObjectBytes
	}
	return option.DefaultMaxBufferedObjectBytes
}

// acquire reserves n bytes, and reports whether the caller may proceed.
//
// A request larger than the whole budget is refused rather than allowed
// through as a special case. It is the honest answer -- the server cannot hold
// it -- and the alternative is that the one request big enough to matter is
// the one request that skips the check.
func (b *budget) acquire(n int64) (release func(), ok bool) {
	if n <= 0 {
		return func() {}, true
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+n > b.limit() {
		return nil, false
	}
	b.used += n

	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.used -= n
			if b.used < 0 {
				b.used = 0
			}
		})
	}, true
}

// holdForObject reserves the memory an object lane is about to spend, writing
// the 503 itself when there is none. The caller releases when the bytes are no
// longer held.
func holdForObject(w http.ResponseWriter, size int64) (func(), bool) {
	release, ok := objectBudget.acquire(size)
	if !ok {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "The server is holding as much object data as it can; retry shortly",
			http.StatusServiceUnavailable)
		return nil, false
	}
	return release, true
}
