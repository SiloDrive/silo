package api

// Logout. docs/auth.md's list of what is left called this "the smallest gap":
// the route table had grown a way to mint a credential and had none to
// discard one, so a client that wanted to sign out had to ask an operator
// with shell access to run `silo token revoke`.
//
// There are two routes rather than one flag in a body, because the difference
// between them is a difference in what the request is *about* -- one credential
// or the whole account -- and that is the question the narrowing is asked. A
// field inside the body would put the answer somewhere the middleware cannot
// see it, and the scope check would have to move into the handler.

import (
	"net/http"

	"github.com/SiloDrive/silo/fileserver/credential"
	"github.com/SiloDrive/silo/fileserver/middleware"
	"github.com/SiloDrive/silo/fileserver/option"
	log "github.com/sirupsen/logrus"
)

// revokedResponse is what both routes answer with, and what the password
// change reports as well. The count is worth returning: "signed out of 4
// places" is a sentence a client can show, and it cannot be derived from a
// 204.
type revokedResponse struct {
	Revoked int64 `json:"revoked"`
	// SessionsStillLive is set only in the one case that needs saying: the
	// password changed and the revocation that follows it did not, so the
	// account's other credentials are still live. Omitted everywhere else, so
	// logout's two routes and a clean password change answer exactly as before.
	//
	// The wire name says sessions and now means every kind, because the
	// password change stopped sparing device credentials. It is left alone
	// deliberately: it is a field clients already decode -- client/enrol.go
	// does, and silo-drive may -- and renaming it would turn a widened meaning
	// into a broken decode. What a client does with it is unchanged either
	// way: tell the person the sign-out did not finish.
	//
	// It names the exception rather than the norm because that is the shape
	// omitempty rewards — a bool that is usually false and is worth reading
	// when it is true. Framing it the other way round ("signed out: false")
	// would be omitted in precisely the case a client has to notice.
	//
	// It exists because that case has to be a success — the password did
	// change — while still being distinguishable from a clean one. Reporting
	// it as a 500 made it indistinguishable from the OTHER 500 on that
	// handler, where nothing changed, and a client cannot cache the right
	// password without knowing which happened.
	SessionsStillLive bool `json:"sessions_still_live,omitempty"`

	// Current says the revoked row was the one that made the request, so a
	// client can tell "I signed something else out" from "I have just signed
	// myself out" without comparing ids it may not have kept. Set only by
	// DELETE account/credentials/{id}: the logout routes do not need it --
	// one is always the caller and the other is always everything -- and
	// omitempty keeps their bodies byte-for-byte what they were.
	Current bool `json:"current,omitempty"`
}

// LogoutHandler handles POST /api/silo/v1/auth/logout: discard the credential
// that made the request.
//
// Neither logout route asks for write permission. Revocation only ever takes
// access away, so a read-only credential may do it -- refusing would mean the
// client that has been narrowed to the point of harmlessness is also the one
// that cannot slam the door.
func LogoutHandler(w http.ResponseWriter, r *http.Request) {
	cred := middleware.GetCredential(r)
	if cred == nil {
		// The route is mounted under RequireOwnCredential, so this is a route
		// registered outside it rather than an unauthenticated caller.
		log.Errorf("Logout reached %s with no credential in context", r.URL.Path)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	// The account id comes from the resolved credential rather than the
	// request, so this cannot be aimed at somebody else's row: Revoke matches
	// on both columns.
	gone, err := credential.Revoke(ctx, cred.ID, cred.AccountID)
	if err != nil {
		log.Errorf("Failed to revoke credential %s: %v", cred.ID, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// gone is false only if something else revoked the row between Resolve
	// and here, which is the outcome the caller asked for. Reporting 0 rather
	// than 1 is the honest answer and needs no separate status: either way
	// the credential does not work any more.
	var n int64
	if gone {
		n = 1
	}
	writeJSON(w, http.StatusOK, revokedResponse{Revoked: n})
}

// LogoutEverywhereHandler handles POST /api/silo/v1/auth/logout/everywhere:
// discard every credential the account holds, including the one that asked.
//
// This is the account's own panic button, and it is the reason revocation had
// to become one table. It is mounted on the ordinary authenticated lane, so a
// credential narrowed to one library is refused it before reaching here --
// signing an account out of everything is strictly wider than a scope cut to
// a single library.
func LogoutEverywhereHandler(w http.ResponseWriter, r *http.Request) {
	acct := middleware.GetAccount(r)
	if acct == nil {
		log.Errorf("Logout reached %s with no account in context", r.URL.Path)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	n, err := credential.RevokeAll(ctx, acct.ID)
	if err != nil {
		log.Errorf("Failed to revoke every credential for %s: %v", acct.Email, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	log.Infof("Signed %s out everywhere: %d credential(s) revoked", acct.Email, n)
	writeJSON(w, http.StatusOK, revokedResponse{Revoked: n})
}

// LogoutOthersHandler handles POST /api/silo/v1/auth/logout/others: discard
// every credential the account holds except the one that asked.
//
// The third of three verbs, and the one that was missing. `logout` is this
// credential, `logout/everywhere` is all of them including this one, and
// neither could say "the others" -- so a person who wanted to sign a lost
// laptop out from the desktop they were sitting at had to sign the desktop out
// too, or revoke ids one at a time.
//
// It is what makes the password change able to revoke nothing by default. That
// default is only defensible because the operation it stopped doing silently
// is available by name; see docs/plans/credential-management.md § Step 3.
//
// Account-wide, so it takes the narrowing, exactly as logout/everywhere does.
// No write permission, as no revocation route asks for one.
func LogoutOthersHandler(w http.ResponseWriter, r *http.Request) {
	acct := middleware.GetAccount(r)
	if acct == nil {
		log.Errorf("Logout reached %s with no account in context", r.URL.Path)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	cur := middleware.GetCredential(r)
	if cur == nil {
		// Unreachable from the authenticated lane, and a 500 rather than a
		// fallback to RevokeAll: guessing here would sign the caller out of
		// the session they are holding, which is the one thing this route
		// exists not to do.
		log.Errorf("Logout reached %s with no credential in context", r.URL.Path)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	n, err := credential.RevokeOthers(ctx, acct.ID, cur.ID)
	if err != nil {
		log.Errorf("Failed to revoke other credentials for %s: %v", acct.Email, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	log.Infof("Signed %s out of everywhere else: %d credential(s) revoked", acct.Email, n)
	writeJSON(w, http.StatusOK, revokedResponse{Revoked: n})
}
