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
	"context"
	"net/http"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/authmgr"
	"github.com/dkam/silo/fileserver/credential"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/store"
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

	_, stored, err := account.PasswordHash(ctx, acct.Email)
	if err != nil {
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

	if !writeNewSecret(w, ctx, acct, req) {
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
		log.Errorf("Password changed for %s, but revoking their sessions failed: %v", acct.Email, err)
		writeJSON(w, http.StatusOK, revokedResponse{Revoked: 0, SessionsStillLive: true})
		return
	}
	log.Infof("Password changed for %s; %d session credential(s) revoked", acct.Email, n)
	writeJSON(w, http.StatusOK, revokedResponse{Revoked: n})
}
