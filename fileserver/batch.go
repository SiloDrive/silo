package silod

import (
	"fmt"
	"net/http"
	upath "path"
	"syscall"
	"time"

	"github.com/dkam/silo/fileserver/fsmgr"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/repomgr"
	"github.com/dkam/silo/fileserver/utils"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// Batch: many operations, one commit.
//
//	POST /api/silo/v1/repos/{repo}/batch   {"ops":[…]}
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

func batchHandler(w http.ResponseWriter, r *http.Request) {
	acct := middleware.GetAccount(r)
	user := acct.Email
	repoID := mux.Vars(r)["repoid"]

	repo, head, ok := loadRepoAndCommit(w, repoID, acct.ID)
	if !ok {
		return
	}

	var body struct {
		Ops []batchOp `json:"ops"`
	}
	if appErr := decodeLimitedJSON(w, r, maxBatchBody, &body); appErr != nil {
		if appErr.Code == http.StatusBadRequest {
			appErr.Message = `Expected a JSON body such as {"ops":[{"op":"mkdir","path":"/new"}]}`
		}
		http.Error(w, appErr.Message, appErr.Code)
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
	if !preconditionsHold(w, r, repo, "/") {
		return
	}

	// Read before any block is looked at, so a GC starting mid-batch is caught
	// as a conflict rather than leaving a commit that names reclaimed blocks.
	gcID, err := repomgr.GetCurrentGCID(repo.StoreID)
	if err != nil {
		log.WithContext(r.Context()).WithError(err).Errorf("failed to get gc id for repo %s", repoID)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	root := head.RootID
	for i, op := range body.Ops {
		next, fail := applyBatchOp(repo, root, user, op)
		if fail != nil {
			// Nothing is committed, so nothing has to be undone: every tree
			// written so far is an unreferenced object, which is exactly what
			// an abandoned upload leaves behind and is handled the same way.
			writeEntryJSON(w, fail.code, map[string]any{
				"error": fail.message,
				"index": i,
				"op":    op.Op,
				"path":  op.Path,
			})
			return
		}
		root = next
	}

	// A batch of moves that all land where they started, or a mkdir of a
	// directory that exists. Committing an identical tree would put a commit
	// in the history that says nothing happened, which is worse than saying so.
	if root == head.RootID {
		w.Header().Set("ETag", `"`+etagPrefix+root+`"`)
		writeEntryJSON(w, http.StatusOK, map[string]any{
			"commit_id": head.CommitID, "ops": len(body.Ops), "changed": false,
		})
		return
	}

	// handleConncurrentUpdate false: a batch that lost the race answers 503
	// with Retry-After rather than being merged into whatever arrived first.
	// Merging is right for uploads, which only add; a batch can delete and move,
	// and a three-way merge of those against an unseen commit is a guess. The
	// client re-reads and rebuilds, which is what its If-Match asked for.
	desc := fmt.Sprintf("Batch of %d operations", len(body.Ops))
	commitID, err := GenNewCommit(repo, head, root, user, desc, false, gcID, true)
	if err != nil {
		writeCommitErr(w, r, err, fmt.Sprintf("batch of %d operations in repo %s", len(body.Ops), repoID))
		return
	}

	w.Header().Set("ETag", `"`+etagPrefix+root+`"`)
	writeEntryJSON(w, http.StatusOK, map[string]any{
		"commit_id": commitID, "ops": len(body.Ops), "changed": true,
	})
}

// applyBatchOp performs one operation against a working root and returns the
// root that results. It is the single-operation handlers with the HTTP taken
// out: the same guards, in the same order, reported to the caller instead of to
// a ResponseWriter.
//
// Every lookup is against the working root rather than the head, which is what
// makes an ordered batch mean anything — an operation has to see what the ones
// before it did.
func applyBatchOp(repo *repomgr.Repo, root, user string, op batchOp) (string, *batchFailure) {
	path := entryPath(op.Path)

	switch op.Op {
	case "mkdir":
		return batchMkdir(repo, root, user, path)
	case "delete":
		return batchDelete(repo, root, path)
	case "move", "copy":
		return batchMoveOrCopy(repo, root, user, path, entryPath(op.To), op.Op == "copy")
	case "create":
		return batchCreate(repo, root, user, path, op.Blocks)
	default:
		return "", &batchFailure{http.StatusBadRequest,
			`Unsupported op; the operations are "mkdir", "delete", "move", "copy" and "create"`}
	}
}

func batchMkdir(repo *repomgr.Repo, root, user, path string) (string, *batchFailure) {
	parentDir, name := splitEntry(path)
	if !validEntryName(name) {
		return "", &batchFailure{http.StatusBadRequest, "Invalid name"}
	}
	if fail := requireParent(repo, root, parentDir); fail != nil {
		return "", fail
	}
	// Already a directory: the operation asked for a state that holds, so the
	// batch continues. A batch is a description of where the library should
	// end up, and failing here would make "create the folder if it is missing"
	// impossible to express in one request.
	if existing, err := lookup(repo, root, path); err == nil && existing != nil {
		if fsmgr.IsDir(existing.Mode) {
			return root, nil
		}
		return "", &batchFailure{http.StatusConflict, "A file already exists at " + path}
	}

	mode := uint32(syscall.S_IFDIR | 0644)
	dent := fsmgr.NewDirent(fsmgr.EmptySha1, name, mode, time.Now().Unix(), "", 0)
	return postToTree(repo, root, parentDir, dent, user, false)
}

func batchDelete(repo *repomgr.Repo, root, path string) (string, *batchFailure) {
	if path == "/" {
		return "", &batchFailure{http.StatusBadRequest, "The library root cannot be deleted; delete the library instead"}
	}
	parentDir, name := splitEntry(path)
	newRoot, err := DelFileFromTree(repo.StoreID, root, parentDir, name)
	if err != nil {
		// DelFileFromTree reports a missing entry and a broken store the same
		// way, so this is 404: the store being broken would have failed the
		// lookups above first.
		return "", &batchFailure{http.StatusNotFound, "Not found: " + path}
	}
	return newRoot, nil
}

func batchMoveOrCopy(repo *repomgr.Repo, root, user, src, dst string, isCopy bool) (string, *batchFailure) {
	if src == "/" {
		return "", &batchFailure{http.StatusBadRequest, "The library root cannot be the source of a move or copy"}
	}
	if src == dst {
		return "", &batchFailure{http.StatusBadRequest, "src and dst are the same"}
	}
	dstDir, dstName := splitEntry(dst)
	if !validEntryName(dstName) {
		return "", &batchFailure{http.StatusBadRequest, "Invalid name"}
	}

	srcEntry, err := lookup(repo, root, src)
	if err != nil || srcEntry == nil {
		return "", &batchFailure{http.StatusNotFound, "Source not found: " + src}
	}
	// A copy names the source's object, which is the subtree as it stands, so
	// copying into itself is a snapshot of a finite thing. A move cannot be:
	// its delete would take the copy with it.
	if !isCopy && fsmgr.IsDir(srcEntry.Mode) && movesIntoOwnSubtree(src, dstDir) {
		return "", &batchFailure{http.StatusBadRequest, "Cannot move a directory into itself"}
	}
	if fail := requireParent(repo, root, dstDir); fail != nil {
		return "", fail
	}
	dstEntry, err := lookup(repo, root, dst)
	if err != nil {
		dstEntry = nil
	}
	if reason := destructiveCollision(srcEntry.Mode, dstEntry); reason != "" {
		return "", &batchFailure{http.StatusConflict, reason}
	}

	dent := fsmgr.NewDirent(srcEntry.ID, dstName, srcEntry.Mode, time.Now().Unix(), srcEntry.Modifier, srcEntry.Size)
	newRoot, fail := postToTree(repo, root, dstDir, dent, user, true)
	if fail != nil {
		return "", fail
	}
	if isCopy {
		return newRoot, nil
	}

	srcDir, srcName := splitEntry(src)
	newRoot, err = DelFileFromTree(repo.StoreID, newRoot, srcDir, srcName)
	if err != nil {
		return "", &batchFailure{http.StatusInternalServerError, "Failed to remove the source of the move"}
	}
	return newRoot, nil
}

func batchCreate(repo *repomgr.Repo, root, user, path string, blocks []string) (string, *batchFailure) {
	if repo.IsEncrypted {
		return "", &batchFailure{http.StatusBadRequest,
			"An encrypted library cannot be written block by block; PUT the file content instead"}
	}
	parentDir, name := splitEntry(path)
	if !validEntryName(name) {
		return "", &batchFailure{http.StatusBadRequest, "Invalid name"}
	}
	if fail := requireParent(repo, root, parentDir); fail != nil {
		return "", fail
	}
	for _, id := range blocks {
		if !utils.IsObjectIDValid(id) {
			return "", &batchFailure{http.StatusBadRequest, "Not a block id: " + id}
		}
	}

	missing, size, err := blockInventory(repo.StoreID, blocks)
	if err != nil {
		return "", &batchFailure{http.StatusInternalServerError, "Failed to inspect the blocks"}
	}
	if len(missing) > 0 {
		// The same instruction as the single-file path: upload these, then send
		// the identical request again. A batch is all-or-nothing, so "again"
		// means the whole batch, which is why the reply names the blocks rather
		// than only the operation.
		return "", &batchFailure{http.StatusFailedDependency,
			fmt.Sprintf("%d blocks are not on the server, starting with %s; upload them and retry", len(missing), missing[0])}
	}

	fileID, err := writeSeafile(repo.StoreID, repo.Version, size, blocks)
	if err != nil {
		return "", &batchFailure{http.StatusInternalServerError, "Failed to write the file object"}
	}
	mode := uint32(syscall.S_IFREG | 0644)
	dent := fsmgr.NewDirent(fileID, name, mode, time.Now().Unix(), user, size)
	return postToTree(repo, root, parentDir, dent, user, true)
}

// postToTree adds one dirent and returns the new root, turning the empty string
// postMultiFilesRecursive returns for a path it could not walk into an error.
// Left as it is, that empty root would be committed as the library's new state
// — every file in it gone, reported as success.
func postToTree(repo *repomgr.Repo, root, parentDir string, dent *fsmgr.SeafDirent, user string, replace bool) (string, *batchFailure) {
	var names []string
	newRoot, err := DoPostMultiFiles(repo, root, parentDir, []*fsmgr.SeafDirent{dent}, user, replace, &names)
	if err != nil || newRoot == "" {
		log.Errorf("Failed to add %s to %s in repo %s: %v", dent.Name, parentDir, repo.ID, err)
		return "", &batchFailure{http.StatusInternalServerError, "Failed to write " + dent.Name}
	}
	return newRoot, nil
}

// requireParent refuses an operation whose destination directory does not
// exist. Parents are never created implicitly on this lane — a typo in a path
// would otherwise produce a directory tree rather than an error — and a batch
// that wants one says mkdir first, which is the whole point of it being ordered.
func requireParent(repo *repomgr.Repo, root, parentDir string) *batchFailure {
	if parentDir == "/" {
		return nil
	}
	parent, err := lookup(repo, root, parentDir)
	if err != nil || parent == nil || !fsmgr.IsDir(parent.Mode) {
		return &batchFailure{http.StatusNotFound, "Parent directory does not exist: " + parentDir}
	}
	return nil
}

func lookup(repo *repomgr.Repo, root, path string) (*fsmgr.SeafDirent, error) {
	return fsmgr.GetDirentByPath(repo.StoreID, root, path)
}

// splitEntry divides a rooted path into the directory holding it and its name,
// with the directory in the form the tree walkers expect.
func splitEntry(path string) (parentDir, name string) {
	return entryPath(upath.Dir(path)), upath.Base(path)
}
