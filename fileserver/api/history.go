package api

import (
	"net/http"

	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/objmgr"
	"github.com/dkam/silo/fileserver/share"
	"github.com/dkam/silo/store"
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
	id := middleware.GetAccountID(r)
	libraryID := mux.Vars(r)["libraryid"]

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

	if perm := share.CheckPerm(libraryID, id); perm == "" {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return
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
		public, err := st.GetCommitPublic(id)
		if err != nil {
			break
		}

		info := commitInfo{ID: cur, CreatedAt: public.CreatedAt}
		// Best effort, and deliberately not fatal: the walk's job is the
		// shape of the history, and a commit whose sealed half will not
		// decode should still appear in the list by id and time.
		if plain {
			if c, err := st.GetCommit(id); err == nil {
				info.Author, info.Message = c.Author, c.Message
			}
		}
		out = append(out, info)

		if len(public.Parents) == 0 {
			break
		}
		cur = public.Parents[0].String()
	}
	return out, ""
}
