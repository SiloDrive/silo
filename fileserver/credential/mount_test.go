package credential

import (
	"errors"
	"testing"
)

// The API lane accepts a session credential from the CLI and a device
// credential from Porter on the same routes, so Resolve has to be askable
// about a set rather than a single kind.
func TestResolveAcceptsAnyOfSeveralKinds(t *testing.T) {
	pair := testDB(t)
	dan := addUser(t, pair, "dan@example.com", true)

	_, session, err := Issue(ctx(t), IssueOpts{
		Kind: KindSession, AccountID: dan, Label: "cli", Perm: "rw",
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	_, device, err := Issue(ctx(t), IssueOpts{
		Kind: KindDevice, AccountID: dan, Label: "macbook", Perm: "rw",
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	for name, secret := range map[string]string{"session": session, "device": device} {
		if _, err := Resolve(bearer(secret), KindSession, KindDevice); err != nil {
			t.Errorf("%s credential: %v", name, err)
		}
	}

	// A kind outside the set is still refused: widening the question must not
	// widen the answer to every lane.
	_, access, err := Issue(ctx(t), IssueOpts{
		Kind: KindAccess, AccountID: dan, Label: "capability url", Perm: "r",
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := Resolve(bearer(access), KindSession, KindDevice); !errors.Is(err, ErrWrongKind) {
		t.Errorf("err = %v, want ErrWrongKind", err)
	}
}

// Asking for no kind at all is a caller that forgot to say what lane it is,
// and must not be read as "any lane will do".
func TestResolveWithNoKindsRefuses(t *testing.T) {
	pair := testDB(t)
	dan := addUser(t, pair, "dan@example.com", true)
	_, secret, err := Issue(ctx(t), IssueOpts{
		Kind: KindSession, AccountID: dan, Label: "cli", Perm: "rw",
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := Resolve(bearer(secret)); !errors.Is(err, ErrWrongKind) {
		t.Errorf("err = %v, want ErrWrongKind", err)
	}
}

// The middleware needs the account, not just its id: handlers still speak in
// addresses for commit authors and response bodies. It has to come out of the
// join Resolve already does, or every authenticated request costs a second
// query -- which is the trade the join was written to avoid.
func TestResolveCarriesTheAccount(t *testing.T) {
	pair := testDB(t)
	dan := addUser(t, pair, "dan@example.com", true)
	if _, err := pair.Write.Exec("UPDATE Account SET is_staff = 1 WHERE id = ?", dan); err != nil {
		t.Fatalf("setting is_staff: %v", err)
	}

	_, secret, err := Issue(ctx(t), IssueOpts{
		Kind: KindSession, AccountID: dan, Label: "cli", Perm: "rw",
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	cred, err := Resolve(bearer(secret), KindSession)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	acct := cred.Account()
	if acct == nil {
		t.Fatal("Resolve returned no account")
	}
	if acct.ID != dan {
		t.Errorf("account id = %s, want %s", acct.ID, dan)
	}
	if acct.Email != "dan@example.com" {
		t.Errorf("email = %q, want dan@example.com", acct.Email)
	}
	if !acct.IsActive {
		t.Error("account should be active")
	}
	if !acct.IsStaff {
		t.Error("is_staff did not survive the join")
	}
}
