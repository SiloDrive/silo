package api

// The share surface: the grant model over HTTP.
//
// docs/plans/sharing.md § The grant model owns what a grant is; nothing here
// re-decides any of it. share.Add replaces rather than accumulates because two
// grants to one principal on one path are not two facts, share.Remove treats an
// absent grant as the state the caller asked for, and share.CheckPerm reads
// what these routes write. What the endpoints add is the one question the model
// does not answer: whose decision a share is.
//
// The answer is the owner's, and only the owner's. Being able to write in
// somebody's library is not being able to hand it on -- a grant is a decision
// about a relationship and re-sharing would make it a decision the grantee can
// copy, which is the shape that turns a share of one library into a share
// nobody can account for. An administrator is not an exception: metadata for
// every library, content for none, and a grant is a key to content.
//
// Whole-library grants only. The model carries a path because a share of a
// folder is a grant on the virtual library that gives that folder an id, and
// nothing creates a VirtualLibrary row -- so a path in this request would be a
// field with no mechanism under it.

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/SiloDrive/silo/fileserver/account"
	"github.com/SiloDrive/silo/fileserver/libmgr"
	"github.com/SiloDrive/silo/fileserver/middleware"
	"github.com/SiloDrive/silo/fileserver/notif"
	"github.com/SiloDrive/silo/fileserver/option"
	"github.com/SiloDrive/silo/fileserver/share"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// sharePayload is one grant as an operator reads it: who, as what, where.
//
// Email is filled in for user principals and empty for the others, because an
// account id is not something anybody shared a library at. It is a second read
// per row and the row count is a library's share list, which is small by the
// nature of the thing -- an N+1 that grows with a decision somebody made by
// hand rather than with the install.
type sharePayload struct {
	Principal string `json:"principal"`
	Email     string `json:"email,omitempty"`
	Perm      string `json:"perm"`
	Path      string `json:"path"`
	Ctime     int64  `json:"ctime"`
}

// ownedLibrary answers with the library this request is about, or refuses the
// request itself and returns false.
//
// The refusals are DeleteLibraryHandler's, in the same order and with the same
// meanings: a read-only credential cannot make this decision even though the
// account could, a library that does not exist is `404`, and one somebody else
// owns is `403`.
//
// The scope is asked on every method, including the listing. A grant is a fact
// about the whole library -- who else holds it, and as what -- so a credential
// cut to one folder has no business reading it either, and the write half is
// what GET is exempt from rather than the narrowing. This used to ask
// CredentialCanWrite, which is only the permission half of the ceiling, so a
// path-scoped credential could hand the whole library to somebody else; see
// middleware.CredentialCanWriteLibrary for the shape of that mistake.
func ownedLibrary(w http.ResponseWriter, r *http.Request) (string, bool) {
	libraryID := mux.Vars(r)["libraryid"]
	if !middleware.CredentialReachesLibrary(r, libraryID) {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return "", false
	}
	if r.Method != http.MethodGet && !middleware.CredentialCanWriteLibrary(r, libraryID) {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return "", false
	}
	owner, err := libmgr.GetLibraryOwner(libraryID)
	if err != nil {
		log.Errorf("Failed to get library owner: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return "", false
	}
	if owner.IsZero() {
		http.Error(w, "Library not found", http.StatusNotFound)
		return "", false
	}
	if owner != middleware.GetAccountID(r) {
		http.Error(w, "Only the library owner can manage its shares", http.StatusForbidden)
		return "", false
	}
	return libraryID, true
}

// ListSharesHandler handles GET /api/silo/v1/libraries/{libraryid}/shares.
func ListSharesHandler(w http.ResponseWriter, r *http.Request) {
	libraryID, ok := ownedLibrary(w, r)
	if !ok {
		return
	}

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	grants, err := share.ForLibrary(ctx, libraryID)
	if err != nil {
		log.Errorf("Failed to list the shares on %s: %v", libraryID, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	out := make([]sharePayload, 0, len(grants))
	for _, g := range grants {
		p := sharePayload{
			Principal: string(g.Principal),
			Perm:      g.Perm,
			Path:      g.Path,
			Ctime:     g.Ctime,
		}
		if id, ok := userPrincipalID(g.Principal); ok {
			if acct, err := account.ByID(ctx, id); err == nil {
				p.Email = acct.Email
			}
			// A principal naming an account that cannot be read is listed
			// without an address rather than dropped. A grant that is doing
			// something must be visible to whoever can take it away.
		}
		out = append(out, p)
	}
	writeJSON(w, http.StatusOK, out)
}

type createShareRequest struct {
	Email string `json:"email"`
	Perm  string `json:"perm"`
}

// CreateShareHandler handles POST /api/silo/v1/libraries/{libraryid}/shares.
func CreateShareHandler(w http.ResponseWriter, r *http.Request) {
	libraryID, ok := ownedLibrary(w, r)
	if !ok {
		return
	}

	var req createShareRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	email := account.Normalize(req.Email)
	if email == "" {
		http.Error(w, "email is required", http.StatusBadRequest)
		return
	}
	// Spelled here as well as in share.Add, because the model's message is
	// about a grant and this one is about a request: a caller sending "read"
	// or "write" needs to be told the vocabulary, not that a grant was refused.
	if req.Perm != "r" && req.Perm != "rw" {
		http.Error(w, `perm must be "r" or "rw"`, http.StatusBadRequest)
		return
	}

	// Refused before the recipient is looked up, and refused rather than
	// ignored. An owner already holds rw on their own library, so a grant to
	// themselves would be a row that changes nothing -- and one that survives
	// the library changing hands, which is a decision nobody made.
	owner := middleware.GetAccount(r)
	if owner != nil && account.Normalize(owner.Email) == email {
		http.Error(w, "A library is already its owner's", http.StatusBadRequest)
		return
	}

	// An encrypted library is refused here rather than shared into
	// unreadability. The grant is only half of that share: the other half is a
	// content key wrapped to the recipient's public key, which only the
	// sharer's client can produce, and without it the recipient gets a library
	// whose every name and byte is sealed to somebody else. Saying so is better
	// than a share that appears to work. docs/plans/e2ee-completion.md step 3.
	if lib := libmgr.Get(libraryID); lib != nil && lib.Format.E2EE {
		http.Error(w,
			"Sharing an end-to-end encrypted library needs the content key wrapped to the "+
				"recipient, which is not built yet: a grant alone would hand them a library they "+
				"cannot read.",
			http.StatusNotImplemented)
		return
	}

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	// An address nobody has enrolled is held for them, which is
	// docs/plans/sharing.md's identity split: the share mints the inactive
	// account the address will belong to, and redemption of an invite to that
	// address claims it along with whatever was shared to it while nobody was
	// there. The alternative -- refusing until the person exists -- makes the
	// order of two independent decisions matter, and there is nothing to
	// enumerate here that the address does not already tell the caller, since
	// they typed it.
	recipient, err := account.ByEmail(ctx, email)
	var recipientID account.ID
	switch {
	case err == nil:
		recipientID = recipient.ID
	case errors.Is(err, account.ErrNotFound):
		recipientID, err = account.Tombstone(ctx, email, account.DefaultRole)
		if err != nil {
			log.Errorf("Failed to hold a share for %s: %v", email, err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
	default:
		log.Errorf("Failed to read the account for %s: %v", email, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	principal := share.UserPrincipal(recipientID)
	g := share.Grant{
		Principal: principal,
		LibraryID: libraryID,
		Perm:      req.Perm,
		CreatedBy: middleware.GetAccountID(r),
		Ctime:     time.Now().Unix(),
	}
	if err := share.Add(ctx, g); err != nil {
		log.Errorf("Failed to share %s with %s: %v", libraryID, email, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// The recipient's set moved, and nothing they hold is subscribed to a
	// library they could not see a moment ago -- so it is the account lane that
	// has to be told, exactly as it is for a library that was just created.
	notif.NotifyAccountUpdate(recipientID)

	log.Infof("Library %s shared %s with %s", libraryID, req.Perm, email)
	writeJSON(w, http.StatusCreated, sharePayload{
		Principal: string(principal),
		Email:     email,
		Perm:      g.Perm,
		Path:      "/",
		Ctime:     g.Ctime,
	})
}

// DeleteShareHandler handles
// DELETE /api/silo/v1/libraries/{libraryid}/shares/{principal}.
//
// Idempotent, which is share.Remove's rule rather than this handler's: the
// caller asked for a state, and a second delete is somebody making sure.
func DeleteShareHandler(w http.ResponseWriter, r *http.Request) {
	libraryID, ok := ownedLibrary(w, r)
	if !ok {
		return
	}

	principal := share.Principal(mux.Vars(r)["principal"])
	// Checked, because idempotence and a typo are indistinguishable otherwise:
	// a `DELETE .../user@example.com` would answer 204 and remove nothing, and
	// the caller would believe the share was gone.
	switch principal.Kind() {
	case "user", "group", "anon", "link":
	default:
		http.Error(w, `principal must be "user:<id>", "group:<id>", "link:<id>" or "anon"`,
			http.StatusBadRequest)
		return
	}

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	if err := share.Remove(ctx, principal, libraryID, "/"); err != nil {
		log.Errorf("Failed to remove the grant to %s on %s: %v", principal, libraryID, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if id, ok := userPrincipalID(principal); ok {
		// Told the same way an arrival is, and for the stronger reason: a
		// socket holding a subscription to a library it can no longer see has
		// to drop it, and only a resync does that.
		notif.NotifyAccountUpdate(id)
	}

	log.Infof("The grant to %s on %s was removed", principal, libraryID)
	w.WriteHeader(http.StatusNoContent)
}

// userPrincipalID reads the account out of a user principal, and reports false
// for every other kind.
func userPrincipalID(p share.Principal) (account.ID, bool) {
	rest, found := strings.CutPrefix(string(p), "user:")
	if !found {
		return account.Zero, false
	}
	id, err := account.ParseID(rest)
	if err != nil {
		return account.Zero, false
	}
	return id, true
}
