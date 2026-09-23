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

	"github.com/SiloDrive/silo/fileserver/account"
	"github.com/SiloDrive/silo/fileserver/middleware"
	"github.com/SiloDrive/silo/fileserver/option"
	log "github.com/sirupsen/logrus"
)

type accountResponse struct {
	// AccountID is the holder every wrap binds, in the canonical lower-case
	// hyphenated UUID spelling store.WrapIdentity requires. Any other
	// spelling of the same id produces a blob that does not open.
	AccountID string `json:"account_id"`
	Email     string `json:"email"`

	// HasPassword is false for an account that signs in only through an
	// identity provider. A client uses it to hide "change password": that
	// form asks for the current password, and there is none to give.
	//
	// A new key on an existing response, which is the change
	// docs/bugs/fixed/adding-a-number-to-a-token-response-breaks-clients.md
	// is about. Checked rather than assumed: the Linux client decodes this
	// response into a struct, Android sets ignoreUnknownKeys, and the macOS
	// client does not call it.
	HasPassword bool `json:"has_password"`
}

// AccountHandler handles GET /api/silo/v1/account.
func AccountHandler(w http.ResponseWriter, r *http.Request) {
	acct := middleware.GetAccount(r)
	if acct == nil {
		log.Errorf("Account asked for on %s with no account in context", r.URL.Path)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()
	hasPassword, err := account.HasPassword(ctx, acct.ID)
	if err != nil {
		log.Errorf("Failed to read whether %s has a password: %v", acct.Email, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, accountResponse{
		AccountID:   acct.ID.String(),
		Email:       acct.Email,
		HasPassword: hasPassword,
	})
}
