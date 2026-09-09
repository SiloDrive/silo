package api

import (
	"errors"
	"net/http"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/authmgr"
	"github.com/dkam/silo/fileserver/credential"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/setup"
	log "github.com/sirupsen/logrus"
)

// setupRequest creates a server's first account.
//
// Deliberately not loginRequest's enrolment superset. Widening a request later
// is backward compatible and narrowing it is not, and there is nothing yet that
// would use kind, perm or scope here: a credential scoped to a library, on a
// server whose first account is being created in this request, cannot name a
// library that exists. If a native client ever wants to claim a server
// directly, embedding loginRequest and reusing enrolmentOpts is the widening.
type setupRequest struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	SetupToken string `json:"setup_token"`
}

// SetupHandler handles POST /api/silo/v1/auth/setup: the one request that turns
// a server with no accounts into a server with one.
//
// It is a separate endpoint rather than a field on LoginHandler, and the split
// is not tidiness. Login verifies a password against an account that exists,
// spends the per-account rate-limit bucket, and runs the dummy-hash defence
// that makes a missing account cost what a present one does. Setup does none of
// those -- there is no account to look up, and the address in the request is
// one the operator is inventing as they type it. Folding the two would give one
// handler with two disjoint halves and a reader no way to tell which rules were
// in force on which line.
//
// docs/auth.md's rule that every secret a client presents is a Credential row
// resolved by credential.Resolve has one documented exception, the notification
// token. This is the second, and the reason is structural rather than a
// convenience: Credential.account_id references Account(id), so on a server
// with no accounts the rule's own table cannot hold this secret. It
// authenticates nobody and authorises exactly one transition.
func SetupHandler(w http.ResponseWriter, r *http.Request) {
	var req setupRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.Email == "" || req.Password == "" || req.SetupToken == "" {
		http.Error(w, "Email, password and setup token are required", http.StatusBadRequest)
		return
	}

	releaseAttempt, ok := allowSetupAttempt(w, r)
	if !ok {
		return
	}
	defer releaseAttempt()

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	// The 409 pre-flight, and it asks the question 409 actually means: does
	// this server have an account? setup.Required would answer false for a
	// second reason -- no token row -- and reporting that as "already set up"
	// would be a lie. ClaimSetup re-checks inside its transaction and is the
	// guard; this is a fast path in the same sense as the existence check in
	// authmgr.CreateAccount.
	exists, err := account.Exists(ctx)
	if err != nil {
		log.Errorf("Failed to look for an existing account: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if exists {
		http.Error(w, "This server has already been set up", http.StatusConflict)
		return
	}

	// A malformed token and a wrong one get the same reply, so that a guesser
	// learns nothing about which half of the guess to keep.
	presented, err := setup.Parse(req.SetupToken)
	if err != nil {
		setupFailed(r)
		http.Error(w, "Invalid setup token", http.StatusUnauthorized)
		return
	}

	acctID, err := authmgr.ClaimSetup(ctx, presented, req.Email, req.Password)
	switch {
	case errors.Is(err, setup.ErrBadToken):
		setupFailed(r)
		log.Warn("A setup attempt presented the wrong token")
		http.Error(w, "Invalid setup token", http.StatusUnauthorized)
		return
	case errors.Is(err, setup.ErrAlreadySetUp):
		http.Error(w, "This server has already been set up", http.StatusConflict)
		return
	case err != nil:
		log.Errorf("Failed to claim the setup token: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	log.Warnf("Setup token claimed; this server's first account is %s", account.Normalize(req.Email))

	opts := defaultSessionOpts()
	opts.AccountID = acctID
	opts.Label = credentialLabel(r)
	_, token, err := credential.Issue(ctx, opts)
	if err != nil {
		// The account exists and the token is spent, so this is not something
		// to retry setup over -- and an operator who read "internal server
		// error" and tried again would get a 409 and conclude they were locked
		// out of a server that is working. Say which it is.
		log.Errorf("Failed to issue the first session credential: %v", err)
		http.Error(w, accountCreatedButNoCredential, http.StatusInternalServerError)
		return
	}

	// Login's shape exactly, so a client reuses its parsing. 201 rather than
	// login's 200 because this request created something: docs/responses.md
	// gives 201 to the requests that bring a thing into existence, and an
	// account is the largest one Silo has.
	writeJSON(w, http.StatusCreated, loginResponse{Token: token})
}

// accountCreatedButNoCredential is the body for the narrow window where setup
// wrote the account but could not hand back a credential for it.
const accountCreatedButNoCredential = "The account was created, but a session could not be issued. " +
	"Log in with the email and password you just chose; the setup token is spent."
