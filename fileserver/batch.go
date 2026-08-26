package silod

import (
	"fmt"
	"net/http"

	"github.com/dkam/silo/fileserver/middleware"
	"github.com/gorilla/mux"
)

// Batch: many operations, one commit.
//
//	POST /api/silo/v1/libraries/{library}/batch   {"ops":[…]}
//
// Five hundred files dragged into a folder was five hundred requests, five
// hundred GenNewCommit calls, five hundred rounds of branch-head contention,
// and a history five hundred commits deep for one drag-and-drop. The tree
// operations underneath were already the right shape for this — each takes a
// root id and returns a new one — so the whole change is threading that root
// through a list instead of committing after every step.
//
// Paired with the block surface it covers the case it was built for: upload the
// blocks of five hundred files, which skips everything the server already
// holds, then create all five hundred in one commit.
//
// **All or nothing.** Operations apply to a working tree that exists only in
// this request; if any fails, nothing is written and the library is untouched.
// Half-applied is the one outcome a client cannot recover from, because it has
// no way to find out which half.
//
// **Ordered.** Operations see the effects of the ones before them, so a mkdir
// followed by creates inside it is a batch rather than two requests. That is
// also why a failure reports its index: "operation 7 failed" is actionable
// where "the batch failed" is not.

// maxBatchOps bounds one request. The cost of a batch is linear in its length
// and it holds a working tree for the duration, so this is where the promise
// that a request is bounded gets kept.
const maxBatchOps = 1000

const maxBatchBody = 4 << 20

type batchOp struct {
	Op     string   `json:"op"`
	Path   string   `json:"path"`
	To     string   `json:"to,omitempty"`
	Blocks []string `json:"blocks,omitempty"`
}

// batchFailure is an operation's refusal, carrying where in the list it
// happened. The status code is the one the same operation would have answered
// on its own, so a client that already handles 404 on a move does not learn a
// second vocabulary for batches.
type batchFailure struct {
	code    int
	message string
}

// errUnsupportedOp names the operations both lanes accept. One string because
// the set is one set: a client reading it after a typo must not be told
// different things depending on which lane its library is on.
const errUnsupportedOp = `Unsupported op; the operations are "mkdir", "delete", "move", "copy" and "create"`

func batchHandler(w http.ResponseWriter, r *http.Request) {
	acct := middleware.GetAccount(r)
	user := acct.Email
	libraryID := mux.Vars(r)["libraryid"]

	// Library-level, so a path-scoped credential cannot batch.
	//
	// This comment used to add "nothing mints a path-scoped credential yet",
	// which was already false when it was written: POST /auth/login passes any
	// scope a client sends straight through ParseScope, so the refusal is
	// reachable behaviour rather than a placeholder.
	//
	// The refusal still stands, but on narrower ground than the rest of that
	// comment claimed. Scope covering is a prefix test, so checking every path
	// and every destination a batch names would in fact settle it -- prepBatch
	// has them all before the tree is touched. What is genuinely undecided is
	// the root ETag precondition below, which reads the whole library. That is
	// a small question, and until it is answered this stays shut.
	library := entryLibrary(w, r, libraryID, "", true)
	if library == nil {
		return
	}

	var body struct {
		Ops []batchOp `json:"ops"`
	}
	if !decodeJSONBody(w, r, maxBatchBody, &body, `Expected a JSON body such as {"ops":[{"op":"mkdir","path":"/new"}]}`) {
		return
	}
	if len(body.Ops) == 0 {
		http.Error(w, "ops is empty; a batch that does nothing is a client bug, not a no-op", http.StatusBadRequest)
		return
	}
	if len(body.Ops) > maxBatchOps {
		http.Error(w, fmt.Sprintf("a batch is limited to %d operations", maxBatchOps), http.StatusRequestEntityTooLarge)
		return
	}

	// The precondition is about the library as a whole, which is what a batch
	// writes to. The root's id is its ETag on the entries surface already —
	// GET entries/ returns it — so "apply only if the library is still what I
	// read" is spelled the same way here as anywhere else.
	if !preconditionsHold(w, r, library, "/") {
		return
	}

	batchV2(w, r, library, user, body.Ops)
}
