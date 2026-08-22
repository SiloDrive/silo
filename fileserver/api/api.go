package api

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/dkam/silo/fileserver/authmgr"
	"github.com/dkam/silo/fileserver/fsmgr"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/repomgr"
	"github.com/dkam/silo/fileserver/share"
	"github.com/dkam/silo/fileserver/tokenstore"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

var seafileDB *sql.DB // read handle

func Init(readDB, _ *sql.DB) {
	seafileDB = readDB
}

type siloServerInfo struct {
	Version  string   `json:"version"`
	Features []string `json:"features"`
	// BlockSize is the offset a client must chunk at for its block ids to
	// match the ones this store already holds. It is reported rather than
	// assumed because it is configurable, and a client that guesses wrong
	// does not fail — it silently uploads blocks that dedup against nothing
	// and are useless to every other client of the same library.
	BlockSize uint64 `json:"block_size"`
}

// features names the capabilities a client may branch on, so a new client can
// ask this server what it does instead of comparing version strings against a
// changelog. Version numbers answer "which build is this"; they answer "can I
// call this" only for someone holding the release notes, and a client talking
// to a server it did not ship with is exactly the case that has neither.
//
// A name is added in the release its capability ships, and then never removed
// and never reused. Removing one breaks the clients that checked for it, and
// reusing one for something else is worse than having no name at all, because
// the check still passes.
//
// Runtime configuration belongs here too, which is why notifications is
// conditional: a client that sees the name can go straight to notify-token
// instead of learning from a 404 that this server was built without it.
func features() []string {
	f := []string{
		"entries",            // one addressable noun, HTTP methods as its verbs
		"entries-copy",       // POST {"op":"copy","to":…}
		"conditional-writes", // If-Match / If-None-Match on every mutating method
		"ranged-reads",       // Range on GET entries, unencrypted libraries
		"changes",            // GET repos/{id}/changes?since=
		"repo-rename",        // PATCH repos/{id}
		"blocks",             // blocks/missing, PUT blocks/{sha1}, PUT entries?type=blocks
		"pagination",         // ?limit on changes and directory listings, Link: rel="next"
		"batch",              // POST repos/{id}/batch — many operations, one commit
	}
	if option.EnableNotification {
		f = append(f, "notifications") // WS /notification, POST repos/{id}/notify-token
	}
	return f
}

// ServerInfoHandler handles GET /api/silo/v1/server-info.
func ServerInfoHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, siloServerInfo{
		Version:   option.Version,
		Features:  features(),
		BlockSize: option.FixedBlockSize,
	})
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Errorf("Failed to encode JSON response: %v", err)
	}
}

// decodeJSON reads and decodes a JSON request body (max 1MB).
func decodeJSON(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return false
	}
	return true
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginResponse struct {
	Token string `json:"token"`
}

func LoginHandler(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.Email == "" || req.Password == "" {
		http.Error(w, "Email and password are required", http.StatusBadRequest)
		return
	}

	if !allowLoginAttempt(w, r, req.Email) {
		return
	}

	acct, err := authmgr.ValidatePassword(req.Email, req.Password)
	if err != nil {
		loginFailed(r, req.Email)
		log.Infof("Login failed for %s: %v", req.Email, err)
		http.Error(w, "Invalid email or password", http.StatusUnauthorized)
		return
	}
	loginSucceeded(req.Email)

	token, err := authmgr.GenerateSessionToken(acct.ID)
	if err != nil {
		log.Errorf("Failed to generate session token: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, loginResponse{Token: token})
}

type accessTokenRequest struct {
	RepoID  string `json:"repo_id"`
	ObjID   string `json:"obj_id"`
	Op      string `json:"op"`
	OneTime bool   `json:"one_time"`
}

type accessTokenResponse struct {
	Token string `json:"token"`
}

// tokenOps maps each access-token operation to the repo permission needed to
// mint a token for it. The handlers that consume these tokens (/files/,
// /blks/, /zip/, /upload-api/, ...) authorize from the token alone and never
// re-check the caller's permission, so this map is the only gate on them.
//
// Ops absent from the map are rejected rather than passed through: an
// unrecognized op must not be able to produce a bearer credential.
var tokenOps = map[string]string{
	// Read: any permission on the repo is enough.
	"view":                "r",
	"download":            "r",
	"download-link":       "r",
	"downloadblks":        "r",
	"download-dir":        "r",
	"download-dir-link":   "r",
	"download-multi":      "r",
	"download-multi-link": "r",

	// Write: "rw" required. A read-only share must not yield an upload token.
	"upload":      "rw",
	"upload-link": "rw",
	"update":      "rw",
	"update-link": "rw",
}

func CreateAccessTokenHandler(w http.ResponseWriter, r *http.Request) {
	acct := middleware.GetAccount(r)

	var req accessTokenRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.RepoID == "" || req.Op == "" {
		http.Error(w, "repo_id and op are required", http.StatusBadRequest)
		return
	}

	needed, ok := tokenOps[req.Op]
	if !ok {
		http.Error(w, "Unsupported op", http.StatusBadRequest)
		return
	}

	// CheckPerm returns "" for a repo the user can't see and for one that
	// doesn't exist, so a 403 here also avoids confirming which repo IDs are
	// real.
	perm := share.CheckPerm(req.RepoID, acct.ID)
	if perm == "" || (needed == "rw" && perm != "rw") {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return
	}

	// The token carries the address, not the id. It has already been
	// permission-checked here, and what the upload path does with the string
	// is write it into a commit as the author — display data, and baked into
	// a content hash that could never be rewritten anyway.
	token := tokenstore.CreateToken(req.RepoID, req.ObjID, req.Op, acct.Email, req.OneTime)
	writeJSON(w, http.StatusOK, accessTokenResponse{Token: token})
}

type repoInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	UpdateTime int64  `json:"update_time"`
	Encrypted  bool   `json:"encrypted"`
	// HeadCommitID is where the library is now. It is here because it is the
	// anchor the changes endpoint needs, and listing libraries is the first
	// thing a sync client does: without it a client's opening move is to
	// enumerate a library with no way to name the state it just enumerated,
	// so its first delta call has nothing to pass as `since`.
	HeadCommitID string `json:"head_commit_id,omitempty"`
}

// repoSelect is shared by the owned and shared queries so the two cannot drift
// into scanning different columns than they select. The join is LEFT because a
// library with no branch row is broken but should still be listable — a client
// that can see it can delete it.
func repoSelect(alias string) string {
	return "SELECT " + alias + ".repo_id, i.name, i.update_time, i.is_encrypted, b.commit_id "
}

func scanRepos(rows *sql.Rows) []repoInfo {
	// Allocated rather than declared, so an empty result set marshals as [] and
	// not null. /changes already promises "always an array, never null", and a
	// client has no way to learn that the two list endpoints on the same lane
	// disagree except by emptying an account and looking. Go hides it — a nil
	// slice ranges zero times — but an account with no libraries is the state
	// every new account is in, so null is the first response a fresh client
	// sees, and in TypeScript, Python or Swift it is not iterable.
	repos := make([]repoInfo, 0)
	for rows.Next() {
		var repo repoInfo
		var name, isEncrypted, commitID sql.NullString
		var updateTime sql.NullInt64
		if err := rows.Scan(&repo.ID, &name, &updateTime, &isEncrypted, &commitID); err != nil {
			log.Warnf("Failed to scan repo row: %v", err)
			continue
		}
		repo.Name = name.String
		repo.UpdateTime = updateTime.Int64
		repo.Encrypted = isEncrypted.String == "1"
		repo.HeadCommitID = commitID.String
		repos = append(repos, repo)
	}
	return repos
}

func ListReposHandler(w http.ResponseWriter, r *http.Request) {
	id := middleware.GetAccountID(r)
	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	rows, err := seafileDB.QueryContext(ctx,
		repoSelect("o")+
			"FROM RepoOwner o LEFT JOIN RepoInfo i ON o.repo_id = i.repo_id "+
			"LEFT JOIN Branch b ON b.repo_id = o.repo_id AND b.name = 'master' "+
			"WHERE o.account_id = ?", id)
	if err != nil {
		log.Errorf("Failed to query repos: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = rows.Close() }()

	repos := scanRepos(rows)
	seen := make(map[string]bool, len(repos))
	for _, r := range repos {
		seen[r.ID] = true
	}

	sharedRows, err := seafileDB.QueryContext(ctx,
		repoSelect("s")+
			"FROM SharedRepo s LEFT JOIN RepoInfo i ON s.repo_id = i.repo_id "+
			"LEFT JOIN Branch b ON b.repo_id = s.repo_id AND b.name = 'master' "+
			"WHERE s.to_account_id = ?", id)
	if err != nil {
		log.Errorf("Failed to query shared repos: %v", err)
	} else {
		defer func() { _ = sharedRows.Close() }()
		for _, r := range scanRepos(sharedRows) {
			if !seen[r.ID] {
				seen[r.ID] = true
				repos = append(repos, r)
			}
		}
	}

	writeJSON(w, http.StatusOK, repos)
}

type syncTokenResponse struct {
	Token string `json:"token"`
}

func CreateRepoSyncTokenHandler(w http.ResponseWriter, r *http.Request) {
	id := middleware.GetAccountID(r)
	vars := mux.Vars(r)
	repoID := vars["repoid"]

	perm := share.CheckPerm(repoID, id)
	if perm == "" {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return
	}

	token, err := repomgr.GenerateRepoToken(repoID, id)
	if err != nil {
		log.Errorf("Failed to generate repo token: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, syncTokenResponse{Token: token})
}

type createRepoRequest struct {
	Name string `json:"name"`
}

type createRepoResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func CreateRepoHandler(w http.ResponseWriter, r *http.Request) {
	acct := middleware.GetAccount(r)

	var req createRepoRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.Name == "" {
		http.Error(w, "Name is required", http.StatusBadRequest)
		return
	}

	repoID, err := repomgr.CreateRepo(req.Name, acct)
	if err != nil {
		log.Errorf("Failed to create repo: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, createRepoResponse{ID: repoID, Name: req.Name})
}

func DeleteRepoHandler(w http.ResponseWriter, r *http.Request) {
	id := middleware.GetAccountID(r)
	vars := mux.Vars(r)
	repoID := vars["repoid"]

	owner, err := repomgr.GetRepoOwner(repoID)
	if err != nil {
		log.Errorf("Failed to get repo owner: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if owner.IsZero() {
		http.Error(w, "Repo not found", http.StatusNotFound)
		return
	}
	if owner != id {
		http.Error(w, "Only the repo owner can delete it", http.StatusForbidden)
		return
	}

	if err := repomgr.DeleteRepo(repoID); err != nil {
		log.Errorf("Failed to delete repo: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

type dirEntry struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	ID       string `json:"id"`
	Size     int64  `json:"size,omitempty"`
	Mtime    int64  `json:"mtime"`
	Modifier string `json:"modifier,omitempty"`
}

// ListDirByID writes the listing of a directory the caller has already resolved
// and authorized. It is the only way to list a directory: getEntry resolves the
// path to answer conditional requests, so it arrives holding the id.
//
// Taking the id rather than the path is the point. A path-taking entry point
// has to re-check the permission, re-load the repository and walk from the root
// a second time, and none of that is cached — CheckPerm is two or more queries,
// repomgr.Get is a query plus a commit read, and every directory object on the
// way down is a fresh read and inflate. The old GET /repos/{id}/dir/?path=
// handler did exactly that second walk and was deleted with the rest of the
// pre-entries surface; do not reintroduce a path-taking variant.
func ListDirByID(w http.ResponseWriter, r *http.Request, storeID, dirID string) {
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}

	// A cursor pins the directory object the first page was served from, so a
	// listing stays consistent while the directory is written to. The pinned
	// object is still readable: directory objects are immutable and nothing
	// reclaims them inside a live library.
	offset := 0
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, ok := decodeCursor(raw)
		if !ok || c.Dir == "" {
			http.Error(w, "cursor is not one this server issued; list again from the start", http.StatusBadRequest)
			return
		}
		// The directory may have changed under the client mid-listing. It
		// keeps reading the version it started on rather than half of each.
		offset, dirID = c.Offset, c.Dir
	}

	// A window is not the representation the id names, so it carries no
	// validator: an ETag here would validate a request for a different page of
	// the same listing. The caller set one before it knew this was paged.
	if limit > 0 || offset > 0 {
		w.Header().Del("ETag")
		w.Header().Del("Last-Modified")
	}

	dir, err := fsmgr.GetSeafdir(storeID, dirID)
	if err != nil {
		log.Errorf("Failed to get directory object %s in store %s: %v", dirID, storeID, err)
		http.Error(w, "Directory not found", http.StatusNotFound)
		return
	}

	entries := dirEntries(dir)
	from, to, more := window(len(entries), offset, limit)
	if more {
		setNextLink(w, r, encodeCursor(pageCursor{Dir: dirID, Offset: to}))
	}
	// The body stays an array whether or not it is paged. Pagination lives in
	// a header precisely so that adding it did not change the shape of a
	// response every existing client already parses.
	writeJSON(w, http.StatusOK, entries[from:to])
}

func dirEntries(dir *fsmgr.SeafDir) []dirEntry {
	entries := make([]dirEntry, 0, len(dir.Entries))
	for _, e := range dir.Entries {
		entryType := "file"
		if fsmgr.IsDir(e.Mode) {
			entryType = "dir"
		}
		entries = append(entries, dirEntry{
			Name:     e.Name,
			Type:     entryType,
			ID:       e.ID,
			Size:     e.Size,
			Mtime:    e.Mtime,
			Modifier: e.Modifier,
		})
	}
	return entries
}
