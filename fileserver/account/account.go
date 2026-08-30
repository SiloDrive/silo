// Package account is the one place an address becomes a user.
//
// Silo used to key a user by their email address, repeated as a foreign key
// across nine tables. docs/auth.md's identity split makes the address an
// attribute of an opaque account id instead, so that changing an address is
// one row rather than a migration, an account can hold more than one address,
// and an external identity has somewhere to live that is not an address at
// all.
//
// Every translation between the two happens here. That matters more than the
// convenience: as long as exactly one function turns an address into an id,
// the spelling rule is applied uniformly by construction rather than by
// everyone remembering.
package account

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/dkam/silo/fileserver/dbutil"
)

var (
	readDB  *sql.DB
	writeDB *sql.DB
)

// Init wires the package to the database.
func Init(siloReadDB, siloWriteDB *sql.DB) {
	readDB = siloReadDB
	writeDB = siloWriteDB
}

// ErrNotFound means no account owns the address or id that was asked about.
var ErrNotFound = errors.New("no such account")

// ID is an account's primary key: a UUIDv7 as its sixteen raw bytes.
//
// It is an array rather than a slice so that it compares with ==, works as a
// map key, and cannot be aliased by whatever produced it. google/uuid's own
// type would do all of that, but its driver.Valuer writes the 36-character
// text form, and the column is a BLOB.
type ID [16]byte

// Zero is the id of no account. A handler that reaches a permission check
// holding it has lost the user somewhere upstream.
var Zero ID

// IsZero reports whether the id names no account.
func (id ID) IsZero() bool { return id == Zero }

// String renders the id in the usual UUID text form, for logs and errors.
func (id ID) String() string { return uuid.UUID(id).String() }

// Value writes the id as sixteen bytes.
func (id ID) Value() (driver.Value, error) { return id[:], nil }

// Scan reads an id back. It is strict about the length, because a short read
// silently truncated would compare unequal to everything and deny every
// permission with no error anywhere to explain it.
func (id *ID) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*id = Zero
		return nil
	case []byte:
		if len(v) != len(*id) {
			return fmt.Errorf("account id is %d bytes, want %d", len(v), len(*id))
		}
		copy(id[:], v)
		return nil
	default:
		return fmt.Errorf("cannot read an account id from %T", src)
	}
}

// NewID mints an account id.
//
// UUIDv7 rather than v4: it leads with a millisecond timestamp, so accounts
// created together land together in the index instead of scattering across it.
func NewID() (ID, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return Zero, fmt.Errorf("minting an account id: %v", err)
	}
	return ID(u), nil
}

// Account is a user, and the primary address they are known by.
//
// Email is carried alongside the id because it is still the thing a client is
// shown, the thing a commit records as its author, and the thing a log line
// should name. What it is no longer is the key any of those are looked up by.
type Account struct {
	ID       ID
	Email    string
	IsActive bool
	IsStaff  bool
}

// Normalize is the single spelling rule for an address.
//
// The whole string is lowercased, not just the domain. RFC 5321 says the local
// part is case-sensitive and no mail provider has behaved that way in decades,
// but the argument here is narrower than that: an address is what a human
// types into a login form, and two spellings of one address reaching two
// different accounts is the failure the identity split exists to prevent.
// AccountEmail's primary key is this form, so the rule is enforced by the
// schema rather than by everyone remembering it.
func Normalize(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

const selectAccount = `SELECT a.id, e.email, a.is_active, a.is_staff
                       FROM Account a JOIN AccountEmail e ON e.account_id = a.id`

func scanOne(row *sql.Row) (*Account, error) {
	var a Account
	err := row.Scan(&a.ID, &a.Email, &a.IsActive, &a.IsStaff)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading an account: %v", err)
	}
	return &a, nil
}

// ByEmail finds the account that owns an address, whatever its spelling.
func ByEmail(ctx context.Context, email string) (*Account, error) {
	norm := Normalize(email)
	if norm == "" {
		return nil, ErrNotFound
	}
	return scanOne(readDB.QueryRowContext(ctx,
		selectAccount+" WHERE e.email = ?", norm))
}

// ByID finds an account and its primary address.
func ByID(ctx context.Context, id ID) (*Account, error) {
	if id.IsZero() {
		return nil, ErrNotFound
	}
	return scanOne(readDB.QueryRowContext(ctx,
		selectAccount+" WHERE a.id = ? AND e.is_primary = 1", id))
}

// Create makes an account for an address, or reports that one already exists.
//
// It is the only way an account comes into being, so it is also the only place
// that has to get the three-table insert right. passwordHash may be empty, for
// an account that will sign in some other way -- and then no AccountPassword
// row is written at all, rather than a row holding a sentinel that some future
// comparison could misread as "matches anything".
func Create(ctx context.Context, email, passwordHash string, isStaff bool) (id ID, created bool, err error) {
	tx, err := writeDB.BeginTx(ctx, nil)
	if err != nil {
		return Zero, false, fmt.Errorf("creating an account: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	id, created, err = CreateTx(ctx, tx, email, passwordHash, isStaff)
	if err != nil {
		return Zero, false, err
	}

	if err := tx.Commit(); err != nil {
		return Zero, false, fmt.Errorf("creating an account: %v", err)
	}
	return id, created, nil
}

// CreateTx is Create inside a transaction the caller already holds.
//
// The split exists for the setup token, which has to consume itself and create
// the first account in one commit -- a claim that succeeded and an insert that
// then failed would leave a server with no account and no way to make one.
//
// It cannot be done by calling Create from inside another transaction. writeDB
// is a pool of exactly one connection (see dbutil), so the nested BeginTx would
// not nest: it would wait for the connection the outer transaction is holding,
// until the context times out. Two other callers in fileserver/credential
// already have that comment; this is the third.
func CreateTx(ctx context.Context, tx *sql.Tx, email, passwordHash string, isStaff bool) (id ID, created bool, err error) {
	norm := Normalize(email)
	if norm == "" {
		return Zero, false, fmt.Errorf("refusing to create an account with no address")
	}

	var existing ID
	err = tx.QueryRowContext(ctx,
		"SELECT account_id FROM AccountEmail WHERE email = ?", norm).Scan(&existing)
	if err == nil {
		return existing, false, nil
	}
	if err != sql.ErrNoRows {
		return Zero, false, fmt.Errorf("looking up the address %s: %v", norm, err)
	}

	id, err = NewID()
	if err != nil {
		return Zero, false, err
	}
	now := time.Now().Unix()

	// Account before AccountEmail: the address references the account, so the
	// row it points at has to exist first.
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO Account (id, display, is_active, is_staff, ctime) VALUES (?, NULL, 1, ?, ?)",
		id, isStaff, now); err != nil {
		return Zero, false, fmt.Errorf("creating an account: %v", err)
	}

	// A plain INSERT, not INSERT OR IGNORE. The primary key is what excludes a
	// concurrent second caller, and losing that race has to abort this whole
	// transaction -- an ignored conflict would leave the Account row behind
	// with no address pointing at it.
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO AccountEmail (email, account_id, is_primary, verified_at) VALUES (?, ?, 1, NULL)",
		norm, id); err != nil {
		return Zero, false, fmt.Errorf("claiming the address %s: %v", norm, err)
	}

	if passwordHash != "" {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO AccountPassword (account_id, hash, changed_at) VALUES (?, ?, ?)",
			id, passwordHash, now); err != nil {
			return Zero, false, fmt.Errorf("storing the password: %v", err)
		}
	}

	return id, true, nil
}

// PasswordHash returns the stored hash for an address, and the account it
// belongs to.
//
// An account with no password row returns ErrNotFound, exactly as an address
// nobody holds does. That is deliberate: "this account cannot sign in with a
// password" and "there is no such account" are the same answer to a password
// prompt, and telling them apart would answer the question "does this address
// exist here" for anyone who asks.
func PasswordHash(ctx context.Context, email string) (ID, string, error) {
	norm := Normalize(email)
	if norm == "" {
		return Zero, "", ErrNotFound
	}

	var id ID
	var hash string
	err := readDB.QueryRowContext(ctx,
		`SELECT p.account_id, p.hash
		 FROM AccountEmail e JOIN AccountPassword p ON p.account_id = e.account_id
		 WHERE e.email = ?`, norm).Scan(&id, &hash)
	if err == sql.ErrNoRows {
		return Zero, "", ErrNotFound
	}
	if err != nil {
		return Zero, "", fmt.Errorf("reading a password: %v", err)
	}
	return id, hash, nil
}

// SetPassword stores a hash for an account, replacing any it already had, and
// clears the client KDF parameters beside it.
//
// The clearing is the part worth explaining, because it looks like collateral
// damage and is the opposite. client_kdf_params governs how a login password
// becomes the authKey the hash column is made of. A hash written here is of
// whatever was handed over — for this caller, a raw password — so parameters
// left standing beside it would describe a stretching that no longer leads to
// the stored hash: a crossed-over client would derive an authKey under the old
// salt, present it, and be refused, with nothing on the server disagreeing
// with itself in any way it could report. Dropping them says plainly what the
// write did, which is to put the account back on password login.
//
// It used to happen anyway, and by accident: INSERT OR REPLACE deletes the
// conflicting row and inserts a fresh one, so a column absent from the list
// came back NULL. Same outcome, no statement of intent, and no test — which is
// how it would have survived a change to an upsert that "obviously" preserved
// the other columns, and quietly reintroduced the mismatch above.
//
// What it deliberately does not touch is AccountIdentityKey and
// AccountRecoveryWrap. Those are not wrapped under the password: each recovery
// blob opens with a recovery code, so a user whose password an operator has
// just reset redeems one, recovers the identity key, re-wraps it under the new
// password and republishes. Deleting them here would turn a password reset
// into the permanent loss of every library the account can read.
//
// A caller that has both halves — the new hash and the parameters it was
// derived under — wants SetPasswordAndKDFParams instead.
func SetPassword(ctx context.Context, id ID, hash string) error {
	if id.IsZero() || hash == "" {
		return fmt.Errorf("refusing to store an empty password")
	}
	if _, err := writeDB.ExecContext(ctx,
		dbutil.InsertOrReplace("AccountPassword", "account_id, hash, changed_at, client_kdf_params"),
		id, hash, time.Now().Unix(), nil); err != nil {
		return fmt.Errorf("storing a password: %v", err)
	}
	return nil
}

// SetPasswordAndKDFParams stores a hash and the client KDF parameters it was
// derived under, in one statement.
//
// This is the split-derivation crossover write. The two are one fact: the hash
// is of an authKey, and the parameters are how a password becomes that
// authKey, so a state where one has been written and the other has not is an
// account nobody can log in to. Two statements have a window between them that
// a crash can land in; one has none.
//
// The parameters are required. A caller with nothing to put here is changing a
// password rather than crossing an account over, and SetPassword is that
// caller — it says so by clearing the column rather than by leaving whatever
// was there.
func SetPasswordAndKDFParams(ctx context.Context, id ID, hash, kdfParams string) error {
	if id.IsZero() || hash == "" {
		return fmt.Errorf("refusing to store an empty password")
	}
	if kdfParams == "" {
		return fmt.Errorf("refusing to store a password hash with no client KDF parameters: " +
			"use SetPassword to put an account back on password login")
	}
	if _, err := writeDB.ExecContext(ctx,
		dbutil.InsertOrReplace("AccountPassword", "account_id, hash, changed_at, client_kdf_params"),
		id, hash, time.Now().Unix(), kdfParams); err != nil {
		return fmt.Errorf("storing a password and client KDF parameters: %v", err)
	}
	return nil
}

// SetActive enables or disables an account. Disabling it is what stops every
// lane at once: credential.Resolve, the session middleware and the sync token
// path all ask.
func SetActive(ctx context.Context, id ID, active bool) error {
	if id.IsZero() {
		return fmt.Errorf("no account named")
	}
	if _, err := writeDB.ExecContext(ctx,
		"UPDATE Account SET is_active = ? WHERE id = ?", active, id); err != nil {
		return fmt.Errorf("setting an account active: %v", err)
	}
	return nil
}

// ExistsTx reports whether this server has any account at all, inside a
// transaction the caller holds.
//
// Two things about it are deliberate.
//
// It takes a transaction rather than reading readDB, because its one caller is
// the setup token's claim, and readDB is a separate pool on a separate WAL
// snapshot -- it cannot see the claiming transaction's own writes, so a check
// made through it would be a decoration rather than a guard.
//
// And it counts tombstones, where the first-boot count this replaced excluded
// them. That count wanted "is there anyone who can log in", so an install whose
// only rows were the inactive stand-ins minted for orphaned shares was treated
// as empty. The setup token wants a different question -- "has this server ever
// been set up" -- because the answer gates minting a fresh credential and
// printing it to the log. Under the old predicate, disabling your last account
// would put the server back into setup mode and hand a way in to anyone who
// could read its logs. An account that is disabled is still an account; the way
// back from locking yourself out is `silo user enable`, which needs the same
// shell access the token does.
func ExistsTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	return existsIn(ctx, tx)
}

// Exists is ExistsTx outside a transaction, for the callers that only want to
// know and are not about to write.
func Exists(ctx context.Context) (bool, error) {
	return existsIn(ctx, readDB)
}

// rowQuerier is the one method existsIn needs, and both *sql.DB and *sql.Tx
// have it. It is here so that the transactional and non-transactional forms
// above are two names for one query rather than two copies of it -- the same
// split Create and CreateTx make, for the same reason.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func existsIn(ctx context.Context, q rowQuerier) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, "SELECT 1 FROM Account LIMIT 1").Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("looking for any account: %v", err)
	}
	return true, nil
}

// Listed is one account as an operator listing them wants to see it: the
// account, plus the two facts a request handler never asks for.
type Listed struct {
	Account
	Ctime int64

	// HasPassword distinguishes an account that can sign in with a password
	// from one that cannot -- an identity-only account, or a tombstone minted
	// for a share to an address nobody has enrolled. Both are active rows
	// that no password will ever open, and an operator debugging "why can
	// this person not log in" should not have to infer that.
	HasPassword bool
}

// List returns every account, oldest first. It is the operator's view: there
// is no paging because there is no request behind it, and no filtering
// because deciding what to hide is the caller's business.
func List(ctx context.Context) ([]Listed, error) {
	// LEFT JOIN on the address, not an inner one. Create always writes a
	// primary address, so an account without one should not exist -- which is
	// exactly why a listing that silently omitted it would be the wrong tool
	// to find out with.
	const q = `SELECT a.id, e.email, a.is_active, a.is_staff, a.ctime,
	                  p.account_id IS NOT NULL
	           FROM Account a
	           LEFT JOIN AccountEmail e ON e.account_id = a.id AND e.is_primary = 1
	           LEFT JOIN AccountPassword p ON p.account_id = a.id
	           ORDER BY a.ctime, e.email`

	rows, err := readDB.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("listing accounts: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Listed
	for rows.Next() {
		var l Listed
		var email sql.NullString
		if err := rows.Scan(&l.ID, &email, &l.IsActive, &l.IsStaff, &l.Ctime, &l.HasPassword); err != nil {
			return nil, fmt.Errorf("listing accounts: %v", err)
		}
		l.Email = email.String
		out = append(out, l)
	}
	return out, rows.Err()
}
