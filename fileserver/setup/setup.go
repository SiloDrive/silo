package setup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/SiloDrive/silo/fileserver/account"
	"github.com/SiloDrive/silo/fileserver/admin"
)

var readDB *sql.DB
var writeDB *sql.DB

// Init points the package at the database. Called once at startup, alongside
// the other stores, and by the CLI commands that read the token.
func Init(siloReadDB, siloWriteDB *sql.DB) {
	readDB = siloReadDB
	writeDB = siloWriteDB
}

// ErrAlreadySetUp reports a server that has an account already. The setup token
// is spent for good at that point, whatever any leftover row says.
var ErrAlreadySetUp = errors.New("this server has already been set up")

// ErrBadToken reports a token that is not the stored one -- malformed, stale,
// or simply wrong. One error for all three, because telling them apart would
// tell a guesser which half of the guess to keep.
var ErrBadToken = errors.New("invalid setup token")

// Ensure returns this server's setup token, minting one if it has never had an
// account and has no token yet.
//
// It is idempotent, which is what makes "the same token on every boot" true:
// the second call returns the first call's row rather than replacing it. The
// operator who scrolled past the first boot's log can restart, or run
// `silo setup-token`, and get the same string.
//
// It returns the zero token when the server already has an account. In that
// case Ensure also clears any leftover row, because `silo user add` can create
// the first account without ever going through Claim -- and a live row on a
// server that has an account would have the boot log advertising a token that
// Claim will refuse. Claim re-checks regardless; this is what stops the
// advertisement.
func Ensure(ctx context.Context) (Token, error) {
	tx, err := writeDB.BeginTx(ctx, nil)
	if err != nil {
		return Token{}, fmt.Errorf("preparing the setup token: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	exists, err := account.ExistsTx(ctx, tx)
	if err != nil {
		return Token{}, err
	}
	if exists {
		if _, err := tx.ExecContext(ctx, "DELETE FROM SetupToken"); err != nil {
			return Token{}, fmt.Errorf("clearing a spent setup token: %v", err)
		}
		if err := tx.Commit(); err != nil {
			return Token{}, fmt.Errorf("clearing a spent setup token: %v", err)
		}
		return Token{}, nil
	}

	stored, err := readToken(ctx, tx)
	if err != nil {
		return Token{}, err
	}
	if !stored.IsZero() {
		return stored, tx.Commit()
	}

	fresh, err := Generate()
	if err != nil {
		return Token{}, fmt.Errorf("generating a setup token: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO SetupToken (id, token, ctime) VALUES (1, ?, ?)",
		fresh.String(), time.Now().Unix()); err != nil {
		return Token{}, fmt.Errorf("storing the setup token: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return Token{}, fmt.Errorf("storing the setup token: %v", err)
	}
	return fresh, nil
}

// Peek returns the stored token, or the zero token when there is none. It reads
// and never writes.
//
// It exists so that a caller can reject a wrong guess before paying for a
// password hash. That is a fast path and not a guard: Claim reads the row again
// inside its own transaction, which is where the comparison that matters
// happens.
func Peek(ctx context.Context) (Token, error) {
	return readToken(ctx, readDB)
}

// Required reports whether this server still needs setting up: no account, and
// a token waiting to be spent. It reads and never writes, so it is what a
// request handler asks.
func Required(ctx context.Context) (bool, error) {
	exists, err := account.Exists(ctx)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}

	stored, err := Peek(ctx)
	if err != nil {
		return false, err
	}
	return !stored.IsZero(), nil
}

// Claim spends the setup token and creates this server's first account, with
// the admin role, in one transaction.
//
// One transaction is the whole design. Consuming the token and inserting the
// account cannot be two, in either order: burn first and a failed insert leaves
// a server nobody can ever set up, insert first and a failed delete leaves a
// live token on a server that has an account. And because writeDB is a pool of
// one connection, this transaction also serialises against every other claim --
// the second one in blocks until the first commits, then reads an account and
// stops. That is why there is no lock here and no retry loop.
//
// It holds the process's only write connection for the length of a three-table
// insert. That is acceptable exactly here: a server with no accounts has no
// traffic. Hash the password before calling -- 600k PBKDF2 rounds inside this
// transaction would hold the connection for eighty milliseconds to no purpose.
func Claim(ctx context.Context, presented Token, email, passwordHash string) (account.ID, error) {
	tx, err := writeDB.BeginTx(ctx, nil)
	if err != nil {
		return account.Zero, fmt.Errorf("claiming the setup token: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Asked first, and asked through tx. A server that already has an account
	// is finished with setup whatever the token row says, and this is the check
	// that closes the window between the handler's pre-flight read and the
	// write it leads to.
	exists, err := account.ExistsTx(ctx, tx)
	if err != nil {
		return account.Zero, err
	}
	if exists {
		return account.Zero, ErrAlreadySetUp
	}

	stored, err := readToken(ctx, tx)
	if err != nil {
		return account.Zero, err
	}
	if stored.IsZero() || !stored.Equal(presented) {
		return account.Zero, ErrBadToken
	}

	id, created, err := account.CreateTx(ctx, tx, email, passwordHash, account.RoleAdmin)
	if err != nil {
		return account.Zero, err
	}
	if !created {
		// Unreachable under the check above -- an address can only be taken if
		// an account holds it. Treated as a refusal rather than trusted,
		// because the cost of being wrong is a token spent on nothing.
		return account.Zero, ErrAlreadySetUp
	}

	// The full capability set, in this same transaction. The role alone says
	// what kind of account it is and grants nothing on its own -- admin.Can is
	// a conjunction -- so a first boot that wrote the role and stopped would
	// produce a server whose only account is an administrator who can do
	// nothing, with no second account to fix it from. That is not a recoverable
	// state, which is the same reason the token is spent in here rather than
	// after.
	if err := admin.AssignTx(ctx, tx, id, admin.All()...); err != nil {
		return account.Zero, err
	}

	res, err := tx.ExecContext(ctx, "DELETE FROM SetupToken WHERE id = 1")
	if err != nil {
		return account.Zero, fmt.Errorf("spending the setup token: %v", err)
	}
	gone, err := res.RowsAffected()
	if err != nil {
		return account.Zero, fmt.Errorf("spending the setup token: %v", err)
	}
	if gone != 1 {
		// The row was read a moment ago inside this transaction, so it cannot
		// have gone. Refusing to commit is the only safe answer: committing
		// would create the account and leave the token live.
		return account.Zero, fmt.Errorf("the setup token vanished while it was being spent")
	}

	if err := tx.Commit(); err != nil {
		return account.Zero, fmt.Errorf("claiming the setup token: %v", err)
	}
	return id, nil
}

// rowQuerier is the one method readToken needs, and both *sql.DB and *sql.Tx
// have it. Peek reads outside a transaction and Ensure and Claim read inside
// one; this is what keeps that a difference of handle rather than a second copy
// of the query and its error handling.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// readToken returns the stored token, or the zero token when there is none.
func readToken(ctx context.Context, q rowQuerier) (Token, error) {
	var s string
	err := q.QueryRowContext(ctx, "SELECT token FROM SetupToken WHERE id = 1").Scan(&s)
	if err == sql.ErrNoRows {
		return Token{}, nil
	}
	if err != nil {
		return Token{}, fmt.Errorf("reading the setup token: %v", err)
	}

	tok, err := Parse(s)
	if err != nil {
		// Written by String, so it parses -- unless someone edited the row by
		// hand. Refusing beats silently minting a replacement over the top.
		return Token{}, fmt.Errorf("the stored setup token is not a setup token")
	}
	return tok, nil
}
