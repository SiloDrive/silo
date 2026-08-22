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
	"time"

	"github.com/google/uuid"

	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/option"
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
// Email is carried alongside the id because it is still the thing SeaDrive is
// shown, the thing a commit records as its author, and the thing a log line
// should name. What it is no longer is the key any of those are looked up by.
type Account struct {
	ID       ID
	Email    string
	IsActive bool
	IsStaff  bool
}

// Normalize is the single spelling rule for an address. See
// dbutil.NormalizeEmail for the argument.
func Normalize(email string) string { return dbutil.NormalizeEmail(email) }

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

// EmailOf returns an account's primary address, for the responses and commit
// authors that still speak in addresses.
//
// An id with no primary address returns the empty string rather than an
// error. That case is a tombstone -- an account minted for a share whose user
// was deleted -- and a listing that names one should show a blank owner, not
// fail.
func EmailOf(ctx context.Context, id ID) (string, error) {
	if id.IsZero() {
		return "", nil
	}
	var email string
	err := readDB.QueryRowContext(ctx,
		"SELECT email FROM AccountEmail WHERE account_id = ? AND is_primary = 1", id).Scan(&email)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading an address: %v", err)
	}
	return email, nil
}

// EmailsOf resolves several ids at once, for a listing that would otherwise
// run one query per row.
func EmailsOf(ctx context.Context, ids []ID) (map[ID]string, error) {
	out := make(map[ID]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	// The ids are the package's own sixteen-byte keys, never anything a client
	// sent, so the placeholder list is built rather than parameterised only
	// because IN wants one per value.
	args := make([]any, 0, len(ids))
	placeholders := make([]byte, 0, 2*len(ids))
	seen := make(map[ID]bool, len(ids))
	for _, id := range ids {
		if id.IsZero() || seen[id] {
			continue
		}
		seen[id] = true
		args = append(args, id)
		if len(placeholders) > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
	}
	if len(args) == 0 {
		return out, nil
	}

	rows, err := readDB.QueryContext(ctx,
		"SELECT account_id, email FROM AccountEmail WHERE is_primary = 1 AND account_id IN ("+
			string(placeholders)+")", args...)
	if err != nil {
		return nil, fmt.Errorf("reading addresses: %v", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var id ID
		var email string
		if err := rows.Scan(&id, &email); err != nil {
			return nil, fmt.Errorf("reading addresses: %v", err)
		}
		out[id] = email
	}
	return out, rows.Err()
}

// Create makes an account for an address, or reports that one already exists.
//
// It is the only way an account comes into being, so it is also the only place
// that has to get the three-table insert right. passwordHash may be empty, for
// an account that will sign in some other way -- and then no AccountPassword
// row is written at all, rather than a row holding a sentinel that some future
// comparison could misread as "matches anything".
func Create(ctx context.Context, email, passwordHash string, isStaff bool) (id ID, created bool, err error) {
	norm := Normalize(email)
	if norm == "" {
		return Zero, false, fmt.Errorf("refusing to create an account with no address")
	}

	tx, err := writeDB.BeginTx(ctx, nil)
	if err != nil {
		return Zero, false, fmt.Errorf("creating an account: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

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

	if err := tx.Commit(); err != nil {
		return Zero, false, fmt.Errorf("creating an account: %v", err)
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

// SetPassword stores a hash for an account, replacing any it already had.
func SetPassword(ctx context.Context, id ID, hash string) error {
	if id.IsZero() || hash == "" {
		return fmt.Errorf("refusing to store an empty password")
	}
	if _, err := writeDB.ExecContext(ctx,
		dbutil.InsertOrReplace("AccountPassword", "account_id, hash, changed_at"),
		id, hash, time.Now().Unix()); err != nil {
		return fmt.Errorf("storing a password: %v", err)
	}
	return nil
}

// IsActive reports whether an account may still do anything at all.
//
// A missing account is inactive rather than an error: deleting a user must not
// leave their credentials working.
func IsActive(ctx context.Context, id ID) (bool, error) {
	if id.IsZero() {
		return false, nil
	}
	var active bool
	err := readDB.QueryRowContext(ctx,
		"SELECT is_active FROM Account WHERE id = ?", id).Scan(&active)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking an account: %v", err)
	}
	return active, nil
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

// Count reports how many accounts exist, for the first-boot bootstrap.
//
// Tombstones are excluded. An install whose only accounts are the inactive
// stand-ins minted for orphaned shares still has nobody who can log in, and
// counting them would leave that server unreachable.
func Count(ctx context.Context) (int, error) {
	var n int
	if err := readDB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM Account WHERE is_active = 1").Scan(&n); err != nil {
		return 0, fmt.Errorf("counting accounts: %v", err)
	}
	return n, nil
}

// WithTimeout is the context every call in this package would otherwise build
// for itself.
func WithTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), option.DBOpTimeout)
}
