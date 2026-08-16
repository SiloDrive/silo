// Package apitokenstore persists SeaDrive/Seahub-style API tokens (40-char hex
// strings) in the seafile DB so they survive server restarts. Each token maps
// to a user email and carries an expiry.
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

func Create(email string) (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)

	now := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), option.DBOpTimeout)
	defer cancel()

	_, err := writeDB.ExecContext(ctx,
		"INSERT INTO ApiToken (token, email, ctime, expires_at) VALUES (?, ?, ?, ?)",
		token, email, now.Unix(), now.Add(option.APITokenTTL).Unix(),
	)
	if err != nil {
		return "", err
	}
	return token, nil
}

// Lookup returns the email a token belongs to, or ErrNotFound if the token is
// unknown or has expired. Using a token slides its expiry forward.
func Lookup(token string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), option.DBOpTimeout)
	defer cancel()

	var email string
	var expiresAt sql.NullInt64
	err := readDB.QueryRowContext(ctx,
		"SELECT email, expires_at FROM ApiToken WHERE token = ?", token,
	).Scan(&email, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}

	// A NULL expiry means a row the migration didn't reach — treat it as
	// expiring one TTL from now rather than as "never expires", so a token
	// cannot dodge the policy by predating it.
	now := time.Now()
	if !expiresAt.Valid {
		renew(token, now)
		return email, nil
	}

	if now.Unix() >= expiresAt.Int64 {
		return "", ErrNotFound
	}

	// Slide the expiry once the token is more than renewThreshold through its
	// life. Anything fresher is left alone to keep writes off the hot path.
	elapsed := option.APITokenTTL - time.Duration(expiresAt.Int64-now.Unix())*time.Second
	if elapsed > time.Duration(float64(option.APITokenTTL)*renewThreshold) {
		renew(token, now)
	}

	return email, nil
}

// renew pushes a token's expiry out by a full TTL. Failures are logged and
// swallowed: the caller has already authenticated successfully, and refusing
// the request because the sliding write failed would turn a transient DB
// hiccup into a spurious logout.
func renew(token string, now time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), option.DBOpTimeout)
	defer cancel()

	if _, err := writeDB.ExecContext(ctx,
		"UPDATE ApiToken SET expires_at = ? WHERE token = ?",
		now.Add(option.APITokenTTL).Unix(), token,
	); err != nil {
		log.Warnf("Failed to renew API token expiry: %v", err)
	}
}

// Delete revokes a single token. Used by logout.
func Delete(token string) error {
	ctx, cancel := context.WithTimeout(context.Background(), option.DBOpTimeout)
	defer cancel()

	_, err := writeDB.ExecContext(ctx, "DELETE FROM ApiToken WHERE token = ?", token)
	return err
}

// DeleteByEmail revokes every token belonging to a user, signing out all of
// their devices. Returns the number of tokens removed.
func DeleteByEmail(email string) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), option.DBOpTimeout)
	defer cancel()

	res, err := writeDB.ExecContext(ctx, "DELETE FROM ApiToken WHERE email = ?", email)
	if err != nil {
		return 0, err
	}
	// The delete itself succeeded; only the count is unavailable. Report zero
	// rather than failing a caller that only wants the rows gone.
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// DeleteExpired removes rows whose expiry has passed, returning the count.
func DeleteExpired() (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), option.DBOpTimeout)
	defer cancel()

	res, err := writeDB.ExecContext(ctx,
		"DELETE FROM ApiToken WHERE expires_at IS NOT NULL AND expires_at <= ?",
		time.Now().Unix())
	if err != nil {
		return 0, err
	}
	// The delete itself succeeded; only the count is unavailable. Report zero
	// rather than failing a caller that only wants the rows gone.
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
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
