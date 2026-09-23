package silod

import (
	"bytes"
	"strings"
	"testing"

	"github.com/SiloDrive/silo/client"
	"github.com/SiloDrive/silo/store"
)

// Recovery codes, from the side that mints them.
//
// Everything about them was built except the step that produces one, so no
// account held any and the recovery half of E2EE was a mechanism with no
// population. What makes that invisible until it matters is that nothing fails
// meanwhile: an account with no recovery wraps looks exactly like an account
// with ten until the day its password is gone.
//
// The test that carries the weight here is the operator reset. It is the whole
// reason the wraps exist -- `silo user passwd` cannot re-wrap an identity key,
// because re-wrapping means unwrapping and that needs the password whoever is
// resetting does not have -- and until now it had to print the bad version of
// its warning for every account on the server.

// enrolWithCodes enrols the wire account and returns the client, the account
// and the one set of codes that will ever be shown.
func enrolWithCodes(t *testing.T, base string) (*client.APIClient, *client.Account, []string) {
	t.Helper()
	c := client.NewClient(base)
	acct, codes, err := c.Enrol("wire@example.com", wirePassword)
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	return c, acct, codes
}

func TestEnrolMintsARecoverySetAndTheServerHoldsNoCode(t *testing.T) {
	base, _ := wire(t)
	c, _, codes := enrolWithCodes(t, base)

	if len(codes) != store.RecoveryCodeSetSize {
		t.Fatalf("Enrol returned %d codes, want a set of %d", len(codes), store.RecoveryCodeSetSize)
	}
	seen := map[string]bool{}
	for _, code := range codes {
		if _, err := store.RecoveryCodeSecret(code); err != nil {
			t.Errorf("Enrol returned %q, which is not a recovery code: %v", code, err)
		}
		if !strings.Contains(code, "-") {
			t.Errorf("code %q is not in display form", code)
		}
		if seen[code] {
			t.Errorf("the set repeats %q", code)
		}
		seen[code] = true
	}

	keys, err := c.AccountKeys()
	if err != nil {
		t.Fatalf("AccountKeys: %v", err)
	}
	if len(keys.Recovery) != store.RecoveryCodeSetSize {
		t.Fatalf("the server holds %d recovery wraps, want %d",
			len(keys.Recovery), store.RecoveryCodeSetSize)
	}
	ordinals := map[int]bool{}
	for _, w := range keys.Recovery {
		if ordinals[w.Ordinal] {
			t.Errorf("two wraps carry ordinal %d", w.Ordinal)
		}
		ordinals[w.Ordinal] = true
	}

	// The server stores a wrap per code and never a code. Asserted against the
	// bytes rather than trusted, because the whole value of a recovery wrap is
	// that the party holding it cannot use it.
	for _, code := range codes {
		secret, err := store.RecoveryCodeSecret(code)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range keys.Recovery {
			if bytes.Contains(w.WrappedKey, secret) {
				t.Fatal("a stored wrap contains the code that opens it")
			}
		}
	}
}

// The event the codes exist for: an operator reset, which leaves the identity
// key in place and unopenable by anything the account now knows.
func TestARecoveryCodeReopensAnIdentityStrandedByAnOperatorReset(t *testing.T) {
	base, _ := wire(t)
	c, acct, codes := enrolWithCodes(t, base)

	lib, kr, err := acct.CreateEncryptedLibrary("Sealed before the reset")
	if err != nil {
		t.Fatalf("CreateEncryptedLibrary: %v", err)
	}
	fs := client.NewEncryptedLibrary(c, lib.ID, kr)
	want := []byte("written under the password that is about to be lost")
	if err := fs.WriteFile("notes.txt", want, 1000); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// The operator resets the password. This is the real command, so the
	// warning it prints about key material is the one a person would read.
	const reset = "a password the operator chose"
	withStdin(t, reset+"\n")
	if err := passwdUser("wire@example.com", false); err != nil {
		t.Fatalf("passwdUser: %v", err)
	}

	// The account logs in and cannot open its own identity: the blob is sealed
	// under a wrapKey derived from a password nobody has.
	stranded := client.NewClient(base)
	if _, err := stranded.OpenAccount("wire@example.com", reset); err == nil {
		t.Fatal("OpenAccount opened an identity key wrapped under the old password")
	}

	// The code is what closes that gap, and it ends with the account whole:
	// identity re-wrapped under the password it can use, and back on derived
	// login.
	recovered := client.NewClient(base)
	back, err := recovered.Recover("wire@example.com", reset, codes[3])
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if back.ID != acct.ID {
		t.Errorf("Recover returned account %s, want %s", back.ID, acct.ID)
	}
	if back.Identity.Public() != acct.Identity.Public() {
		t.Fatal("Recover returned a different identity key")
	}

	// Which is only worth anything if the libraries open again.
	reader, err := back.Open(lib.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got, err := reader.ReadFile("notes.txt"); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("ReadFile after recovery = %q, %v", got, err)
	}

	// And an ordinary new device now works from the password alone, with no
	// code -- the account is not left in a recovered-once state.
	fresh := client.NewClient(base)
	if _, err := fresh.OpenAccount("wire@example.com", reset); err != nil {
		t.Fatalf("OpenAccount after recovery: %v", err)
	}
}

func TestRedeemingACodeSpendsItAndLeavesTheRestStanding(t *testing.T) {
	base, _ := wire(t)
	_, _, codes := enrolWithCodes(t, base)

	const reset = "a password the operator chose"
	withStdin(t, reset+"\n")
	if err := passwdUser("wire@example.com", false); err != nil {
		t.Fatalf("passwdUser: %v", err)
	}

	first := client.NewClient(base)
	if _, err := first.Recover("wire@example.com", reset, codes[3]); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	keys, err := first.AccountKeys()
	if err != nil {
		t.Fatalf("AccountKeys: %v", err)
	}
	if len(keys.Recovery) != store.RecoveryCodeSetSize-1 {
		t.Errorf("%d wraps remain, want %d", len(keys.Recovery), store.RecoveryCodeSetSize-1)
	}
	for _, w := range keys.Recovery {
		if w.Ordinal == 3 {
			t.Error("the redeemed wrap is still stored")
		}
	}

	// The spent code is spent, and it says so rather than failing as a wrong
	// code would somewhere else.
	if _, err := client.NewClient(base).Recover("wire@example.com", reset, codes[3]); err == nil {
		t.Error("the redeemed code worked a second time")
	}

	// The rest of the set is untouched, which is the rule the whole
	// one-blob-per-code shape exists to make cheap.
	if _, err := client.NewClient(base).Recover("wire@example.com", reset, codes[7]); err != nil {
		t.Errorf("a second code from the same set: %v", err)
	}
}

func TestRecoverRefusesACodeNoWrapOpensAndChangesNothing(t *testing.T) {
	base, _ := wire(t)
	c, _, _ := enrolWithCodes(t, base)

	stranger, err := store.GenerateRecoveryCode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.NewClient(base).Recover("wire@example.com", wirePassword, stranger); err == nil {
		t.Fatal("Recover accepted a code from no set")
	}

	// A refused attempt must not have spent anything on the way to refusing.
	keys, err := c.AccountKeys()
	if err != nil {
		t.Fatalf("AccountKeys: %v", err)
	}
	if len(keys.Recovery) != store.RecoveryCodeSetSize {
		t.Errorf("%d wraps remain after a refused code, want %d",
			len(keys.Recovery), store.RecoveryCodeSetSize)
	}
}

// What the operator is told when they reset a password.
//
// warnAboutKeyMaterial has always had two branches and, until a client minted
// a set, only one of them could ever run: every account on the server held
// zero wraps, so `silo user passwd` had to print "there is no way back to that
// identity key" whatever the truth was. This pins the other branch, which is
// the operator-facing half of the same fact the tests above assert.
func TestTheOperatorResetNamesTheRecoveryWrapsAnAccountHolds(t *testing.T) {
	base, _ := wire(t)
	enrolWithCodes(t, base)

	withStdin(t, "a password the operator chose\n")
	var err error
	out := captureStdout(t, func() {
		err = passwdUser("wire@example.com", false)
	})
	if err != nil {
		t.Fatalf("passwdUser: %v", err)
	}
	if !strings.Contains(out, "recovery wrap") {
		t.Errorf("the reset does not mention the recovery wraps:\n%s", out)
	}
	if strings.Contains(out, "no way back") {
		t.Errorf("the reset tells an account holding a set that there is no way back:\n%s", out)
	}
}
