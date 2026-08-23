package silod

import (
	"errors"
	"fmt"
	"net/http"
	upath "path"

	"github.com/dkam/silo/fileserver/objmgr"
	"github.com/dkam/silo/fileserver/repomgr"
	"github.com/dkam/silo/store"
)

// batchV2 applies a whole batch to a store-v2 library and commits it once.
//
// The shape the Seafile lane has to build by hand — a working root that each
// operation advances, committed only if every one of them succeeded — is what
// mutateTree's closure already is. So the batch runs inside one attempt: if
// the head moves underneath it, the entire batch re-applies to the head that
// won rather than being merged into it, which is the same all-or-nothing
// promise the endpoint makes, extended to cover contention as well as failure.
//
// Content is the exception, and it has to be. Chunks named by a create are
// looked up and turned into a manifest BEFORE the closure runs, because a
// retry must not re-read and re-store content it already has — the manifest id
// is content-addressed and identical on the second pass anyway.
func batchV2(w http.ResponseWriter, r *http.Request, repo *repomgr.Repo, user string, ops []batchOp) {
	st, err := repo.Store()
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Manifests first, indexed by the operation that needs one. A create in an
	// E2EE library is refused here rather than inside the closure so the batch
	// fails before anything is written.
	manifests := make(map[int]store.ID, len(ops))
	for i, op := range ops {
		if op.Op != "create" {
			continue
		}
		id, fail := chunkManifest(st, repo, op.Blocks)
		if fail != nil {
			batchFailed(w, i, op, fail)
			return
		}
		manifests[i] = id
	}

	var failedAt int
	var failure *batchFailure
	root, changed, err := mutateTree(repo, user, func(st *objmgr.Store, start store.ID, now int64) (store.ID, error) {
		failure = nil
		root := start
		for i, op := range ops {
			next, fail := applyBatchOpV2(st, root, op, manifests[i], now)
			if fail != nil {
				failedAt, failure = i, fail
				// Abandon the attempt by returning the tree as it was, NOT the
				// working root — the ops before this one succeeded against it,
				// and handing that back would commit a half-applied batch.
				// mutateTree reads an unchanged root as "nothing happened" and
				// mints no commit, which is what all-or-nothing means here.
				return start, nil
			}
			root = next
		}
		return root, nil
	})
	if failure != nil {
		batchFailed(w, failedAt, ops[failedAt], failure)
		return
	}
	if err != nil {
		writeCommitErr(w, r, err, fmt.Sprintf("batch of %d operations in repo %s", len(ops), repo.ID))
		return
	}

	// changed is false for a batch of moves that all land where they started,
	// or a mkdir of a directory that exists. mutateTree mints no commit for an
	// identical tree — one saying nothing happened is worse than saying so —
	// and it is the one that knows, so it is the one that reports it.
	after, err := repomgr.GetWithReason(repo.ID)
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("ETag", `"`+etagPrefix+root.String()+`"`)
	writeEntryJSON(w, http.StatusOK, map[string]any{
		"commit_id": after.HeadCommitID,
		"ops":       len(ops),
		"changed":   changed,
	})
}

func batchFailed(w http.ResponseWriter, i int, op batchOp, fail *batchFailure) {
	writeEntryJSON(w, fail.code, map[string]any{
		"error": fail.message,
		"index": i,
		"op":    op.Op,
		"path":  op.Path,
	})
}

// applyBatchOpV2 performs one operation against a working root.
//
// Every lookup is against that working root rather than the head, which is
// what makes an ordered batch mean anything: an operation has to see what the
// ones before it did.
func applyBatchOpV2(st *objmgr.Store, root store.ID, op batchOp, manifest store.ID, now int64) (store.ID, *batchFailure) {
	path := entryPath(op.Path)

	switch op.Op {
	case "mkdir":
		return v2Result(st.Mkdir(root, path, defaultDirMode, now))
	case "delete":
		return v2Result(st.Remove(root, path, now))
	case "move":
		return v2Result(st.Rename(root, path, entryPath(op.To), now))
	case "copy":
		node, err := st.Resolve(root, path)
		if err != nil {
			return v2Result(store.ID{}, err)
		}
		// A copy is a move with the delete left off: the new entry points at
		// the object the source already names, so no content moves and a
		// directory copies in constant time however large it is.
		dst := entryPath(op.To)
		return v2Result(st.PutNode(root, dst, objmgr.Node{
			ID: node.ID, Type: node.Type, Name: upath.Base(dst),
			Mtime: now, Mode: node.Mode,
		}, now))
	case "create":
		return v2Result(st.PutNode(root, path, objmgr.Node{
			ID: manifest, Type: store.NodeFile, Name: upath.Base(path),
			Mtime: now, Mode: defaultFileMode,
		}, now))
	default:
		return store.ID{}, &batchFailure{http.StatusBadRequest,
			fmt.Sprintf("unknown op %q", op.Op)}
	}
}

// v2Result turns an objmgr error into the status the same operation would have
// answered on its own, so a client that already handles 404 on a move does not
// learn a second vocabulary for batches.
func v2Result(id store.ID, err error) (store.ID, *batchFailure) {
	if err == nil {
		return id, nil
	}
	switch {
	case errors.Is(err, objmgr.ErrNotFound):
		return store.ID{}, &batchFailure{http.StatusNotFound, err.Error()}
	case errors.Is(err, objmgr.ErrExists):
		return store.ID{}, &batchFailure{http.StatusConflict, "Entry already exists"}
	case errors.Is(err, objmgr.ErrIsDir):
		return store.ID{}, &batchFailure{http.StatusConflict, "That path is a directory"}
	case errors.Is(err, objmgr.ErrNotDir):
		return store.ID{}, &batchFailure{http.StatusConflict, "A path component is not a directory"}
	case errors.Is(err, objmgr.ErrInvalidPath):
		return store.ID{}, &batchFailure{http.StatusBadRequest, err.Error()}
	case errors.Is(err, objmgr.ErrNoContentKey):
		return store.ID{}, &batchFailure{http.StatusForbidden, errE2EEWriteByID}
	}
	return store.ID{}, &batchFailure{http.StatusInternalServerError, "Failed to apply the operation"}
}

// chunkManifest builds and stores the manifest for a create, from chunks the
// client says are already here.
func chunkManifest(st *objmgr.Store, repo *repomgr.Repo, blocks []string) (store.ID, *batchFailure) {
	if repo.Format.E2EE {
		return store.ID{}, &batchFailure{http.StatusForbidden, errE2EEWriteByID}
	}
	ids := make([]store.ID, 0, len(blocks))
	for _, raw := range blocks {
		id, err := store.ParseID(raw)
		if err != nil {
			return store.ID{}, &batchFailure{http.StatusBadRequest, "Not a chunk id: " + raw}
		}
		ids = append(ids, id)
	}

	refs, missing, err := chunkRefs(st, ids)
	if err != nil {
		return store.ID{}, &batchFailure{http.StatusInternalServerError, "Failed to inspect the chunks"}
	}
	if len(missing) > 0 {
		// The same instruction as the single-file path: upload these, then send
		// the identical request again. A batch is all-or-nothing, so "again"
		// means the whole batch, which is why the reply names the chunks rather
		// than only the operation.
		return store.ID{}, &batchFailure{http.StatusFailedDependency,
			fmt.Sprintf("%d chunks are not on the server, starting with %s; upload them and retry",
				len(missing), missing[0])}
	}

	var size int64
	for _, ref := range refs {
		size += ref.Size
	}
	if overBound(size) {
		return store.ID{}, &batchFailure{http.StatusRequestEntityTooLarge, "File is too large"}
	}

	m, fail := manifestFor(st, size, refs)
	if fail != nil {
		return store.ID{}, fail
	}
	id, err := st.PutManifest(m)
	if err != nil {
		return store.ID{}, &batchFailure{http.StatusInternalServerError, "Failed to write the manifest"}
	}
	return id, nil
}
