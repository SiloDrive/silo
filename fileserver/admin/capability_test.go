package admin

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/SiloDrive/silo/fileserver/account"
	"github.com/SiloDrive/silo/fileserver/dbutil"
	"github.com/SiloDrive/silo/fileserver/option"
)

func testDB(t *testing.T) *dbutil.DBPair {
	t.Helper()

	origTimeout := option.DBOpTimeout
	if option.DBOpTimeout <= 0 {
		option.DBOpTimeout = 30 * time.Second
	}
	pair, err := dbutil.OpenSQLite(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	if err := dbutil.Prepare(pair.Write); err != nil {
		t.Fatalf("creating test tables: %v", err)
	}

	origRead, origWrite := readDB, writeDB
	Init(pair.Read, pair.Write)
	account.Init(pair.Read, pair.Write)
	t.Cleanup(func() {
		readDB, writeDB = origRead, origWrite
		option.DBOpTimeout = origTimeout
		_ = pair.Close()
	})
	return pair
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := option.WithDBTimeout(context.Background())
	t.Cleanup(cancel)
	return c
}

// mkAccount creates an account in the given role and assigns it capabilities
// by the install's own hand, which is what setup and the CLI have.
func mkAccount(t *testing.T, email string, role account.Role, caps ...Capability) *account.Account {
	t.Helper()
	id, _, err := account.Create(ctx(t), email, "", role)
	if err != nil {
		t.Fatalf("creating %s: %v", email, err)
	}
	if len(caps) > 0 {
		if err := Assign(ctx(t), id, caps...); err != nil {
			t.Fatalf("assigning %v to %s: %v", caps, email, err)
		}
	}
	acct, err := account.ByEmail(ctx(t), email)
	if err != nil {
		t.Fatalf("reading back %s: %v", email, err)
	}
	return acct
}

// The vocabulary is closed, for the reason ParseRole's is: a capability read
// back that matches no rule is not an error at the point it is written, it is
// an authority nothing recognises -- and whether that refuses everything or
// allows everything depends on which way the rule reading it is written.
func TestAnUnknownCapabilityIsRefusedAtTheDoor(t *testing.T) {
	for _, s := range []string{"", "admin", "Users", "users ", "read_any_library", "*"} {
		if c, err := ParseCapability(s); err == nil {
			t.Errorf("ParseCapability(%q) = %q, want an error", s, c)
		}
	}
	if len(All()) != 6 {
		t.Errorf("the vocabulary holds %d capabilities, want the six", len(All()))
	}
	for _, want := range All() {
		if got, err := ParseCapability(string(want)); err != nil || got != want {
			t.Errorf("ParseCapability(%q) = %q, %v", want, got, err)
		}
	}
}

// The rule is a conjunction and this is the whole of it. Neither half implies
// the other: rows on a non-admin grant nothing, and being an admin grants
// nothing on its own.
func TestCanIsRoleAndRowAndNeitherAlone(t *testing.T) {
	testDB(t)
	rowsButNotAdmin := mkAccount(t, "user@example.com", account.RoleUser, All()...)
	adminNoRows := mkAccount(t, "bare@example.com", account.RoleAdmin)
	both := mkAccount(t, "admin@example.com", account.RoleAdmin, CapUsers)

	for _, c := range []struct {
		name string
		acct *account.Account
		cap  Capability
		want bool
	}{
		{"every row but not an admin", rowsButNotAdmin, CapUsers, false},
		{"an admin holding no rows", adminNoRows, CapUsers, false},
		{"an admin holding the row", both, CapUsers, true},
		{"an admin holding a different row", both, CapQuota, false},
	} {
		got, err := Can(ctx(t), c.acct, c.cap)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: Can(%s) = %v, want %v", c.name, c.cap, got, c.want)
		}
	}

	// A nil account is the unauthenticated caller, and it holds nothing.
	if got, err := Can(ctx(t), nil, CapUsers); err != nil || got {
		t.Errorf("Can(nil) = %v, %v, want false", got, err)
	}
}

// Otherwise grant is grant-plus-everything, spelled indirectly.
func TestNobodyGrantsWhatTheyDoNotHold(t *testing.T) {
	testDB(t)
	actor := mkAccount(t, "granter@example.com", account.RoleAdmin, CapGrant, CapUsers)
	target := mkAccount(t, "target@example.com", account.RoleAdmin)

	if err := Grant(ctx(t), actor, target.ID, CapUsers); err != nil {
		t.Fatalf("granting a capability the actor holds: %v", err)
	}
	err := Grant(ctx(t), actor, target.ID, CapRetention)
	if !errors.Is(err, ErrNotHeld) {
		t.Errorf("granting a capability the actor lacks = %v, want ErrNotHeld", err)
	}
	// And it did not land by half: a refused grant writes nothing.
	held, err := Of(ctx(t), target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || held[0] != CapUsers {
		t.Errorf("the target holds %v after a refused grant, want just users", held)
	}

	// Taking one away is the same rule read the other way. An account holding
	// only grant must not be able to strip every other administrator of an
	// authority it was never trusted with itself.
	stripper := mkAccount(t, "stripper@example.com", account.RoleAdmin, CapGrant)
	if err := Revoke(ctx(t), stripper, target.ID, CapUsers); !errors.Is(err, ErrNotHeld) {
		t.Errorf("revoking a capability the actor lacks = %v, want ErrNotHeld", err)
	}
}

// grant is the escalation boundary, so holding the operations without it is
// not enough to hand them on.
func TestGrantingWithoutTheGrantCapabilityIsRefused(t *testing.T) {
	testDB(t)
	actor := mkAccount(t, "operator@example.com", account.RoleAdmin, CapUsers, CapQuota)
	target := mkAccount(t, "target@example.com", account.RoleAdmin)

	if err := Grant(ctx(t), actor, target.ID, CapUsers); !errors.Is(err, ErrNeedsGrant) {
		t.Errorf("granting without the grant capability = %v, want ErrNeedsGrant", err)
	}

	// A user who is not an admin at all holds nothing, whatever rows say.
	notAdmin := mkAccount(t, "ordinary@example.com", account.RoleUser, All()...)
	if err := Grant(ctx(t), notAdmin, target.ID, CapUsers); !errors.Is(err, ErrNeedsGrant) {
		t.Errorf("granting as a non-admin = %v, want ErrNeedsGrant", err)
	}
}

// A server nobody can administer is a data-loss event with extra steps. The
// rule is about the last holder of grant, not the last admin -- an install can
// have several administrators of whom one manages authority.
func TestTheLastHolderOfGrantMayNotDropIt(t *testing.T) {
	testDB(t)
	only := mkAccount(t, "only@example.com", account.RoleAdmin, All()...)
	// A second admin, holding everything except grant, so that "the last admin"
	// and "the last holder of grant" are different accounts in this test.
	mkAccount(t, "deputy@example.com", account.RoleAdmin, CapUsers, CapQuota)

	err := Revoke(ctx(t), only, only.ID, CapGrant)
	if !errors.Is(err, ErrLastGrant) {
		t.Errorf("the last holder dropping grant = %v, want ErrLastGrant", err)
	}
	// The install's own hand is refused too. It is the same invariant, and the
	// CLI can always hand grant to somebody else first.
	if err := Withdraw(ctx(t), only.ID, CapGrant); !errors.Is(err, ErrLastGrant) {
		t.Errorf("withdrawing the last grant = %v, want ErrLastGrant", err)
	}

	// With a second holder it is an ordinary revocation.
	second := mkAccount(t, "second@example.com", account.RoleAdmin)
	if err := Grant(ctx(t), only, second.ID, CapGrant); err != nil {
		t.Fatalf("granting grant: %v", err)
	}
	if err := Revoke(ctx(t), only, only.ID, CapGrant); err != nil {
		t.Errorf("dropping grant with a second holder standing: %v", err)
	}
	if can, err := Can(ctx(t), only, CapGrant); err != nil || can {
		t.Errorf("the capability survived its revocation: %v, %v", can, err)
	}
}

// Granting is idempotent: a capability an account already holds is not an
// error, because the caller's intent is a state and not an increment.
func TestGrantingTwiceIsNotAnError(t *testing.T) {
	testDB(t)
	actor := mkAccount(t, "granter@example.com", account.RoleAdmin, CapGrant, CapUsers)
	target := mkAccount(t, "target@example.com", account.RoleAdmin)

	for range 2 {
		if err := Grant(ctx(t), actor, target.ID, CapUsers); err != nil {
			t.Fatalf("granting: %v", err)
		}
	}
	held, err := Of(ctx(t), target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 {
		t.Errorf("the target holds %v, want one row", held)
	}
}

// Changing a role is gated by grant, because the conjunction makes a demotion
// exactly as powerful as a revocation: an account demoted out of admin holds
// its rows and none of them mean anything.
func TestChangingARoleNeedsTheGrantCapability(t *testing.T) {
	testDB(t)
	actor := mkAccount(t, "granter@example.com", account.RoleAdmin, All()...)
	plain := mkAccount(t, "operator@example.com", account.RoleAdmin, CapUsers)
	target := mkAccount(t, "target@example.com", account.RoleUser)

	if err := SetRole(ctx(t), plain, target.ID, account.RoleGuest); !errors.Is(err, ErrNeedsGrant) {
		t.Errorf("a role change without grant = %v, want ErrNeedsGrant", err)
	}
	if err := SetRole(ctx(t), actor, target.ID, account.RoleGuest); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	got, err := account.ByID(ctx(t), target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Role != account.RoleGuest {
		t.Errorf("the role is %q after the change, want guest", got.Role)
	}
	if err := SetRole(ctx(t), actor, target.ID, account.Role("wheel")); err == nil {
		t.Error("SetRole accepted a role outside the closed set")
	}
}

// The last-holder rule protects the row. This protects the other half of the
// same conjunction: demoting the only account that is both an admin and a
// holder of grant leaves a server nobody can administer just as surely as
// taking the row away would, and nothing else would have said so.
func TestTheLastAdministratorMayNotBeDemoted(t *testing.T) {
	testDB(t)
	only := mkAccount(t, "only@example.com", account.RoleAdmin, All()...)
	// A second admin without grant, and a second grant-holder who is not an
	// admin, so that neither half alone rescues the install.
	mkAccount(t, "deputy@example.com", account.RoleAdmin, CapUsers)
	rowsOnly := mkAccount(t, "rows@example.com", account.RoleUser)
	if err := Grant(ctx(t), only, rowsOnly.ID, CapGrant); err != nil {
		t.Fatal(err)
	}

	if err := SetRole(ctx(t), only, only.ID, account.RoleUser); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("demoting the last administrator = %v, want ErrLastAdmin", err)
	}

	// Promoting the account that already holds the row is what makes the
	// demotion allowed, because then there are two.
	if err := SetRole(ctx(t), only, rowsOnly.ID, account.RoleAdmin); err != nil {
		t.Fatalf("promoting the second holder: %v", err)
	}
	if err := SetRole(ctx(t), only, only.ID, account.RoleUser); err != nil {
		t.Errorf("demoting with a second administrator standing: %v", err)
	}
}

// A disabled account can administer nothing, and the guard has to count that
// way or it miscounts.
//
// credential.load refuses an inactive account outright, so an account that is
// disabled holds no authority whatever its role and rows say. That makes
// is_active the third term of the conjunction, and leaving it out of the
// counting query is not a cosmetic gap: a disabled administrator standing in
// the table looks exactly like a live second holder, so the guard waves through
// the change that leaves nobody able to administer the server.
func TestADisabledAccountCanNothing(t *testing.T) {
	pair := testDB(t)
	acct := mkAccount(t, "sleeping@example.com", account.RoleAdmin, All()...)
	if _, err := pair.Write.Exec("UPDATE Account SET is_active = 0 WHERE id = ?", acct.ID); err != nil {
		t.Fatal(err)
	}
	reread, err := account.ByID(ctx(t), acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if can, err := Can(ctx(t), reread, CapGrant); err != nil || can {
		t.Errorf("Can on a disabled administrator = %v, %v, want false", can, err)
	}
}

// The counting question, asked with a disabled administrator standing in the
// table. Disable an old admin today and demote the current one tomorrow, and
// nothing catches it.
func TestADisabledAdministratorDoesNotCountAsTheSecondOne(t *testing.T) {
	pair := testDB(t)
	live := mkAccount(t, "live@example.com", account.RoleAdmin, All()...)
	sleeping := mkAccount(t, "sleeping@example.com", account.RoleAdmin, All()...)
	if _, err := pair.Write.Exec("UPDATE Account SET is_active = 0 WHERE id = ?", sleeping.ID); err != nil {
		t.Fatal(err)
	}

	if err := SetRole(ctx(t), live, live.ID, account.RoleUser); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("demoting the last live administrator = %v, want ErrLastAdmin", err)
	}
	if err := Revoke(ctx(t), live, live.ID, CapGrant); !errors.Is(err, ErrLastGrant) {
		t.Errorf("revoking from the last live administrator = %v, want ErrLastGrant", err)
	}
}

// The third door. Deactivation stops every lane at once -- that is what the
// is_active join in credential.Resolve does -- so disabling the last
// administrator reaches the same state as demoting them, by a route the other
// two guards never saw.
//
// It is the worst of the three while it is open, because it inverts the
// escalation boundary: an admin holding only users could lock the install out,
// while an admin holding only grant is refused the identical outcome. The fix
// is the guard rather than a capability change -- once deactivation asks the
// same question, users can no longer do what grant cannot.
func TestTheLastAdministratorMayNotBeDeactivated(t *testing.T) {
	testDB(t)
	only := mkAccount(t, "only@example.com", account.RoleAdmin, All()...)
	mkAccount(t, "deputy@example.com", account.RoleAdmin, CapUsers)

	if err := SetActive(ctx(t), only.ID, false); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("disabling the last administrator = %v, want ErrLastAdmin", err)
	}

	// With a second live administrator it is an ordinary disable, and
	// re-enabling never needs the guard at all.
	second := mkAccount(t, "second@example.com", account.RoleAdmin)
	if err := Grant(ctx(t), only, second.ID, CapGrant); err != nil {
		t.Fatal(err)
	}
	if err := SetActive(ctx(t), only.ID, false); err != nil {
		t.Errorf("disabling with a second administrator standing: %v", err)
	}
	if err := SetActive(ctx(t), only.ID, true); err != nil {
		t.Errorf("re-enabling: %v", err)
	}
}

// Disabling an ordinary account is not the guard's business, and a guard that
// made an operator prove otherwise on every disable would be one they route
// around.
func TestDisablingAnOrdinaryAccountIsNotGuarded(t *testing.T) {
	testDB(t)
	mkAccount(t, "only@example.com", account.RoleAdmin, All()...)
	ordinary := mkAccount(t, "ordinary@example.com", account.RoleUser)

	if err := SetActive(ctx(t), ordinary.ID, false); err != nil {
		t.Errorf("disabling an ordinary account: %v", err)
	}
}
