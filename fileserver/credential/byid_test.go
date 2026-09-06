package credential

import (
	"errors"
	"testing"
	"time"
)

// ByID answers the question a long-lived holder has: is this credential still
// good, and what does it reach now.
func TestByIDReReadsALiveCredential(t *testing.T) {
	pair := testDB(t)
	dan := addUser(t, pair, "dan@example.com", true)

	issued, _, err := Issue(ctx(t), IssueOpts{Kind: KindDevice, AccountID: dan, Label: "macbook", Perm: "rw"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	got, err := ByID(ctx(t), issued.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if got.ID != issued.ID || got.AccountID != dan {
		t.Errorf("got credential %s for %s, want %s for %s", got.ID, got.AccountID, issued.ID, dan)
	}
	if got.Account() == nil || got.Account().ID != dan {
		t.Error("the account did not come back with the row")
	}
}

// Each way a credential stops being good is its own Resolve error, so a
// holder can tell a revocation it should act on from a store it cannot reach.
func TestByIDReportsWhyACredentialIsNoLongerGood(t *testing.T) {
	pair := testDB(t)
	dan := addUser(t, pair, "dan@example.com", true)
	off := addUser(t, pair, "off@example.com", false)

	revoked, _, err := Issue(ctx(t), IssueOpts{Kind: KindDevice, AccountID: dan, Label: "a", Perm: "rw"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := Revoke(ctx(t), revoked.ID, dan); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	expired, _, err := Issue(ctx(t), IssueOpts{Kind: KindDevice, AccountID: dan, Label: "b", Perm: "rw", Lifetime: time.Hour})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := Expire(ctx(t), expired.ID); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	disabled, _, err := Issue(ctx(t), IssueOpts{Kind: KindDevice, AccountID: off, Label: "c", Perm: "rw"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	for _, tc := range []struct {
		name string
		id   string
		want error
	}{
		{"revoked", revoked.ID, ErrInvalid},
		{"expired", expired.ID, ErrExpired},
		{"disabled account", disabled.ID, ErrInactive},
	} {
		if _, err := ByID(ctx(t), tc.id); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}
