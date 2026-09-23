package client

// Enrolment, and the crossover that comes with it.
//
// Enrolling mints the identity key every library content key is wrapped to and
// publishes it wrapped under a key the server never sees. In the same breath it
// moves the account onto split-derivation login, because the two are one fact:
// the wrapKey that opens the blob is derived from the password, so an account
// that publishes a blob and keeps sending its password to log in has published
// nothing -- the server sees the wrapping secret at every login. docs/auth.md
// § Split-derivation login is the owning document, and silo#13 is the gate
// this closes.

import (
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/SiloDrive/silo/store"
)

// AccountInfo is what GET /account says about the caller.
type AccountInfo struct {
	// AccountID is the holder every wrap binds as associated data. It is the
	// one thing an account needs before it can publish anything, which is why
	// it is served by an endpoint that answers before enrolment rather than
	// only alongside the blobs it opens.
	AccountID string `json:"account_id"`
	Email     string `json:"email"`
}

// AccountInfo asks the server who this credential belongs to.
func (c *APIClient) AccountInfo() (AccountInfo, error) {
	var info AccountInfo
	if err := c.doRequest("GET", "/api/silo/v1/account", nil, &info); err != nil {
		return AccountInfo{}, err
	}
	if info.AccountID == "" {
		return AccountInfo{}, errors.New("client: the server named no account")
	}
	return info, nil
}

// PublishKeys replaces the account's key material.
//
// The whole set at once, because that is what the endpoint is: a password
// change produces a new wrapKey, so every blob the account holds is re-wrapped
// together or the account is left holding one its parameters no longer open.
//
// recovery is passed through rather than rebuilt. A recovery wrap seals the
// identity key under a key derived from its code, so it does not depend on the
// password and survives a change of it unaltered -- but this endpoint replaces
// the whole set, so a caller that omits them deletes them.
func (c *APIClient) PublishKeys(public, wrapped []byte, params store.KDFParams, recovery []RecoveryWrap) error {
	return c.doRequest("PUT", "/api/silo/v1/account/keys", struct {
		PublicKey  []byte         `json:"public_key"`
		WrappedKey []byte         `json:"wrapped_key"`
		KDFParams  string         `json:"kdf_params"`
		Recovery   []RecoveryWrap `json:"recovery"`
	}{public, wrapped, params.String(), recovery}, nil)
}

// Enrol gives an account an identity key and moves it onto derived login.
//
// The order is the part that matters, and it is the safe one of the two
// available. The identity is published first, wrapped under the wrapKey this
// password already produces, and only then does the login secret change. A
// failure in between leaves an account that still logs in with its password
// and holds a blob that password opens -- recoverable by running this again.
// The other order leaves an account whose login has moved and whose blob was
// never stored, which is not.
//
// It returns the opened account, because the caller has just paid argon2id and
// holds the identity in memory; making them call OpenAccount afterwards would
// pay it a second time for a key already in hand.
//
// It also returns the account's recovery codes, and this is the only moment
// they exist. They are wrapped to the identity key here and nowhere else --
// the server never sees one, this client does not keep one, and no later call
// can produce them, because minting a set means wrapping the private half and
// after this returns nothing holds it. A caller that discards the slice has
// made an account whose password is its single point of failure; the codes are
// a return value rather than a field so that discarding them has to be
// deliberate.
func (c *APIClient) Enrol(email, password string) (*Account, []string, error) {
	// The password, because this account has not crossed over yet -- that is
	// what enrolling is about to do.
	if err := c.Login(email, password); err != nil {
		return nil, nil, err
	}
	info, err := c.AccountInfo()
	if err != nil {
		return nil, nil, err
	}

	var salt [store.KDFSaltSize]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return nil, nil, fmt.Errorf("client: generating a KDF salt: %w", err)
	}
	params := store.DefaultKDFParams(salt)
	creds, err := store.DeriveCredentials(password, params)
	if err != nil {
		return nil, nil, err
	}

	identity, err := store.GenerateIdentity()
	if err != nil {
		return nil, nil, err
	}
	priv := identity.Private()
	wrapped, err := store.WrapIdentity(creds.WrapKey, info.AccountID, params, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("client: wrapping the identity key: %w", err)
	}
	// The recovery set goes up in the same PUT as the identity blob. Not a
	// later call: this endpoint replaces everything it is given, so a second
	// publish adding the wraps would be a second chance to fail with the
	// identity key already stored and no way back to it.
	codes, recovery, err := mintRecovery(info.AccountID, priv)
	if err != nil {
		return nil, nil, err
	}
	pub := identity.Public()
	if err := c.PublishKeys(pub[:], wrapped, params, recovery); err != nil {
		return nil, nil, fmt.Errorf("client: publishing the identity key: %w", err)
	}

	if err := c.crossOver(password, creds, params); err != nil {
		return nil, nil, err
	}
	return &Account{ID: info.AccountID, Identity: identity, c: c}, codes, nil
}

// rewrapIdentity re-seals the identity key under a new password and publishes
// it, carrying the recovery wraps forward.
//
// Fresh parameters, which means a fresh salt: the old ones describe a
// derivation from a password that is being retired, and reusing the salt would
// leave a new password stretched under the same input as the old one.
func (c *APIClient) rewrapIdentity(id *store.Identity, keys AccountKeys, next string) (store.Credentials, store.KDFParams, error) {
	var salt [store.KDFSaltSize]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return store.Credentials{}, store.KDFParams{}, fmt.Errorf("client: generating a KDF salt: %w", err)
	}
	params := store.DefaultKDFParams(salt)
	creds, err := store.DeriveCredentials(next, params)
	if err != nil {
		return store.Credentials{}, store.KDFParams{}, err
	}
	priv := id.Private()
	wrapped, err := store.WrapIdentity(creds.WrapKey, keys.AccountID, params, priv)
	if err != nil {
		return store.Credentials{}, store.KDFParams{}, fmt.Errorf("client: re-wrapping the identity key: %w", err)
	}
	if err := c.PublishKeys(keys.PublicKey, wrapped, params, keys.Recovery); err != nil {
		return store.Credentials{}, store.KDFParams{}, fmt.Errorf("client: republishing the identity key: %w", err)
	}
	return creds, params, nil
}

// crossOver moves the account's login secret from the password to the authKey
// derived from it, and records the parameters that derivation used.
//
// A password change in the protocol's terms, and it is one: what the server
// stores as the login secret changes. What does not change is the wrapKey, so
// the identity blob still opens and nothing has to be re-wrapped -- the whole
// reason the crossover derives under the parameters the blob was sealed with
// rather than minting fresh ones.
func (c *APIClient) crossOver(password string, creds store.Credentials, params store.KDFParams) error {
	authKey := creds.AuthKeyString()
	err := c.doRequest("POST", "/api/silo/v1/auth/password", map[string]string{
		"current_password":  password,
		"new_password":      authKey,
		"client_kdf_params": params.String(),
	}, nil)
	if err != nil {
		return fmt.Errorf("client: crossing over to derived login: %w", err)
	}
	// The change revoked every credential the account holds, this one included,
	// so the cached secret has to be the new one before anything triggers a
	// re-login.
	c.mu.Lock()
	c.password = authKey
	c.mu.Unlock()
	return c.Login(c.emailOf(), authKey)
}

// changeSecret performs the password change and returns the secret the account
// now logs in with -- the new password, or the authKey derived from it.
//
// On an enrolled account the identity key is re-wrapped and republished BEFORE
// the login secret moves. Neither order is atomic and both leave a window, so
// the choice is about which window is survivable: this one leaves the account
// reachable with the password the caller still has, and the identity opened by
// the one they just chose. The other order leaves an account whose login has
// moved and whose blob was never re-wrapped, where the secret that opens the
// identity is the one being retired.
//
// A retry of the same call closes that window, which is why the identity is
// opened under either password: a second attempt finds a blob already sealed
// under next, opens it with next, republishes the same bytes, and completes the
// half that failed.
func (c *APIClient) changeSecret(current, next string) (string, int, error) {
	email := c.emailOf()
	// What the server accepts as this account's secret today. login answers
	// the question on the way in: non-empty credentials mean it crossed over.
	creds, err := c.login(email, current)
	if err != nil {
		return "", 0, err
	}
	currentSecret := current
	if len(creds.AuthKey) > 0 {
		currentSecret = creds.AuthKeyString()
	}

	keys, err := c.AccountKeys()
	switch {
	case isNotFound(err):
		// No identity key, so nothing to re-wrap and nothing to cross over.
		// An ordinary password change, which is what an account that has never
		// enrolled has.
		revoked, err := c.postPasswordChange(currentSecret, next, "")
		return next, revoked, err
	case err != nil:
		return "", 0, err
	}

	id, err := openIdentity(current, creds, keys)
	if err != nil {
		// The blob may already be under next, from an attempt that published
		// and then failed to move the login secret.
		id, err = openIdentity(next, store.Credentials{}, keys)
		if err != nil {
			return "", 0, fmt.Errorf("client: opening the identity key to re-wrap it: %w", err)
		}
	}

	nextCreds, nextParams, err := c.rewrapIdentity(id, keys, next)
	if err != nil {
		return "", 0, err
	}
	authKey := nextCreds.AuthKeyString()
	revoked, err := c.postPasswordChange(currentSecret, authKey, nextParams.String())
	return authKey, revoked, err
}

// postPasswordChange is the request itself. params turns it into the
// split-derivation crossover; empty leaves it an ordinary password change.
func (c *APIClient) postPasswordChange(current, next, params string) (int, error) {
	body := map[string]string{"current_password": current, "new_password": next}
	if params != "" {
		body["client_kdf_params"] = params
	}
	var result struct {
		Revoked int `json:"revoked"`
		// Set when the password changed but the credentials it should have
		// signed out are still live. A success with a caveat, not a failure --
		// the server reports it as 200 for exactly that reason. The field name
		// says sessions and means every kind; see api.revokedResponse.
		SessionsStillLive bool `json:"sessions_still_live"`
	}
	if err := c.doRequest("POST", "/api/silo/v1/auth/password", body, &result); err != nil {
		return 0, err
	}
	return result.Revoked, nil
}

func (c *APIClient) emailOf() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.email
}
