// Package credential owns the single credential store described in
// docs/auth.md: one row for every secret a client presents to Silo, and one
// function that verifies any of them.
//
// This file is the scope encoding: the narrowing half of a credential, and the
// test for whether a scope reaches a given path.
package credential

import (
	"fmt"
	"path"
	"strings"
)

// Scope is the narrowing half of a credential. docs/auth.md's table comment
// describes it as "NULL = all libraries; else a library id"; a library id alone
// cannot express "this credential may read one folder", which is the case a
// scoped mount actually wants, so the encoding here is a superset:
//
//	""                  every library
//	"<library-id>"         one library, entirely
//	"<library-id>:<path>"  one library, at <path> and below
//
// Every scope auth.md writes still means exactly what it says there.
//
// The separator needs no escaping: the split takes the first colon, so a library
// id cannot contain one by construction, and a path may contain as many as it
// likes. ParseScope rejects only the shapes a library id can never have, rather
// than requiring a UUID, so a caller may hold a scope for a library this
// server has never heard of.
type Scope struct {
	// LibraryID is empty for a credential that reaches every library.
	LibraryID string

	// Path is empty for a credential that reaches a whole library. Otherwise
	// it is cleaned and rooted ("/photos/2024"), naming a directory that the
	// credential covers along with everything beneath it.
	//
	// A path without a LibraryID names nothing: the encoding cannot express it
	// and String drops it. Build a Scope through ParseScope and the case
	// cannot arise.
	Path string
}

// ParseScope reads the stored column value. An empty string is the unscoped
// credential, which is a valid and common row rather than an error.
//
// Parsing normalises: a caller may pass an unrooted or untidy path and get the
// canonical form back. Storage writes Scope.String(), so what lands in the
// column is canonical regardless of who built it — the same clamp-on-write,
// canonical-on-emit discipline the object format uses for timestamps.
func ParseScope(s string) (Scope, error) {
	if s == "" {
		return Scope{}, nil
	}

	libraryID, rest, hasPath := strings.Cut(s, ":")
	if libraryID == "" {
		return Scope{}, fmt.Errorf("scope %q: empty library id", s)
	}
	if strings.ContainsAny(libraryID, "/ \t") {
		return Scope{}, fmt.Errorf("scope %q: library id contains a separator", s)
	}
	if !hasPath {
		return Scope{LibraryID: libraryID}, nil
	}

	// "library:" is a typo. It cannot be a deliberate spelling of either form
	// above, both of which are available without the trailing colon, so
	// refusing it costs a caller nothing and tells them they built the string
	// wrong. Note this is a rule about the empty string, not about the
	// library root: "library:/" is a path that cleans to the root, and is
	// normalised below rather than refused.
	if rest == "" {
		return Scope{}, fmt.Errorf("scope %q: colon with no path", s)
	}

	p := cleanPath(rest)
	if p == "" {
		// The path cleaned down to the library root, which is the whole
		// library — the form without a colon. Normalise rather than keep two
		// encodings of one fact.
		return Scope{LibraryID: libraryID}, nil
	}
	return Scope{LibraryID: libraryID, Path: p}, nil
}

// String returns the canonical stored form.
func (s Scope) String() string {
	switch {
	case s.LibraryID == "":
		return ""
	case s.Path == "":
		return s.LibraryID
	default:
		return s.LibraryID + ":" + s.Path
	}
}

// Covers reports whether the scope permits reaching p inside libraryID.
//
// p is the path as the request names it. In an end-to-end encrypted library
// that is ciphertext, and so is a stored path scope — the server compares
// bytes either way, exactly as it already routes entries/{path} on names it
// cannot read. That works only because a directory's name salt is generated
// once and carried forward (docs/storage.md), which is what keeps a
// path's ciphertext stable across writes; a fresh salt per write would expire
// every path-scoped credential on the next commit.
//
// Two things still move a path out from under a scope, and both are quiet.
// Renaming an ancestor leaves every segment ciphertext untouched — storage.md
// is explicit that renaming an ancestor re-encrypts nothing — but the path
// those segments spell is a different one, exactly as a plaintext path scope
// stops following a renamed folder. Directory merge is the other way round: it
// re-encrypts the loser's names, so an unchanged path acquires new ciphertext.
// Either way the scope goes on matching nothing rather than failing loudly, so
// a credential can outlive the thing it was cut to reach.
func (s Scope) Covers(libraryID, p string) bool {
	if s.LibraryID == "" {
		return true
	}
	if libraryID != s.LibraryID {
		return false
	}
	if s.Path == "" {
		return true
	}

	// No case folding and no Unicode normalisation, per encryption.md's
	// guardrail: the server must not derive behaviour from the shape of a
	// name. Comparison is bytewise.
	c := cleanPath(p)
	if c == s.Path {
		return true
	}
	// The separator matters: /photos covers /photos/2024 but not /photos-old.
	return strings.HasPrefix(c, s.Path+"/")
}

// cleanPath returns a rooted, tidy path, or "" for the library root.
//
// path.Clean on a rooted path cannot escape above it, so a scope containing
// ".." collapses to something inside the library rather than reaching out of
// one.
func cleanPath(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	p = path.Clean(p)
	if p == "/" {
		return ""
	}
	return p
}
