package api

import (
	"net/http"

	"github.com/dkam/silo/fileserver/apitokenstore"
	"github.com/dkam/silo/fileserver/authmgr"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/repomgr"
	"github.com/dkam/silo/fileserver/share"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// SeaDriveAuthTokenHandler handles POST /api2/auth-token/.
// SeaDrive expects a 40-char hex API token (Seahub/DRF format), not a JWT.
func SeaDriveAuthTokenHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")

	if username == "" || password == "" {
		http.Error(w, "Username and password are required", http.StatusBadRequest)
		return
	}

	if !allowLoginAttempt(w, r, username) {
		return
	}

	acct, err := authmgr.ValidatePassword(username, password)
	if err != nil {
		loginFailed(r, username)
		log.Infof("SeaDrive login failed for %s: %v", username, err)
		http.Error(w, "Invalid username or password", http.StatusUnauthorized)
		return
	}
	loginSucceeded(username)

	token, err := apitokenstore.Create(acct.ID)
	if err != nil {
		log.Errorf("Failed to generate API token: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, loginResponse{Token: token})
}

// SeaDriveAuthPingHandler handles GET /api2/auth/ping/.
func SeaDriveAuthPingHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, "pong")
}

// SeaDriveLogoutHandler handles POST /api2/auth/logout/, revoking the API
// token that authenticated the request. Tokens are minted per login, so this
// signs out the calling device only and leaves the user's other devices
// syncing.
//
// This endpoint is Silo-specific: upstream Seahub has no API-token logout, and
// a client is expected to simply discard the token. It exists so that a token
// known to be compromised can actually be invalidated server-side, which is
// otherwise impossible for a credential with a 30-day sliding expiry.
func SeaDriveLogoutHandler(w http.ResponseWriter, r *http.Request) {
	token := middleware.GetAPIToken(r)
	if token == "" {
		// RequireAPIToken guarantees a token reached the handler, so an empty
		// one means the middleware and this handler have got out of step.
		log.Error("Logout handler reached with no API token in context")
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	if err := apitokenstore.Delete(token); err != nil {
		log.Errorf("Failed to revoke API token: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

type serverInfoResponse struct {
	Version         string   `json:"version"`
	Features        []string `json:"features"`
	NotificationURL string   `json:"notification_url,omitempty"`
}

func SeaDriveServerInfoHandler(w http.ResponseWriter, r *http.Request) {
	features := []string{"seafile-basic"}
	var notificationURL string
	if option.EnableNotification {
		features = append(features, "notification")
		scheme := "ws"
		if r.TLS != nil {
			scheme = "wss"
		}
		notificationURL = scheme + "://" + r.Host + "/notification"
	}
	writeJSON(w, http.StatusOK, serverInfoResponse{
		Version:         "11.0.0",
		Features:        features,
		NotificationURL: notificationURL,
	})
}

// seadriveRepo is the Seahub-compatible shape for /api2/repos/ entries.
type seadriveRepo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Owner      string `json:"owner"`
	Permission string `json:"permission"`
	Type       string `json:"type"`
	Encrypted  bool   `json:"encrypted"`
	Size       int64  `json:"size"`
	MTime      int64  `json:"mtime"`
	HeadCommit string `json:"head_commit_id"`
	Version    int    `json:"version"`
	Root       string `json:"root"`
}

// SeaDriveCreateRepoHandler handles POST /api2/repos/. SeaDrive sends a
// form-encoded body with the library name.
//
// SeaDrive expects the response to contain repo_id, head_commit_id, and token
// (a sync token). Without all three it logs "Invalid resp from create repo api"
// and fails to sync the newly created library.
func SeaDriveCreateRepoHandler(w http.ResponseWriter, r *http.Request) {
	acct := middleware.GetAccount(r)

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}
	name := r.FormValue("name")
	if name == "" {
		http.Error(w, "Name is required", http.StatusBadRequest)
		return
	}

	// SeaDrive cannot hold a content key, so this lane creates
	// server-readable libraries and always will.
	repoID, err := repomgr.CreateRepo(name, acct, repomgr.DefaultFormat(false))
	if err != nil {
		log.Errorf("Failed to create repo: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	repo := repomgr.Get(repoID)
	if repo == nil {
		log.Errorf("Repo %s not found after creation", repoID)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	token, err := repomgr.GenerateRepoToken(repoID, acct.ID)
	if err != nil {
		log.Errorf("Failed to generate sync token for new repo %s: %v", repoID, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"repo_id":        repoID,
		"head_commit_id": repo.HeadCommitID,
		"token":          token,
	})
}

// SeaDriveReposHandler handles GET /api2/repos/. Returns repos accessible to
// the authenticated user in Seahub's format.
func SeaDriveReposHandler(w http.ResponseWriter, r *http.Request) {
	acct := middleware.GetAccount(r)

	seen := make(map[string]bool)
	var result []seadriveRepo

	add := func(repos []*share.SharedRepo, typ, ownerOverride string) {
		for _, repo := range repos {
			if repo.RepoType != "" {
				continue
			}
			if seen[repo.ID] {
				continue
			}
			seen[repo.ID] = true
			owner := repo.Owner
			if ownerOverride != "" {
				owner = ownerOverride
			}
			perm := repo.Permission
			if perm == "" {
				perm = "rw"
			}
			result = append(result, seadriveRepo{
				ID:         repo.ID,
				Name:       repo.Name,
				Owner:      owner,
				Permission: perm,
				Type:       typ,
				MTime:      repo.MTime,
				HeadCommit: repo.HeadCommitID,
				Version:    repo.Version,
			})
		}
	}

	owned, err := share.GetReposByOwner(acct.ID)
	if err != nil {
		log.Errorf("Failed to get owned repos for %s: %v", acct.Email, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	add(owned, "repo", acct.Email)

	shared, err := share.ListSharedWithMe(acct.ID)
	if err != nil {
		log.Warnf("Failed to list shared repos for %s: %v", acct.Email, err)
	} else {
		add(shared, "srepo", "")
	}

	group, err := share.GetGroupReposByUser(acct.ID)
	if err != nil {
		log.Warnf("Failed to list group repos for %s: %v", acct.Email, err)
	} else {
		add(group, "grepo", "")
	}

	if result == nil {
		result = []seadriveRepo{}
	}
	writeJSON(w, http.StatusOK, result)
}

type accountInfoResponse struct {
	Email       string `json:"email"`
	Name        string `json:"name"`
	Usage       int64  `json:"usage"`
	Total       int64  `json:"total"`
	Institution string `json:"institution"`
}

func SeaDriveAccountInfoHandler(w http.ResponseWriter, r *http.Request) {
	email := middleware.GetUserEmail(r)
	writeJSON(w, http.StatusOK, accountInfoResponse{
		Email: email,
		Name:  email,
		Total: -1, // unlimited
	})
}

// downloadInfoResponse matches Seahub's /api2/repos/{id}/download-info/ response.
type downloadInfoResponse struct {
	RelayID       string `json:"relay_id"`
	RelayAddr     string `json:"relay_addr"`
	RelayPort     string `json:"relay_port"`
	Token         string `json:"token"`
	RepoID        string `json:"repo_id"`
	RepoName      string `json:"repo_name"`
	Email         string `json:"email"`
	RandomKey     string `json:"random_key"`
	EncVersion    int    `json:"enc_version"`
	Magic         string `json:"magic"`
	Salt          string `json:"salt"`
	Encrypted     bool   `json:"encrypted"`
	RepoVersion   int    `json:"repo_version"`
	HeadCommitID  string `json:"head_commit_id"`
	FileServerURL string `json:"file_server_url"`
}

// SeaDriveDownloadInfoHandler returns the sync token + repo metadata + file
// server URL so SeaDrive can start syncing the library.
func SeaDriveDownloadInfoHandler(w http.ResponseWriter, r *http.Request) {
	acct := middleware.GetAccount(r)
	repoID := mux.Vars(r)["repoid"]

	if share.CheckPerm(repoID, acct.ID) == "" {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return
	}

	repo, err := repomgr.GetWithReason(repoID)
	if err != nil {
		code, msg := repomgr.StatusFor(err)
		http.Error(w, msg, code)
		return
	}

	token, err := repomgr.GenerateRepoToken(repoID, acct.ID)
	if err != nil {
		log.Errorf("Failed to generate repo token: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Build file server URL from the inbound request so it works regardless
	// of what host/port SeaDrive connected to.
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	fileServerURL := scheme + "://" + r.Host

	writeJSON(w, http.StatusOK, downloadInfoResponse{
		Token:         token,
		RepoID:        repo.ID,
		RepoName:      repo.Name,
		Email:         acct.Email,
		RandomKey:     repo.RandomKey,
		EncVersion:    repo.EncVersion,
		Magic:         repo.Magic,
		Salt:          repo.Salt,
		Encrypted:     repo.IsEncrypted,
		RepoVersion:   repo.Version,
		HeadCommitID:  repo.HeadCommitID,
		FileServerURL: fileServerURL,
	})
}
