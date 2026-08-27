package api

// The account side of end-to-end encryption over HTTP: what an account
// publishes, what it reads back, and the one endpoint that answers before
// anybody has logged in.
//
// The server stores four things it cannot read and is careful about exactly
// two of them -- that the parameters a client publishes agree with the ones
// sealed inside its blob (account.Keys.Validate), and that an address nobody
// holds is answered indistinguishably from one somebody does.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"net/http"
	"strconv"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/serversecret"
	"github.com/dkam/silo/store"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// GetAccountKeysHandler handles GET /api/silo/v1/account/keys.
//
// It serves the wrapped blobs as well as the public key, and that is the
// point: this is what a new device reads after logging in, so it can derive
// wrapKey from the password it was just given and open the identity key. The
// blobs are an offline attack target, which is why they are here behind a
// credential rather than beside the parameters on the pre-login endpoint.
func GetAccountKeysHandler(w http.ResponseWriter, r *http.Request) {
	acct := middleware.GetAccount(r)
	if acct == nil {
		log.Errorf("Account keys asked for on %s with no account in context", r.URL.Path)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	keys, err := account.GetKeys(ctx, acct.ID)
	if errors.Is(err, account.ErrNoKeys) {
		http.Error(w, "This account has published no identity key", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Errorf("Failed to read account keys: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, keys)
}

// PutAccountKeysHandler handles PUT /api/silo/v1/account/keys.
//
// PUT rather than POST because the body is the whole of what the account
// holds, not an addition to it: publishing again after a password change
// replaces every blob at once, and a verb that reads as "append" would invite
// exactly the partial update that leaves an account holding one blob its
// parameters no longer open.
func PutAccountKeysHandler(w http.ResponseWriter, r *http.Request) {
	acct := middleware.GetAccount(r)
	if acct == nil {
		log.Errorf("Account keys published on %s with no account in context", r.URL.Path)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	// Publishing is a write. A read-only credential may read what the account
	// holds and must not replace it: replacing the identity key is how a
	// password change lands, and it is the one write that can make an
	// account's own libraries unreadable.
	if !middleware.CredentialCanWrite(r) {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return
	}

	var keys account.Keys
	if !decodeJSON(w, r, &keys) {
		return
	}

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	if err := account.SetKeys(ctx, acct.ID, keys); err != nil {
		if errors.Is(err, account.ErrBadKeys) {
			// Said in full. Every one of these is a client bug whose symptom
			// otherwise appears months later on a device that cannot
			// bootstrap, and the client author is the only audience that can
			// act on it.
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		log.Errorf("Failed to publish account keys: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	stored, err := account.GetKeys(ctx, acct.ID)
	if err != nil {
		log.Errorf("Failed to read back published account keys: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		UpdatedAt int64 `json:"updated_at"`
		Recovery  int   `json:"recovery"`
	}{UpdatedAt: stored.UpdatedAt, Recovery: len(stored.Recovery)})
}

// DeleteRecoveryWrapHandler handles
// DELETE /api/silo/v1/account/keys/recovery/{ordinal}.
//
// This is redemption. The client fetched the set, found the blob its code
// opened, and is telling the server that one is spent; the rest of the set
// stands. The server never learns the code, so it takes the client's word for
// which blob went -- which costs nothing, because the only thing a lying
// client can destroy is its own account's recovery options.
func DeleteRecoveryWrapHandler(w http.ResponseWriter, r *http.Request) {
	acct := middleware.GetAccount(r)
	if acct == nil {
		log.Errorf("A recovery wrap was redeemed on %s with no account in context", r.URL.Path)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if !middleware.CredentialCanWrite(r) {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return
	}

	ordinal, err := strconv.Atoi(mux.Vars(r)["ordinal"])
	if err != nil {
		http.Error(w, "The recovery ordinal must be a number", http.StatusBadRequest)
		return
	}

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	gone, err := account.DeleteRecoveryWrap(ctx, acct.ID, ordinal)
	if err != nil {
		log.Errorf("Failed to redeem a recovery wrap: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if !gone {
		http.Error(w, "No such recovery wrap", http.StatusNotFound)
		return
	}

	keys, err := account.GetKeys(ctx, acct.ID)
	if err != nil {
		log.Errorf("Failed to count remaining recovery wraps: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Remaining int `json:"remaining"`
	}{Remaining: len(keys.Recovery)})
}

// kdfSecretName is the purpose the dummy salt is derived under. Named rather
// than shared: a second use for a server secret gets its own row.
const kdfSecretName = "auth/kdf-dummy/v1"

type kdfParamsRequest struct {
	Email string `json:"email"`
}

type kdfParamsResponse struct {
	KDFParams string `json:"kdf_params"`
}

// KDFParamsHandler handles POST /api/silo/v1/auth/kdf.
//
// It is the pre-login parameters endpoint: given an address it answers with
// the argon2id parameters that address's password is stretched under, so a
// client can derive the authKey it sends and the wrapKey it keeps. It is
// unauthenticated because nothing can be authenticated yet -- that is what
// pre-login means.
//
// Two things about it do not fall out of writing the obvious handler.
//
// **It is an account-enumeration oracle by default.** An address nobody holds
// must receive plausible parameters rather than a 404, and the same ones every
// time, or the difference between two requests answers the question. They are
// derived from HMAC(server secret, normalized address), exactly as the dummy
// password hash closes the same gap on login -- and from a secret that is
// stored rather than generated at boot, because an answer that changed across
// a restart would say "no account here" just as loudly as a 404.
//
// **The answer is attacker-influenced input to the client's KDF.** A server
// answering m=4 GiB does not weaken anything; it takes the device down. The
// client validates against store.KDFParams.Validate before deriving, which is
// the request that makes that guard load-bearing rather than decorative.
//
// POST rather than GET, for a request that reads. The address is the one
// identifier this endpoint takes and the request is unauthenticated, so a
// query string would put every address anyone asked about into the access log,
// the proxy log, and any Referer a browser sent onward.
func KDFParamsHandler(w http.ResponseWriter, r *http.Request) {
	var req kdfParamsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Email == "" {
		http.Error(w, "email is required", http.StatusBadRequest)
		return
	}
	if !allowKDFRequest(w, r) {
		return
	}

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	params, err := account.ClientKDFParams(ctx, req.Email)
	if err == nil {
		writeJSON(w, http.StatusOK, kdfParamsResponse{KDFParams: params})
		return
	}
	if !errors.Is(err, account.ErrNotFound) {
		log.Errorf("Failed to read client KDF parameters: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	fake, err := dummyKDFParams(ctx, req.Email)
	if err != nil {
		// Answering anything else here -- a 404, a fallback salt, a constant
		// -- would tell this caller what the whole handler exists not to say.
		// A database this server cannot read fails every address alike, which
		// says nothing about any of them.
		log.Errorf("Failed to derive dummy KDF parameters: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, kdfParamsResponse{KDFParams: fake})
}

// dummyKDFParams is what an address with no published parameters is answered
// with: deterministic per address, stable for the life of the install, and at
// the default cost, because the default is what a real account will almost
// always be carrying and a fake at any other cost would stand out.
func dummyKDFParams(ctx context.Context, email string) (string, error) {
	secret, err := serversecret.Named(ctx, kdfSecretName)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, secret)
	// The same normalisation the lookup used, so that two spellings of one
	// address cannot be told apart by their answers either.
	mac.Write([]byte(account.Normalize(email)))

	var salt [store.KDFSaltSize]byte
	copy(salt[:], mac.Sum(nil))
	return store.DefaultKDFParams(salt).String(), nil
}
