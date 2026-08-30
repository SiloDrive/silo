package traffic

import (
	"testing"
	"time"
)

func at(c *Counters, sec int64) { c.now = func() time.Time { return time.Unix(sec, 0) } }

func TestTotalsAndWindowCountTheSameBytes(t *testing.T) {
	c := New()
	at(c, 1000)
	c.Record(Bulk, 100, 900)
	c.Record(Control, 5, 50)

	got := c.Read()
	if got.Total["bulk"] != (Direction{In: 100, Out: 900}) {
		t.Errorf("bulk total = %+v", got.Total["bulk"])
	}
	if got.Window["bulk"] != (Direction{In: 100, Out: 900}) {
		t.Errorf("bulk window = %+v", got.Window["bulk"])
	}
	if got.Total["control"] != (Direction{In: 5, Out: 50}) {
		t.Errorf("control total = %+v", got.Total["control"])
	}
	if got.WindowSeconds != WindowSeconds {
		t.Errorf("window covers %d seconds, want %d", got.WindowSeconds, WindowSeconds)
	}
}

// The whole point of the window: a server that stopped transferring reports
// that it stopped. Totals keep what happened, the window does not -- and a
// rate computed from a stale bucket is the one number worse than no number,
// because it says a sync is running when it is not.
func TestTheWindowForgetsButTheTotalDoesNot(t *testing.T) {
	c := New()
	at(c, 1000)
	c.Record(Bulk, 1<<20, 1<<20)

	at(c, 1000+WindowSeconds+1)
	got := c.Read()

	if got.Window["bulk"] != (Direction{}) {
		t.Errorf("a minute after the last byte the window still reports %+v", got.Window["bulk"])
	}
	if got.Total["bulk"] != (Direction{In: 1 << 20, Out: 1 << 20}) {
		t.Errorf("the total forgot what moved: %+v", got.Total["bulk"])
	}
}

// An idle gap shorter than the window must clear exactly the seconds it
// skipped -- no more and no less.
//
// The arithmetic is the part worth pinning, because getting it wrong is
// invisible: the ring is only sixty slots, so a bucket written seventy seconds
// ago is the *same slot* as one written ten seconds ago. A gap that fails to
// clear as it passes reports minute-old traffic as current, which is the one
// number worse than no number.
//
// So: write at 1000 (slot 40), write at 1030 (slot 30), read at 1070. The step
// from 1030 to 1070 is inside the window, so it takes the incremental path,
// and the slots it passes over include slot 40 -- whose bytes are now seventy
// seconds old and must be gone. Only the 1030 write is still inside the
// minute.
func TestPassingOverAStaleBucketClearsIt(t *testing.T) {
	c := New()
	at(c, 1000)
	c.Record(Bulk, 1000, 0)

	at(c, 1030)
	c.Record(Bulk, 30, 0)

	at(c, 1070)
	if got := c.Read().Window["bulk"]; got != (Direction{In: 30}) {
		t.Errorf("window = %+v, want only the 30 from t=1030 -- the 70-second-old bucket was reported as current", got)
	}
}

// The other half of the same rule: a gap must not clear the seconds it did not
// skip. Clearing the whole ring on every step would report an idle server
// correctly and a busy one as idle.
func TestAGapKeepsTheSecondsStillInsideTheWindow(t *testing.T) {
	c := New()
	at(c, 1000)
	c.Record(Bulk, 10, 0)

	at(c, 1010)
	c.Record(Bulk, 7, 0)

	if got := c.Read().Window["bulk"]; got != (Direction{In: 17}) {
		t.Errorf("window = %+v, want 17 -- both writes are inside the minute", got)
	}
}

// A clock that steps backwards must not resurrect cleared buckets or wedge the
// ring. It happens: NTP corrections and suspended laptops both do it.
func TestAClockGoingBackwardsDoesNotResurrectTraffic(t *testing.T) {
	c := New()
	at(c, 2000)
	c.Record(Bulk, 500, 0)

	at(c, 1000)
	c.Record(Bulk, 1, 0)

	got := c.Read()
	if got.Total["bulk"].In != 501 {
		t.Errorf("total in = %d, want 501 -- totals never go backwards", got.Total["bulk"].In)
	}
	if got.Window["bulk"].In > 501 {
		t.Errorf("window in = %d, more than has ever been recorded", got.Window["bulk"].In)
	}
}

func TestBulkIsTheContentSurfaceAndNothingElse(t *testing.T) {
	for _, p := range []string{
		"/api/silo/v1/libraries/r1/chunks",
		"/api/silo/v1/libraries/r1/chunks/fetch",
		"/api/silo/v1/libraries/r1/objects/" + "a0" + "",
		"/api/silo/v1/libraries/r1/entries/notes/today.md",
	} {
		if got := LaneFor(p); got != Bulk {
			t.Errorf("LaneFor(%q) = %v, want bulk", p, got)
		}
	}
	for _, p := range []string{
		"/api/silo/v1/auth/login",
		"/api/silo/v1/libraries",
		"/api/silo/v1/libraries/r1/commits",
		"/api/silo/v1/libraries/r1/head",
		"/api/silo/v1/admin/storage",
	} {
		if got := LaneFor(p); got != Control {
			t.Errorf("LaneFor(%q) = %v, want control", p, got)
		}
	}
}

func TestRecordingIsSafeUnderConcurrentUse(t *testing.T) {
	c := New()
	done := make(chan struct{})
	for range 8 {
		go func() {
			for range 200 {
				c.Record(Bulk, 1, 1)
				_ = c.Read()
			}
			close2(done)
		}()
	}
	for range 8 {
		<-done
	}
	if got := c.Read().Total["bulk"].In; got != 8*200 {
		t.Errorf("total in = %d, want %d", got, 8*200)
	}
}

func close2(ch chan struct{}) { ch <- struct{}{} }
