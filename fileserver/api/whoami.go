package api

// Who the caller is.
//
// Separate from account/keys, which answers what key material the account
// holds, because they are two questions and conflating them left the first one
// unanswerable in the case that needs it most: an account that has never
// enrolled gets 404 from account/keys, and its own id is exactly what it needs
// in order to enrol. store.WrapIdentity binds the account id as associated
// data, so a client cannot wrap anything until it knows the id -- and until
// this endpoint there was no way to learn it before there was a blob carrying
// it. See docs/auth.md § The account's key material.
//
// Nothing here is a secret. It is what the credential already proves: a caller
// holding one can name the account it belongs to.

import (
	"net/http"

	"github.com/dkam/silo/fileserver/middleware"
	log "github.com/sirupsen/logrus"
)

type accountResponse struct {
	// AccountID is the holder every wrap binds, in the canonical lower-case
	// hyphenated UUID spelling store.WrapIdentity requires. Any other
	// spelling of the same id produces a blob that does not open.
	AccountID string `json:"account_id"`
	Email     string `json:"email"`
}

// AccountHandler handles GET /api/silo/v1/account.
func AccountHandler(w http.ResponseWriter, r *http.Request) {
	acct := middleware.GetAccount(r)
	if acct == nil {
		log.Errorf("Account asked for on %s with no account in context", r.URL.Path)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, accountResponse{
		AccountID: acct.ID.String(),
		Email:     acct.Email,
	})
}
