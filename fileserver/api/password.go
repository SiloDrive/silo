package api

// Self-service password change. docs/auth.md fixes two things about it that
// are not obvious from the outside, and both are here:
//
//   - It requires the *current* password even though the request is already
//     authenticated. Otherwise a stolen device credential upgrades itself into
//     account takeover, and the point of a scoped, revocable credential is
//     that it cannot become the account.
//   - It revokes nothing unless the caller sends `revoke_others`, and never
//     the credential that asked. This is the third rule the handler has had,
//     and the two before it are worth keeping in view. It began by revoking
//     only sessions, on the argument that unmounting somebody's laptop as a
//     side effect of routine hygiene teaches them to stop doing hygiene --
//     which spared precisely the credential most worth worrying about, since
//     a device credential is ninety days and renews from itself, so a stolen
//     one outlived the password it was minted under indefinitely. It then
//     revoked everything, the caller included, because changing the password
//     is the action a person already knows to reach for when they think
//     something has been taken and nothing else was pointed at a stolen
//     device row. That argument was right and is now spent: `auth/logout/
//     others` says the same thing by name, so the password change no longer
//     carries it as a side effect. `silo user passwd` -- an administrator
//     resetting a password somebody has lost control of -- still revokes
//     everything, because which thing was lost is not knowable from there.

import (
	"context"
	"errors"
	"net/http"

	"github.com/SiloDrive/silo/fileserver/account"
	"github.com/SiloDrive/silo/fileserver/authmgr"
	"github.com/SiloDrive/silo/fileserver/credential"
	"github.com/SiloDrive/silo/fileserver/middleware"
	"github.com/SiloDrive/silo/fileserver/option"
	"github.com/SiloDrive/silo/store"
	log "github.com/sirupsen/logrus"
)

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
	// ClientKDFParams turns this request into the split-derivation crossover:
	// new_password carries an authKey rather than a password, and this says
	// what it was derived under. The two are written together or not at all —
	// a hash whose parameters did not land describes a secret the client can
	// no longer produce.
	//
	// Absent means an ordinary password change, which is still the only kind
	// any shipped client makes.
	ClientKDFParams string `json:"client_kdf_params"`

	// RevokeOthers signs the account's other hosts out as part of the change.
	//
	// It defaults to **false**, which is a deliberate choice against the safer
	// default and is argued in docs/plans/credential-management.md § Step 3.
	// Rotating a password is not evidence that anything was stolen, and
	// unmounting somebody's laptop, NAS and phone because they improved a
	// password is how people learn not to improve passwords.
	//
	// What the default costs is real: somebody who changes their password
	// *because* they think they have been compromised, and who stops there, is
	// no longer covered by a side effect they did not know they had. That is
	// answered at the point of use -- the response says what is still signed
	// in, and the CLI names the next step -- rather than by revoking hosts
	// nobody asked about.
	//
	// True calls the same credential.RevokeOthers that auth/logout/others
	// does, so "everything but me" is one operation rather than a special case
	// living in here.
	RevokeOthers bool `json:"revoke_others"`
}

// writeNewSecret stores whatever this request is changing the account to, and
// reports whether the handler should carry on. It has already answered the
// caller when it returns false.
//
// Two shapes reach here and they are not interchangeable:
//
//   - With client_kdf_params, new_password is an authKey. Hash and parameters
//     go down in one statement, because a hash whose parameters did not land
//     describes a secret nothing can reproduce.
//   - Without, new_password is a password, and the parameters are cleared —
//     see account.SetPassword for why that is a statement rather than a loss.
//
// The refusal in between is the one worth naming. An account already crossed
// over, sent a change with no parameters, would be put back on password login
// by a client that did not know it was doing it: every device that had been
// sending an authKey would start failing, and the server would look wrong
// rather than the request. A client that means to undo a crossover can say so
// by sending the password change to an account that has not crossed over —
// which is to say, it cannot, and that is deliberate. This is not a state to
// arrive at by omission.
func writeNewSecret(w http.ResponseWriter, ctx context.Context, acct *account.Account, req changePasswordRequest) bool {
	if req.ClientKDFParams != "" {
		if _, err := store.ParseKDFParams(req.ClientKDFParams); err != nil {
			http.Error(w, "client_kdf_params is not a parameter string this server can read: "+err.Error(),
				http.StatusBadRequest)
			return false
		}
		if err := authmgr.SetAccountAuthKey(ctx, acct.ID, req.NewPassword, req.ClientKDFParams); err != nil {
			log.Errorf("Failed to cross %s over to split-derivation login: %v", acct.Email, err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return false
		}
		return true
	}

	// An account with no stored hash at all is not an error here, and there is
	// exactly one way to be one: redemption, where this function writes the
	// first secret an invited account ever has. It has not crossed over --
	// nothing has been written for it to have crossed over with -- so the
	// refusal below has nothing to refuse, and an empty hash says so.
	_, stored, err := account.PasswordHash(ctx, acct.Email)
	if err != nil && !errors.Is(err, account.ErrNotFound) {
		log.Errorf("Failed to read the stored hash for %s: %v", acct.Email, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return false
	}
	if authmgr.IsAuthKeyHash(stored) {
		http.Error(w,
			"This account logs in with a derived key, so a password change has to carry the "+
				"parameters it was derived under. Send client_kdf_params alongside new_password.",
			http.StatusConflict)
		return false
	}

	if err := authmgr.SetAccountPassword(ctx, acct.ID, req.NewPassword); err != nil {
		log.Errorf("Failed to set a new password for %s: %v", acct.Email, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return false
	}
	return true
}

// ChangePasswordHandler handles POST /api/silo/v1/auth/password.
func ChangePasswordHandler(w http.ResponseWriter, r *http.Request) {
	acct := middleware.GetAccount(r)
	if acct == nil {
		log.Errorf("Password change reached %s with no account in context", r.URL.Path)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Asked before the body is read. Setting the account's password is the
	// most consequential write there is, and a credential handed out
	// read-only is the one somebody gave to something they did not fully
	// trust. Nobody is locked out by this: changing a password means holding
	// it, and holding it means a fresh login is available.
	//
	// The narrowing is applied a layer up: this route names no library, so a
	// credential scoped to one never reaches here.
	if !middleware.CredentialCanWrite(r) {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return
	}

	var req changePasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// Both are refused empty, which is the rule `silo user passwd` already
	// holds through confirmPassword: an empty password would leave an account
	// no password can open. One policy, not two.
	if req.CurrentPassword == "" || req.NewPassword == "" {
		http.Error(w, "current_password and new_password are required", http.StatusBadRequest)
		return
	}

	// The same buckets the login endpoint spends. A credential is not a
	// throttle: whoever holds one can guess the password here as fast as the
	// server will hash, which is the attack the current-password requirement
	// exists to stop and would not stop unbounded.
	releaseAttempt, ok := allowLoginAttempt(w, r, acct.Email)
	if !ok {
		return
	}
	defer releaseAttempt()

	// Against the account resolved from the credential, not an address in the
	// body. There is no address in the body for exactly this reason -- a
	// caller changes their own password, and nothing here should be able to
	// name somebody else's.
	if _, err := authmgr.ValidatePassword(acct.Email, req.CurrentPassword); err != nil {
		loginFailed(r, acct.Email)
		log.Infof("Password change refused for %s: %v", acct.Email, err)
		http.Error(w, "Invalid password", http.StatusUnauthorized)
		return
	}
	loginSucceeded(acct.Email)

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	if !writeNewSecret(w, ctx, acct, req) {
		return
	}

	// After the password is set, for the reason `silo user passwd` gives: a
	// revocation that ran and then failed to change the password would sign
	// the user out and leave the old password working, which is the worst of
	// both. Failing here leaves the new password live and some credentials
	// alive, so it is reported rather than swallowed.
	//
	// Nothing, unless the caller asked -- and never the credential that asked.
	//
	// This handler used to revoke everything the account held, the caller's own
	// row included. The argument for that is preserved at the top of this file
	// and is still right about the rule it replaced: sparing device credentials
	// while revoking sessions is the worst of the three options, because it
	// spares precisely the long-lived, self-renewing credential worth worrying
	// about. What it was missing is a third option. There was no way to sign
	// the other hosts out by name, so the password change carried that job as a
	// side effect and everybody rotating a password paid for it.
	//
	// auth/logout/others is that name, and this is now its caller rather than
	// its substitute.
	if !req.RevokeOthers {
		log.Infof("Password changed for %s; other sessions left signed in", acct.Email)
		writeJSON(w, http.StatusOK, revokedResponse{Revoked: 0})
		return
	}
	// The caller keeps its own credential in both modes. It demonstrably holds
	// the password -- it just sent it -- so signing it out proves nothing, and
	// it is the part of the old behaviour that most read as a bug.
	n, err := credential.RevokeOthers(ctx, acct.ID, callerCredentialID(r))
	if err != nil {
		// 200, not 500, and the flag is why. The password IS changed by the
		// time this runs, and a 500 said otherwise: there are two 500s on this
		// handler — the SetAccountPassword failure above, where nothing
		// changed, and this one, where everything did — and a client cannot
		// tell them apart from a status. A client that read this as failure
		// kept caching the old password while the server held the new one, so
		// its next token expiry became a re-login that could not succeed. That
		// is exactly the stranding the caller's own ordering comment says it
		// avoids, arriving through the one path it did not consider.
		//
		// So the operation the caller asked for is reported as what it is —
		// done — and the part that failed is reported beside it rather than
		// instead of it. The operator still sees the failure in the log.
		log.Errorf("Password changed for %s, but revoking their other credentials failed: %v", acct.Email, err)
		writeJSON(w, http.StatusOK, revokedResponse{Revoked: 0, SessionsStillLive: true})
		return
	}
	log.Infof("Password changed for %s; %d other credential(s) revoked", acct.Email, n)
	writeJSON(w, http.StatusOK, revokedResponse{Revoked: n})
}

// callerCredentialID is the row this request authenticated with, or "" if
// there is none -- which the authenticated lane makes unreachable.
//
// Empty is safe rather than merely tolerated: it matches no id, so
// RevokeOthers keeps nothing extra and the revocation is still confined to the
// account. The alternative, refusing, would fail a password change that has
// already succeeded.
func callerCredentialID(r *http.Request) string {
	if cur := middleware.GetCredential(r); cur != nil {
		return cur.ID
	}
	return ""
}
