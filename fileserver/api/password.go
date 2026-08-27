package api

// Self-service password change. docs/auth.md fixes two things about it that
// are not obvious from the outside, and both are here:
//
//   - It requires the *current* password even though the request is already
//     authenticated. Otherwise a stolen device credential upgrades itself into
//     account takeover, and the point of a scoped, revocable credential is
//     that it cannot become the account.
//   - It revokes session credentials and leaves device ones mounted.
//     Unmounting somebody's laptop as a side effect of routine hygiene teaches
//     them to stop doing hygiene. `silo user passwd` is the other case -- an
//     administrator resetting a password somebody has lost control of -- and
//     that one revokes everything.

import (
	"net/http"

	"github.com/dkam/silo/fileserver/authmgr"
	"github.com/dkam/silo/fileserver/credential"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/option"
	log "github.com/sirupsen/logrus"
)

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
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
	if !allowLoginAttempt(w, r, acct.Email) {
		return
	}

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

	if err := authmgr.SetAccountPassword(ctx, acct.ID, req.NewPassword); err != nil {
		log.Errorf("Failed to set a new password for %s: %v", acct.Email, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// After the password is set, for the reason `silo user passwd` gives: a
	// revocation that ran and then failed to change the password would sign
	// the user out and leave the old password working, which is the worst of
	// both. Failing here leaves the new password live and some sessions
	// alive, so it is reported rather than swallowed.
	//
	// The session that asked is revoked along with the rest. It gets no
	// exemption: the rule is about kinds, and a carve-out for "this one"
	// would mean a client could not tell from the count whether it had been
	// signed out.
	n, err := credential.RevokeKind(ctx, acct.ID, credential.KindSession)
	if err != nil {
		log.Errorf("Password changed for %s, but revoking their sessions failed: %v", acct.Email, err)
		http.Error(w, "The password was changed, but signing out other sessions failed",
			http.StatusInternalServerError)
		return
	}
	log.Infof("Password changed for %s; %d session credential(s) revoked", acct.Email, n)
	writeJSON(w, http.StatusOK, revokedResponse{Revoked: n})
}
