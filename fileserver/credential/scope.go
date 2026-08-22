// Package credential owns the single credential store described in
// docs/auth.md: one row for every secret a client presents to Silo, and one
// function that verifies any of them.
//
// This file is only the scope encoding. It lands before Resolve because the
// encoding is the part that cannot be changed later: once credentials exist in
// the field, widening the column means rewriting rows on somebody else's
// server.
package credential

import (
	"fmt"
	"path"
	"strings"
)

// Scope is the narrowing half of a credential. docs/auth.md's table comment
// describes it as "NULL = all libraries; else a repo id"; a repo id alone
// cannot express "this credential may read one folder", which is the case a
// scoped mount actually wants, so the encoding here is a superset:
//
//	""                  every library
//	"<repo-id>"         one library, entirely
//	"<repo-id>:<path>"  one library, at <path> and below
//
// Every scope auth.md writes still means exactly what it says there.
//
// A repo id is a UUID and never contains a colon, so the separator is
// unambiguous and no escaping is needed.
type Scope struct {
	// RepoID is empty for a credential that reaches every library.
	RepoID string

	// Path is empty for a credential that reaches a whole library. Otherwise
	// it is cleaned and rooted ("/photos/2024"), naming a directory that the
	// credential covers along with everything beneath it.
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

	repoID, rest, hasPath := strings.Cut(s, ":")
	if repoID == "" {
		return Scope{}, fmt.Errorf("scope %q: empty repo id", s)
	}
	if strings.ContainsAny(repoID, "/ \t") {
		return Scope{}, fmt.Errorf("scope %q: repo id contains a separator", s)
	}
	if !hasPath {
		return Scope{RepoID: repoID}, nil
	}

	// "repo:" is a typo for one of the two forms above and there is no way to
	// tell which was meant. Refusing it costs a caller nothing and stops a
	// truncated string from silently widening a credential to a whole library.
	if rest == "" {
		return Scope{}, fmt.Errorf("scope %q: colon with no path", s)
	}

	p := cleanPath(rest)
	if p == "" {
		// The path cleaned down to the library root, which is the whole
		// library — the form without a colon. Normalise rather than keep two
		// encodings of one fact.
		return Scope{RepoID: repoID}, nil
	}
	return Scope{RepoID: repoID, Path: p}, nil
}

// String returns the canonical stored form.
func (s Scope) String() string {
	switch {
	case s.RepoID == "":
		return ""
	case s.Path == "":
		return s.RepoID
	default:
		return s.RepoID + ":" + s.Path
	}
}

// Covers reports whether the scope permits reaching p inside repoID.
//
// p is the path as the request names it. In an end-to-end encrypted library
// that is ciphertext, and so is a stored path scope — the server compares
// bytes either way, exactly as it already routes entries/{path} on names it
// cannot read. That works only because a directory's name salt is generated
// once and carried forward (docs/plans/store-v2.md), which is what keeps a
// path's ciphertext stable across writes; a fresh salt per write would expire
// every path-scoped credential on the next commit.
//
// The consequence for operators: a path scope in an E2EE library is
// unreadable in the credential list, so the row's label is the only legible
// record of what it reaches. auth.md already makes label the thing that turns
// revocation from a guess into a decision; this is a second reason.
func (s Scope) Covers(repoID, p string) bool {
	if s.RepoID == "" {
		return true
	}
	if repoID != s.RepoID {
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
