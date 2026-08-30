package account

// Roles: what an account is allowed to be, as opposed to what it is allowed to
// reach.
//
// The distinction is the whole reason this is not part of the grant model. A
// grant answers "may this principal do op at (library, path)" and is a fact
// about a relationship; a role answers "what kind of account is this" and is a
// fact about the account itself. Creating a library has no library to ask a
// grant about, which is exactly the shape of question that belongs here.
//
// docs/plans/sharing.md § Accounts is the owning document.

import "fmt"

// Role is one of three, and the set is closed.
type Role string

const (
	// RoleAdmin curates the install: it mints invites, and no install setting
	// takes any capability away from it.
	RoleAdmin Role = "admin"
	// RoleUser is an ordinary account. What it may do beyond reading what it
	// has been granted is the install's decision.
	RoleUser Role = "user"
	// RoleGuest sees only what has been shared to it and creates nothing. The
	// consumer-deployment shape, where an admin curates the libraries and
	// everybody else syncs what they have been given.
	RoleGuest Role = "guest"
)

// DefaultRole is what an account is when nobody said otherwise, and what the
// column defaults to.
const DefaultRole = RoleUser

// ParseRole turns stored or typed text into a role, and refuses anything else.
//
// Strict, including about case and whitespace, because the failure of a
// permissive parser here is silent in both directions. An unrecognised role
// that fell through to a zero value would be an account matching no rule --
// refused everything, or allowed everything, depending on which way the rule
// that reads it happens to be written -- and nothing would say which.
//
// The empty string is an error rather than a default. A caller with nothing to
// say should name DefaultRole, so that "no role given" is a decision at the
// call site and not a silent one here.
func ParseRole(s string) (Role, error) {
	switch Role(s) {
	case RoleAdmin, RoleUser, RoleGuest:
		return Role(s), nil
	}
	return "", fmt.Errorf("unknown role %q: one of %s, %s, %s", s, RoleAdmin, RoleUser, RoleGuest)
}

// MayCreateLibrary answers whether this role can make a library on an install
// where usersMayCreate says whether ordinary users are permitted to.
//
// One function rather than a condition at the call site, because there is more
// than one call site -- the HTTP route and, when it lands, the invite that
// decides what a redeemed account may do -- and two spellings of this rule
// would differ on the case nobody tested.
//
// An admin is not subject to the setting. An install whose administrator
// cannot create a library has locked itself out of the thing the setting
// exists to curate, and a guest is not subject to it either: creating nothing
// is what the role means, and a per-install knob that turned guests into
// creators would leave the word meaning nothing.
func (r Role) MayCreateLibrary(usersMayCreate bool) bool {
	switch r {
	case RoleAdmin:
		return true
	case RoleUser:
		return usersMayCreate
	default:
		return false
	}
}

// IsAdmin is the question the admin routes ask.
func (r Role) IsAdmin() bool { return r == RoleAdmin }
