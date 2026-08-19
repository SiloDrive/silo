package silod

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/commitmgr"
	"github.com/dkam/silo/fileserver/fsmgr"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/repomgr"
)

const contendedRepo = "7c1e9b40-5f2a-4d63-9a18-0b7e6c3f5d21"

// contendedRepoTestDB seeds one library with an empty tree and a real head
// commit, ready to be written to concurrently.
func contendedRepoTestDB(t *testing.T) *commitmgr.Commit {
	t.Helper()

	sqliteTestDB(t)
	repomgr.Init(seafilePair.Read, seafilePair.Write)

	confPath := t.TempDir()
	dataDir := filepath.Join(confPath, "seafile-data")
	fsmgr.Init(confPath, dataDir, option.FsCacheLimit)
	commitmgr.Init(confPath, dataDir)

	root, err := fsmgr.NewSeafdir(1, nil)
	if err != nil {
		t.Fatalf("failed to create root dir: %v", err)
	}
	if err := fsmgr.SaveSeafdir(contendedRepo, root); err != nil {
		t.Fatalf("failed to save root dir: %v", err)
	}

	head := commitmgr.NewCommit(contendedRepo, "", root.DirID, repoOwner, "Initial commit")
	head.Version = 1
	if err := commitmgr.Save(head); err != nil {
		t.Fatalf("failed to save head commit: %v", err)
	}

	dbExec(t, "INSERT INTO Repo (repo_id) VALUES (?)", contendedRepo)
	dbExec(t, "INSERT INTO Branch (name, repo_id, commit_id) VALUES ('master', ?, ?)",
		contendedRepo, head.CommitID)
	dbExec(t, "INSERT INTO RepoHead (repo_id, branch_name) VALUES (?, 'master')", contendedRepo)
	dbExec(t, "INSERT INTO RepoOwner (repo_id, owner_id) VALUES (?, ?)", contendedRepo, repoOwner)

	return head
}

// The bug: a write that lost the race for the branch head ten times running
// came back as an ordinary error, indistinguishable from a real failure, so
// every handler answered 500. 500 is the one class a client must not retry
// blind — it means the server may have applied part of the request — and here
// nothing was applied and the same request would have succeeded a moment
// later. porter-fuse maps 5xx to EIO, and an EIO from close(2) is data loss
// from the application's point of view.
//
// Eight writers start from the same head. One wins; the rest must come back
// saying "contention", not "broken".
func TestConcurrentWritesReportContentionNotFailure(t *testing.T) {
	base := contendedRepoTestDB(t)

	// No retry budget, so a single lost race exhausts it. The retries are not
	// what is under test — what the exhaustion is *called* is.
	orig := genNewCommitRetries
	genNewCommitRetries = 0
	t.Cleanup(func() { genNewCommitRetries = orig })

	const writers = 8
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			repo := repomgr.Get(contendedRepo)
			if repo == nil {
				errs[i] = errors.New("repo vanished")
				return
			}
			_, errs[i] = GenNewCommit(repo, base, base.RootID, repoOwner,
				fmt.Sprintf("Write %d", i), true, "", false)
		}()
	}
	wg.Wait()

	var won, lost int
	for i, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrRetriesExhausted):
			lost++
		default:
			t.Errorf("writer %d failed with something other than contention: %v", i, err)
		}
	}
	if won == 0 {
		t.Fatal("no writer committed; the test is not measuring contention")
	}
	if lost == 0 {
		// Possible if the scheduler happened to run all eight serially. Not a
		// failure — TestLostRaceIsReportedAsContention covers the same path
		// without depending on a schedule — but worth saying, so a run that
		// quietly stopped racing does not read as a run that proved something.
		t.Logf("all %d writers committed; this run did not actually contend", won)
	}

	// And the whole point: what a client is told about it.
	for _, err := range errs {
		if err == nil {
			continue
		}
		w := httptest.NewRecorder()
		writeCommitErr(w, httptest.NewRequest("PUT", "/api/silo/v1/repos/x/entries/f", nil), err, "write")
		if w.Code == http.StatusInternalServerError {
			t.Fatalf("a contended write still tells the client the server is broken: %d", w.Code)
		}
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
		}
	}
}

func TestWriteCommitErr(t *testing.T) {
	cases := []struct {
		name           string
		err            error
		wantCode       int
		wantRetryAfter bool
	}{
		{"retries exhausted", fmt.Errorf("stop updating repo x: %w", ErrRetriesExhausted),
			http.StatusServiceUnavailable, true},
		// The non-replace upload path returns this bare, one level further out.
		{"conflict", ErrConflict, http.StatusServiceUnavailable, true},
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
	if budget := time.Duration(genNewCommitRetries) * ceiling; budget > 10*time.Second {
		t.Errorf("worst-case retry budget %v is back to holding the request path", budget)
	}
	if worst == 0 {
		t.Error("every sampled backoff was zero — the jitter is not jittering")
	}
}

// The same exhaustion, without depending on a schedule: commit once so the
// branch moves, then hand GenNewCommit a head that is one commit behind. Its
// compare-and-swap cannot match, so the retry budget is guaranteed to run out.
func TestLostRaceIsReportedAsContention(t *testing.T) {
	base := contendedRepoTestDB(t)

	orig := genNewCommitRetries
	genNewCommitRetries = 0
	t.Cleanup(func() { genNewCommitRetries = orig })

	repo := repomgr.Get(contendedRepo)
	if repo == nil {
		t.Fatal("seeded repo did not load")
	}
	if _, err := GenNewCommit(repo, base, base.RootID, repoOwner, "The winner", true, "", false); err != nil {
		t.Fatalf("uncontended write failed: %v", err)
	}

	// repo still carries the pre-commit head, which is exactly the state a
	// writer that lost the race is holding.
	_, err := GenNewCommit(repo, base, base.RootID, repoOwner, "The loser", true, "", false)
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
