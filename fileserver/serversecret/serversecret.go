// Package serversecret holds the secrets this server keeps for its own use:
// values that are not any account's, that must be the same after a restart,
// and that nothing outside the process ever sees.
//
// The first consumer is the pre-login KDF endpoint, which answers an address
// nobody holds with parameters derived from one of these. That answer has to
// be identical every time it is asked, forever -- an address whose salt
// changed when the server restarted would be an address with no account, which
// is the one thing the endpoint exists not to say. A package-level random
// value would have been the obvious implementation and the wrong one.
package serversecret

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var readDB *sql.DB
var writeDB *sql.DB

// Init points the package at the database. It is called once at startup,
// alongside the other stores.
func Init(siloReadDB, siloWriteDB *sql.DB) {
	readDB = siloReadDB
	writeDB = siloWriteDB
}

// Size is how many bytes a secret is. Thirty-two, because every use so far is
// an HMAC key and that is SHA-256's block-independent full strength.
const Size = 32

// ErrNoName reports a secret asked for under no name. A name is the whole
// separation between two purposes, so an empty one would quietly hand the
// second caller the first caller's key.
var ErrNoName = errors.New("a server secret needs a name")

// Named returns the secret held under name, minting and storing one the first
// time it is asked for.
//
// It reads the database on every call rather than memoising. The cost is one
// primary-key lookup on paths that are already rare -- a pre-login request per
// login, not per API call -- and what it buys is that there is no in-process
// copy to go stale, and no cache to make a test pass that a restart would
// fail.
//
// The mint is racy by construction: two callers can both find nothing and both
// insert. The insert is INSERT OR IGNORE followed by a read, so the loser of
// that race returns the winner's secret rather than its own -- which is the
// only outcome that keeps "this value is stable" true.
func Named(ctx context.Context, name string) ([]byte, error) {
	if name == "" {
		return nil, ErrNoName
	}

	secret, err := read(ctx, name)
	if err != nil || secret != nil {
		return secret, err
	}

	fresh := make([]byte, Size)
	if _, err := rand.Read(fresh); err != nil {
		return nil, fmt.Errorf("generating the %s secret: %v", name, err)
	}
	if _, err := writeDB.ExecContext(ctx,
		"INSERT OR IGNORE INTO ServerSecret (name, secret, ctime) VALUES (?, ?, ?)",
		name, fresh, time.Now().Unix()); err != nil {
		return nil, fmt.Errorf("storing the %s secret: %v", name, err)
	}

	secret, err = read(ctx, name)
	if err != nil {
		return nil, err
	}
	if secret == nil {
		// The row was inserted or was already there; either way it is there
		// now. Reaching here means something else deleted it in between, and
		// returning the value in hand would be returning one that is not
		// stored -- the exact failure this package is here to prevent.
		return nil, fmt.Errorf("the %s secret vanished between writing it and reading it back", name)
	}
	return secret, nil
}

// read returns the stored secret, or nil with no error when there is none.
func read(ctx context.Context, name string) ([]byte, error) {
	var secret []byte
	err := readDB.QueryRowContext(ctx,
		"SELECT secret FROM ServerSecret WHERE name = ?", name).Scan(&secret)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the %s secret: %v", name, err)
	}
	return secret, nil
}
