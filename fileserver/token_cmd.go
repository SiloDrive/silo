package silod

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/apitokenstore"
	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/option"
)

// RunToken lists and revokes the credentials a user holds.
//
// Both stores could already revoke — libmgr.DeleteLibraryToken,
// libmgr.DeleteLibraryTokensByAccount and apitokenstore.DeleteByAccount were
// written for it and documented as the way to invalidate a token — but
// nothing outside their own tests ever called them. Sync tokens have no
// expiry by design, because the clients they were for persist them and treat them as
// durable, so with no reachable revocation a token copied off a stolen laptop
// kept read/write access to the library forever. A password change did not
// touch it. The only remedy was editing SQLite by hand.
//
// This is a CLI rather than an HTTP endpoint because the account it most
// needs to work for is one whose credentials are already compromised, and
// because there is no admin role in the API layer to gate such an endpoint
// with — every authenticated user has equal permissions today.
func RunToken(args []string) error {
	flags := commandFlags("token")
	rest, done, err := parseCommandArgs("token", flags, args)
	if err != nil || done {
		return err
	}
	if len(rest) < 2 {
		return fmt.Errorf("usage:\n" +
			"  silo token list <email>\n" +
			"  silo token revoke <email>          revoke every token the user holds\n" +
			"  silo token revoke <email> <token>  revoke one token")
	}
	action, email := rest[0], rest[1]

	if err := openStores(); err != nil {
		return err
	}
	apitokenstore.Init(siloPair.Read, siloPair.Write)
	account.Init(siloPair.Read, siloPair.Write)

	// The operator names a person by their address, which is what they know.
	// It is resolved once, here at the edge, and everything below works in
	// account ids.
	acct, err := resolveAccount(email)
	if err != nil {
		return err
	}

	switch action {
	case "list":
		return listTokens(acct)
	case "revoke":
		if len(rest) > 2 {
			return revokeOneToken(acct, rest[2])
		}
		return revokeAllTokens(acct)
	default:
		return fmt.Errorf("unknown token subcommand %q; expected list or revoke", action)
	}
}

func listTokens(acct *account.Account) error {
	syncTokens, err := libmgr.ListLibraryTokensByAccount(acct.ID)
	if err != nil {
		return err
	}
	apiTokens, err := apitokenstore.ListByAccount(acct.ID)
	if err != nil {
		return err
	}

	if len(syncTokens) == 0 && len(apiTokens) == 0 {
		fmt.Printf("No tokens for %s.\n", acct.Email)
		return nil
	}

	if len(syncTokens) > 0 {
		fmt.Printf("Sync tokens (%d) — legacy, no expiry, nothing validates them:\n", len(syncTokens))
		for _, t := range syncTokens {
			fmt.Printf("  %s  library %s  created %s\n", t.Token, t.LibraryID, formatUnix(t.Ctime))
		}
	}
	if len(apiTokens) > 0 {
		if len(syncTokens) > 0 {
			fmt.Println()
		}
		fmt.Printf("API tokens (%d) — /api2/ credentials, expiry slides on use:\n", len(apiTokens))
		now := time.Now().Unix()
		for _, t := range apiTokens {
			state := "expires " + formatTime(t.ExpiresAt)
			if t.ExpiresAt <= now {
				state = "EXPIRED " + formatTime(t.ExpiresAt)
			}
			fmt.Printf("  %s  created %s  %s\n", t.Token, formatUnix(t.Ctime), state)
		}
	}
	return nil
}

func revokeAllTokens(acct *account.Account) error {
	syncCount, err := libmgr.DeleteLibraryTokensByAccount(acct.ID)
	if err != nil {
		return err
	}
	apiCount, err := apitokenstore.DeleteByAccount(acct.ID)
	if err != nil {
		return fmt.Errorf("revoked %d sync token%s, but failed to revoke API tokens: %v",
			syncCount, pluralS(syncCount), err)
	}

	fmt.Printf("Revoked %d sync token%s and %d API token%s for %s.\n",
		syncCount, pluralS(syncCount), apiCount, pluralS(apiCount), acct.Email)
	warnAboutServerCache(syncCount + apiCount)
	return nil
}

// revokeOneToken revokes a single credential, whichever store it lives in.
// The token is matched against the user's own tokens rather than deleted by
// value, so a typo cannot revoke someone else's credential.
func revokeOneToken(acct *account.Account, token string) error {
	syncTokens, err := libmgr.ListLibraryTokensByAccount(acct.ID)
	if err != nil {
		return err
	}
	var found int
	for _, t := range syncTokens {
		if t.Token != token {
			continue
		}
		// One token can appear against several libraries.
		if err := libmgr.DeleteLibraryToken(t.LibraryID, t.Token, acct.ID); err != nil {
			return err
		}
		found++
	}
	if found > 0 {
		fmt.Printf("Revoked sync token for %s (%d librar%s).\n", acct.Email, found, pluralY(found))
		warnAboutServerCache(int64(found))
		return nil
	}

	apiTokens, err := apitokenstore.ListByAccount(acct.ID)
	if err != nil {
		return err
	}
	for _, t := range apiTokens {
		if t.Token != token {
			continue
		}
		if err := apitokenstore.Delete(token); err != nil {
			return err
		}
		fmt.Printf("Revoked API token for %s.\n", acct.Email)
		warnAboutServerCache(1)
		return nil
	}

	return fmt.Errorf("no token %q belongs to %s; run \"silo token list %s\" to see what does",
		token, acct.Email, acct.Email)
}

// warnAboutServerCache states the window in which a credential that should
// have stopped working still does. validateToken answers from an in-memory
// cache and a hit is not re-checked against the database, so neither revoking
// a token nor disabling the account behind one takes effect at once. This
// being a separate process, there is no way to reach in and purge that cache
// — only the TTL bounds it.
// resolveAccount turns the address an operator typed into the account the
// tables hold, for the commands that act on one person.
//
// It distinguishes "no such account" from a store that would not answer. The
// three copies this replaced all reported a database failure as a missing
// user, which is the wrong thing to tell somebody whose recovery tool has
// just failed for a reason they could fix.
func resolveAccount(email string) (*account.Account, error) {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	acct, err := account.ByEmail(ctx, email)
	if errors.Is(err, account.ErrNotFound) {
		return nil, fmt.Errorf("no account for %s", email)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up %s: %v", email, err)
	}
	return acct, nil
}

func warnAboutServerCache(affected int64) {
	if affected == 0 || option.AuthCacheTTL <= 0 {
		return
	}
	fmt.Printf("\nA running server caches token lookups for up to %s (SILO_AUTH_CACHE_TTL),\n"+
		"so a client that is already syncing can keep working until its cache entry ages\n"+
		"out. Restart the server to apply this immediately.\n", option.AuthCacheTTL)
}

func formatUnix(v sql.NullInt64) string {
	if !v.Valid {
		return "unknown"
	}
	return formatTime(v.Int64)
}

func formatTime(sec int64) string {
	if sec == 0 {
		return "unknown"
	}
	return time.Unix(sec, 0).Format(time.RFC3339)
}

func pluralS(n int64) string {
	if n == 1 {
		return ""
	}
	return "s"
}
