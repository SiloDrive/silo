// Package apitokenstore persists SeaDrive/Seahub-style API tokens (40-char hex
// strings) in the seafile DB so they survive server restarts. Each token maps
// to an account and carries an expiry.
//
// Tokens are minted per login and never deduplicated: two devices logging in
// as the same user get two tokens, so revoking one does not sign the other
// out. Table growth is bounded by expiry rather than by reuse.
package apitokenstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/option"
	log "github.com/sirupsen/logrus"
)

// ErrNotFound is returned by Lookup when the token is absent from the store,
// or present but expired. Callers should treat this distinctly from
// DB/connectivity errors so an outage doesn't masquerade as "invalid token".
var ErrNotFound = errors.New("api token not found")

// CleanupInterval is how often expired rows are swept. Expiry is enforced on
// every lookup regardless, so this only reclaims table space — an expired
// token is already unusable the moment it expires, sweeper or no sweeper.
const CleanupInterval = 1 * time.Hour

// renewThreshold is the fraction of the TTL that must have elapsed before a
// lookup rewrites expires_at. Sliding the expiry on *every* request would put
// a DB write in the path of every authenticated call; renewing only past the
// halfway mark bounds that to roughly one write per token per half-TTL while
// still meaning an actively used token never expires.
const renewThreshold = 0.5

var (
	readDB  *sql.DB
	writeDB *sql.DB
)

func Init(read, write *sql.DB) {
	readDB = read
	writeDB = write
}

func Create(id account.ID) (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)

	now := time.Now()
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	_, err := writeDB.ExecContext(ctx,
		"INSERT INTO ApiToken (token, account_id, ctime, expires_at) VALUES (?, ?, ?, ?)",
		token, id, now.Unix(), now.Add(option.APITokenTTL).Unix(),
	)
	if err != nil {
		return "", err
	}
	return token, nil
}

// LookupAccount returns the account a token belongs to, or ErrNotFound if the
// token is unknown or has expired. Using a token slides its expiry forward.
//
// The account is joined in rather than left to the caller. This is the /api2
// lane's auth query and it runs on every request a desktop client makes, so
// the difference between one statement and two is worth the join — and every
// column it needs is already indexed for it.
//
// A token whose account row is gone resolves to ErrNotFound rather than to a
// dangling id: deleting a user must not leave their tokens answering.
func LookupAccount(ctx context.Context, token string) (*account.Account, error) {
	ctx, cancel := option.WithDBTimeout(ctx)
	defer cancel()

	const q = `SELECT a.id, e.email, a.is_active, a.is_staff, t.expires_at
	           FROM ApiToken t
	           JOIN Account a ON a.id = t.account_id
	           JOIN AccountEmail e ON e.account_id = a.id AND e.is_primary = 1
	           WHERE t.token = ?`

	var (
		acct      account.Account
		expiresAt int64
	)
	err := readDB.QueryRowContext(ctx, q, token).Scan(
		&acct.ID, &acct.Email, &acct.IsActive, &acct.IsStaff, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	now := time.Now()
	if now.Unix() >= expiresAt {
		return nil, ErrNotFound
	}

	// Slide the expiry once the token is more than renewThreshold through its
	// life. Anything fresher is left alone to keep writes off the hot path.
	elapsed := option.APITokenTTL - time.Duration(expiresAt-now.Unix())*time.Second
	if elapsed > time.Duration(float64(option.APITokenTTL)*renewThreshold) {
		renew(token, now)
	}

	return &acct, nil
}

// renew pushes a token's expiry out by a full TTL. Failures are logged and
// swallowed: the caller has already authenticated successfully, and refusing
// the request because the sliding write failed would turn a transient DB
// hiccup into a spurious logout.
func renew(token string, now time.Time) {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	if _, err := writeDB.ExecContext(ctx,
		"UPDATE ApiToken SET expires_at = ? WHERE token = ?",
		now.Add(option.APITokenTTL).Unix(), token,
	); err != nil {
		log.Warnf("Failed to renew API token expiry: %v", err)
	}
}

// Token is one row of the ApiToken table.
type Token struct {
	Token     string
	Ctime     sql.NullInt64
	ExpiresAt int64
}

// ListByAccount returns every API token an account holds, including expired
// ones. Lookup hides expired tokens because they cannot authenticate; an
// operator deciding what to revoke needs to see what is actually in the table.
func ListByAccount(id account.ID) ([]Token, error) {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	rows, err := readDB.QueryContext(ctx,
		"SELECT token, ctime, expires_at FROM ApiToken WHERE account_id = ? ORDER BY ctime", id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var tokens []Token
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.Token, &t.Ctime, &t.ExpiresAt); err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

// Delete revokes a single token. Used by logout.
func Delete(token string) error {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	_, err := writeDB.ExecContext(ctx, "DELETE FROM ApiToken WHERE token = ?", token)
	return err
}

// DeleteByAccount revokes every token belonging to an account, signing out all
// of their devices. Returns the number of tokens removed.
func DeleteByAccount(id account.ID) (int64, error) {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	res, err := writeDB.ExecContext(ctx, "DELETE FROM ApiToken WHERE account_id = ?", id)
	if err != nil {
		return 0, err
	}
	return dbutil.RowsAffected(res), nil
}

// DeleteExpired removes rows whose expiry has passed, returning the count.
func DeleteExpired() (int64, error) {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	res, err := writeDB.ExecContext(ctx,
		"DELETE FROM ApiToken WHERE expires_at <= ?",
		time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return dbutil.RowsAffected(res), nil
}

// StartCleanup runs DeleteExpired on a ticker for the life of the process.
func StartCleanup() {
	go func() {
		ticker := time.NewTicker(CleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			n, err := DeleteExpired()
			if err != nil {
				log.Warnf("Failed to sweep expired API tokens: %v", err)
				continue
			}
			if n > 0 {
				log.Infof("Swept %d expired API token(s)", n)
			}
		}
	}()
}
