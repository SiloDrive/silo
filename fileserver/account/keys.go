package account

// The account side of end-to-end encryption: an account's published X25519
// public key, the private half wrapped under a password-derived key, the
// recovery wraps of that same private half, and the parameters a client
// stretches the password under to get there.
//
// The server holds all four and can read none of them. Its whole job is to
// store what a client publishes, hand it back to the account it belongs to,
// and refuse the shapes that would produce a blob nobody can ever open. See
// docs/auth.md, "The client's KDF is not this one, and it needs four columns",
// and docs/storage.md's E2EE section for what the blobs are.

import (
	"context"
	"crypto/ecdh"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/store"
)

// ErrNoKeys means the account has published no identity key. It is separate
// from ErrNotFound because the two are different answers -- "there is no such
// account" and "this account has not enrolled in E2EE" -- to a caller that has
// already authenticated and so is entitled to tell them apart.
var ErrNoKeys = errors.New("this account has published no identity key")

// ErrBadKeys reports key material this server will not store: the shapes that
// would leave an account holding a blob that cannot be opened. Every one of
// them is a client bug, and every one of them is silent if it is written --
// the failure surfaces on a new device, months later, as a password that no
// longer works.
var ErrBadKeys = errors.New("invalid key material")

// MaxWrappedKeyBytes bounds a wrapped blob. A wrap of a 32-byte private key is
// a little over a hundred bytes; this is that with a wide margin, and it is
// here so a hostile client cannot use an opaque column as free storage.
const MaxWrappedKeyBytes = 4 << 10

// RecoveryWrap is one recovery code's wrap of the identity private key.
//
// Ordinal says which of the set it is and is never the code itself -- the
// server never learns a code. Redemption is the client fetching the set,
// trying each blob, and deleting the one that opened.
type RecoveryWrap struct {
	Ordinal    int    `json:"ordinal"`
	WrappedKey []byte `json:"wrapped_key"`
	Ctime      int64  `json:"-"`
}

// Keys is everything an account publishes for E2EE, written and read as one
// thing.
//
// It is one struct rather than three calls because it is one decision. A
// password change re-wraps the identity key and re-wraps every recovery blob
// under the new parameters; landing those separately would leave a window in
// which the stored parameters open none of the stored blobs.
type Keys struct {
	PublicKey  []byte         `json:"public_key"`
	WrappedKey []byte         `json:"wrapped_key"`
	KDFParams  string         `json:"kdf_params"`
	Recovery   []RecoveryWrap `json:"recovery"`
	UpdatedAt  int64          `json:"updated_at"`
}

// Validate refuses the shapes that would produce an unopenable account.
//
// The parameters check is the one worth naming. They are stored twice -- in
// client_kdf_params, so the pre-login endpoint can answer without handing out
// the blob, and inside the wrapped blob itself, because store.WrapIdentity
// makes every wrap self-describing. store.WrapIdentity's own comment names the
// hazard in the alternative: "a pair that can drift, after which the blob is
// unopenable and nothing says why". One fact stored twice needs a check at the
// write, and this is it.
func (k Keys) Validate() error {
	if _, err := ecdh.X25519().NewPublicKey(k.PublicKey); err != nil {
		return fmt.Errorf("%w: public_key is not an X25519 public key: %v", ErrBadKeys, err)
	}
	if len(k.WrappedKey) == 0 || len(k.WrappedKey) > MaxWrappedKeyBytes {
		return fmt.Errorf("%w: wrapped_key is %d bytes", ErrBadKeys, len(k.WrappedKey))
	}

	declared, err := store.ParseKDFParams(k.KDFParams)
	if err != nil {
		return fmt.Errorf("%w: kdf_params: %v", ErrBadKeys, err)
	}
	// Validated as well as parsed, because these are what a client will derive
	// under on a new device. A set outside the bounds is refused at the write
	// rather than at every read, and refused here as well as on the client:
	// the server is the one party that sees every publish.
	if err := declared.Validate(); err != nil {
		return fmt.Errorf("%w: kdf_params: %v", ErrBadKeys, err)
	}

	sealed, err := store.KDFParamsFromBlob(k.WrappedKey)
	if err != nil {
		return fmt.Errorf("%w: wrapped_key: %v", ErrBadKeys, err)
	}
	if sealed.String() != declared.String() {
		return fmt.Errorf("%w: wrapped_key was sealed under %s but kdf_params says %s",
			ErrBadKeys, sealed, declared)
	}

	if len(k.Recovery) > store.RecoveryCodeSetSize {
		return fmt.Errorf("%w: %d recovery wraps, and a set is %d",
			ErrBadKeys, len(k.Recovery), store.RecoveryCodeSetSize)
	}
	seen := make(map[int]bool, len(k.Recovery))
	for _, w := range k.Recovery {
		if w.Ordinal < 0 || w.Ordinal >= store.RecoveryCodeSetSize {
			return fmt.Errorf("%w: recovery ordinal %d is outside a set of %d",
				ErrBadKeys, w.Ordinal, store.RecoveryCodeSetSize)
		}
		if seen[w.Ordinal] {
			return fmt.Errorf("%w: two recovery wraps carry ordinal %d", ErrBadKeys, w.Ordinal)
		}
		seen[w.Ordinal] = true
		if len(w.WrappedKey) == 0 || len(w.WrappedKey) > MaxWrappedKeyBytes {
			return fmt.Errorf("%w: recovery wrap %d is %d bytes",
				ErrBadKeys, w.Ordinal, len(w.WrappedKey))
		}
	}
	return nil
}

// SetKeys publishes an account's key material, replacing whatever it held.
//
// Replacing rather than merging, and in one transaction. A password change
// produces a new wrapKey, so every blob the account holds is re-wrapped at
// once; a stale recovery wrap left standing beside the new set is a blob that
// still opens with a code the user was told to throw away, and a stale
// identity blob is one that still opens with the old password.
func SetKeys(ctx context.Context, id ID, k Keys) error {
	if id.IsZero() {
		return ErrNotFound
	}
	if err := k.Validate(); err != nil {
		return err
	}

	tx, err := writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("publishing keys: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx,
		dbutil.InsertOrReplace("AccountIdentityKey", "account_id, public_key, wrapped_key, updated_at"),
		id, k.PublicKey, k.WrappedKey, now); err != nil {
		return fmt.Errorf("storing an identity key: %v", err)
	}

	// The parameters land on AccountPassword, which may have no row: an
	// OIDC-only account has no password. That is refused rather than skipped.
	// The parameters describe how a password becomes the wrapKey that opens
	// the blob above, so an account with no password has nothing for them to
	// describe -- and an UPDATE that quietly matches nothing would store an
	// identity key whose parameters the pre-login endpoint can never serve,
	// which is a working publish followed by a new device that can never
	// bootstrap.
	res, err := tx.ExecContext(ctx,
		"UPDATE AccountPassword SET client_kdf_params = ? WHERE account_id = ?",
		k.KDFParams, id)
	if err != nil {
		return fmt.Errorf("storing client KDF parameters: %v", err)
	}
	if dbutil.RowsAffected(res) == 0 {
		return fmt.Errorf("%w: this account has no password, so there is nothing "+
			"for client KDF parameters to describe", ErrBadKeys)
	}

	if _, err := tx.ExecContext(ctx,
		"DELETE FROM AccountRecoveryWrap WHERE account_id = ?", id); err != nil {
		return fmt.Errorf("clearing recovery wraps: %v", err)
	}
	for _, w := range k.Recovery {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO AccountRecoveryWrap (account_id, ordinal, wrapped_key, ctime) VALUES (?, ?, ?, ?)",
			id, w.Ordinal, w.WrappedKey, now); err != nil {
			return fmt.Errorf("storing recovery wrap %d: %v", w.Ordinal, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("publishing keys: %v", err)
	}
	return nil
}

// GetKeys returns everything an account has published, ErrNoKeys if it has
// published nothing.
func GetKeys(ctx context.Context, id ID) (*Keys, error) {
	if id.IsZero() {
		return nil, ErrNotFound
	}

	k := &Keys{}
	var params sql.NullString
	err := readDB.QueryRowContext(ctx,
		`SELECT k.public_key, k.wrapped_key, k.updated_at, p.client_kdf_params
		   FROM AccountIdentityKey k
		   LEFT JOIN AccountPassword p ON p.account_id = k.account_id
		  WHERE k.account_id = ?`, id).
		Scan(&k.PublicKey, &k.WrappedKey, &k.UpdatedAt, &params)
	if err == sql.ErrNoRows {
		return nil, ErrNoKeys
	}
	if err != nil {
		return nil, fmt.Errorf("reading an identity key: %v", err)
	}
	k.KDFParams = params.String

	rows, err := readDB.QueryContext(ctx,
		`SELECT ordinal, wrapped_key, ctime FROM AccountRecoveryWrap
		  WHERE account_id = ? ORDER BY ordinal`, id)
	if err != nil {
		return nil, fmt.Errorf("reading recovery wraps: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var w RecoveryWrap
		if err := rows.Scan(&w.Ordinal, &w.WrappedKey, &w.Ctime); err != nil {
			return nil, fmt.Errorf("reading a recovery wrap: %v", err)
		}
		k.Recovery = append(k.Recovery, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading recovery wraps: %v", err)
	}
	return k, nil
}

// PublicKey returns an account's published X25519 public key.
//
// It exists apart from GetKeys because the two have different audiences: this
// one answers "who do I wrap a content key to", and is read by members of a
// library who are not the key's owner. It returns nothing that is not already
// meant to be public.
func PublicKey(ctx context.Context, id ID) ([]byte, error) {
	if id.IsZero() {
		return nil, ErrNotFound
	}
	var pub []byte
	err := readDB.QueryRowContext(ctx,
		"SELECT public_key FROM AccountIdentityKey WHERE account_id = ?", id).Scan(&pub)
	if err == sql.ErrNoRows {
		return nil, ErrNoKeys
	}
	if err != nil {
		return nil, fmt.Errorf("reading a public key: %v", err)
	}
	return pub, nil
}

// DeleteRecoveryWrap consumes one recovery wrap, reporting whether one went.
//
// One blob, not the set. A person redeeming a code has just proved they lost a
// device; invalidating the nine codes they are still holding is the moment
// they can least afford it.
func DeleteRecoveryWrap(ctx context.Context, id ID, ordinal int) (bool, error) {
	if id.IsZero() {
		return false, ErrNotFound
	}
	res, err := writeDB.ExecContext(ctx,
		"DELETE FROM AccountRecoveryWrap WHERE account_id = ? AND ordinal = ?", id, ordinal)
	if err != nil {
		return false, fmt.Errorf("redeeming recovery wrap %d: %v", ordinal, err)
	}
	return dbutil.RowsAffected(res) > 0, nil
}

// ClientKDFParams returns the parameters an address's password is stretched
// under before it reaches this server.
//
// It takes an address rather than an id because its caller has neither: this
// is what the pre-login endpoint reads, and pre-login is the one moment
// nothing has been authenticated. ErrNotFound covers every way there is no
// answer -- no such address, no password row, no parameters published -- and
// they collapse deliberately, because the caller's job is to be unable to tell
// them apart. What it substitutes instead is its business, not this
// function's.
func ClientKDFParams(ctx context.Context, email string) (string, error) {
	norm := Normalize(email)
	if norm == "" {
		return "", ErrNotFound
	}
	var params sql.NullString
	err := readDB.QueryRowContext(ctx,
		`SELECT p.client_kdf_params
		   FROM AccountEmail e JOIN AccountPassword p ON p.account_id = e.account_id
		  WHERE e.email = ?`, norm).Scan(&params)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("reading client KDF parameters: %v", err)
	}
	if !params.Valid || params.String == "" {
		return "", ErrNotFound
	}
	return params.String, nil
}
