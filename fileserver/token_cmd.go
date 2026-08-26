package silod

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/credential"
	"github.com/dkam/silo/fileserver/option"
)

// RunToken lists and revokes the credentials a user holds.
//
// It used to reach into two stores and could not see a third. Sync tokens
// lived in LibraryUserToken, API tokens in ApiToken, and sessions were JWTs
// signed against a server-wide secret with no row anywhere -- so "revoke
// everything this person holds" was two deletes and a shrug, and the shrug
// was the lane a stolen laptop actually used. One table is what makes the
// command answer the question it has always claimed to.
//
// This is a CLI rather than an HTTP endpoint because the account it most
// needs to work for is one whose credentials are already compromised, and
// because there is no admin role in the API layer to gate such an endpoint
// with -- every authenticated user has equal permissions today.
func RunToken(args []string) error {
	flags := commandFlags("token")
	rest, done, err := parseCommandArgs("token", flags, args)
	if err != nil || done {
		return err
	}
	if len(rest) < 2 {
		return fmt.Errorf("usage:\n" +
			"  silo token list <email>\n" +
			"  silo token revoke <email>       revoke every credential the user holds\n" +
			"  silo token revoke <email> <id>  revoke one, by the id shown in list")
	}
	action, email := rest[0], rest[1]

	if err := openStores(); err != nil {
		return err
	}
	account.Init(siloPair.Read, siloPair.Write)
	credential.Init(siloPair.Read, siloPair.Write)

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

// listTokens prints what an account holds, in the form the revoke command
// takes back.
//
// The id is the column an operator acts on, so it comes first. It is the
// public half of the credential and appears in the token itself, which is what
// makes "revoke the one my laptop is using" answerable without anybody reading
// out a secret.
func listTokens(acct *account.Account) error {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	creds, err := credential.ListByAccount(ctx, acct.ID)
	if err != nil {
		return err
	}
	if len(creds) == 0 {
		fmt.Printf("No credentials for %s.\n", acct.Email)
		return nil
	}

	fmt.Printf("Credentials for %s (%d):\n", acct.Email, len(creds))
	now := time.Now().Unix()
	for _, c := range creds {
		fmt.Printf("  %s  %-7s  %s\n", c.ID, c.Kind, c.Label)
		fmt.Printf("  %s  created %s  %s  last used %s\n",
			blanks(len(c.ID)), formatTime(c.Ctime), expiryState(c.ExpiresAt, now), lastUsed(c.LastUsed))
		if s := c.Scope.String(); s != "" {
			fmt.Printf("  %s  scope %s  perm %s\n", blanks(len(c.ID)), s, c.Perm)
		}
	}
	return nil
}

func revokeAllTokens(acct *account.Account) error {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	n, err := credential.RevokeAll(ctx, acct.ID)
	if err != nil {
		return err
	}
	fmt.Printf("Revoked %d credential%s for %s.\n", n, pluralS(n), acct.Email)
	noteRevocationIsImmediate(n)
	return nil
}

// revokeOneToken revokes a single credential by its id.
//
// The account is part of the delete rather than checked before it, so a typo
// that names somebody else's credential removes nothing rather than removing
// theirs.
func revokeOneToken(acct *account.Account, id string) error {
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	gone, err := credential.Revoke(ctx, id, acct.ID)
	if err != nil {
		return err
	}
	if !gone {
		return fmt.Errorf("no credential %q belongs to %s; run \"silo token list %s\" to see what does",
			id, acct.Email, acct.Email)
	}
	fmt.Printf("Revoked credential %s for %s.\n", id, acct.Email)
	noteRevocationIsImmediate(1)
	return nil
}

// noteRevocationIsImmediate replaces a warning that used to be necessary.
//
// The old lanes answered from an in-memory cache that a hit never re-checked,
// so revoking a token left a window -- bounded only by SILO_AUTH_CACHE_TTL --
// in which it kept working, and this process had no way to reach into that
// one and purge it. Resolve reads the row on every request, so there is no
// window and no restart to recommend.
func noteRevocationIsImmediate(affected int64) {
	if affected == 0 {
		return
	}
	fmt.Println("\nRevocation takes effect on the next request; no restart is needed.")
}

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

func expiryState(expiresAt, now int64) string {
	switch {
	case expiresAt == 0:
		return "no expiry"
	case expiresAt <= now:
		return "EXPIRED " + formatTime(expiresAt)
	default:
		return "expires " + formatTime(expiresAt)
	}
}

// lastUsed distinguishes a credential that has never been presented from one
// presented long ago. They are the two ends of the same question -- is anybody
// still using this? -- and "unknown" would answer neither.
func lastUsed(sec int64) string {
	if sec == 0 {
		return "never"
	}
	return formatTime(sec)
}

func formatTime(sec int64) string {
	if sec == 0 {
		return "unknown"
	}
	return time.Unix(sec, 0).Format(time.RFC3339)
}

func blanks(n int) string {
	return fmt.Sprintf("%*s", n, "")
}

func pluralS(n int64) string {
	if n == 1 {
		return ""
	}
	return "s"
}
