package credential

// This file is the write half of the store: minting a credential, listing what
// an account holds, and revoking one or all of them. store.go verifies rows;
// nothing but a test could create one until this existed.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/option"
	log "github.com/sirupsen/logrus"
)

// Issue's refusals. Each one is a row that would resolve to something nobody
// intended, caught at the write rather than at every read afterwards.
var (
	ErrBadKind   = errors.New("unknown credential kind")
	ErrBadPerm   = errors.New("credential perm must be r or rw")
	ErrNoAccount = errors.New("credential needs an account")
	ErrNoLabel   = errors.New("credential needs a label")
	// ErrBadLifetime is a negative lifetime. It reads as "already expired",
	// and the arithmetic below would have written no expiry at all -- so the
	// one request that most obviously means "this must not work" would have
	// produced the one credential that works forever.
	ErrBadLifetime = errors.New("credential lifetime cannot be negative")
)

// IssueOpts describes the credential to mint. The zero value is not valid:
// Kind, AccountID, Label and Perm are all required, and each is required
// because a row missing it is a row that cannot be reasoned about later.
type IssueOpts struct {
	Kind      Kind
	AccountID account.ID
	Label     string
	Scope     Scope
	Perm      string
	ClientID  string

	// Lifetime is how long the credential lives from now. Zero means no
	// expiry, which is a deliberate spelling rather than an oversight -- see
	// the kinds table in docs/auth.md for which lanes may use it. Negative is
	// refused; see ErrBadLifetime.
	Lifetime time.Duration

	// PublicKey makes this a proof-of-possession credential: the row stores
	// the key, no secret is generated, and Issue returns an empty token
	// string because there is nothing to hand back.
	PublicKey []byte
}

// Issue mints a credential and writes its row, returning the row and the
// string the client presents.
//
// The returned string is the only time the secret is representable. It is not
// stored, cannot be recovered, and belongs in a response body and nowhere else
// -- not a log line, not an error, not a span attribute.
//
// A label is mandatory even though the column would take an empty string.
// docs/auth.md's argument for the column is that it "turns revocation from a
// guess into a decision", and a row an operator cannot tell apart from four
// others is exactly the guess it was added to prevent. The caller is the one
// that knows what to call it, so the default lives at the call site rather
// than here.
func Issue(ctx context.Context, o IssueOpts) (*Credential, string, error) {
	if !o.Kind.valid() {
		return nil, "", fmt.Errorf("%w: %q", ErrBadKind, o.Kind)
	}
	if o.AccountID.IsZero() {
		return nil, "", ErrNoAccount
	}
	if o.Label == "" {
		return nil, "", ErrNoLabel
	}
	// Checked here rather than trusted, because minPerm reads anything it does
	// not recognise as no access at all: a typo would mint a credential that
	// authenticates perfectly and then silently permits nothing.
	if permRank(o.Perm) == 0 {
		return nil, "", fmt.Errorf("%w: %q", ErrBadPerm, o.Perm)
	}
	if o.Lifetime < 0 {
		return nil, "", fmt.Errorf("%w: %s", ErrBadLifetime, o.Lifetime)
	}

	tok, secret, err := NewToken(o.Kind)
	if err != nil {
		return nil, "", err
	}

	c := &Credential{
		ID:        tok.ID,
		Kind:      o.Kind,
		AccountID: o.AccountID,
		Label:     o.Label,
		Scope:     o.Scope,
		Perm:      o.Perm,
		ClientID:  o.ClientID,
		Ctime:     time.Now().Unix(),
		publicKey: o.PublicKey,
	}
	if o.Lifetime > 0 {
		c.ExpiresAt = c.Ctime + int64(o.Lifetime.Seconds())
	}

	// The schema's CHECK permits one or the other. A public-key row has no
	// secret to present, so the token string is discarded along with it --
	// returning one would hand the caller a credential that can never resolve.
	if len(o.PublicKey) == 0 {
		c.secretHash = tok.SecretHash()
	} else {
		secret = ""
	}

	var expires, clientID any
	if c.ExpiresAt != 0 {
		expires = c.ExpiresAt
	}
	if c.ClientID != "" {
		clientID = c.ClientID
	}

	const q = `INSERT INTO Credential
	             (id, kind, secret_hash, public_key, account_id, label, scope,
	              perm, client_id, ctime, expires_at, last_used)
	           VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := writeDB.ExecContext(ctx, q,
		c.ID, string(c.Kind), c.secretHash, nullBytes(c.publicKey), c.AccountID,
		c.Label, c.Scope.String(), c.Perm, clientID, c.Ctime, expires, nil); err != nil {
		return nil, "", fmt.Errorf("issuing credential: %v", err)
	}
	return c, secret, nil
}

// nullBytes keeps an empty blob out of a column the CHECK constraint reads.
// A zero-length BLOB is not NULL, so writing one into public_key would trip
// "secret_hash IS NULL OR public_key IS NULL" for every bearer credential.
func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// ListByAccount returns every credential an account holds, newest first.
//
// The proof material is left behind: the columns are unexported on Credential
// precisely so a listing cannot leak them, and a caller listing credentials is
// deciding which to revoke rather than verifying one.
func ListByAccount(ctx context.Context, id account.ID) ([]*Credential, error) {
	const q = `SELECT id, kind, label, scope, perm, client_id, last_ua, ctime, expires_at, last_used
	           FROM Credential WHERE account_id = ? ORDER BY ctime DESC, id`

	rows, err := readDB.QueryContext(ctx, q, id)
	if err != nil {
		return nil, fmt.Errorf("listing credentials: %v", err)
	}
	defer rows.Close()

	var out []*Credential
	for rows.Next() {
		var (
			c        = Credential{AccountID: id}
			kind     string
			scope    string
			clientID sql.NullString
			lastUA   sql.NullString
			expires  sql.NullInt64
			lastUsed sql.NullInt64
		)
		if err := rows.Scan(&c.ID, &kind, &c.Label, &scope, &c.Perm,
			&clientID, &lastUA, &c.Ctime, &expires, &lastUsed); err != nil {
			return nil, fmt.Errorf("reading a credential: %v", err)
		}
		c.Kind = Kind(kind)
		c.ClientID = clientID.String
		c.LastUA = lastUA.String
		c.ExpiresAt = expires.Int64
		c.LastUsed = lastUsed.Int64

		// A scope that will not parse is reported rather than skipped. This
		// listing is what an operator revokes from, and a row quietly missing
		// from it is a credential they believe they have already dealt with.
		if c.Scope, err = ParseScope(scope); err != nil {
			return nil, fmt.Errorf("credential %s has an unparseable scope: %v", c.ID, err)
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// Revoke deletes one credential belonging to an account.
//
// The account id is part of the WHERE clause rather than checked beforehand:
// the alternative is a read, a comparison and a delete, which is three chances
// to get the ownership test wrong and a race between the second and the third.
// It reports whether a row went, so a caller can tell "revoked" from "there
// was nothing there" without asking a second question.
func Revoke(ctx context.Context, id string, owner account.ID) (bool, error) {
	res, err := writeDB.ExecContext(ctx,
		"DELETE FROM Credential WHERE id = ? AND account_id = ?", id, owner)
	if err != nil {
		return false, fmt.Errorf("revoking credential: %v", err)
	}
	// dbutil.RowsAffected rather than res.RowsAffected: the delete has already
	// succeeded by the time the count is asked for, so a driver that cannot
	// report one must not turn a completed revocation into a caller-visible
	// failure.
	return dbutil.RowsAffected(res) > 0, nil
}

// Expire ends a credential's life without removing its row.
//
// Revoke deletes, which is right for a credential an operator is finished with:
// the row is the credential, and a revoked one should leave nothing behind to
// resolve. Expire is for the credential that is part of a record -- an invite,
// whose Invite row references it and is the trail saying who was invited and
// when they arrived. Deleting the credential would take that reference with it,
// or be refused by it, which is what the foreign key is for.
//
// The token is as dead either way: every path that resolves one checks the
// expiry, and a time in the past fails it.
func Expire(ctx context.Context, id string) error {
	if _, err := writeDB.ExecContext(ctx,
		"UPDATE Credential SET expires_at = ? WHERE id = ?", time.Now().Unix()-1, id); err != nil {
		return fmt.Errorf("expiring credential: %v", err)
	}
	return nil
}

// RevokeByLibrary deletes every credential scoped to a library, inside the
// caller's transaction.
//
// It lives here because the shape of a scope -- "<id>" or "<id>:<path>" -- is
// this package's to know. libmgr.DeleteLibrary hand-coded that encoding in SQL
// so it could clear the rows alongside its other per-library deletes, which
// meant ParseScope and a DELETE in another package had to agree forever, with
// nothing to catch them diverging.
//
// Matched by exact prefix rather than LIKE: a library id carries no wildcard
// today, but a pattern match whose safety rests on what the data happens to
// look like stops being safe the day the id format changes. An unscoped
// credential (” -- every library) and one scoped elsewhere are untouched,
// because deleting one library must not sign the account out of the rest.
func RevokeByLibrary(ctx context.Context, tx *sql.Tx, libraryID string) error {
	_, err := tx.ExecContext(ctx,
		"DELETE FROM Credential WHERE scope = ? OR substr(scope, 1, length(?) + 1) = ? || ':'",
		libraryID, libraryID, libraryID)
	if err != nil {
		return fmt.Errorf("revoking credentials scoped to %s: %v", libraryID, err)
	}
	return nil
}

// RevokeAll deletes every credential an account holds and returns how many.
//
// This is the operation a compromised account needs, and it is why revocation
// had to become one table: the three stores it replaces each had a working
// delete, and no way to run all three as one decision.
func RevokeAll(ctx context.Context, owner account.ID) (int64, error) {
	res, err := writeDB.ExecContext(ctx,
		"DELETE FROM Credential WHERE account_id = ?", owner)
	if err != nil {
		return 0, fmt.Errorf("revoking credentials: %v", err)
	}
	return dbutil.RowsAffected(res), nil
}

// RevokeKind deletes every credential of one lane an account holds, and
// returns how many.
//
// It has no caller today. It was written for the self-service password change,
// which revoked sessions and left device credentials mounted so that routine
// hygiene did not unmount somebody's laptop; that rule is gone -- see
// api.ChangePasswordHandler for why -- and the password change now calls
// RevokeAll like everything else that ends an account's access.
//
// It is kept because the distinction it draws is one docs/auth.md still wants
// somewhere else: § Logout and revocation says a backchannel logout token
// should revoke session credentials and leave device ones to explicit
// revocation, since signing out of a web session on a phone should not unmount
// a laptop. That lane is designed and not built, and this is the operation it
// will need.
//
// The kind is validated rather than passed through: an unrecognised one
// matches no row, so a typo would report a successful revocation that revoked
// nothing -- which is the one failure mode a caller cannot see.
func RevokeKind(ctx context.Context, owner account.ID, kind Kind) (int64, error) {
	if !kind.valid() {
		return 0, fmt.Errorf("%w: %q", ErrBadKind, kind)
	}
	res, err := writeDB.ExecContext(ctx,
		"DELETE FROM Credential WHERE account_id = ? AND kind = ?", owner, string(kind))
	if err != nil {
		return 0, fmt.Errorf("revoking %s credentials: %v", kind, err)
	}
	return dbutil.RowsAffected(res), nil
}

// CleanupInterval is how often expired rows are swept.
//
// Expiry is enforced inside Resolve regardless, so this reclaims table space
// and nothing else: an expired credential is unusable the moment it expires,
// sweeper or no sweeper. What makes it worth having is that a client re-logs
// in when its credential lapses, so a long-lived mount leaves one dead row
// behind per day and nothing else would ever collect them.
const CleanupInterval = 1 * time.Hour

// DeleteExpired removes credentials whose expiry has passed, and returns how
// many went.
//
// A row with no expiry has expires_at NULL, and NULL fails the comparison
// rather than reading as zero, so the credentials most worth keeping -- the
// long-lived device ones -- are the ones this cannot touch.
func DeleteExpired() (int64, error) {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	res, err := writeDB.ExecContext(ctx,
		"DELETE FROM Credential WHERE expires_at IS NOT NULL AND expires_at <= ?",
		time.Now().Unix())
	if err != nil {
		return 0, fmt.Errorf("sweeping expired credentials: %v", err)
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
				log.Warnf("Failed to sweep expired credentials: %v", err)
				continue
			}
			if n > 0 {
				log.Infof("Swept %d expired credential(s)", n)
			}
		}
	}()
}
