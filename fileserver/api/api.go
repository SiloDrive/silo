package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"

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

	email, err := authmgr.ValidatePassword(req.Email, req.Password)
	if err != nil {
		loginFailed(r, req.Email)
		log.Infof("Login failed for %s: %v", req.Email, err)
		http.Error(w, "Invalid email or password", http.StatusUnauthorized)
		return
	}
	loginSucceeded(req.Email)

	token, err := authmgr.GenerateSessionToken(email)
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
	user := middleware.GetUserEmail(r)

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
	perm := share.CheckPerm(req.RepoID, user)
	if perm == "" || (needed == "rw" && perm != "rw") {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return
	}

	token := tokenstore.CreateToken(req.RepoID, req.ObjID, req.Op, user, req.OneTime)
	writeJSON(w, http.StatusOK, accessTokenResponse{Token: token})
}

type repoInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	UpdateTime int64  `json:"update_time"`
	Encrypted  bool   `json:"encrypted"`
}

func scanRepos(rows *sql.Rows) []repoInfo {
	var repos []repoInfo
	for rows.Next() {
		var repo repoInfo
		var name, isEncrypted sql.NullString
		var updateTime sql.NullInt64
		if err := rows.Scan(&repo.ID, &name, &updateTime, &isEncrypted); err != nil {
			log.Warnf("Failed to scan repo row: %v", err)
			continue
		}
		repo.Name = name.String
		repo.UpdateTime = updateTime.Int64
		repo.Encrypted = isEncrypted.String == "1"
		repos = append(repos, repo)
	}
	return repos
}

func ListReposHandler(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUserEmail(r)
	ctx, cancel := context.WithTimeout(r.Context(), option.DBOpTimeout)
	defer cancel()

	rows, err := seafileDB.QueryContext(ctx,
		"SELECT o.repo_id, i.name, i.update_time, i.is_encrypted "+
			"FROM RepoOwner o LEFT JOIN RepoInfo i ON o.repo_id = i.repo_id "+
			"WHERE o.owner_id = ?", user)
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
		"SELECT s.repo_id, i.name, i.update_time, i.is_encrypted "+
			"FROM SharedRepo s LEFT JOIN RepoInfo i ON s.repo_id = i.repo_id "+
			"WHERE s.to_email = ?", user)
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
	user := middleware.GetUserEmail(r)
	vars := mux.Vars(r)
	repoID := vars["repoid"]

	perm := share.CheckPerm(repoID, user)
	if perm == "" {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return
	}

	token, err := repomgr.GenerateRepoToken(repoID, user)
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
	user := middleware.GetUserEmail(r)

	var req createRepoRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.Name == "" {
		http.Error(w, "Name is required", http.StatusBadRequest)
		return
	}

	repoID, err := repomgr.CreateRepo(req.Name, user)
	if err != nil {
		log.Errorf("Failed to create repo: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, createRepoResponse{ID: repoID, Name: req.Name})
}

func DeleteRepoHandler(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUserEmail(r)
	vars := mux.Vars(r)
	repoID := vars["repoid"]

	owner, err := repomgr.GetRepoOwner(repoID)
	if err != nil {
		log.Errorf("Failed to get repo owner: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if owner == "" {
		http.Error(w, "Repo not found", http.StatusNotFound)
		return
	}
	if owner != user {
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

func ListDirHandler(w http.ResponseWriter, r *http.Request) {
	user := middleware.GetUserEmail(r)
	vars := mux.Vars(r)
	repoID := vars["repoid"]

	perm := share.CheckPerm(repoID, user)
	if perm == "" {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return
	}

	repo := repomgr.Get(repoID)
	if repo == nil {
		http.Error(w, "Repo not found", http.StatusNotFound)
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		path = "/"
	}
	path, _ = url.QueryUnescape(path)

	dir, err := fsmgr.GetSeafdirByPath(repo.StoreID, repo.RootID, path)
	if err != nil {
		log.Errorf("Failed to get directory %s in repo %s: %v", path, repoID, err)
		http.Error(w, "Directory not found", http.StatusNotFound)
		return
	}

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

	writeJSON(w, http.StatusOK, entries)
}
