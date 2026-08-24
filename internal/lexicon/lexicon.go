// Package lexicon holds the one word this project had two of, and the guard
// that keeps it to one.
//
// Silo calls the thing an account owns a library. It called it a repo for as
// long as it was Seafile-derived, and for a while it called it both: the prose
// in fileserver/api/api.go said "library" in every sentence while the struct
// beside the sentence said Repo and the route under it said /repos. That is
// the same drift the Seafile deletion was about, and it comes back the moment
// somebody adds a handler by copying the one above it.
//
// So the word is pinned by a test rather than by intention. A guard like this
// earns its keep because what it protects is not a behaviour — nothing breaks
// when a variable is called repoID, which is exactly why nothing else catches
// it.
//
// This package is deliberately the only place in the tree allowed to say the
// old word, which is why the guard skips its own directory.
package lexicon

import "strings"

// foreign are another product's names, which this project quotes and must not
// rewrite. Seafile called the thing a repo and shipped that word in a header
// and a URL lane; a sweep that renamed those would leave the documentation
// describing a header that never existed.
//
// Written as tokens rather than as a list of exempt files, because a file-wide
// exemption pardons the next real slip that lands in the same file. The Seafile
// lanes themselves were deleted from the server; what remains is documentation
// of what a Seafile-family client sends, which is still true of those clients.
var foreign = []string{
	"Seafile-Repo-Token",
	"/api2/repos",
	"/api/v2.1/repos",
	"/seafhttp/repo",
	"repo-tokens",
}

// english are the words that begin with the same four letters and are not the
// noun being retired, given as what follows "repo".
//
// A list this short is only possible because it was measured rather than
// imagined: these three stems are every non-library word in the tree that
// starts with those letters. The alternative rule — "repo not followed by a
// lowercase letter" — reads better and is wrong, because it silently pardons
// repomgr and the {repoid} route variable, which are the two most important
// things a guard like this has to catch.
var english = []string{
	"rt",     // report, reports, reported, reporting, reportable
	"sitor",  // repository, repositories
	"pulate", // repopulate
}

// SaysOldWord reports whether s contains the retired noun, in any case, in any of
// the shapes it takes: Repo, repos, repoID, repo_id, repomgr, /repos,
// {repoid}, RepoOwner.
func SaysOldWord(s string) bool {
	low := strings.ToLower(stripForeign(s))
	for i := 0; i+4 <= len(low); i++ {
		if low[i:i+4] != "repo" {
			continue
		}
		rest := low[i+4:]
		if !startsWithAny(rest, english) {
			return true
		}
	}
	return false
}

func startsWithAny(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// stripForeign removes the other product's names before the scan.
//
// "/repo" is Seafile's sync lane and comes out; "/repos" was ours and stays
// in, because that is the spelling this whole sweep is about and a rule that
// pardoned it would pardon the thing it exists to find. The placeholder is
// what keeps the shorter token from eating the longer one.
func stripForeign(s string) string {
	const keep = "\x01"
	for _, f := range foreign {
		s = strings.ReplaceAll(s, f, "")
	}
	s = strings.ReplaceAll(s, "/repos", keep)
	s = strings.ReplaceAll(s, "/Repos", keep)
	s = strings.ReplaceAll(s, "/repo", "")
	s = strings.ReplaceAll(s, "/Repo", "")
	return strings.ReplaceAll(s, keep, "/repos")
}

// RetiredNoun and RetiredSegment are the old word itself, published so that the
// tests asserting it is gone can say what they are asserting without saying it.
//
// Without these, a test that hits "/api/silo/v1/repos" to prove the route 404s
// is a test the guard has to flag — and the sweep that introduced the guard
// rewrote exactly those assertions into nonsense, each one checking that the
// new spelling was absent from the server that serves it. Naming the old word
// once, here, in the only package allowed to hold it, is what keeps the two
// from fighting.
const (
	RetiredNoun    = "repo"
	RetiredSegment = "repos"
)
