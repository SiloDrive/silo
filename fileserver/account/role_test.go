package account

import "testing"

// A role is a closed set, and the parser is where that is enforced.
//
// The alternative is a TEXT column holding whatever a caller wrote, which
// fails in the direction that matters: an unrecognised role read back through
// a permission check is not an error, it is a principal that matches no rule
// and is quietly refused everything -- or, written the other way round,
// quietly allowed it.
func TestAnUnknownRoleIsRefusedRatherThanRead(t *testing.T) {
	for _, s := range []string{"", "wheel", "Admin", "administrator", "root", "user "} {
		if r, err := ParseRole(s); err == nil {
			t.Errorf("ParseRole(%q) = %q, want an error", s, r)
		}
	}
	for _, want := range []Role{RoleAdmin, RoleUser, RoleGuest} {
		got, err := ParseRole(string(want))
		if err != nil || got != want {
			t.Errorf("ParseRole(%q) = %q, %v", want, got, err)
		}
	}
}

// Who may create a library is one rule with two inputs, stated once.
//
// A guest never can: that is what the role means, and no install setting
// reaches it. A user may unless the install reserves creation for its admin,
// which is the curated-deployment shape. An admin always may -- an install
// whose own administrator cannot make a library has locked itself out of the
// thing the setting exists to control.
func TestWhoMayCreateALibrary(t *testing.T) {
	for _, c := range []struct {
		role           Role
		usersMayCreate bool
		want           bool
	}{
		{RoleAdmin, true, true},
		{RoleAdmin, false, true},
		{RoleUser, true, true},
		{RoleUser, false, false},
		{RoleGuest, true, false},
		{RoleGuest, false, false},
	} {
		if got := c.role.MayCreateLibrary(c.usersMayCreate); got != c.want {
			t.Errorf("%s.MayCreateLibrary(%v) = %v, want %v",
				c.role, c.usersMayCreate, got, c.want)
		}
	}
}
