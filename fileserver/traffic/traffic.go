// Package traffic counts the bytes this server moves, so that an operator can
// answer "how fast is this transferring" about their own install.
//
// Nothing else could. middleware/logging.go counts requests rather than bytes
// and is only installed when debug logging is on; the Sentry timings are
// sampled at a tenth and live on somebody else's machine. Neither is a number
// the person running the server can look at.
//
// These are wire bytes, counted at the HTTP boundary: what actually crossed
// the socket, request bodies in and response bodies out. They are not stored
// bytes and must not be reconciled against `silo df` or the storage panel,
// which count what is on the disk. The two diverge on purpose and by a lot --
// a stored frame carries its own overhead, a sealed pack carries a footer and
// a filter, dedup means a chunk that arrived is often not written at all, and
// a re-sent request after a 401 crosses the wire twice and lands once. Wire
// bytes are the right currency for "how fast is this transferring" and the
// wrong one for "how full is the disk", which is why this is a separate figure
// rather than another column beside the census.
//
// Two kinds of figure, because they answer different questions and one of them
// alone would mislead. Totals say how much has moved since the process
// started, which is the honest cumulative number and is not a rate. The window
// says how much moved in the last minute, which is the rate, and is what
// somebody watching a sync actually wants -- an average over an eight-hour
// uptime says nothing about whether anything is moving now.
package traffic

import (
	"strings"
	"sync"
	"time"
)

// WindowSeconds is how far back the recent-rate figure looks.
//
// A minute because it has to outlast one slow request without smoothing away a
// stall: shorter and a single large chunk makes the rate jump around, longer
// and a sync that stopped thirty seconds ago still looks busy.
const WindowSeconds = 60

// Lane separates the bytes a sync moves from everything else.
//
// Worth separating because they answer different questions and mixing them
// makes the interesting one quieter: an operator asking how fast a sync is
// going does not want listings, logins and head moves averaged into it. The
// split is by route rather than by handler, so a new bulk route is one line
// here instead of a counter somebody has to remember to add.
type Lane int

const (
	// Bulk is the content surface: chunks, objects and entries.
	Bulk Lane = iota
	// Control is everything else -- auth, listings, commits, head moves.
	Control
	numLanes
)

func (l Lane) String() string {
	if l == Bulk {
		return "bulk"
	}
	return "control"
}

// LaneFor classifies a request path.
//
// Prefix-free substring matching, because these segments cannot appear
// anywhere else in a Silo URL: a library id is a UUID and a chunk or object id
// is sixty-four hex characters, so neither can spell "chunks" or "entries".
func LaneFor(path string) Lane {
	switch {
	case strings.Contains(path, "/chunks"),
		strings.Contains(path, "/objects"),
		strings.Contains(path, "/entries"):
		return Bulk
	}
	return Control
}

// Counters is a running total and a rolling window per lane.
//
// Recorded once per request rather than once per Write: the wrapper adds up a
// request's bytes without a lock and hands over the sum at the end, so a
// megabyte body costs one lock acquisition rather than one per 32 KiB.
type Counters struct {
	// now is injectable so a test can drive the window without sleeping
	// through it. Nil means time.Now.
	now func() time.Time

	mu     sync.Mutex
	total  [numLanes]Direction
	ring   [numLanes][WindowSeconds]Direction
	second int64 // unix second the newest bucket belongs to
}

// Direction is bytes in each direction: in is what clients sent, out is what
// this server sent back.
type Direction struct {
	In  int64 `json:"in"`
	Out int64 `json:"out"`
}

func (d *Direction) add(in, out int64) {
	d.In += in
	d.Out += out
}

// New returns counters that have seen nothing.
func New() *Counters { return &Counters{} }

func (c *Counters) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Record adds one request's bytes.
func (c *Counters) Record(l Lane, in, out int64) {
	if l < 0 || l >= numLanes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	c.total[l].add(in, out)
	c.advance(c.clock().Unix())
	c.ring[l][c.bucket()].add(in, out)
}

// advance moves the ring forward to second now, clearing the buckets that were
// skipped.
//
// Clearing the skipped ones is the whole job. A server that was idle for five
// minutes and then moved a byte would otherwise report the traffic from five
// minutes ago as this minute's, because those buckets still hold what they
// held when they were last current.
func (c *Counters) advance(now int64) {
	if now == c.second {
		return
	}
	if c.second == 0 {
		c.second = now
		return
	}
	elapsed := now - c.second
	if elapsed < 0 {
		// The clock went backwards. Nothing sensible to roll, and rolling
		// backwards would resurrect buckets that were already cleared.
		c.second = now
		return
	}
	if elapsed >= WindowSeconds {
		for l := range c.ring {
			c.ring[l] = [WindowSeconds]Direction{}
		}
		c.second = now
		return
	}
	for i := int64(1); i <= elapsed; i++ {
		slot := (c.second + i) % WindowSeconds
		for l := range c.ring {
			c.ring[l][slot] = Direction{}
		}
	}
	c.second = now
}

func (c *Counters) bucket() int64 { return c.second % WindowSeconds }

// Snapshot is what the counters hold, at one moment.
type Snapshot struct {
	// Total is everything since the process started.
	Total map[string]Direction `json:"total"`
	// Window is what moved in the last WindowSeconds, which is the figure a
	// rate is computed from. Divide by WindowSeconds for bytes per second.
	Window map[string]Direction `json:"window"`
	// WindowSeconds says what Window covers, so a reader never has to assume
	// it.
	WindowSeconds int `json:"window_seconds"`
}

// Read returns the current figures.
//
// It advances the ring first, so an idle server reports an idle window rather
// than whatever it was doing when it stopped -- a reader that only ever reads
// would otherwise see a stale minute forever.
func (c *Counters) Read() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.advance(c.clock().Unix())

	out := Snapshot{
		Total:         map[string]Direction{},
		Window:        map[string]Direction{},
		WindowSeconds: WindowSeconds,
	}
	for l := range c.total {
		name := Lane(l).String()
		out.Total[name] = c.total[l]

		var win Direction
		for _, d := range c.ring[l] {
			win.add(d.In, d.Out)
		}
		out.Window[name] = win
	}
	return out
}

// Default is the process's counters, which is what the middleware writes to
// and the admin surface reads.
//
// A package-level instance rather than one threaded through every handler,
// because the thing being counted is the process and there is exactly one of
// it. Tests that need isolation build their own with New.
var Default = New()
