package api

import (
	"net/http"
	"time"

	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/share"
	"github.com/dkam/silo/fileserver/utils"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// notifyTokenTTL matches what the deleted sync-lane endpoint issued, because it is
// the same token. A holder is expected to re-mint rather than keep one for the
// life of a process.
const notifyTokenTTL = 72 * time.Hour

type notifyTokenResponse struct {
	Token     string `json:"jwt_token"`
	ExpiresAt int64  `json:"expires_at"`
}

// CreateNotifyTokenHandler handles POST /api/silo/v1/repos/{repoid}/notify-token.
// It mints the same notification JWT the deleted /repo/{id}/jwt-token
// route, but authorizes the session user against share.CheckPerm instead of
// requiring a repo token — so a Silo-lane client never has to touch the
// compatibility surface to get onto the notification socket.
func CreateNotifyTokenHandler(w http.ResponseWriter, r *http.Request) {
	// Checked before permission, so a server without the notification endpoint
	// says so plainly rather than answering 403 about a library the caller can
	// read perfectly well.
	if !option.EnableNotification {
		http.Error(w, "Notification server is not enabled", http.StatusNotFound)
		return
	}

	acct := middleware.GetAccount(r)
	repoID := mux.Vars(r)["repoid"]

	if share.CheckPerm(repoID, acct.ID) == "" {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return
	}

	expires := time.Now().Add(notifyTokenTTL)
	token, err := utils.GenNotifJWTToken(repoID, acct.Email, expires.Unix())
	if err != nil {
		log.Errorf("Failed to generate notification token for repo %s: %v", repoID, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, notifyTokenResponse{Token: token, ExpiresAt: expires.Unix()})
}
