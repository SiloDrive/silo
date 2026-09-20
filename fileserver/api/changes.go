package api

import (
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/SiloDrive/silo/fileserver/libmgr"
	"github.com/SiloDrive/silo/fileserver/middleware"
	"github.com/SiloDrive/silo/fileserver/objmgr"
	"github.com/SiloDrive/silo/store"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// changesResponse is what a client applies to its own view of a library.
//
// Anchor is the commit the changes bring you up to: hand it back as `since` on
// the next call. It is returned even when nothing changed, so a caller that
// polls can advance without special-casing the empty answer.
// Anchor is absent on every page but the last, and that is the contract rather
// than an omission: a client that records the anchor whenever it is present is
// correct by construction, where one told "record it only at the end" has to
// remember to. Recording it early would skip everything still unread.
type changesResponse struct {
	Anchor  string   `json:"anchor,omitempty"`
	Changes []change `json:"changes"`
}

// change is one path that differs between two commits.
//
// Paths are library-relative and start with "/". OldPath is set only for renames
// and moves — the two are the same operation here, since a rename is a move
// within one directory, and a client that has to tell them apart can compare
// the parent directories itself.
type change struct {
	Op      string `json:"op"`                 // create | delete | modify | move
	Path    string `json:"path"`               // where the item is now
	OldPath string `json:"old_path,omitempty"` // where it was, for moves
	ID      string `json:"id,omitempty"`       // content hash, absent for deletes
	Size    int64  `json:"size"`
	IsDir   bool   `json:"is_dir"`
}

// ChangesHandler answers GET /api/silo/v1/libraries/{libraryid}/changes?since={commit}
// with everything that differs between that commit and the current head.
//
// It exists so a sync client does not have to replicate the object store to
// find out what moved. The server already holds both trees and already has the
// diff, so the client issues one request instead of walking commits, fetching
// fs objects and comparing them itself — which is the difference between a few
// hundred lines of client and the tens of thousands a full sync daemon carries to do the
// same job without server cooperation.
//
// A directory appears as its own change only when it is empty. One that arrives
// with content is reported solely as the paths inside it, because the diff
// walks to where the trees actually differ and a directory that exists in only
// one tree differs at its contents. An applier therefore needs mkdir -p
// semantics: create the parents of every path it is told about. The same is
// true in reverse for deletes of non-empty directories.
//
// A `since` the server cannot resolve is 410 Gone rather than an error: the
// commit is not wrong, it is merely no longer reachable — garbage collected, or
// from a library that was reset — and the caller's recovery is to enumerate
// from scratch. That is a different instruction from "retry", so it gets a
// different status.
//
// `?limit=N` pages the answer: each page carries a `Link: …; rel="next"` header
// until the last, which carries the anchor instead. Pages are served from the
// head as it was on the first page, so a library being written to does not make
// items skip or repeat under the cursor; the writes show up on the next pass.
// See pagination.go.
//
// What paging bounds is the response and the client's apply loop, not the
// server's work: the diff is recomputed per page, because a Merkle diff is
// proportional to what changed and there is no way to resume one part-way. A
// client that wants the server to do less should ask more often, not for
// smaller pages.
func ChangesHandler(w http.ResponseWriter, r *http.Request) {
	libraryID := mux.Vars(r)["libraryid"]

	// Authorization comes before the arguments are validated, not after. A
	// caller who may not see this library must not learn from a 400 or a 410
	// whether the anchor they guessed was a real one -- that is the same
	// enumeration oracle the 403-not-404 rule closes, one level down.
	//
	// "" for the path: the feed covers the whole library, so a credential
	// scoped to a folder is refused rather than answered partially.
	if middleware.Perm(r, libraryID, "") == "" {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return
	}

	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}

	// Validated before touching the database: a missing parameter is the
	// caller's mistake either way, and answering it costs nothing, whereas the
	// permission check below is several queries.
	since := r.URL.Query().Get("since")
	var pinned string
	offset := 0
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, ok := decodeCursor(raw)
		if !ok || c.Since == "" || c.Target == "" {
			http.Error(w, "cursor is not one this server issued; start again from since", http.StatusBadRequest)
			return
		}
		// The cursor supersedes since rather than being checked against it: it
		// was issued by this endpoint and carries the same field, so a
		// disagreement is a client stitching the two together by hand.
		since, pinned, offset = c.Since, c.Target, c.Offset
	} else if since == "" {
		http.Error(w, "since is required: pass the anchor from a previous call, or enumerate instead", http.StatusBadRequest)
		return
	}

	library, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		code, msg := libmgr.StatusFor(err)
		http.Error(w, msg, code)
		return
	}

	// Nothing has happened since the caller last looked. Answered before
	// loading anything, because this is the common case: a client polling a
	// quiet library asks this question far more often than any other.
	if pinned == "" && since == library.HeadCommitID {
		writeJSON(w, http.StatusOK, changesResponse{Anchor: library.HeadCommitID, Changes: []change{}})
		return
	}

	// The target is the head as of the first page. Pinning it is what keeps a
	// paged answer consistent: without it, a commit landing between pages
	// changes the diff the offset indexes into, and items shift across the page
	// boundary in both directions.
	target, changes, err := libraryChanges(library, since, pinned)
	if err != nil {
		writeChangesErr(w, err, since, target, libraryID)
		return
	}

	// Sorted so the order is a property of the data rather than of the tree
	// walk that produced it. Paging indexes into this list from a separate
	// request, so "the same two commits yield the same sequence" has to be true
	// for a reason, not by observation of the current traversal.
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Path != changes[j].Path {
			return changes[i].Path < changes[j].Path
		}
		return changes[i].Op < changes[j].Op
	})

	from, to, more := window(len(changes), offset, limit)
	page := changesResponse{Changes: changes[from:to]}
	if more {
		setNextLink(w, r, encodeCursor(pageCursor{Since: since, Target: target, Offset: to}))
	} else {
		page.Anchor = target
	}
	writeJSON(w, http.StatusOK, page)
}

// errHistoryCut reports a since or a cursor target the library can no longer
// resolve. It is 410 Gone rather than a 404: the commit is not wrong, it is
// merely no longer reachable — collected under a retention limit, or from a
// library that was reset — and the caller's recovery is to enumerate from
// scratch. That is a different instruction from "retry", so it gets a
// different status.
var errHistoryCut = errors.New("history cut")

// libraryChanges computes a library's diff, keylessly.
//
// It works on an E2EE library, which is the point: the commits give up their
// roots through the public decoder, the directories give up their edges the
// same way, and the paths come back as base64url of the SIV ciphertext —
// exactly what entries/{path} routes on. The server answers what changed
// without learning what any of it is called.
func libraryChanges(library *libmgr.Library, since, pinned string) (string, []change, error) {
	st, err := library.Store()
	if err != nil {
		return "", nil, err
	}

	target, targetRoot := library.HeadCommitID, library.RootID
	if pinned != "" {
		id, err := store.ParseID(pinned)
		if err != nil {
			return "", nil, fmt.Errorf("%w: %v", errHistoryCut, err)
		}
		commit, err := st.GetCommitPublic(id)
		if err != nil {
			return "", nil, fmt.Errorf("%w: %v", errHistoryCut, err)
		}
		target, targetRoot = pinned, commit.Root.String()
	}

	sinceID, err := store.ParseID(since)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", errHistoryCut, err)
	}
	sinceCommit, err := st.GetCommitPublic(sinceID)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", errHistoryCut, err)
	}
	newRoot, err := store.ParseID(targetRoot)
	if err != nil {
		return "", nil, err
	}

	diffs, err := st.Diff(sinceCommit.Root, newRoot)
	if err != nil {
		return "", nil, err
	}

	changes := make([]change, 0, len(diffs))
	for _, d := range diffs {
		c := change{Op: d.Op, Path: d.Path, OldPath: d.OldPath, Size: d.Size, IsDir: d.IsDir}
		if d.Op != "delete" {
			c.ID = d.ID.String()
		}
		changes = append(changes, c)
	}
	return target, changes, nil
}

// writeChangesErr answers a failed diff. A baseline the library can no longer
// resolve is 410 with the head to start again from; anything else is damage.
func writeChangesErr(w http.ResponseWriter, err error, since, target, libraryID string) {
	if errors.Is(err, errHistoryCut) {
		http.Error(w, "since is no longer reachable; enumerate from scratch", http.StatusGone)
		return
	}
	// A tree with more paths through it than objects in it has no listing that
	// is both complete and finite. Refused as the client error it is, rather
	// than truncated: a short answer presented as a complete one would have a
	// client delete files it still holds.
	var tooLarge *objmgr.TooLargeError
	if errors.As(err, &tooLarge) {
		http.Error(w, "that range spans more changes than can be listed", http.StatusRequestEntityTooLarge)
		return
	}
	log.Errorf("Failed to diff %s..%s in library %s: %v", since, target, libraryID, err)
	http.Error(w, "Failed to compute changes", http.StatusInternalServerError)
}

// absPath makes a diff's library-relative name into the rooted path the rest of
// the API speaks, so a client can pass it straight back to any other endpoint.
func absPath(name string) string {
	if name == "" {
		return "/"
	}
	if name[0] == '/' {
		return name
	}
	return "/" + name
}
