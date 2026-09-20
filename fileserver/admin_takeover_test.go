package silod

import (
	"net/http"
	"testing"

	"github.com/SiloDrive/silo/fileserver/account"
	"github.com/SiloDrive/silo/fileserver/admin"
	"github.com/SiloDrive/silo/fileserver/invite"
)

// The capability model's invariant is that nobody hands on, or takes away, an
// authority they do not hold themselves — mayChange enforces it on both Grant
// and Revoke, and it is what stops grant from meaning everything.
//
// The password lane went around it. A reset is impersonation, which the route's
// own comment says, and impersonating an account IS acquiring its capabilities:
// set the password, log in as them, and every row they hold is yours. So an
// administrator trusted only to onboard staff could become the one account
// holding grant, in two requests, and the ceiling that exists in one
// fail-closed function was never asked.
func TestAPasswordsAdminCannotResetAnAccountThatOutranksThem(t *testing.T) {
	base, _, _ := adminWire(t)

	// The attacker: passwords and nothing else. The install's reason for
	// splitting the capability in the first place is that this account may
	// onboard staff without being able to become them.
	_, attackerToken := makeAccount(t, base, "resetter@example.com", "the resetter's password",
		account.RoleAdmin, admin.CapPasswords)
	// The target: the authority the attacker does not hold.
	victimID, _ := makeAccount(t, base, "authority@example.com", "the authority's password",
		account.RoleAdmin, admin.CapGrant)

	code, body := call(t, "POST", base+"/api/silo/v1/admin/accounts/"+victimID.String()+"/password",
		attackerToken, `{"password":"a password the attacker chose"}`)
	if code != http.StatusForbidden {
		t.Fatalf("resetting the grant holder's password = %d (%s), want 403", code, body)
	}

	// And the account is still theirs.
	loginToken(t, base, "authority@example.com", "the authority's password")
}

// A reset of somebody holding nothing more than the resetter is the route
// doing its job, and must keep working.
func TestAPasswordsAdminCanStillResetAnOrdinaryAccount(t *testing.T) {
	base, _, _ := adminWire(t)

	_, attackerToken := makeAccount(t, base, "resetter@example.com", "the resetter's password",
		account.RoleAdmin, admin.CapPasswords)
	subjectID, _ := makeAccount(t, base, "subject@example.com", "the subject's password",
		account.RoleUser)

	code, body := call(t, "POST", base+"/api/silo/v1/admin/accounts/"+subjectID.String()+"/password",
		attackerToken, `{"password":"a password the operator chose"}`)
	if code != http.StatusOK {
		t.Fatalf("resetting an ordinary account = %d (%s), want 200", code, body)
	}
	loginToken(t, base, "subject@example.com", "a password the operator chose")
}

// The second route to the same place, and it does not need the password
// capability at all. Mint refuses an address whose account is ACTIVE — so
// deactivate the account first, and the address is free again. Redemption then
// sets a password of the redeemer's choosing on an account that already exists,
// with every capability row it ever held still attached.
//
// An invite is for an address that has no account yet. A deactivated account is
// not that: it is somebody's account with the lights off, and re-inviting the
// address is taking it, not creating it.
func TestAnInviteCannotClaimTheAddressOfAnEnrolledAccount(t *testing.T) {
	base, _, _ := adminWire(t)

	victimID, _ := makeAccount(t, base, "authority@example.com", "the authority's password",
		account.RoleAdmin, admin.CapGrant)
	// Deactivating is an ordinary users-capability action, so this costs the
	// attacker nothing they were not already trusted with.
	if err := account.SetActive(adminCtx(t), victimID, false); err != nil {
		t.Fatal(err)
	}

	attackerID, _ := makeAccount(t, base, "resetter@example.com", "the resetter's password",
		account.RoleAdmin, admin.CapUsers)

	_, _, err := invite.Mint(adminCtx(t), invite.Options{
		Email: "authority@example.com",
		Role:  account.RoleAdmin,
		By:    attackerID,
	})
	if err == nil {
		t.Fatal("an invite was minted for the address of an account that has a password")
	}
	t.Logf("refused: %v", err)
}
