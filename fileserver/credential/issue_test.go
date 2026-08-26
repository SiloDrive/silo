package credential

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/option"
)

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := option.WithDBTimeout(context.Background())
	t.Cleanup(cancel)
	return c
}

// The round trip that matters: what Issue hands back resolves, and resolves to
// the row it wrote.
func TestIssueThenResolve(t *testing.T) {
	pair := testDB(t)
	dan := addUser(t, pair, "dan@example.com", true)

	cred, secret, err := Issue(ctx(t), IssueOpts{
		Kind:      KindDevice,
		AccountID: dan,
		Label:     "dan's macbook, porter-fuse",
		Scope:     Scope{LibraryID: "library-1", Path: "/photos"},
		Perm:      "r",
		ClientID:  "porter-fuse",
		Lifetime:  90 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if !strings.HasPrefix(secret, "silo_device_") {
		t.Errorf("token = %q, want a silo_device_ prefix", secret)
	}

	got, err := Resolve(bearer(secret), KindDevice)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ID != cred.ID {
		t.Errorf("id = %q, want %q", got.ID, cred.ID)
	}
	if got.AccountID != dan {
		t.Errorf("account = %s, want %s", got.AccountID, dan)
	}
	if got.Label != "dan's macbook, porter-fuse" {
		t.Errorf("label = %q", got.Label)
	}
	if s := got.Scope.String(); s != "library-1:/photos" {
		t.Errorf("scope = %q, want %q", s, "library-1:/photos")
	}
	if got.Perm != "r" {
		t.Errorf("perm = %q, want r", got.Perm)
	}
	if got.ClientID != "porter-fuse" {
		t.Errorf("client_id = %q, want porter-fuse", got.ClientID)
	}
	if got.ExpiresAt != cred.ExpiresAt || got.ExpiresAt == 0 {
		t.Errorf("expires_at = %d, want %d", got.ExpiresAt, cred.ExpiresAt)
	}
	if !got.Bearer() {
		t.Error("a credential issued without a public key should be bearer")
	}
}

// Lifetime 0 is "no expiry" rather than "expired at the epoch", which is the
// difference between a credential that works forever and one that never works.
func TestIssueWithNoLifetimeDoesNotExpire(t *testing.T) {
	pair := testDB(t)
	dan := addUser(t, pair, "dan@example.com", true)

	cred, secret, err := Issue(ctx(t), IssueOpts{
		Kind: KindSession, AccountID: dan, Label: "cli", Perm: "rw",
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if cred.ExpiresAt != 0 {
		t.Errorf("expires_at = %d, want 0", cred.ExpiresAt)
	}
	if _, err := Resolve(bearer(secret), KindSession); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
}

// A public-key row has no secret to hand back. Returning one would be a
// credential the caller could never present.
func TestIssueWithPublicKeyReturnsNoSecret(t *testing.T) {
	pair := testDB(t)
	dan := addUser(t, pair, "dan@example.com", true)

	cred, secret, err := Issue(ctx(t), IssueOpts{
		Kind: KindDevice, AccountID: dan, Label: "laptop", Perm: "rw",
		PublicKey: []byte("not-a-real-spki"),
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if secret != "" {
		t.Errorf("secret = %q, want empty", secret)
	}
	if cred.Bearer() {
		t.Error("a public-key credential must not report as bearer")
	}
}

func TestIssueRefusesBadRows(t *testing.T) {
	pair := testDB(t)
	dan := addUser(t, pair, "dan@example.com", true)
	ok := IssueOpts{Kind: KindSession, AccountID: dan, Label: "cli", Perm: "rw"}

	cases := []struct {
		name string
		opts IssueOpts
		want error
	}{
		{"unknown kind", func() IssueOpts { o := ok; o.Kind = "wat"; return o }(), ErrBadKind},
		{"no account", func() IssueOpts { o := ok; o.AccountID = account.Zero; return o }(), ErrNoAccount},
		{"no label", func() IssueOpts { o := ok; o.Label = ""; return o }(), ErrNoLabel},
		{"empty perm", func() IssueOpts { o := ok; o.Perm = ""; return o }(), ErrBadPerm},
		// The one that would otherwise mint a credential that authenticates
		// and then permits nothing, because minPerm reads it as no access.
		{"misspelled perm", func() IssueOpts { o := ok; o.Perm = "read"; return o }(), ErrBadPerm},
		// "Already expired" must not round to "never expires", which is what
		// the arithmetic would otherwise have done.
		{"negative lifetime", func() IssueOpts { o := ok; o.Lifetime = -time.Hour; return o }(), ErrBadLifetime},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := Issue(ctx(t), tc.opts); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestListByAccountAndRevoke(t *testing.T) {
	pair := testDB(t)
	dan := addUser(t, pair, "dan@example.com", true)
	eve := addUser(t, pair, "eve@example.com", true)

	first, _, err := Issue(ctx(t), IssueOpts{Kind: KindSession, AccountID: dan, Label: "cli", Perm: "rw"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	second, secret, err := Issue(ctx(t), IssueOpts{
		Kind: KindDevice, AccountID: dan, Label: "macbook", Perm: "r",
		Scope: Scope{LibraryID: "library-1"},
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, _, err := Issue(ctx(t), IssueOpts{Kind: KindSession, AccountID: eve, Label: "eve", Perm: "rw"}); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	list, err := ListByAccount(ctx(t), dan)
	if err != nil {
		t.Fatalf("ListByAccount: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d credentials, want 2", len(list))
	}
	for _, c := range list {
		if c.Bearer() {
			t.Errorf("credential %s carried proof material into a listing", c.ID)
		}
	}
	labels := map[string]string{list[0].ID: list[0].Label, list[1].ID: list[1].Label}
	if labels[first.ID] != "cli" || labels[second.ID] != "macbook" {
		t.Errorf("labels = %v", labels)
	}

	// Eve cannot revoke Dan's credential, and is told nothing went rather than
	// being told it does not exist.
	gone, err := Revoke(ctx(t), second.ID, eve)
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if gone {
		t.Fatal("revoking another account's credential reported success")
	}
	if _, err := Resolve(bearer(secret), KindDevice); err != nil {
		t.Fatalf("credential should still resolve: %v", err)
	}

	gone, err = Revoke(ctx(t), second.ID, dan)
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if !gone {
		t.Fatal("revoking own credential reported nothing went")
	}
	if _, err := Resolve(bearer(secret), KindDevice); !errors.Is(err, ErrInvalid) {
		t.Errorf("after revocation err = %v, want ErrInvalid", err)
	}
}

func TestRevokeAll(t *testing.T) {
	pair := testDB(t)
	dan := addUser(t, pair, "dan@example.com", true)
	eve := addUser(t, pair, "eve@example.com", true)

	for _, label := range []string{"cli", "macbook", "phone"} {
		if _, _, err := Issue(ctx(t), IssueOpts{
			Kind: KindSession, AccountID: dan, Label: label, Perm: "rw",
		}); err != nil {
			t.Fatalf("Issue: %v", err)
		}
	}
	if _, _, err := Issue(ctx(t), IssueOpts{Kind: KindSession, AccountID: eve, Label: "eve", Perm: "rw"}); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	n, err := RevokeAll(ctx(t), dan)
	if err != nil {
		t.Fatalf("RevokeAll: %v", err)
	}
	if n != 3 {
		t.Errorf("revoked %d, want 3", n)
	}
	list, err := ListByAccount(ctx(t), eve)
	if err != nil {
		t.Fatalf("ListByAccount: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("eve holds %d credentials, want 1", len(list))
	}
}

// The sweep reaches expired rows and nothing else. It is table space only:
// Resolve refuses an expired credential whether or not the sweeper has run,
// which is why a missed sweep is a tidiness problem rather than a security
// one.
func TestDeleteExpiredReachesOnlyExpiredRows(t *testing.T) {
	pair := testDB(t)
	dan := addUser(t, pair, "dan@example.com", true)

	live, liveSecret, err := Issue(ctx(t), IssueOpts{
		Kind: KindSession, AccountID: dan, Label: "live", Perm: "rw", Lifetime: time.Hour,
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	forever, foreverSecret, err := Issue(ctx(t), IssueOpts{
		Kind: KindDevice, AccountID: dan, Label: "no expiry", Perm: "rw",
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	stale, _, err := Issue(ctx(t), IssueOpts{
		Kind: KindSession, AccountID: dan, Label: "stale", Perm: "rw", Lifetime: time.Hour,
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := pair.Write.Exec("UPDATE Credential SET expires_at = ? WHERE id = ?",
		time.Now().Add(-time.Minute).Unix(), stale.ID); err != nil {
		t.Fatalf("backdating: %v", err)
	}

	n, err := DeleteExpired()
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if n != 1 {
		t.Errorf("swept %d rows, want 1", n)
	}

	// A row with no expiry is not an expired row. Reading NULL as "expired at
	// the epoch" would delete every device credential on the first sweep.
	for _, c := range []struct {
		name   string
		secret string
		kind   Kind
	}{
		{"the live session", liveSecret, KindSession},
		{"the credential with no expiry", foreverSecret, KindDevice},
	} {
		if _, err := Resolve(bearer(c.secret), c.kind); err != nil {
			t.Errorf("%s was swept: %v", c.name, err)
		}
	}
	_ = live
	_ = forever
}
