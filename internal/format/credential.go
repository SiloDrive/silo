// Package format holds the rendering shared by the two places a credential
// list is printed.
//
// There are two, and they are not the same command: `silo token list <email>`
// is an operator reading the database on the host, and `silo credential list`
// is a person asking the server about themselves over HTTP. They print the
// same four facts about a row -- when it was made, whether it has lapsed,
// whether anybody still uses it -- and a second copy of that arithmetic would
// drift. The wording is the contract here, not the code: an operator and a
// person comparing notes should be reading the same words.
package format

import (
	"fmt"
	"time"
)

// Expiry says what a credential's expiry means right now, rather than printing
// an instant and leaving the reader to compare it with the clock.
//
// A lapsed credential is shouted because it is the row an operator is usually
// looking for, and a credential with no expiry says so rather than showing the
// zero that stands for it in the column.
func Expiry(expiresAt, now int64) string {
	switch {
	case expiresAt == 0:
		return "no expiry"
	case expiresAt <= now:
		return "EXPIRED " + Time(expiresAt)
	default:
		return "expires " + Time(expiresAt)
	}
}

// LastUsed distinguishes a credential that has never been presented from one
// presented long ago. They are the two ends of the same question -- is anybody
// still using this? -- and "unknown" would answer neither.
func LastUsed(sec int64) string {
	if sec == 0 {
		return "never"
	}
	return Time(sec)
}

// Time renders a stored second. Zero is not an instant here -- it is the
// column's way of saying nothing was recorded -- so it is named rather than
// rendered as 1970.
func Time(sec int64) string {
	if sec == 0 {
		return "unknown"
	}
	return time.Unix(sec, 0).Format(time.RFC3339)
}

// Blanks is the indent that lines up a continuation line under an id whose
// width is not known until it is printed.
func Blanks(n int) string {
	return fmt.Sprintf("%*s", n, "")
}
