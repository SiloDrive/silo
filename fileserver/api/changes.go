package api

import (
	"net/http"
	"sort"

	"github.com/dkam/silo/fileserver/commitmgr"
	"github.com/dkam/silo/fileserver/diff"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/repomgr"
	"github.com/dkam/silo/fileserver/share"
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
// Paths are repo-relative and start with "/". OldPath is set only for renames
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

// ChangesHandler answers GET /api/silo/v1/repos/{repoid}/changes?since={commit}
// with everything that differs between that commit and the current head.
//
// It exists so a sync client does not have to replicate the object store to
// find out what moved. The server already holds both trees and already has the
// diff, so the client issues one request instead of walking commits, fetching
// fs objects and comparing them itself — which is the difference between a few
// hundred lines of client and the tens of thousands SeaDrive carries to do the
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
	acct := middleware.GetAccount(r)
	repoID := mux.Vars(r)["repoid"]

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

	if perm := share.CheckPerm(repoID, acct.ID); perm == "" {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return
	}

	repo, err := repomgr.GetWithReason(repoID)
	if err != nil {
		code, msg := repomgr.StatusFor(err)
		http.Error(w, msg, code)
		return
	}

	// Nothing has happened since the caller last looked. Answered before
	// loading anything, because this is the common case: a client polling a
	// quiet library asks this question far more often than any other.
	if pinned == "" && since == repo.HeadCommitID {
		writeJSON(w, http.StatusOK, changesResponse{Anchor: repo.HeadCommitID, Changes: []change{}})
		return
	}

	// The target is the head as of the first page. Pinning it is what keeps a
	// paged answer consistent: without it, a commit landing between pages
	// changes the diff the offset indexes into, and items shift across the page
	// boundary in both directions.
	target, targetRoot := repo.HeadCommitID, repo.RootID
	if pinned != "" {
		targetCommit, err := commitmgr.Load(repoID, pinned)
		if err != nil {
			http.Error(w, "the commit this cursor was issued against is no longer reachable; start again from since", http.StatusGone)
			return
		}
		target, targetRoot = pinned, targetCommit.RootID
	}

	// Commits live in the repo's own store; fs objects live in StoreID, which
	// differs for a virtual repo. Mixing them up would diff the wrong tree.
	sinceCommit, err := commitmgr.Load(repoID, since)
	if err != nil {
		http.Error(w, "since is no longer reachable; enumerate from scratch", http.StatusGone)
		return
	}

	var entries []*diff.DiffEntry
	// foldDirDiff false: a client maintaining per-item identity needs every
	// path that changed, not a directory standing in for its contents.
	if err := diff.DiffCommitRoots(repo.StoreID, sinceCommit.RootID, targetRoot, &entries, false); err != nil {
		log.Errorf("Failed to diff %s..%s in repo %s: %v", since, target, repoID, err)
		http.Error(w, "Failed to compute changes", http.StatusInternalServerError)
		return
	}

	changes := changesFromDiff(entries)

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

// changesFromDiff translates the diff package's vocabulary into the one a sync
// client thinks in. The diff distinguishes files from directories by using a
// different status letter for each; a client cares about the distinction, but
// as a property of the item rather than of the operation, so it moves into
// is_dir and the four operations collapse to create/delete/modify/move.
func changesFromDiff(entries []*diff.DiffEntry) []change {
	changes := make([]change, 0, len(entries))
	for _, e := range entries {
		c := change{Path: absPath(e.Name), ID: e.Sha1, Size: e.Size}

		switch e.Status {
		case diff.DiffStatusAdded:
			c.Op = "create"
		case diff.DiffStatusDeleted:
			c.Op = "delete"
			c.ID = ""
		case diff.DiffStatusModified:
			c.Op = "modify"
		case diff.DiffStatusRenamed:
			c.Op = "move"
			c.OldPath = absPath(e.Name)
			c.Path = absPath(e.NewName)
		case diff.DiffStatusDirAdded:
			c.Op, c.IsDir = "create", true
		case diff.DiffStatusDirDeleted:
			c.Op, c.IsDir, c.ID = "delete", true, ""
		case diff.DiffStatusDirRenamed:
			c.Op, c.IsDir = "move", true
			c.OldPath = absPath(e.Name)
			c.Path = absPath(e.NewName)
		default:
			// Unmerged, and anything a later diff version adds. Skipped rather
			// than guessed at: a client that applies an operation the server
			// did not mean corrupts its own view, and the fallback — a full
			// enumeration — is always available.
			log.Warnf("Skipping diff entry with unhandled status %q at %s", string(e.Status), e.Name)
			continue
		}

		changes = append(changes, c)
	}
	return changes
}

// absPath makes a diff's repo-relative name into the rooted path the rest of
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
