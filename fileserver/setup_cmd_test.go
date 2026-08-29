package silod

import (
	"strings"
	"testing"

	"github.com/dkam/silo/fileserver/authmgr"
	"github.com/dkam/silo/fileserver/setup"
)

// setupCmdStore is what RunSetupToken's body wires up for itself, over an empty
// database.
//
// Deliberately not userTestStore: that one is built on tokenTestStore, which
// mints two accounts before the test starts. An account is exactly the thing a
// server under this command must not have.
func setupCmdStore(t *testing.T) {
	t.Helper()
	sqliteTestDB(t)
	authmgr.Init(siloPair.Read, siloPair.Write)
	setup.Init(siloPair.Read, siloPair.Write)
}

// seedAccount creates one the way `silo user add` does -- without going near
// the setup token, which is the case the command has to cope with.
func seedAccount(t *testing.T, email string) {
	t.Helper()
	if _, err := authmgr.CreateAccount(t.Context(), email, "correct horse battery staple", false); err != nil {
		t.Fatalf("seeding %s: %v", email, err)
	}
}

// runSetupToken is RunSetupToken's body without openStores, which would reopen
// the process-wide database the harness has already pointed at a temp dir.
func runSetupToken(t *testing.T) (out string, err error) {
	t.Helper()
	out = captureStdout(t, func() {
		var tok setup.Token
		tok, err = setup.Ensure(t.Context())
		if err != nil {
			return
		}
		if tok.IsZero() {
			err = errSetupAlreadyDone
			return
		}
		printSetupToken(tok)
	})
	return out, err
}

// The token on stdout, on its own, so a script can read it. The prose that goes
// with it is on stderr and is deliberately not part of this.
func TestSetupTokenCommandPrintsTheStoredToken(t *testing.T) {
	setupCmdStore(t)

	out, err := runSetupToken(t)
	if err != nil {
		t.Fatalf("setup-token: %v", err)
	}

	printed := strings.TrimSpace(out)
	if strings.Count(printed, "\n") != 0 {
		t.Errorf("stdout carries more than the token: %q", out)
	}

	tok, parseErr := setup.Parse(printed)
	if parseErr != nil {
		t.Fatalf("what was printed does not parse as a token: %q", printed)
	}

	stored, err := setup.Ensure(t.Context())
	if err != nil {
		t.Fatalf("reading the token back: %v", err)
	}
	if !tok.Equal(stored) {
		t.Errorf("printed %q but the server holds %q", tok, stored)
	}
}

// Running it twice must not invalidate the string the operator already copied.
func TestSetupTokenCommandIsIdempotent(t *testing.T) {
	setupCmdStore(t)

	first, err := runSetupToken(t)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := runSetupToken(t)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if strings.TrimSpace(first) != strings.TrimSpace(second) {
		t.Errorf("two runs printed different tokens: %q then %q", first, second)
	}
}

// Once the server has an account the command refuses, and prints nothing that
// could be mistaken for a token.
func TestSetupTokenCommandRefusesOnceAnAccountExists(t *testing.T) {
	setupCmdStore(t)
	seedAccount(t, "someone@example.com")

	out, err := runSetupToken(t)
	if err == nil {
		t.Fatal("setup-token succeeded on a server that already has an account")
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("the refusal still printed to stdout: %q", out)
	}
	if !strings.Contains(err.Error(), "already has an account") {
		t.Errorf("the error does not say why: %v", err)
	}
}
