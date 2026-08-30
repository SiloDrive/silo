package client

// Key bootstrap: turning a password into an identity, and an identity into a
// library's keyring.
//
// These are the two calls every E2EE operation starts from, and they are here
// rather than in porter or cmd/silo so that one implementation of the
// bootstrap serves both. docs/plans/e2ee-completion.md step 1 owns the
// sequence; store/ owns every derivation this file performs.
//
// What this file does not do is derive an auth key. Login still sends the
// password, so the server still sees the secret that opens the identity blob;
// closing that is split-derivation login, and until it lands the encryption
// here is real against a stolen disk and theatre against the server itself.
// docs/storage.md § One password, split client-side is the owning document.

import (
	"errors"
	"fmt"

	"github.com/dkam/silo/store"
)

// AccountKeys is what GET /account/keys serves: the account's public key, the
// blobs that open its private one, and the id every one of them is bound to.
type AccountKeys struct {
	// AccountID is the holder: the string the wraps below bind as associated
	// data. Without it the blobs cannot be opened, which is why it travels
	// with them.
	AccountID  string         `json:"account_id"`
	PublicKey  []byte         `json:"public_key"`
	WrappedKey []byte         `json:"wrapped_key"`
	KDFParams  string         `json:"kdf_params"`
	Recovery   []RecoveryWrap `json:"recovery"`
	UpdatedAt  int64          `json:"updated_at"`
}

// RecoveryWrap is one recovery-code wrap of the same identity key.
type RecoveryWrap struct {
	Ordinal    int    `json:"ordinal"`
	WrappedKey []byte `json:"wrapped_key"`
	Ctime      int64  `json:"ctime"`
}

// Account is an opened account: the identity key, and who it belongs to.
//
// The password is not in it, and is not kept anywhere after OpenAccount
// returns. The identity key is the long-term secret a device holds; a password
// is the thing that opened it once.
type Account struct {
	// ID is the account's own id, and the holder every wrap is bound to.
	ID string
	// Identity is the X25519 keypair every library content key is wrapped to.
	Identity *store.Identity

	// c is the client this account was opened through. Held so that
	// OpenLibrary reads like the one call it is, rather than one that has to
	// be handed back the client it came from.
	c *APIClient
}

// ErrNoAccountKeys reports an account that has published no identity key. It
// is not an error in the sense of a failure: it is what an account that has
// never enrolled looks like, and the caller's move is to enrol it.
var ErrNoAccountKeys = errors.New("client: this account has published no identity key")

// AccountKeys fetches the account's published key material.
func (c *APIClient) AccountKeys() (AccountKeys, error) {
	var keys AccountKeys
	if err := c.doRequest("GET", "/api/silo/v1/account/keys", nil, &keys); err != nil {
		return AccountKeys{}, err
	}
	return keys, nil
}

// ClientKDFParams asks what an address's password should be stretched under,
// before anybody has logged in.
//
// The answer is attacker-influenced input to argon2id, so it is parsed and
// validated here rather than trusted: a server answering with a memory cost of
// four gigabytes does not weaken anything, it takes the device down.
//
// An address with no account is answered with plausible parameters rather than
// a 404, on purpose — see the server's KDFParamsHandler. So a set coming back
// is not evidence that an account exists.
func (c *APIClient) ClientKDFParams(email string) (store.KDFParams, error) {
	var out struct {
		KDFParams string `json:"kdf_params"`
	}
	if err := c.doRequest("POST", "/api/silo/v1/auth/kdf",
		map[string]string{"email": email}, &out); err != nil {
		return store.KDFParams{}, err
	}
	p, err := store.ParseKDFParams(out.KDFParams)
	if err != nil {
		return store.KDFParams{}, fmt.Errorf("client: the server's KDF parameters: %w", err)
	}
	if err := p.Validate(); err != nil {
		return store.KDFParams{}, fmt.Errorf("client: the server's KDF parameters: %w", err)
	}
	return p, nil
}

// OpenAccount logs in and opens the account's identity key.
//
// The whole new-device bootstrap: log in, read the published key material,
// derive under the parameters the blob itself carries, unwrap. It returns the
// identity and forgets the password.
//
// The parameters are read from the blob rather than from the pre-login
// endpoint, because store.OpenIdentityWithPassword is the one implementation
// of this flow with the argon2id floor already inside it, and a client that
// assembles the steps itself is a client that can assemble them in an order
// that skips the floor. ClientKDFParams exists for enrolment, which has no
// blob to read parameters out of yet.
func (c *APIClient) OpenAccount(email, password string) (*Account, error) {
	if err := c.Login(email, password); err != nil {
		return nil, err
	}
	keys, err := c.AccountKeys()
	if err != nil {
		return nil, err
	}
	if len(keys.WrappedKey) == 0 {
		return nil, ErrNoAccountKeys
	}
	if keys.AccountID == "" {
		return nil, errors.New("client: the server served key material with no holder to open it with")
	}
	id, _, err := store.OpenIdentityWithPassword(password, keys.AccountID, keys.WrappedKey)
	if err != nil {
		return nil, fmt.Errorf("client: opening the identity key: %w", err)
	}
	return &Account{ID: keys.AccountID, Identity: id, c: c}, nil
}

// OpenLibrary fetches this account's wrap of a library's content key and opens
// it into a keyring.
//
// store.ErrNoKeyring reports a library this account holds no wrap for. That is
// the plain-library answer and the not-shared-with-me answer alike, and the
// server does not distinguish them either: both are a 404 from a route the
// caller is allowed to call.
func (a *Account) OpenLibrary(libraryID string) (*store.Keyring, error) {
	var out struct {
		WrappedKey []byte `json:"wrapped_key"`
	}
	err := a.c.doRequest("GET", "/api/silo/v1/libraries/"+libraryID+"/key", nil, &out)
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", store.ErrNoKeyring, libraryID)
		}
		return nil, err
	}
	kr, err := store.OpenKeyring(a.Identity, libraryID, out.WrappedKey)
	if err != nil {
		return nil, fmt.Errorf("client: unwrapping the content key: %w", err)
	}
	return kr, nil
}
