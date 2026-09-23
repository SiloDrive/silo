package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/SiloDrive/silo/client"
	"github.com/SiloDrive/silo/internal/format"
)

// Self-service credential management, over HTTP against the caller's own
// account. `silo token list <email>` is the operator's version of this table,
// read straight from the database on the host; this one is a person asking the
// server about themselves, and it is the one that works from a machine that is
// not the server -- which is the whole point, because the laptop you want to
// revoke is the laptop you cannot revoke it from.
//
// Singular, matching `silo token` and `silo user`, so nobody has to remember
// which subcommand took the s.

// cmdCredential dispatches `silo credential list` and
// `silo credential revoke <id>`.
//
// Both sign this command's own session out when they are done, including when
// they fail. The CLI's HTTP lane logs in with the password and receives a
// twenty-four-hour session credential, so a command that printed the credential
// list without cleaning up would add a row to the table every time it showed
// one -- and the row it marked "this command" would be its own throwaway rather
// than anything the person came to look at.
func cmdCredential(c *client.APIClient, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: silo credential list|revoke")
	}

	// Named so the row this command adds says what it is while it exists. The
	// server takes a credential's label from the User-Agent, and without this
	// the list would show "Go-http-client/1.1" next to the laptop and the
	// phone.
	c.UserAgent = "silo credential"

	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return withLogout(c, func() error { return credentialList(c, rest) })
	case "revoke":
		return credentialRevoke(c, rest)
	default:
		return fmt.Errorf("unknown credential subcommand: %s (want list or revoke)", sub)
	}
}

// withLogout runs a command and discards this process's session afterwards,
// whether it succeeded or not.
//
// The command's own error wins if both fail: a failed logout leaves one extra
// row in a list the person can now see and revoke, while the error they asked
// about is the one they need. It is still reported when nothing else went
// wrong, because silently leaking a credential per invocation is how the table
// fills up with rows nobody can account for.
func withLogout(c *client.APIClient, run func() error) error {
	err := run()
	if lerr := discardSession(c); lerr != nil && err == nil {
		return fmt.Errorf("signing this command's own session out: %w", lerr)
	}
	return err
}

// discardSession signs this process's throwaway session out, and does nothing
// when the credential in use is one `silo login` stored. That one is the host's
// sign-in rather than this command's, and discarding it would sign the host out
// as a side effect of looking at a list.
func discardSession(c *client.APIClient) error {
	if !c.OwnsSession() {
		return nil
	}
	return c.Logout()
}

func credentialList(c *client.APIClient, args []string) error {
	fs := newFlagSet("credential list")
	jsonOut := fs.Bool("json", false, "output as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	creds, err := c.ListCredentials()
	if err != nil {
		return err
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(creds)
	}

	if len(creds) == 0 {
		fmt.Println("No credentials.")
		return nil
	}
	fmt.Printf("Credentials (%d):\n", len(creds))
	now := time.Now().Unix()
	for _, cr := range creds {
		// "this command" rather than "current" for a password sign-in: the
		// row is this invocation's throwaway session, and a person who read
		// it as their laptop would revoke the wrong thing. It is about to be
		// signed out anyway. On the credential `silo login` stored it is the
		// opposite -- the host's own sign-in, and the one row to keep.
		marker := ""
		if cr.Current {
			marker = "  <- this command"
			if !c.OwnsSession() {
				marker = "  <- this host"
			}
		}
		fmt.Printf("  %s  %-7s  %s%s\n", cr.ID, cr.Kind, cr.Label, marker)
		fmt.Printf("  %s  created %s  %s  last used %s\n",
			format.Blanks(len(cr.ID)), format.Time(cr.Created),
			format.Expiry(deref(cr.ExpiresAt), now), format.LastUsed(deref(cr.LastUsed)))
		if cr.Scope != "" {
			fmt.Printf("  %s  scope %s  perm %s\n", format.Blanks(len(cr.ID)), cr.Scope, cr.Perm)
		} else if cr.Perm != "rw" {
			fmt.Printf("  %s  every library, perm %s\n", format.Blanks(len(cr.ID)), cr.Perm)
		}
		if cr.ClientID != "" {
			fmt.Printf("  %s  device %s\n", format.Blanks(len(cr.ID)), cr.ClientID)
		}
		if cr.LastUA != "" {
			fmt.Printf("  %s  last seen %s\n", format.Blanks(len(cr.ID)), cr.LastUA)
		}
	}
	return nil
}

// deref reads an omitted timestamp as zero, which is what the formatters
// already treat as "nothing recorded".
func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func credentialRevoke(c *client.APIClient, args []string) error {
	fs := newFlagSet("credential revoke")
	// --others rather than an id, because "sign every other host out" is the
	// thing people come here for after losing a laptop and it should not
	// require reading ids off a list first. There is no --everywhere: that is
	// auth/logout/everywhere, and a flag that signs the running command out
	// reads as a bug in the command.
	others := fs.Bool("others", false, "revoke every other credential, keeping this command's own session")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *others {
		if fs.NArg() != 0 {
			return fmt.Errorf("silo credential revoke --others takes no id")
		}
		return withLogout(c, func() error {
			n, err := c.LogoutOthers()
			if err != nil {
				return err
			}
			fmt.Printf("Revoked %s.\n", plural(n, "other credential"))
			return nil
		})
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: silo credential revoke <credential-id>, or --others")
	}
	id := fs.Arg(0)

	current, err := c.RevokeCredential(id)
	if err != nil {
		// The session is still live, so it still needs discarding. The
		// revocation's own error is what the person asked about and is the one
		// returned.
		_ = discardSession(c)
		return err
	}
	if current && !c.OwnsSession() {
		// The stored sign-in, revoked from itself. Nothing else was touched,
		// and this host now has to sign in again to do anything.
		fmt.Printf("Revoked %s, which was this host's own sign-in. Run `silo login` to sign in again.\n", id)
		return nil
	}
	if current {
		// The id given was this command's own throwaway session, so the
		// revocation *was* the logout. Saying so matters more than it looks:
		// without it, somebody who copied the id marked "this command" would
		// see a bare success and reasonably conclude they had just signed out
		// a device.
		fmt.Printf("Revoked %s, which was this command's own session. Nothing else was signed out.\n", id)
		return nil
	}
	fmt.Printf("Revoked %s.\n", id)
	return discardSession(c)
}
