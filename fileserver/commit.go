package silod

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"time"

	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/notif"
	"github.com/dkam/silo/fileserver/objmgr"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/store"
	log "github.com/sirupsen/logrus"
)

// contentionBackoff is how long to wait before retry number attempt (0-based)
// of a write that lost the race for the branch head: exponential from 50ms to
// a 1s ceiling, with full jitter.
//
// It replaces a flat "random 100–3000 ms". That made the first retry wait an
// average of 1.55s to re-take a head the winning writer had claimed in 20ms,
// and it put ten such waits on the request path — a contended write could hold
// a connection for 30 seconds before answering, which is most of why the
// failures in docs/bugs/fixed/write-contention-returns-500.md took a median of
// 14 seconds to arrive. Full jitter (uniform over [0, window), not the window
// itself) is the part that actually spreads a thundering herd; the doubling is
// what stops a persistent loser from hammering.
func contentionBackoff(attempt int) time.Duration {
	const base, ceiling = 50 * time.Millisecond, time.Second
	window := base << min(attempt, 5)
	if window > ceiling {
		window = ceiling
	}
	return time.Duration(rand.Int63n(int64(window)))
}

// commitAttempts bounds the compare-and-swap retry loop.
//
// A retry is cheap and a livelock is not: each pass re-reads the head and
// re-applies one tree mutation onto the new root, and the content it is
// committing was stored before the loop began and is not rewritten. Five
// passes is far past what contention on one library produces; beyond that the
// honest answer is that the caller is losing a race it should be told about.
//
// A var only so a test can lower it to force exhaustion without racing a
// scheduler — nothing in the server writes it. One name rather than a const
// and a shadowing var, so the loop and the message it ends with cannot report
// different budgets.
var commitAttempts = 5

// errE2EEWriteByID is what a client is told when it asks the server to write
// into an end-to-end encrypted library through a path. Spelled once because it
// is a wire string three handlers hand back, and three copies of a sentence a
// client may match on is three chances for them to stop being the same
// sentence.
const errE2EEWriteByID = "This library is end-to-end encrypted; write its objects by id"

// writeTreeErr answers a failed tree mutation.
//
// It exists for the reason writeCommitErr does, one layer up: the objmgr
// sentinels that deserve an answer other than 500 are a fixed set, and every
// handler that mutates a tree was deciding the set again. The ones still to be
// written — rename, copy, move — would each have decided it a fourth time,
// and the arm that goes missing is never the first one.
//
// notFound is a parameter because the 404 is the one answer that is genuinely
// about the request: a mkdir says the parent is missing, a delete says the
// entry is. Everything below it is about the library, and is the same
// everywhere. Anything unrecognised falls through to writeCommitErr, which
// owns the contention-versus-breakage decision and keeps it.
func writeTreeErr(w http.ResponseWriter, r *http.Request, err error, notFound, what string) {
	if fail := treeFailure(err, notFound); fail != nil {
		http.Error(w, fail.message, fail.code)
		return
	}
	writeCommitErr(w, r, err, what)
}

// treeFailure is that fixed set as a value rather than a response, for the
// callers that cannot let the check answer for them — a batch names the
// operation that failed, so it needs the status without the writing.
//
// It is the same table for both, which is the point: before this, a batch had
// its own copy, and the copies had already diverged. Writing a file over an
// existing directory answered 409 inside a batch and 500 outside it, on the
// same library. nil means "not one of these", and the caller decides
// what an unrecognised error is — for the wire that is writeCommitErr, which
// owns the contention-versus-breakage decision and keeps it.
func treeFailure(err error, notFound string) *batchFailure {
	switch {
	case errors.Is(err, objmgr.ErrExists):
		return &batchFailure{http.StatusConflict, "Entry already exists"}
	case errors.Is(err, objmgr.ErrNotFound):
		return &batchFailure{http.StatusNotFound, notFound}
	case errors.Is(err, objmgr.ErrIsDir):
		return &batchFailure{http.StatusConflict, "That path is a directory"}
	case errors.Is(err, objmgr.ErrNotDir):
		return &batchFailure{http.StatusConflict, "A path component is not a directory"}
	case errors.Is(err, objmgr.ErrInvalidPath):
		return &batchFailure{http.StatusBadRequest, err.Error()}
	case errors.Is(err, objmgr.ErrNoContentKey), errors.Is(err, objmgr.ErrSealedChunks):
		return &batchFailure{http.StatusForbidden, errE2EEWriteByID}
	}
	return nil
}

// defaultFileMode is the mode a server-written regular file gets. The format
// carries permission bits only — file type lives in NodeType — so this is the
// ordinary 0644 and nothing more.
const defaultFileMode = 0o644

// defaultDirMode is the mode a server-created directory gets. Permission bits
// only — a dirent records the entry's type in NodeType, so the
// S_IFDIR that a stat-shaped mode ors in here has no place in it.
const defaultDirMode = 0o755

// mutateTree applies one change to a library's tree and moves the
// head to a commit describing it, retrying if it loses the race for the head.
//
// The difference worth naming is what it does NOT do. The lane this replaced,
// on losing the head, merged: it read both trees and reconciled them. That cannot exist
// here, because merging trees means reading names, and in an E2EE library the
// server cannot. So a lost race re-applies the same mutation to the new root
// and swaps again — which is the same loop a client runs against PUT head, and
// it is what "no server-side merge" means in practice rather than in the
// abstract.
//
// The mutation is a function of the root rather than a value because that is
// exactly what makes the retry correct: on the second pass it must be applied
// to the head that won, not re-proposed against the one that lost. It is
// handed the attempt's timestamp too, so the mtime a dirent records and the
// commit's created_at are the same instant rather than two calls to the clock.
//
// The commit id comes back because this is the function that has it, and a
// caller that needs it otherwise re-reads the library row to recover a value
// that was just written — and can read back a different writer's commit that
// landed in between. A zero id means nothing was committed, which is a real
// outcome rather than a failure: a mutation that changed nothing mints no
// commit, so the head is still the one the caller already holds.
//
// An error from the mutation abandons the whole attempt: no commit, no retry,
// and the tree is left exactly as it was. That is what makes it safe for a
// caller applying several changes at once to stop partway.
func mutateTree(library *libmgr.Library, author string, mutate func(st *objmgr.Store, root store.ID, now int64) (store.ID, error)) (store.ID, store.ID, error) {
	st, err := library.Store()
	if err != nil {
		return store.ID{}, store.ID{}, err
	}

	head := library
	for attempt := 0; attempt < commitAttempts; attempt++ {
		// Read before the mutation, so a GC that starts mid-write is caught by
		// the generation check below rather than racing the objects this is
		// about to publish.
		gcID, err := libmgr.GetCurrentGCID(head.StoreID)
		if err != nil {
			return store.ID{}, store.ID{}, fmt.Errorf("failed to read gc id: %w", err)
		}

		oldRoot, err := store.ParseID(head.RootID)
		if err != nil {
			return store.ID{}, store.ID{}, err
		}
		now := time.Now().Unix()
		newRoot, err := mutate(st, oldRoot, now)
		if err != nil {
			return store.ID{}, store.ID{}, err
		}

		// A mutation that changed nothing mints no commit. The tree is
		// content-addressed, so an identical root is not merely equivalent to
		// the old one, it IS the old one — and a commit whose root equals its
		// parent's would make changes?since= report a modification that did
		// not happen.
		if newRoot == oldRoot {
			return oldRoot, store.ID{}, nil
		}

		parent, err := store.ParseID(head.HeadCommitID)
		if err != nil {
			return store.ID{}, store.ID{}, err
		}
		commitID, err := st.PutCommit(&store.Commit{
			Root:      newRoot,
			Parents:   []store.ID{parent},
			CreatedAt: now,
			Author:    author,
		})
		if err != nil {
			return store.ID{}, store.ID{}, err
		}

		err = updateBranch(library.ID, head.StoreID, headMove{
			CommitID: commitID.String(),
			RootID:   newRoot.String(),
			Author:   author,
			Ctime:    now,
		}, head.HeadCommitID, gcID)
		if err == nil {
			return newRoot, commitID, nil
		}
		if errors.Is(err, ErrGCConflict) {
			return store.ID{}, store.ID{}, err
		}

		// Lost the head, or lost it to a GC generation change. Re-read and
		// rebuild on whatever won. The objects already written stay written:
		// they are addressed by content, so the next pass reuses them and the
		// ones the losing commit orphaned are the collector's business.
		head, err = libmgr.GetWithReason(library.ID)
		if err != nil {
			return store.ID{}, store.ID{}, err
		}
		// Jittered, so eight writers that arrived together do not re-collide
		// in lockstep. Bounded, so the whole budget stays on the request path
		// rather than holding a connection for half a minute.
		time.Sleep(contentionBackoff(attempt))
	}
	// Wrapped in a sentinel rather than left bare, so
	// writeCommitErr classifies this as contention rather than breakage: the
	// caller lost a race, nothing was applied, and the identical request will
	// usually succeed. Left bare it fell to the default arm — a 500 with no
	// Retry-After, filed to Sentry — which is exactly the outcome
	// docs/bugs/fixed/write-contention-returns-500.md exists to prevent.
	return store.ID{}, store.ID{}, fmt.Errorf("gave up after %d attempts to move the head of %s: %w", commitAttempts, library.ID, ErrRetriesExhausted)
}

// ErrGCConflict and ErrRetriesExhausted are the two ways a write can lose
// rather than break.
var (
	ErrGCConflict = errors.New("GC Conflict")
	// ErrRetriesExhausted is a write that kept losing the race for the branch
	// head until its retry budget ran out. It is contention, not breakage:
	// nothing was applied, and the same request will usually succeed on a
	// retry. It exists so callers can tell that apart from a real failure —
	// without it the exhaustion case is an ordinary error and every HTTP
	// handler answers 500, which is the one class a client must not retry.
	ErrRetriesExhausted = errors.New("write contention: retries exhausted")
)

// writeCommitErr answers a request whose commit failed, and says so in the log
// exactly once. It lives beside the sentinels because every handler that
// commits needs it, and two copies of this decision would drift — the same
// reason libmgr.StatusFor exists.
//
// The distinction that matters to a client is contention versus breakage. 500
// is the one class a client must not retry blind: it means the server hit an
// unexpected condition and may have applied part of the request, so the safe
// response is to stop and surface it. A lost race for the branch head is the
// opposite — nothing was applied, and the identical request will usually
// succeed a moment later.
//
// 503 rather than 409, because 409 is not free: docs/bugs/fixed/
// move-onto-directory-destroys-it.md gave it to destination collisions, and
// Porter maps that to NSFileProviderError.filenameCollision — "return the
// existing item so the system renames". Answering a contended write with 409
// would tell a File Provider client to rename the user's file. 503 says
// transient, and Retry-After says when.
//
// A GC conflict is the same instruction wearing a different number. The commit
// lost a race with the garbage collector, nothing was applied, and the fix is
// to send the identical request again — which is what 503 already means here,
// so it goes there too rather than keeping 409 overloaded between "retry" and
// "rename".
func writeCommitErr(w http.ResponseWriter, r *http.Request, err error, what string) {
	switch {
	case errors.Is(err, ErrGCConflict), errors.Is(err, ErrRetriesExhausted):
		// Logged below error level on purpose: contention is an expected
		// outcome of concurrent writers, and errors go to Sentry. The 3000-file
		// seeding run in docs/bugs/fixed/write-contention-returns-500.md would have
		// filed 74 reports of the server working as designed.
		log.WithContext(r.Context()).WithError(err).Infof("%s lost the race for the branch head", what)
		w.Header().Set("Retry-After", "1")
		if errors.Is(err, ErrGCConflict) {
			http.Error(w, "GC conflict; retry", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "write contention; retry", http.StatusServiceUnavailable)
	default:
		log.WithContext(r.Context()).WithError(err).Errorf("%s failed", what)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// headMove is a proposed new head: the commit, the root it names, and who
// moved it when.
//
// It exists so that updateBranch takes facts rather than a commit object.
// Those four values are all it ever read out of one, so the compare-and-swap,
// the GC generation check and the catalog record are written against the facts
// rather than against the shape of whatever object supplied them.
type headMove struct {
	CommitID string
	RootID   string
	Author   string
	Ctime    int64
}

// updateBranch moves a library's head from oldCommitID to the commit move
// names, or fails.
//
// It takes a headMove rather than an id because the head and the root it names
// are one fact in two columns, and they are written in one UPDATE: a reader
// that found them disagreeing would have no way to tell which was current. The
// same call is where the catalog learns who moved the head and when, which are
// the server's own observations rather than anything read back out of the
// commit.
func updateBranch(libraryID, storeID string, move headMove, oldCommitID, lastGCID string) error {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	trans, err := siloPair.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to start transaction: %v", err)
	}

	// The generation is the store's, not the library's: a virtual library is
	// collected with the origin it shares objects with, which is what StoreID
	// names.
	var gcID sql.NullString
	row := trans.QueryRowContext(ctx, "SELECT gc_id FROM GCID WHERE library_id = ?", storeID)
	if err := row.Scan(&gcID); err != nil && err != sql.ErrNoRows {
		_ = trans.Rollback()
		return err
	}
	if lastGCID != gcID.String {
		_ = trans.Rollback()
		return fmt.Errorf("head branch update for library %s conflicts with GC: %w", libraryID, ErrGCConflict)
	}

	const name = "master"
	var commitID string
	row = trans.QueryRowContext(ctx, "SELECT commit_id FROM Branch WHERE name = ? AND library_id = ?", name, libraryID)
	if err := row.Scan(&commitID); err != nil && err != sql.ErrNoRows {
		_ = trans.Rollback()
		return err
	}
	if oldCommitID != commitID {
		_ = trans.Rollback()
		return fmt.Errorf("head commit id has changed")
	}

	if _, err := trans.ExecContext(ctx,
		"UPDATE Branch SET commit_id = ?, root_id = ? WHERE name = ? AND library_id = ?",
		move.CommitID, move.RootID, name, libraryID); err != nil {
		_ = trans.Rollback()
		return err
	}

	// In the same transaction as the head it describes: see RecordHeadMove.
	if err := libmgr.RecordHeadMove(ctx, trans, libraryID, move.Author, move.Ctime); err != nil {
		_ = trans.Rollback()
		return err
	}

	if err := trans.Commit(); err != nil {
		return fmt.Errorf("failed to commit branch update: %v", err)
	}

	onBranchUpdated(libraryID, move.CommitID)
	return nil
}

// onBranchUpdated tells whoever is listening that a library moved.
//
// Announcement only, after the transaction has committed: a listener that is
// not there is not a failed write, so there is nothing here for a caller to
// handle.
func onBranchUpdated(libraryID string, commitID string) {
	if option.EnableNotification {
		notif.NotifyLibraryUpdate(libraryID, commitID)
	}
	publishUpdateEvent(libraryID, commitID)
}
