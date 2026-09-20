package silod

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/SiloDrive/silo/fileserver/account"
	"github.com/SiloDrive/silo/fileserver/credential"
	"github.com/SiloDrive/silo/fileserver/option"
)

const (
	victim    = "victim@example.com"
	bystander = "bystander@example.com"

	sharedLibraryID = "b1f2ad61-9164-418a-a47f-ab805dbd5694"
	otherLibraryID  = "c2e3be72-a275-429b-b580-bc916ece6705"
	unknownCredID   = "aaaaaaaaaaaaaaaa"
)

// held is one seeded credential, kept so a test can name it by id.
type held struct {
	id    string
	label string
}

// tokenTestStore seeds the credential table with two users, so every test can
// check that a revocation stopped at the user it named.
//
// It seeds two lanes for the victim -- a session from the CLI and a device
// credential from a mount -- because the failure this command exists for is a
// person whose laptop was stolen, and the whole point of one table is that
// revoking reaches both without the operator knowing which is which.
func tokenTestStore(t *testing.T) (victimSession, victimDevice, bystanderSession held) {
	t.Helper()

	sqliteTestDB(t)

	victimAcct := mintAccount(t, victim)
	bystanderAcct := mintAccount(t, bystander)

	victimSession = seedCredential(t, victimAcct, credential.KindSession, "silo-cli", credential.Scope{})
	victimDevice = seedCredential(t, victimAcct, credential.KindDevice, "victim's macbook",
		credential.Scope{LibraryID: sharedLibraryID})
	bystanderSession = seedCredential(t, bystanderAcct, credential.KindSession, "bystander's cli",
		credential.Scope{LibraryID: otherLibraryID})
	return
}

func seedCredential(t *testing.T, acct *account.Account, kind credential.Kind, label string, scope credential.Scope) held {
	t.Helper()
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	c, _, err := credential.Issue(ctx, credential.IssueOpts{
		Kind: kind, AccountID: acct.ID, Label: label, Scope: scope, Perm: "rw",
	})
	if err != nil {
		t.Fatalf("issuing a %s credential for %s: %v", kind, acct.Email, err)
	}
	return held{id: c.ID, label: label}
}

// acctFor resolves one of this file's test addresses.
func acctFor(t *testing.T, email string) *account.Account {
	t.Helper()
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	acct, err := account.ByEmail(ctx, email)
	if err != nil {
		t.Fatalf("no account for %s: %v", email, err)
	}
	return acct
}

func countRows(t *testing.T, query string, args ...interface{}) int {
	t.Helper()
	var n int
	if err := siloPair.Read.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("failed to count with %q: %v", query, err)
	}
	return n
}

// The operation this command exists for: one person's credentials all stop,
// whatever lane each of them is on, and nobody else's do.
//
// It used to reach two tables and miss a third -- sessions were JWTs with no
// row at all -- so "revoke everything" was a claim the command could not make
// good on.
func TestRevokeAllTokensRemovesEveryLane(t *testing.T) {
	tokenTestStore(t)

	if err := revokeAllTokens(acctFor(t, victim)); err != nil {
		t.Fatalf("revokeAllTokens returned %v", err)
	}

	if n := countRows(t, "SELECT COUNT(*) FROM Credential WHERE account_id = ?", acctFor(t, victim).ID); n != 0 {
		t.Errorf("%d credentials survived revocation, want 0", n)
	}
	// Revoking one account must not sign out the rest of the server.
	if n := countRows(t, "SELECT COUNT(*) FROM Credential WHERE account_id = ?", acctFor(t, bystander).ID); n != 1 {
		t.Errorf("bystander holds %d credentials, want 1", n)
	}
}

// Revoking one device must leave the user's others working — that is the whole
// reason a credential is minted per client rather than shared between them.
func TestRevokeOneCredentialLeavesTheOthers(t *testing.T) {
	victimSession, victimDevice, _ := tokenTestStore(t)

	if err := revokeOneToken(acctFor(t, victim), victimDevice.id); err != nil {
		t.Fatalf("revokeOneToken returned %v", err)
	}

	if n := countRows(t, "SELECT COUNT(*) FROM Credential WHERE id = ?", victimDevice.id); n != 0 {
		t.Error("the revoked credential is still present")
	}
	if n := countRows(t, "SELECT COUNT(*) FROM Credential WHERE id = ?", victimSession.id); n != 1 {
		t.Error("the user's other credential was removed too")
	}
}

// The account is part of the delete rather than checked before it. Deleting by
// id alone would let a mistyped email revoke somebody else's credential and
// report success for it.
func TestRevokeOneTokenRefusesAnotherUsersCredential(t *testing.T) {
	_, _, bystanderSession := tokenTestStore(t)

	for _, id := range []string{bystanderSession.id, unknownCredID} {
		if err := revokeOneToken(acctFor(t, victim), id); err == nil {
			t.Errorf("revokeOneToken(%s, %s) succeeded, want an error", victim, id)
		}
	}
	if n := countRows(t, "SELECT COUNT(*) FROM Credential WHERE account_id = ?", acctFor(t, bystander).ID); n != 1 {
		t.Error("bystander's credential was revoked")
	}
}

func TestListTokens(t *testing.T) {
	tokenTestStore(t)

	if err := listTokens(mintAccount(t, "nobody@example.com")); err != nil {
		t.Errorf("listTokens returned %v for an account with no credentials", err)
	}
	if err := listTokens(acctFor(t, victim)); err != nil {
		t.Errorf("listTokens returned %v", err)
	}
}

// The label alone cannot say what is running, because renewal inherits it: a
// device enrolled on one build still carries that build's name after every
// upgrade. The listing has to show the two columns that answer the rest --
// which device this is across its whole credential chain, and what was last
// heard from it -- or an operator reads a frozen label as a live one.
func TestListTokensShowsTheDeviceAndWhatWasLastSeen(t *testing.T) {
	sqliteTestDB(t)
	acct := mintAccount(t, victim)

	const (
		domainID = "com.nmilne.SiloDrive.7C9A1E4F-0B2D-4A11-9E6C-5F3D2A8B1C40"
		ua       = "SiloDrive/0.1.0 (macOS 26.5; extension; build 126; 500d40c)"
	)
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()
	c, _, err := credential.Issue(ctx, credential.IssueOpts{
		Kind: credential.KindDevice, AccountID: acct.ID, Label: "dan's macbook",
		Perm: "rw", ClientID: domainID,
	})
	if err != nil {
		t.Fatalf("issuing: %v", err)
	}
	if _, err := siloPair.Write.Exec(
		"UPDATE Credential SET last_ua = ? WHERE id = ?", ua, c.ID); err != nil {
		t.Fatalf("stamping last_ua: %v", err)
	}

	var out string
	captureAndCheck(t, &out, acctFor(t, victim))
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		lines = append(lines, strings.TrimSpace(l))
	}
	for _, want := range []string{"device " + domainID, "last seen " + ua} {
		if !slices.Contains(lines, want) {
			t.Errorf("the listing does not show %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "dan's macbook") {
		t.Errorf("the listing does not show the label:\n%s", out)
	}
}

// A row with neither is the common case -- a CLI session, or a device that has
// not been seen since the column existed -- and printing an empty "device" or
// "last seen" line for it would pad every listing with facts nobody has.
func TestListTokensOmitsWhatItDoesNotKnow(t *testing.T) {
	tokenTestStore(t)

	var out string
	captureAndCheck(t, &out, acctFor(t, victim))
	// Matched against whole trimmed lines rather than as substrings: "device"
	// is also the kind column, so a Contains check here passes for the wrong
	// reason on every device credential in the listing.
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		for _, unwanted := range []string{"device ", "last seen "} {
			if strings.HasPrefix(line, unwanted) {
				t.Errorf("the listing shows %q for a row that has none:\n%s", line, out)
			}
		}
	}
}

// captureAndCheck runs the listing and fails the test if it errored, so each
// caller reads as the assertion it is about rather than as plumbing.
func captureAndCheck(t *testing.T, out *string, acct *account.Account) {
	t.Helper()
	var err error
	*out = captureStdout(t, func() { err = listTokens(acct) })
	if err != nil {
		t.Fatalf("listTokens: %v", err)
	}
}
