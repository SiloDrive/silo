package silod

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/objmgr"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/store"
)

// The bug: a write that lost the race for the branch head every time came back
// as an ordinary error, indistinguishable from a real failure, so every
// handler answered 500. 500 is the one class a client must not retry blind —
// it means the server may have applied part of the request — and here nothing
// was applied and the same request would have succeeded a moment later.
// silo-drive maps 5xx to EIO, and an EIO from close(2) is data loss from the
// application's point of view.
//
// Eight writers start from the same head. One wins; the rest either win a
// later attempt or come back saying "contention", never "broken".
func TestConcurrentWritesReportContentionNotFailure(t *testing.T) {
	libraryID, acct := testLibrary(t)

	const writers = 8
	codes := make([]int, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			vars := map[string]string{"libraryid": libraryID, "path": fmt.Sprintf("f%d.txt", i)}
			w := do(t, entriesHandler, acct, "PUT", "/x", vars, bytes.Repeat([]byte("x"), 64))
			codes[i] = w.Code
		}()
	}
	wg.Wait()

	for i, code := range codes {
		switch code {
		case http.StatusCreated, http.StatusServiceUnavailable:
		default:
			t.Errorf("writer %d answered %d; contention must read as 201 or 503, never 500", i, code)
		}
	}
}

// The same exhaustion without depending on a schedule: move the head, then
// hand mutateTree a library still carrying the commit it read before that. Its
// compare-and-swap cannot match on the first attempt, so with no retries left
// the budget is guaranteed to run out.
func TestLostRaceIsReportedAsContention(t *testing.T) {
	libraryID, acct := testLibrary(t)

	stale, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		t.Fatal(err)
	}

	// A real write, so the branch moves out from under the stale handle.
	vars := map[string]string{"libraryid": libraryID, "path": "winner.txt"}
	if w := do(t, entriesHandler, acct, "PUT", "/x", vars, []byte("first")); w.Code != http.StatusCreated {
		t.Fatalf("uncontended write = %d (%s)", w.Code, w.Body.String())
	}

	orig := option.CommitAttempts
	option.CommitAttempts = 1
	t.Cleanup(func() { option.CommitAttempts = orig })

	_, _, err = mutateTree(stale, acct.Email, func(st *objmgr.Store, root store.ID, now int64) (store.ID, error) {
		return st.Mkdir(root, "/loser", defaultDirMode, now)
	})
	if err == nil {
		t.Fatal("a write against a stale head succeeded; the branch update is no longer a compare-and-swap")
	}
	if !errors.Is(err, ErrRetriesExhausted) {
		t.Fatalf("lost race reported as %v, want ErrRetriesExhausted", err)
	}

	w := httptest.NewRecorder()
	writeCommitErr(w, httptest.NewRequest("PUT", "/x", nil), err, "write")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After: the client is told to come back but not when")
	}
}

func TestWriteCommitErr(t *testing.T) {
	cases := []struct {
		name           string
		err            error
		wantCode       int
		wantRetryAfter bool
	}{
		{"retries exhausted", fmt.Errorf("stop updating library x: %w", ErrRetriesExhausted),
			http.StatusServiceUnavailable, true},
		// A GC conflict is retried identically, so it is told apart from a lost
		// branch-head race only in the log — never by the status, which would
		// have to be 409, which now means "rename and retry".
		{"gc conflict", fmt.Errorf("wrapped: %w", ErrGCConflict), http.StatusServiceUnavailable, true},
		// A real failure must still read as one, or the fix has only moved the
		// lie in the other direction.
		{"genuine failure", errors.New("disk on fire"), http.StatusInternalServerError, false},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeCommitErr(w, httptest.NewRequest("PUT", "/x", nil), tt.err, "write")

			if w.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", w.Code, tt.wantCode)
			}
			if got := w.Header().Get("Retry-After") != ""; got != tt.wantRetryAfter {
				t.Errorf("Retry-After present = %v, want %v", got, tt.wantRetryAfter)
			}
			if w.Body.Len() == 0 {
				t.Error("no body: the client is told a number and nothing else")
			}
		})
	}
}

// The 14-second median wait in the report was ten sleeps averaging 1.55s each.
// A client with a request timeout shorter than that never sees the status at
// all, only a timeout, and has even less to go on than a wrong status code.
func TestContentionBackoffIsBounded(t *testing.T) {
	const ceiling = time.Second

	var worst time.Duration
	for attempt := range 20 {
		// Sampled, because the value is jittered — the ceiling has to hold for
		// every draw, not on average.
		for range 200 {
			d := contentionBackoff(attempt)
			if d < 0 || d >= ceiling {
				t.Fatalf("contentionBackoff(%d) = %v, outside [0, %v)", attempt, d, ceiling)
			}
			if d > worst {
				worst = d
			}
		}
	}

	// Ten attempts at the old flat 100–3000ms could hold a connection for 30
	// seconds before answering. Every window is capped at the ceiling, so the
	// whole budget is bounded by attempts × ceiling.
	if budget := time.Duration(option.CommitAttempts) * ceiling; budget > 10*time.Second {
		t.Errorf("worst-case retry budget %v is back to holding the request path", budget)
	}
	if worst == 0 {
		t.Error("every sampled backoff was zero — the jitter is not jittering")
	}
}
