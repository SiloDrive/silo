package dbutil

import "strings"

// NormalizeEmail is the single spelling rule for an address.
//
// The whole string is lowercased, not just the domain. RFC 5321 says the local
// part is case-sensitive and no mail provider has behaved that way in decades,
// but the argument here is narrower than that: an address is what a human
// types into a login form, and two spellings of one address reaching two
// different accounts is the failure the identity split exists to prevent.
// AccountEmail's primary key is this form, so the rule is enforced by the
// schema rather than by everyone remembering it.
//
// It lives here, and not in the account package, so that it is available to
// anything holding a database handle without pulling the account package in.
// account.Normalize is the name the rest of the server calls it by.
func NormalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
