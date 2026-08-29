package silod

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/setup"
)

// RunSetupToken prints the token that creates this server's first account.
//
// The server prints it at every boot until it is claimed, but a boot log is a
// thing an operator scrolls past, rotates away or loses to a container that
// restarted. This is the way back to it without restarting anything.
//
// It is a local command, and unlike the other local commands there is no remote
// version of it to build later. RunToken is a CLI because the account it serves
// may already be compromised and because there is no admin role to gate an
// endpoint with; this one is a CLI for a stronger reason. Its whole premise is
// that no account exists yet, so an HTTP version could carry no credential --
// an unauthenticated endpoint handing out the setup token is exactly equivalent
// to having no token at all. The credential here *is* read access to the data
// directory, which is the same access that could write an Account row by hand.
//
// So: when the local admin commands eventually grow remote proxying against
// SILO_URL, this one does not go with them.
func RunSetupToken(args []string) error {
	flags := commandFlags("setup-token")
	rest, done, err := parseCommandArgs("setup-token", flags, args)
	if err != nil || done {
		return err
	}
	if len(rest) > 0 {
		return errors.New("usage:\n" +
			"  silo setup-token   print the token that creates the first account")
	}

	if err := openStores(); err != nil {
		return err
	}
	account.Init(siloPair.Read, siloPair.Write)
	setup.Init(siloPair.Read, siloPair.Write)

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	// Ensure rather than a plain read, so an existing but never-claimed
	// install -- one upgraded to this version, say -- can be given a token
	// without starting the server first. It is idempotent, so running this
	// twice prints the same string rather than invalidating the one the
	// operator already copied.
	tok, err := setup.Ensure(ctx)
	if err != nil {
		return err
	}
	if tok.IsZero() {
		return errSetupAlreadyDone
	}

	printSetupToken(tok)
	return nil
}

// errSetupAlreadyDone is the refusal on a server that has an account. It names
// the way forward, because an operator reaching for this command is one who has
// lost a credential and needs to be told which door is still open.
var errSetupAlreadyDone = errors.New(
	"this server already has an account, so there is no setup token.\n" +
		`Use "silo user add <email>" to create another account`)

// printSetupToken puts the prose on stderr and the token alone on stdout, so
// the command reads well to a person and pipes cleanly to a script without
// either getting the other's half.
func printSetupToken(tok setup.Token) {
	fmt.Fprintln(os.Stderr, "Give this to the setup screen in `silo tui`, with the email and")
	fmt.Fprintln(os.Stderr, "password you want. It stops working once an account exists.")
	fmt.Println(tok)
}
