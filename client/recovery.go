package client

// Recovery codes: minting a set at enrolment, and redeeming one afterwards.
//
// store/recovery.go owns the format and the wrap, and the server owns the
// storage; what lives here is the only step that produces a code and the only
// one that spends it. Neither had a caller before silo#32, which made recovery
// a mechanism with no population -- every account on the server held zero
// wraps, and nothing said so until a password went missing.
//
// The day this is for is an operator reset. `silo user passwd` cannot re-wrap
// an identity key, because re-wrapping means unwrapping first and that needs
// the password whoever is resetting does not have, so the blob is left in
// place and unopenable. A code is the way back to it. docs/auth.md § The
// account's key material is the owning document.

import (
	"errors"
	"fmt"

	"github.com/SiloDrive/silo/store"
)

// ErrNoRecoveryWrap reports a code that opens none of the account's wraps.
//
// One error for three causes -- a code from another account, a code already
// redeemed, and a code mistyped in a way that still parses -- because the
// client cannot tell them apart and neither can the server. Every wrap carries
// its own salt, so the only way to ask whether a code belongs to a set is to
// try it against each blob, and a failure to open says nothing about why.
var ErrNoRecoveryWrap = errors.New("client: no recovery wrap opens that code")

// mintRecovery generates a set of codes and wraps the identity private key to
// each of them.
//
// Called where the identity key is generated, which is the only place it can
// be: the wraps need the private half, and after enrolment returns nobody
// holds it in a form that can produce more without a password. Building this
// later would mean a population of accounts holding no codes and a migration
// to give them some.
func mintRecovery(holder string, priv [store.X25519KeySize]byte) ([]string, []RecoveryWrap, error) {
	codes, err := store.GenerateRecoveryCodeSet()
	if err != nil {
		return nil, nil, err
	}
	wraps := make([]RecoveryWrap, len(codes))
	for i, code := range codes {
		blob, err := store.WrapForRecovery(code, holder, priv)
		if err != nil {
			return nil, nil, fmt.Errorf("client: wrapping the identity key to a recovery code: %w", err)
		}
		// The ordinal is the index in the set, and it is what redemption names
		// to the server. It is not derived from the code and carries nothing
		// about it: the server learns which blob went and nothing else.
		wraps[i] = RecoveryWrap{Ordinal: i, WrappedKey: blob}
	}
	return codes, wraps, nil
}

// Recover opens an identity key with a recovery code and re-wraps it under a
// password the account can use.
//
// It takes a password as well as a code because the two recover different
// things and both have to be in hand. A code recovers the *identity key*; it
// does not recover *login*, and it cannot -- redeeming needs an authenticated
// request, so an account nobody can log in to is one no code can reach. Login
// comes back through the operator, who can set a password and cannot open a
// blob; the code is the half the operator does not have. Neither alone is
// enough, which is the property that keeps a stolen card off somebody's files.
//
// What it leaves behind is an ordinary enrolled account: the identity re-
// wrapped under fresh parameters, the spent code gone, the rest of the set
// standing, and the login crossed back over to a derived key. A device that
// holds only the new password works afterwards with no code at all.
func (c *APIClient) Recover(email, password, code string) (*Account, error) {
	// Parsed before anything is sent, so a mistyped code costs a round trip to
	// nothing and reports the typo rather than a failure to open.
	if _, err := store.RecoveryCodeSecret(code); err != nil {
		return nil, err
	}

	creds, err := c.login(email, password)
	if err != nil {
		return nil, err
	}
	keys, err := c.AccountKeys()
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNoAccountKeys
		}
		return nil, err
	}
	if keys.AccountID == "" {
		return nil, errors.New("client: the server served key material with no holder to open it with")
	}

	// Which blob a code opens is not knowable without trying: each wrap
	// carries its own salt and the ordinal says nothing about the code. So the
	// set is walked, and a wrap that does not open is not an error.
	ordinal := -1
	var priv [store.X25519KeySize]byte
	for _, w := range keys.Recovery {
		opened, err := store.UnwrapWithRecovery(code, keys.AccountID, w.WrappedKey)
		if err != nil {
			continue
		}
		ordinal, priv = w.Ordinal, opened
		break
	}
	if ordinal < 0 {
		return nil, ErrNoRecoveryWrap
	}
	id, err := store.IdentityFromPrivate(priv)
	if err != nil {
		return nil, fmt.Errorf("client: the recovered identity key: %w", err)
	}

	// Spending the code is the same write as re-wrapping the identity, rather
	// than the DELETE that redeems one on its own. PUT account/keys replaces
	// the whole set, so publishing without this wrap removes it -- in one
	// statement with the new identity blob, which is what account.Keys exists
	// to make atomic. Deleting first would spend a code and then, on a failed
	// publish, leave the user holding one fewer and no further forward.
	remaining := make([]RecoveryWrap, 0, len(keys.Recovery)-1)
	for _, w := range keys.Recovery {
		if w.Ordinal != ordinal {
			remaining = append(remaining, w)
		}
	}
	keys.Recovery = remaining

	nextCreds, nextParams, err := c.rewrapIdentity(id, keys, password)
	if err != nil {
		return nil, err
	}
	// And only then the login secret, in the order changeSecret argues for:
	// the window this leaves is one the caller's own password still reaches.
	currentSecret := password
	if len(creds.AuthKey) > 0 {
		currentSecret = creds.AuthKeyString()
	}
	authKey := nextCreds.AuthKeyString()
	if _, err := c.postPasswordChange(currentSecret, authKey, nextParams.String()); err != nil {
		return nil, fmt.Errorf("client: crossing over after recovery: %w", err)
	}
	c.mu.Lock()
	c.password = authKey
	c.mu.Unlock()
	if err := c.Login(email, authKey); err != nil {
		return nil, fmt.Errorf("client: signing back in after recovery: %w", err)
	}
	return &Account{ID: keys.AccountID, Identity: id, c: c}, nil
}
