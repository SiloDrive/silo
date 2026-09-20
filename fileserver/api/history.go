package api

import (
	"errors"
	"net/http"

	"github.com/SiloDrive/silo/fileserver/libmgr"
	"github.com/SiloDrive/silo/fileserver/middleware"
	"github.com/SiloDrive/silo/fileserver/objmgr"
	"github.com/SiloDrive/silo/store"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// commitInfo is one entry in a library's history.
//
// Author and Message are absent under E2EE rather than empty, because the
// server genuinely does not have them: they are sealed under a content key it
// never holds, while Root, Parents and CreatedAt stay public so the walk works
// at all. A client reading an E2EE library decodes them itself from the commit
// object; one reading a plain library gets them here and saves the round trip.
type commitInfo struct {
	ID        string `json:"id"`
	CreatedAt int64  `json:"created_at"`
	Author    string `json:"author,omitempty"`
	Message   string `json:"message,omitempty"`
}

type commitsResponse struct {
	Commits []commitInfo `json:"commits"`
}

// CommitsHandler answers GET /api/silo/v1/libraries/{libraryid}/commits with
// the library's history, newest first.
//
// It exists because history was reachable but not enumerable. Every commit
// object is already served by GET objects/{id}, and store.Commit carries
// Parents, so a client could always walk backwards — at one round trip per
// commit, having first been told where to start. This is that walk, done
// server-side, which is what a point-in-time view needs before it can offer
// anybody a list of times to pick from.
//
// Together with ?at= on the entries endpoint it is the whole of the
// server's contribution to a NetApp-style .history/ view. The directory itself
// is the client's to synthesize, deliberately: a directory the server invented
// has no id, appears in no manifest, and would need excluding from GC's mark,
// from changes?since=, from Measure and from every other walk. This codebase
// rests on an id naming content, and one synthetic entry puts an if into every
// place that assumption is used.
//
// Reading it costs nothing against quota, and nothing had to be done to make
// that true: usage is logical size at head, and history is by definition not at
// head.
//
// `?limit=N` pages the answer with a `Link: …; rel="next"` header, per
// pagination.go. The cursor names the commit to resume the walk at rather than
// an offset into it, because history is a linked list — an offset cursor would
// re-walk from the head on every page, so the last page of a long history would
// cost the whole of it.
func CommitsHandler(w http.ResponseWriter, r *http.Request) {
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

	var from string
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, ok := decodeCursor(raw)
		if !ok || c.From == "" {
			http.Error(w, "cursor is not one this server issued; start again with no cursor", http.StatusBadRequest)
			return
		}
		if _, err := store.ParseID(c.From); err != nil {
			http.Error(w, "cursor is not one this server issued; start again with no cursor", http.StatusBadRequest)
			return
		}
		from = c.From
	}

	library, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		code, msg := libmgr.StatusFor(err)
		http.Error(w, msg, code)
		return
	}

	st, err := library.Store()
	if err != nil {
		log.Errorf("Failed to open the store of %s to walk its history: %v", libraryID, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	start := library.HeadCommitID
	if from != "" {
		start = from
	}
	commits, next := walkHistory(st, start, limit)
	if next != "" {
		setNextLink(w, r, encodeCursor(pageCursor{From: next}))
	}
	writeJSON(w, http.StatusOK, commitsResponse{Commits: commits})
}

// walkHistory follows Parents back from start, returning what it read and the
// commit a next page would resume at.
//
// A commit it cannot read ends the walk rather than failing it, which is the
// difference between this and changes?since=. There, the client named a
// specific commit and is entitled to be told that commit is gone — 410, go and
// enumerate. Here the client asked what history exists, and running out of it
// *is* the answer: once a retention limit is collecting old commits, the walk
// meeting a missing object is the retention boundary, reported by the listing
// simply ending. A 410 would turn the normal case into an error.
//
// Merges take the first parent. Nothing in this server writes a commit with
// more than one today, and a full topological walk of a DAG is a different
// piece of work with a frontier and an ordering question in it; taking the
// mainline is honest for what exists and does not have to be undone to add the
// rest.
func walkHistory(st *objmgr.Store, start string, limit int) ([]commitInfo, string) {
	out := []commitInfo{}
	// A cycle is impossible in a Merkle DAG — a commit's id covers its
	// parents, so a loop would need a hash collision — but the walk is over
	// data the store hands back, and an unbounded loop over damaged data is a
	// hung request rather than a wrong answer.
	seen := map[string]bool{}
	plain := !st.E2EE()

	for cur := start; cur != ""; {
		if limit > 0 && len(out) == limit {
			return out, cur
		}
		if seen[cur] {
			break
		}
		seen[cur] = true

		id, err := store.ParseID(cur)
		if err != nil {
			break
		}
		// A plain commit is one object and one decode: the public section is
		// a prefix of what GetCommit already returns, so reading it twice
		// would open and re-read the same file for every commit on the page.
		// Only an E2EE library needs the public read on its own, because the
		// rest of the object will not decode without the key.
		var createdAt int64
		var parents []store.ID
		if plain {
			c, err := st.GetCommit(id)
			if err != nil {
				break
			}
			createdAt, parents = c.CreatedAt, c.Parents
			out = append(out, commitInfo{ID: cur, CreatedAt: createdAt, Author: c.Author, Message: c.Message})
		} else {
			// The walk's job is the shape of the history, so a commit whose
			// sealed half will not decode still appears in the list by id and
			// time.
			public, err := st.GetCommitPublic(id)
			if err != nil {
				break
			}
			createdAt, parents = public.CreatedAt, public.Parents
			out = append(out, commitInfo{ID: cur, CreatedAt: createdAt})
		}

		if len(parents) == 0 {
			break
		}
		cur = parents[0].String()
	}
	return out, ""
}

// versionInfo is one row of a path's history: a commit at which the entry at
// that path became what it is here.
//
// ID is a pointer because null is a value on this surface. A path deleted and
// recreated is two lives of one name, and a version list that omitted the gap
// would read as one continuous file; a null id is how the gap is spelled. Size
// is a pointer for the humbler reason that an empty file is zero bytes and
// omitempty cannot tell zero from absent.
//
// Type is load-bearing rather than decorative: a client takes the id to GET
// objects/{id}, and has to know whether it is opening a manifest or a
// directory before it can decode what comes back.
type versionInfo struct {
	Commit    string  `json:"commit"`
	CreatedAt int64   `json:"created_at"`
	ID        *string `json:"id"`
	Type      string  `json:"type,omitempty"`
	Size      *int64  `json:"size,omitempty"`
	Author    string  `json:"author,omitempty"`
	Message   string  `json:"message,omitempty"`
}

type versionsResponse struct {
	Versions []versionInfo `json:"versions"`
}

// maxVersionScan is how many commits one request will read looking for
// versions before it hands back a cursor.
//
// limit bounds versions, and ten versions may mean walking the whole history
// to find them, which is a different cost from commits?limit=, where the bound
// and the work are the same number. Without a cap the server's work per
// request is the length of the history rather than the size of the answer,
// and a client that asked for three rows of a forty-thousand-commit library
// would hold a connection open for all of it. With it, a page may come back
// short of limit and still carry a next link; a client already follows Link
// until it is absent, so that costs it nothing.
const maxVersionScan = 1000

// EntryHistoryHandler answers GET entries/{path}?type=history with the
// versions of one path, newest first, and only the commits where its id
// changed.
//
// It exists because the question a file manager asks is "what has this file
// been", and the commits list answers "what did this library look like then".
// From the client side the only way from one to the other is to read the path
// at every commit and discard repeats: hundreds of requests to find three
// versions, because most commits did not touch the file. The collapse is the
// whole feature, and the server is the only side that can do it in one pass,
// because it holds every tree and a client holds only the ones it has fetched.
//
// The rows carry ids, and an id is absolute -- the hash of the encoded
// manifest, identical at every commit the file survived unchanged -- so GET
// objects/{id} and the chunk surface take it from there with no ?at= and no
// second read path. A client's chunk cache is keyed by the same ids, so an old
// version of a file it already holds costs it nothing to open.
//
// It is called from the entries route rather than mounted beside commits,
// which is what lets a path-scoped credential reach it: the permission check
// names the path, exactly as it does for the read this is a history of.
func EntryHistoryHandler(w http.ResponseWriter, r *http.Request, libraryID, path string) {
	// Authorization first, as CommitsHandler: a caller who may not see this
	// library must not learn from a 400 whether their arguments were sound.
	if middleware.Perm(r, libraryID, path) == "" {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return
	}

	// at names one point in time and this is a walk through all of them.
	// They do not compose, and dropping either silently is how a client reads
	// the wrong answer under a 200. The same rule as at on a write.
	if r.URL.Query().Get("at") != "" {
		http.Error(w, "at does not combine with type=history: the walk starts at the head", http.StatusBadRequest)
		return
	}

	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}

	var from string
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, ok := decodeCursor(raw)
		if !ok || c.From == "" {
			http.Error(w, "cursor is not one this server issued; start again with no cursor", http.StatusBadRequest)
			return
		}
		if _, err := store.ParseID(c.From); err != nil {
			http.Error(w, "cursor is not one this server issued; start again with no cursor", http.StatusBadRequest)
			return
		}
		from = c.From
	}

	library, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		code, msg := libmgr.StatusFor(err)
		http.Error(w, msg, code)
		return
	}

	st, err := library.Store()
	if err != nil {
		log.Errorf("Failed to open the store of %s to walk the history of %s: %v", libraryID, path, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// The server cannot resolve a path in a library whose names it cannot
	// read, so the refusal is the one the path-addressed read gives: 403,
	// never 404, because "not found" would be a claim about contents it
	// cannot see. Whether path is even the right key under E2EE is an open
	// question on the client side; until it is answered the honest thing is
	// to refuse.
	if st.E2EE() && !st.HasKey() {
		http.Error(w, "This library is end-to-end encrypted; read its objects by id", http.StatusForbidden)
		return
	}

	start := library.HeadCommitID
	if from != "" {
		start = from
	}
	versions, next, err := walkVersions(st, start, path, limit)
	if err != nil {
		log.Errorf("Walking the history of %s in %s: %v", path, libraryID, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if next != "" {
		setNextLink(w, r, encodeCursor(pageCursor{From: next}))
	}
	writeJSON(w, http.StatusOK, versionsResponse{Versions: versions})
}

// versionRun is the run of consecutive commits, newest to oldest, at which a
// path held one value. It is carried as the oldest commit seen so far, because
// that is the commit that made the version and the one the row will name.
type versionRun struct {
	commit    string
	createdAt int64
	author    string
	message   string
	present   bool
	node      objmgr.Node
}

// walkVersions follows Parents back from start, resolving path in each commit's
// tree, and returns a row for each commit at which the entry's id changed,
// plus the commit a next page would resume at.
//
// A row names the oldest commit of a run of equal values -- the commit that
// wrote the version, which is the one whose created_at and message a person
// wants beside it. The walk is newest first, so it learns where a run began
// only on reading the commit before it; a run is therefore pending until the
// value changes or history ends, and never emitted from the middle.
//
// That is also what makes the cursor simple. Both places the walk stops early
// -- limit reached, or the scan cap -- hand back the pending run's commit, and
// the next page starts by reading that commit again: one duplicate read, and
// the run is rebuilt from a commit known to belong to it. Nothing is emitted
// twice and nothing is skipped.
//
// Absence is a value like any other, so a deletion between two versions is a
// row with no id. The one absence that is not a row is the run that reaches
// the end of history: before the first commit that had the file there was
// nothing, and nothing is not a version. A path that never existed is an
// empty list rather than a 404, because "no versions in the window" is the
// answer and a 404 here would claim knowledge of the whole past.
//
// Like walkHistory, a commit that will not read ends the walk rather than
// failing it: running out of history is the retention boundary, reported by
// the list ending. A tree object that will not read is different -- the
// commit is there and its tree is not -- and is returned as the damage it is.
func walkVersions(st *objmgr.Store, start, path string, limit int) ([]versionInfo, string, error) {
	out := []versionInfo{}
	seen := map[string]bool{}
	var run *versionRun

	emit := func(r *versionRun) error {
		row := versionInfo{Commit: r.commit, CreatedAt: r.createdAt, Author: r.author, Message: r.message}
		if r.present {
			id := r.node.ID.String()
			row.ID = &id
			if r.node.IsDir() {
				row.Type = "dir"
			} else {
				row.Type = "file"
				// One manifest read per version rather than per commit:
				// the size is looked up when the row is written, not while
				// the run is still being extended.
				m, err := st.GetManifestPublic(r.node.ID)
				if err != nil {
					return err
				}
				size := m.FileSize
				row.Size = &size
			}
		}
		out = append(out, row)
		return nil
	}

	for cur, scanned := start, 0; cur != ""; scanned++ {
		if scanned == maxVersionScan && run != nil {
			return out, run.commit, nil
		}
		if seen[cur] {
			break
		}
		seen[cur] = true

		id, err := store.ParseID(cur)
		if err != nil {
			break
		}
		c, err := st.GetCommit(id)
		if err != nil {
			break
		}

		node, present, err := resolveIn(st, c.Root, path)
		if err != nil {
			return nil, "", err
		}
		this := &versionRun{commit: cur, createdAt: c.CreatedAt, author: c.Author, message: c.Message, present: present, node: node}

		switch {
		case run == nil:
			run = this
		case run.present == present && (!present || run.node.ID == node.ID):
			// The same value one commit further back: the run extends,
			// and the row it will become moves to this older commit.
			run = this
		default:
			if err := emit(run); err != nil {
				return nil, "", err
			}
			run = this
			if limit > 0 && len(out) == limit {
				return out, run.commit, nil
			}
		}

		if len(c.Parents) == 0 {
			break
		}
		cur = c.Parents[0].String()
	}

	if run != nil && run.present {
		if err := emit(run); err != nil {
			return nil, "", err
		}
	}
	return out, "", nil
}

// resolveIn looks path up in one tree, telling absence apart from failure.
// The root is a directory that nothing contains, so it resolves to itself.
func resolveIn(st *objmgr.Store, root store.ID, path string) (objmgr.Node, bool, error) {
	if path == "/" {
		return objmgr.Node{ID: root, Type: store.NodeDir, Mode: store.RootMode}, true, nil
	}
	node, err := st.Resolve(root, path)
	switch {
	case err == nil:
		return node, true, nil
	case errors.Is(err, objmgr.ErrNotFound), errors.Is(err, objmgr.ErrNotDir):
		return objmgr.Node{}, false, nil
	}
	return objmgr.Node{}, false, err
}
