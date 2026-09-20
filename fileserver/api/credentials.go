package api

// Self-service credential management: the list of what an account holds, and
// a targeted revoke over one row of it.
//
// These exist because the account's revocation vocabulary was all-or-nothing.
// auth/logout discards the row that asked and auth/logout/everywhere discards
// all of them, so a person who wanted to unmount one stolen laptop and keep
// their phone signed in had no way to say so, and the password change grew a
// blanket revoke to cover the gap. docs/plans/credential-management.md holds
// the whole argument, including why this lands before the password change is
// reconsidered rather than beside it.
//
// The list deliberately omits the invite kind. The only invite row a
// self-service caller can hold is their own spent one, kept because the Invite
// table references it as the record of an arrival; it is not a way in, it
// cannot be revoked -- credential.Revoke excludes the kind -- and offering a
// row whose DELETE answers 404 would be a menu item that does nothing.
// `silo token list` still shows it, because an operator is reading the record.

import (
	"net/http"

	"github.com/SiloDrive/silo/fileserver/credential"
	"github.com/SiloDrive/silo/fileserver/middleware"
	"github.com/SiloDrive/silo/fileserver/option"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// credentialInfo is one row as a client sees it.
//
// It is the stored row minus the secret, which is not stored in a recoverable
// form anyway, and minus the account id, which the caller already knows to be
// their own. Everything here exists to answer one question -- "which of these
// is the laptop I left on the train" -- so the fields that identify a device
// to a person (label, client_id, last_ua, last_used) are the point, and the
// ones that describe its authority (kind, scope, perm) are what tells them
// what revoking it costs.
type credentialInfo struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Label    string `json:"label"`
	Scope    string `json:"scope,omitempty"`
	Perm     string `json:"perm"`
	ClientID string `json:"client_id,omitempty"`
	Created  int64  `json:"created"`
	// ExpiresAt is absent for a credential that does not expire, rather than
	// zero: zero is a real instant and a client rendering it would say 1970.
	ExpiresAt *int64 `json:"expires_at,omitempty"`
	// LastUsed is absent for a credential that has never been used, for the
	// same reason, and that absence is itself worth showing -- a device
	// credential minted and never seen again is the interesting row on the
	// page.
	LastUsed *int64 `json:"last_used,omitempty"`
	LastUA   string `json:"last_ua,omitempty"`
	// Current marks the row this very request authenticated with. It is the
	// point of the endpoint as much as the list is: without it a client cannot
	// render "this device", and a person cannot avoid revoking the session
	// they are looking at.
	Current bool `json:"current,omitempty"`
}

type credentialsResponse struct {
	Credentials []credentialInfo `json:"credentials"`
}

// ListCredentialsHandler handles GET /api/silo/v1/account/credentials.
//
// Mounted on the ordinary authenticated lane rather than under
// RequireOwnCredential, because it is about the account rather than about the
// row presenting it -- so a credential narrowed to one library is refused it
// before reaching here, as it is for logout/everywhere and the password
// change. Reading the whole account's credentials is strictly wider than a
// scope cut to a single library.
func ListCredentialsHandler(w http.ResponseWriter, r *http.Request) {
	acct := middleware.GetAccount(r)
	if acct == nil {
		log.Errorf("Credential listing reached %s with no account in context", r.URL.Path)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	rows, err := credential.ListByAccount(ctx, acct.ID)
	if err != nil {
		log.Errorf("Failed to list credentials for %s: %v", acct.Email, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// The row this request arrived on. Absent only if something mounted this
	// handler off the credential lane, which would be a bug rather than a
	// state -- the list is still correct without the flag, so it is not worth
	// a 500.
	var currentID string
	if cur := middleware.GetCredential(r); cur != nil {
		currentID = cur.ID
	}

	// Non-nil, so an account with nothing to show encodes as [] rather than
	// null and a client can iterate it without a nil check.
	out := make([]credentialInfo, 0, len(rows))
	for _, c := range rows {
		if c.Kind == credential.KindInvite {
			continue
		}
		info := credentialInfo{
			ID:       c.ID,
			Kind:     string(c.Kind),
			Label:    c.Label,
			Scope:    c.Scope.String(),
			Perm:     c.Perm,
			ClientID: c.ClientID,
			Created:  c.Ctime,
			LastUA:   c.LastUA,
			Current:  c.ID == currentID,
		}
		if c.ExpiresAt != 0 {
			info.ExpiresAt = &c.ExpiresAt
		}
		if c.LastUsed != 0 {
			info.LastUsed = &c.LastUsed
		}
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, credentialsResponse{Credentials: out})
}

// RevokeCredentialHandler handles DELETE
// /api/silo/v1/account/credentials/{id}.
//
// Revoking the caller's own id is allowed, and is auth/logout reached by
// another name. Refusing it would make every client special-case the one row
// it can most easily identify, and the response says which happened so a
// client can tell it has just signed itself out.
//
// No write permission is required, which is the rule the logout routes already
// hold: revocation only ever takes access away, so a credential handed out
// read-only may do it. Requiring write here would make the targeted revoke
// stricter than logout/everywhere, which a read-only credential may already
// fire and which takes the laptop's credential along with everything else.
// auth/password is the odd one out because it *sets* something.
func RevokeCredentialHandler(w http.ResponseWriter, r *http.Request) {
	acct := middleware.GetAccount(r)
	if acct == nil {
		log.Errorf("Credential revocation reached %s with no account in context", r.URL.Path)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	id := mux.Vars(r)["id"]

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	gone, err := credential.Revoke(ctx, id, acct.ID)
	if err != nil {
		log.Errorf("Failed to revoke credential %s for %s: %v", id, acct.Email, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if !gone {
		// 404, not 403, and the three cases it covers are deliberately
		// indistinguishable: no such id, an id belonging to somebody else, and
		// the caller's own spent invite. Revoke is owner-scoped in SQL, so it
		// cannot tell them apart -- and telling them apart is what would be
		// wrong, because a 403 for the second case answers "does this id exist
		// on another account". docs/responses.md reserves 403 for a resource
		// you cannot see; this is a sub-resource of your own account, which is
		// the answer DELETE account/keys/recovery/{n} already gives.
		http.Error(w, "No such credential", http.StatusNotFound)
		return
	}

	// Read before the revoke would have been wrong -- the row is gone by now
	// -- so the comparison is against what arrived, which is the same row the
	// listing marked current.
	var current bool
	if cur := middleware.GetCredential(r); cur != nil {
		current = cur.ID == id
	}
	if current {
		log.Infof("%s revoked the credential they were using", acct.Email)
	} else {
		log.Infof("%s revoked credential %s", acct.Email, id)
	}
	writeJSON(w, http.StatusOK, revokedResponse{Revoked: 1, Current: current})
}
