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

// batchErr carries a refused operation out of mutateTree's closure.
//
// The closure's signature already returns an error, and mutateTree already
// treats one as "abandon the attempt: no commit, no retry, tree untouched" —
// which is exactly what a batch needs when an operation fails. Passing the
// refusal out as an error rather than through variables captured beside the
// closure means the all-or-nothing promise rests on the signature rather than
// on a reader noticing that returning the starting root happens to mint no
// commit.
type batchErr struct {
	at   int
	fail *batchFailure
}

func (e *batchErr) Error() string {
	return fmt.Sprintf("operation %d: %s", e.at, e.fail.message)
}

// prepOp is one operation with everything that does not depend on the tree
// already worked out.
//
// The split is what keeps the retry cheap, and it is worth being deliberate
// about which side each piece falls on. Anything a lost race would make the
// server do twice belongs out here: parsing paths, checking names, and above
// all turning a create's chunk list into a stored manifest, which is content
// the server would otherwise read and hash again on every attempt. What is
// left inside is the tree rewrite, which is the one thing that genuinely has
// to be re-applied to whichever root won.
type prepOp struct {
	path     string
	to       string
	manifest store.ID
	// size is what a create adds, kept so the batch can be weighed against
	// quota once, before the tree is touched.
	size int64
}

// batchV2 applies a whole batch to a store-v2 library and commits it once.
//
// The shape a per-operation commit loop has to build by hand — a working root that each
// operation advances, committed only if every one of them succeeded — is what
// mutateTree's closure already is. So the batch runs inside one attempt: if
// the head moves underneath it, the entire batch re-applies to the head that
// won rather than being merged into it, which is the same all-or-nothing
// promise the endpoint makes, extended to cover contention as well as failure.
func batchV2(w http.ResponseWriter, r *http.Request, repo *repomgr.Repo, user string, ops []batchOp) {
	st, err := repo.Store()
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	prepped, ok := prepBatch(w, st, ops)
	if !ok {
		return
	}

	// Weighed once, before the tree is touched, because a batch is
	// all-or-nothing: refusing halfway would mean refusing operations that
	// have no bytes in them at all.
	//
	// Only the creates are counted. A copy adds bytes too — the same content
	// twice is charged twice, since deleting one copy has to give its bytes
	// back — but its size is not known until the tree is walked, and walking
	// it here would cost the batch a lookup per operation to refuse a request
	// the next one refuses anyway. Quota is exceeded by at most one request in
	// this design regardless: the moment the head moves the tree is measured
	// exactly, and the write after this one is refused.
	var adds int64
	for _, p := range prepped {
		adds += p.size
	}
	if refuseOverQuota(w, repo, adds) {
		return
	}

	root, commit, err := mutateTree(repo, user, func(st *objmgr.Store, start store.ID, now int64) (store.ID, error) {
		root := start
		for i, op := range ops {
			next, fail := applyBatchOpV2(st, root, op.Op, prepped[i], now)
			if fail != nil {
				return store.ID{}, &batchErr{i, fail}
			}
			root = next
		}
		return root, nil
	})
	var refused *batchErr
	if errors.As(err, &refused) {
		batchFailed(w, refused.at, ops[refused.at], refused.fail)
		return
	}
	if err != nil {
		writeCommitErr(w, r, err, fmt.Sprintf("batch of %d operations in repo %s", len(ops), repo.ID))
		return
	}

	// A zero commit id is a batch that changed nothing — a set of moves that
	// all land where they started, say. mutateTree mints no commit for an
	// identical tree, so the head is still the one this request loaded, and
	// reporting that is more honest than a commit saying nothing happened.
	head := repo.HeadCommitID
	changed := commit != store.ID{}
	if changed {
		head = commit.String()
	}
	w.Header().Set("ETag", `"`+etagPrefix+root.String()+`"`)
	writeEntryJSON(w, http.StatusOK, map[string]any{
		"commit_id": head,
		"ops":       len(ops),
		"changed":   changed,
	})
}

// prepBatch does every part of a batch that the tree has no say in, and
// answers the client itself if any of it fails.
//
// Running first is the point. A batch may name a thousand operations, and one
// with a typo in the last verb used to be discovered only after the other 999
// had built manifests and rewritten the tree — all of it thrown away. Nothing
// here touches the tree, so nothing here can be undone.
func prepBatch(w http.ResponseWriter, st *objmgr.Store, ops []batchOp) ([]prepOp, bool) {
	prepped := make([]prepOp, len(ops))
	for i, op := range ops {
		p := prepOp{path: entryPath(op.Path)}

		// The name an operation creates is checked here rather than left to
		// the mutation layer, because objmgr.SplitPath rejects only "." and
		// "..". Length, encoding and the ignore list are this package's rule,
		// and validEntryName is where it lives —
		// and every single-op v2 handler is called with it already applied.
		// Without this the batch is the one route by which a name nothing
		// downstream expects enters a store-v2 library.
		var creates string
		switch op.Op {
		case "mkdir", "create":
			creates = p.path
		case "move", "copy":
			p.to = entryPath(op.To)
			creates = p.to
		case "delete":
		default:
			batchFailed(w, i, op, &batchFailure{http.StatusBadRequest, errUnsupportedOp})
			return nil, false
		}
		if creates != "" && !validEntryName(upath.Base(creates)) {
			batchFailed(w, i, op, &batchFailure{http.StatusBadRequest, "Invalid name"})
			return nil, false
		}

		if op.Op == "create" {
			id, size, missing, fail := chunkManifest(st, op.Blocks)
			if len(missing) > 0 {
				// The same instruction as the single-file path: upload these,
				// then send the identical request again. A batch is
				// all-or-nothing, so "again" means the whole batch, which is
				// why the reply names the chunks rather than only the
				// operation.
				fail = &batchFailure{http.StatusFailedDependency,
					fmt.Sprintf("%d chunks are not on the server, starting with %s; upload them and retry",
						len(missing), missing[0])}
			}
			if fail != nil {
				batchFailed(w, i, op, fail)
				return nil, false
			}
			p.manifest = id
			p.size = size
		}
		prepped[i] = p
	}
	return prepped, true
}

// batchFailed answers a refused operation. Both lanes of POST /batch use it,
// so a client does not need two parsers for one endpoint.
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
func applyBatchOpV2(st *objmgr.Store, root store.ID, verb string, p prepOp, now int64) (store.ID, *batchFailure) {
	switch verb {
	case "mkdir":
		return v2Result(st.Mkdir(root, p.path, defaultDirMode, now))
	case "delete":
		return v2Result(st.Remove(root, p.path, now))
	case "move":
		return v2Result(st.Rename(root, p.path, p.to, now))
	case "copy":
		// Rename refuses this for "move" on its own (from has no segments to
		// split a leaf from); copy has no such gate, since it resolves and
		// puts rather than splitting a leaf, and Resolve(root, "/") happily
		// returns the root node. Left unchecked, this would alias the tree's
		// own root into a subpath of itself.
		if p.path == "/" {
			return store.ID{}, &batchFailure{http.StatusBadRequest, "The library root cannot be copied"}
		}
		node, err := st.Resolve(root, p.path)
		if err != nil {
			return v2Result(store.ID{}, err)
		}
		// A copy is a move with the delete left off: the new entry points at
		// the object the source already names, so no content moves and a
		// directory copies in constant time however large it is.
		return v2Result(st.PutNode(root, p.to, objmgr.Node{
			ID: node.ID, Type: node.Type, Name: upath.Base(p.to),
			Mtime: now, Mode: node.Mode,
		}, now))
	case "create":
		return v2Result(st.PutNode(root, p.path, objmgr.Node{
			ID: p.manifest, Type: store.NodeFile, Name: upath.Base(p.path),
			Mtime: now, Mode: defaultFileMode,
		}, now))
	}
	// prepBatch refused anything not in that set before the tree was touched,
	// so this arm is unreachable — and says so rather than quietly treating an
	// unknown verb as whichever case it fell into.
	return store.ID{}, &batchFailure{http.StatusInternalServerError, errUnsupportedOp}
}

// v2Result turns an objmgr error into the status the same operation would have
// answered on its own — the same table writeTreeErr puts on the wire, because
// it is the same table.
func v2Result(id store.ID, err error) (store.ID, *batchFailure) {
	if err == nil {
		return id, nil
	}
	if fail := treeFailure(err, err.Error()); fail != nil {
		return store.ID{}, fail
	}
	return store.ID{}, &batchFailure{http.StatusInternalServerError, "Failed to apply the operation"}
}
