package bind

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SiloDrive/silo/fileserver/account"
	"github.com/SiloDrive/silo/fileserver/credential"
	"github.com/SiloDrive/silo/fileserver/dbutil"
	"github.com/SiloDrive/silo/fileserver/invite"
	"github.com/SiloDrive/silo/fileserver/oidc"
	"github.com/SiloDrive/silo/fileserver/option"
)

const issuer = "https://id.example.com"

func testDB(t *testing.T) *dbutil.DBPair {
	t.Helper()
	if option.DBOpTimeout <= 0 {
		option.DBOpTimeout = 30 * time.Second
	}
	pair, err := dbutil.OpenSQLite(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := dbutil.Prepare(pair.Write); err != nil {
		t.Fatalf("create tables: %v", err)
	}
	t.Cleanup(func() { _ = pair.Close() })
	account.Init(pair.Read, pair.Write)
	credential.Init(pair.Read, pair.Write)
	invite.Init(pair.Read, pair.Write)
	Init(pair.Write)
	return pair
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := option.WithDBTimeout(context.Background())
	t.Cleanup(cancel)
	return c
}

func policy(p oidc.Policy, domains ...string) *oidc.Config {
	return &oidc.Config{Issuer: issuer, ClientID: "silo", ClientSecret: "s", Policy: p, AllowedDomains: domains}
}

func person(sub, email string, verified bool) *oidc.Claims {
	return &oidc.Claims{Issuer: issuer, Subject: sub, Email: email, EmailVerified: verified}
}

// withPassword is somebody who signed up the ordinary way.
func withPassword(t *testing.T, email string) account.ID {
	t.Helper()
	id, _, err := account.Create(ctx(t), email, "PBKDF2SHA256$1$c2FsdA==$aGFzaA==", account.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func linkedTo(t *testing.T, pair *dbutil.DBPair, sub string) (account.ID, bool) {
	t.Helper()
	var id account.ID
	err := pair.Read.QueryRowContext(ctx(t),
		"SELECT account_id FROM AccountIdentity WHERE issuer = ? AND subject = ?", issuer, sub).Scan(&id)
	if err != nil {
		return account.Zero, false
	}
	return id, true
}

func accountCount(t *testing.T, pair *dbutil.DBPair) int {
	t.Helper()
	var n int
	if err := pair.Read.QueryRowContext(ctx(t), "SELECT COUNT(*) FROM Account").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

var allPolicies = []oidc.Policy{oidc.PolicyLink, oidc.PolicyCreate, oidc.PolicyIsolated}

// ---------------------------------------------------------------------------
// The two ways this goes wrong that matter most. Each is an existing person's
// account handed to somebody who only asserted its address.

// Under isolated, the IdP's address claims are exactly what the operator said
// not to believe. An identity arriving with an existing account's address --
// verified or not -- must not come out holding that account.
//
// account.Create is idempotent on the address: asked to create an account for
// a taken address, it returns the account that has it. Provisioning that
// trusted its answer would perform the address match that isolated exists to
// forbid, with no policy check anywhere on the path.
func TestUnderIsolatedAnExistingAddressIsRefusedNotHandedOver(t *testing.T) {
	for _, verified := range []bool{true, false} {
		pair := testDB(t)
		alice := withPassword(t, "alice@example.com")

		got, err := Account(ctx(t), policy(oidc.PolicyIsolated), person("attacker", "alice@example.com", verified))
		if got == alice {
			t.Fatalf("verified=%v: an identity asserting alice's address was handed alice's account", verified)
		}
		// Unverified is refused for being unverified, not for the address
		// being taken: telling somebody who proved nothing that an address
		// has an account here would answer "who uses this server" for anyone
		// who can type addresses at it.
		want := ErrAddressTaken
		if !verified {
			want = ErrNoVerifiedAddress
		}
		if !errors.Is(err, want) {
			t.Errorf("verified=%v: err = %v, want %v", verified, err, want)
		}
		if _, linked := linkedTo(t, pair, "attacker"); linked {
			t.Errorf("verified=%v: the identity was linked to something", verified)
		}
	}
}

// Under create, an unverified address is a claim anybody can make. It must
// neither match the account that holds it nor be provisioned -- and the path
// that would provision it is the same idempotent Create.
func TestAnUnverifiedAddressIsNeverHandedTheAccountThatHoldsIt(t *testing.T) {
	for _, p := range allPolicies {
		pair := testDB(t)
		alice := withPassword(t, "alice@example.com")

		got, err := Account(ctx(t), policy(p), person("attacker", "alice@example.com", false))
		if got == alice {
			t.Fatalf("%s: an unverified claim to alice's address was handed alice's account", p)
		}
		if err == nil {
			t.Errorf("%s: an unverified claim to a taken address was accepted", p)
		}
		if _, linked := linkedTo(t, pair, "attacker"); linked {
			t.Errorf("%s: the identity was linked to something", p)
		}
	}
}

// ---------------------------------------------------------------------------
// Step 0 and step 1: the domain list, and an identity already linked.

func TestALinkedIdentitySignsInToItsAccountUnderEveryPolicy(t *testing.T) {
	for _, p := range allPolicies {
		pair := testDB(t)
		alice := withPassword(t, "alice@example.com")
		if _, err := Account(ctx(t), policy(oidc.PolicyLink), person("alice-sub", "alice@example.com", true)); err != nil {
			t.Fatal(err)
		}

		// From here the address plays no part: a changed, unverified or
		// missing address still reaches the account the subject is linked to.
		for _, email := range []string{"alice@example.com", "alice@elsewhere.example.org", ""} {
			got, err := Account(ctx(t), policy(p), person("alice-sub", email, false))
			if err != nil || got != alice {
				t.Errorf("%s, address %q: = %v, %v; want alice's account", p, email, got, err)
			}
		}
		if n := accountCount(t, pair); n != 1 {
			t.Errorf("%s: %d accounts, want 1", p, n)
		}
	}
}

// A subject is linked once. Someone who changes their address at the IdP to
// alice's stays on their own account, because alice's is linked to alice.
func TestChangingYourAddressAtTheIdPDoesNotMoveYouOntoSomebodyElse(t *testing.T) {
	testDB(t)
	alice := withPassword(t, "alice@example.com")
	bob := withPassword(t, "bob@example.com")
	for _, c := range []*oidc.Claims{
		person("alice-sub", "alice@example.com", true),
		person("bob-sub", "bob@example.com", true),
	} {
		if _, err := Account(ctx(t), policy(oidc.PolicyLink), c); err != nil {
			t.Fatal(err)
		}
	}

	got, err := Account(ctx(t), policy(oidc.PolicyLink), person("bob-sub", "alice@example.com", true))
	if err != nil || got != bob {
		t.Fatalf("bob asserting alice's address = %v, %v; want bob's own account (alice is %v)", got, err, alice)
	}
}

func TestALinkedButDisabledAccountIsRefused(t *testing.T) {
	testDB(t)
	alice := withPassword(t, "alice@example.com")
	if _, err := Account(ctx(t), policy(oidc.PolicyLink), person("alice-sub", "alice@example.com", true)); err != nil {
		t.Fatal(err)
	}
	if err := account.SetActive(ctx(t), alice, false); err != nil {
		t.Fatal(err)
	}

	if _, err := Account(ctx(t), policy(oidc.PolicyLink), person("alice-sub", "alice@example.com", true)); !errors.Is(err, ErrDisabled) {
		t.Fatalf("err = %v, want ErrDisabled", err)
	}
	if a, _ := account.ByID(ctx(t), alice); a.IsActive {
		t.Error("signing in at the IdP switched a disabled account back on")
	}
}

// Checked before anything, including the identity lookup: somebody who has
// left the organisation keeps their IdP subject, and the domain list is how an
// operator says their address no longer counts.
func TestAnAddressOutsideTheAllowedDomainsIsRefusedEvenWhenLinked(t *testing.T) {
	testDB(t)
	withPassword(t, "alice@example.com")
	if _, err := Account(ctx(t), policy(oidc.PolicyLink), person("alice-sub", "alice@example.com", true)); err != nil {
		t.Fatal(err)
	}

	for _, p := range allPolicies {
		if _, err := Account(ctx(t), policy(p, "example.org"), person("alice-sub", "alice@example.com", true)); !errors.Is(err, ErrDomain) {
			t.Errorf("%s: err = %v, want ErrDomain", p, err)
		}
		if _, err := Account(ctx(t), policy(p, "example.org"), person("new-sub", "new@example.com", true)); !errors.Is(err, ErrDomain) {
			t.Errorf("%s, a newcomer: err = %v, want ErrDomain", p, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Step 2: a verified address.

func TestUnderLinkAVerifiedAddressLinksToItsAccount(t *testing.T) {
	for _, p := range []oidc.Policy{oidc.PolicyLink, oidc.PolicyCreate} {
		pair := testDB(t)
		alice := withPassword(t, "Alice@Example.com")

		got, err := Account(ctx(t), policy(p), person("alice-sub", "ALICE@example.COM", true))
		if err != nil || got != alice {
			t.Fatalf("%s: = %v, %v; want alice's account", p, got, err)
		}
		if linked, ok := linkedTo(t, pair, "alice-sub"); !ok || linked != alice {
			t.Errorf("%s: the identity was not linked to alice", p)
		}
		if n := accountCount(t, pair); n != 1 {
			t.Errorf("%s: %d accounts, want 1", p, n)
		}
	}
}

// A disabled account that somebody arrived at -- a password or an identity --
// is not reopened by proving its address.
func TestADisabledAccountIsNotReopenedByItsAddress(t *testing.T) {
	testDB(t)
	alice := withPassword(t, "alice@example.com")
	if err := account.SetActive(ctx(t), alice, false); err != nil {
		t.Fatal(err)
	}
	if _, err := Account(ctx(t), policy(oidc.PolicyCreate), person("alice-sub", "alice@example.com", true)); !errors.Is(err, ErrDisabled) {
		t.Fatalf("err = %v, want ErrDisabled", err)
	}
	if a, _ := account.ByID(ctx(t), alice); a.IsActive {
		t.Error("a disabled account was switched back on")
	}
}

// An outstanding invite is spent by the person proving the address it was
// sent to, with the role it carried, and cannot then be redeemed as a link.
func TestAnIdPLoginAtAnInvitedAddressSpendsTheInvite(t *testing.T) {
	pair := testDB(t)
	admin := withPassword(t, "root@example.com")
	_, token, err := invite.Mint(ctx(t), invite.Options{Email: "new@example.com", Role: account.RoleGuest, By: admin})
	if err != nil {
		t.Fatal(err)
	}

	got, err := Account(ctx(t), policy(oidc.PolicyLink), person("new-sub", "new@example.com", true))
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	a, err := account.ByID(ctx(t), got)
	if err != nil {
		t.Fatal(err)
	}
	if !a.IsActive || a.Role != account.RoleGuest || a.Email != "new@example.com" {
		t.Errorf("account = %+v, want active, guest, new@example.com", a)
	}
	if linked, ok := linkedTo(t, pair, "new-sub"); !ok || linked != got {
		t.Error("the identity was not linked")
	}
	if _, err := invite.Redeem(ctx(t), token); !errors.Is(err, invite.ErrSpent) {
		t.Errorf("redeeming the invite afterwards = %v, want ErrSpent", err)
	}
}

// Under isolated the invite is not spent either: finding it means trusting the
// address, which is the thing isolated says not to do.
func TestUnderIsolatedAnInvitedAddressIsNotClaimed(t *testing.T) {
	testDB(t)
	admin := withPassword(t, "root@example.com")
	if _, _, err := invite.Mint(ctx(t), invite.Options{Email: "new@example.com", Role: account.RoleAdmin, By: admin}); err != nil {
		t.Fatal(err)
	}
	if _, err := Account(ctx(t), policy(oidc.PolicyIsolated), person("new-sub", "new@example.com", true)); !errors.Is(err, ErrAddressTaken) {
		t.Fatalf("err = %v, want ErrAddressTaken", err)
	}
}

func TestAnInviteThatHasLapsedIsNotSpent(t *testing.T) {
	testDB(t)
	admin := withPassword(t, "root@example.com")
	if _, _, err := invite.Mint(ctx(t), invite.Options{
		Email: "new@example.com", Role: account.RoleUser, By: admin, Lifetime: time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := Account(ctx(t), policy(oidc.PolicyLink), person("new-sub", "new@example.com", true)); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("err = %v, want ErrNoAccount", err)
	}
}

// A tombstone with no invite is an address somebody shared to before its person
// arrived. Claiming it is creating an account, so only create does.
func TestATombstoneWithoutAnInviteIsClaimedOnlyUnderCreate(t *testing.T) {
	for p, wantErr := range map[oidc.Policy]error{
		oidc.PolicyLink:     ErrNoAccount,
		oidc.PolicyIsolated: ErrAddressTaken,
		oidc.PolicyCreate:   nil,
	} {
		testDB(t)
		stone, err := account.Tombstone(ctx(t), "new@example.com", account.DefaultRole)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Account(ctx(t), policy(p), person("new-sub", "new@example.com", true))
		if !errors.Is(err, wantErr) {
			t.Errorf("%s: err = %v, want %v", p, err, wantErr)
			continue
		}
		if wantErr == nil {
			a, _ := account.ByID(ctx(t), got)
			if got != stone || !a.IsActive {
				t.Errorf("%s: claimed %v (active %v), want the tombstone %v switched on", p, got, a.IsActive, stone)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Step 3: nobody matched.

func TestUnderLinkNobodyNewGetsAnAccount(t *testing.T) {
	pair := testDB(t)
	if _, err := Account(ctx(t), policy(oidc.PolicyLink), person("new-sub", "new@example.com", true)); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("err = %v, want ErrNoAccount", err)
	}
	if n := accountCount(t, pair); n != 0 {
		t.Errorf("%d accounts exist, want none", n)
	}
}

func TestANewcomerGetsAnAccountUnderCreateAndIsolated(t *testing.T) {
	for _, p := range []oidc.Policy{oidc.PolicyCreate, oidc.PolicyIsolated} {
		pair := testDB(t)
		got, err := Account(ctx(t), policy(p), person("new-sub", "New@Example.com", true))
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		a, err := account.ByID(ctx(t), got)
		if err != nil {
			t.Fatal(err)
		}
		if !a.IsActive || a.Role != account.DefaultRole || a.Email != "new@example.com" {
			t.Errorf("%s: account = %+v", p, a)
		}
		if linked, ok := linkedTo(t, pair, "new-sub"); !ok || linked != got {
			t.Errorf("%s: the identity was not linked", p)
		}
		// And it has no password, so arrival is the identity alone.
		if arrived, _ := account.Arrived(ctx(t), "new@example.com"); !arrived {
			t.Errorf("%s: an account made through the IdP reads as a tombstone", p)
		}
	}
}

// An address is what an account is known by and one account holds it, so an
// account is never made under one the IdP has not proven.
func TestNobodyIsProvisionedWithoutAVerifiedAddress(t *testing.T) {
	for _, p := range []oidc.Policy{oidc.PolicyCreate, oidc.PolicyIsolated} {
		pair := testDB(t)
		for _, c := range []*oidc.Claims{
			person("sub-1", "new@example.com", false),
			person("sub-2", "", true),
		} {
			if _, err := Account(ctx(t), policy(p), c); !errors.Is(err, ErrNoVerifiedAddress) {
				t.Errorf("%s, %+v: err = %v, want ErrNoVerifiedAddress", p, c, err)
			}
		}
		if n := accountCount(t, pair); n != 0 {
			t.Errorf("%s: %d accounts exist, want none", p, n)
		}
	}
}

// Two logins for one new person completing together -- two devices enrolled at
// once -- make one account, not one each or one error.
func TestTwoLoginsForOneNewcomerMakeOneAccount(t *testing.T) {
	pair := testDB(t)
	var wg sync.WaitGroup
	ids := make([]account.ID, 8)
	errs := make([]error, 8)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i], errs[i] = Account(ctx(t), policy(oidc.PolicyCreate), person("new-sub", "new@example.com", true))
		}()
	}
	wg.Wait()
	for i := range ids {
		if errs[i] != nil {
			t.Fatalf("login %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("login %d got %v, login 0 got %v", i, ids[i], ids[0])
		}
	}
	if n := accountCount(t, pair); n != 1 {
		t.Errorf("%d accounts, want 1", n)
	}
}

// The IdP's issuer is part of the key. The same subject from another issuer
// is somebody else.
func TestASubjectIsOnlyMeaningfulBesideItsIssuer(t *testing.T) {
	testDB(t)
	withPassword(t, "alice@example.com")
	if _, err := Account(ctx(t), policy(oidc.PolicyLink), person("shared-sub", "alice@example.com", true)); err != nil {
		t.Fatal(err)
	}
	other := &oidc.Claims{Issuer: "https://other.example.com", Subject: "shared-sub", Email: "stranger@example.com", EmailVerified: true}
	if _, err := Account(ctx(t), policy(oidc.PolicyLink), other); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("the same subject from another issuer = %v, want ErrNoAccount", err)
	}
}
