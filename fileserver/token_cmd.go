package silod

import (
	"database/sql"
	"flag"
	"fmt"
	"time"

	"github.com/dkam/silo/fileserver/apitokenstore"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/repomgr"
)

// RunToken lists and revokes the credentials a user holds.
//
// Both stores could already revoke — repomgr.DeleteRepoToken,
// repomgr.DeleteRepoTokensByEmail and apitokenstore.DeleteByEmail were
// written for it and documented as the way to invalidate a token — but
// nothing outside their own tests ever called them. Sync tokens have no
// expiry by design, because Seafile clients persist them and treat them as
// durable, so with no reachable revocation a token copied off a stolen laptop
// kept read/write access to the library forever. A password change did not
// touch it. The only remedy was editing SQLite by hand.
//
// This is a CLI rather than an HTTP endpoint because the account it most
// needs to work for is one whose credentials are already compromised, and
// because there is no admin role in the API layer to gate such an endpoint
// with — every authenticated user has equal permissions today.
func RunToken(args []string) error {
	flags := flag.NewFlagSet("silo token", flag.ContinueOnError)
	flags.StringVar(&configFile, "C", "", "path to config file (optional)")
	flags.StringVar(&dataDir, "d", "", "data directory (default: $SILO_DATA_DIR or ~/.local/share/silo)")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}

	rest := flags.Args()
	// flag.Parse stops at the first positional, so "token list bob -d /srv"
	// leaves -d unparsed and silently operates on the default data directory —
	// reporting "no tokens" for an account whose tokens are somewhere else.
	if err := rejectTrailingFlags("token", rest); err != nil {
		return err
	}
	if len(rest) < 2 {
		return fmt.Errorf("usage:\n" +
			"  silo token list <email>\n" +
			"  silo token revoke <email>          revoke every token the user holds\n" +
			"  silo token revoke <email> <token>  revoke one token")
	}
	action, email := rest[0], rest[1]

	if err := resolvePaths(); err != nil {
		return err
	}
	option.LoadFileServerOptions(configFile)
	loadDatabases()
	repomgr.Init(seafilePair.Read, seafilePair.Write)
	apitokenstore.Init(seafilePair.Read, seafilePair.Write)

	switch action {
	case "list":
		return listTokens(email)
	case "revoke":
		if len(rest) > 2 {
			return revokeOneToken(email, rest[2])
		}
		return revokeAllTokens(email)
	default:
		return fmt.Errorf("unknown token subcommand %q; expected list or revoke", action)
	}
}

func listTokens(email string) error {
	syncTokens, err := repomgr.ListRepoTokensByEmail(email)
	if err != nil {
		return err
	}
	apiTokens, err := apitokenstore.ListByEmail(email)
	if err != nil {
		return err
	}

	if len(syncTokens) == 0 && len(apiTokens) == 0 {
		fmt.Printf("No tokens for %s.\n", email)
		return nil
	}

	if len(syncTokens) > 0 {
		fmt.Printf("Sync tokens (%d) — used by Seafile Desktop and SeaDrive, no expiry:\n", len(syncTokens))
		for _, t := range syncTokens {
			fmt.Printf("  %s  repo %s  created %s\n", t.Token, t.RepoID, formatUnix(t.Ctime))
		}
	}
	if len(apiTokens) > 0 {
		if len(syncTokens) > 0 {
			fmt.Println()
		}
		fmt.Printf("API tokens (%d) — /api2/ credentials, expiry slides on use:\n", len(apiTokens))
		now := time.Now().Unix()
		for _, t := range apiTokens {
			state := "expires " + formatUnix(t.ExpiresAt)
			if t.ExpiresAt.Valid && t.ExpiresAt.Int64 <= now {
				state = "EXPIRED " + formatUnix(t.ExpiresAt)
			}
			fmt.Printf("  %s  created %s  %s\n", t.Token, formatUnix(t.Ctime), state)
		}
	}
	return nil
}

func revokeAllTokens(email string) error {
	syncCount, err := repomgr.DeleteRepoTokensByEmail(email)
	if err != nil {
		return err
	}
	apiCount, err := apitokenstore.DeleteByEmail(email)
	if err != nil {
		return fmt.Errorf("revoked %d sync token%s, but failed to revoke API tokens: %v",
			syncCount, pluralS(syncCount), err)
	}

	fmt.Printf("Revoked %d sync token%s and %d API token%s for %s.\n",
		syncCount, pluralS(syncCount), apiCount, pluralS(apiCount), email)
	warnAboutServerCache(syncCount + apiCount)
	return nil
}

// revokeOneToken revokes a single credential, whichever store it lives in.
// The token is matched against the user's own tokens rather than deleted by
// value, so a typo cannot revoke someone else's credential.
func revokeOneToken(email, token string) error {
	syncTokens, err := repomgr.ListRepoTokensByEmail(email)
	if err != nil {
		return err
	}
	var found int
	for _, t := range syncTokens {
		if t.Token != token {
			continue
		}
		// One token can appear against several repos.
		if err := repomgr.DeleteRepoToken(t.RepoID, t.Token, email); err != nil {
			return err
		}
		found++
	}
	if found > 0 {
		fmt.Printf("Revoked sync token for %s (%d librar%s).\n", email, found, pluralY(found))
		warnAboutServerCache(int64(found))
		return nil
	}

	apiTokens, err := apitokenstore.ListByEmail(email)
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
		fmt.Printf("Revoked API token for %s.\n", email)
		warnAboutServerCache(1)
		return nil
	}

	return fmt.Errorf("no token %q belongs to %s; run \"silo token list %s\" to see what does",
		token, email, email)
}

// warnAboutServerCache states the window in which a revoked token still
// works. validateToken answers from an in-memory cache, and this being a
// separate process, there is no way to reach into the running server and
// purge it — only the TTL bounds it.
func warnAboutServerCache(revoked int64) {
	if revoked == 0 || option.AuthCacheTTL <= 0 {
		return
	}
	fmt.Printf("\nA running server caches token lookups for up to %s (SILO_AUTH_CACHE_TTL),\n"+
		"so a revoked token can keep working until its cache entry ages out. Restart the\n"+
		"server to apply the revocation immediately.\n", option.AuthCacheTTL)
}

func formatUnix(v sql.NullInt64) string {
	if !v.Valid || v.Int64 == 0 {
		return "unknown"
	}
	return time.Unix(v.Int64, 0).Format(time.RFC3339)
}

func pluralS(n int64) string {
	if n == 1 {
		return ""
	}
	return "s"
}
