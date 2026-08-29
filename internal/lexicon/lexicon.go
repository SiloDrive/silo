// Package lexicon holds the one word this project had two of, so that the
// tests asserting it is gone can name it without saying it.
//
// Silo calls the thing an account owns a library. It called it a repo for as
// long as it carried its upstream's vocabulary, and for a while it called it
// both: the prose in fileserver/api/api.go said "library" in every sentence
// while the struct beside the sentence said Repo and the route under it said
// /repos.
//
// There used to be a guard here that scanned the whole tree for the old word,
// and it did its job: the rename it was written for is finished and the word
// is not coming back. What it turned into afterwards was a tax. Every hit was
// a false one — "not from this repo" in the git sense, a bug report naming the
// route by the spelling it had when it broke — and each cost an allowlist
// entry arguing why a true sentence was allowed to stay true. A guard whose
// every remaining failure is a false positive is one people learn to route
// around, and that habit outlives the guard.
//
// What is left is narrow and permanent: the wire and the CLI still assert that
// the old spellings answer 404 and that the old verbs are refused, and those
// assertions need the old word spelled somewhere. Here, once.
package lexicon

import "strings"

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
//
// It used to strip a set of another product's route and header names first,
// because those quoted the old word without this project saying it. Those
// lanes were deleted in 5d4baa0 and nothing quotes them any more, so the
// exemption went with them: every remaining hit is this project's own.
func SaysOldWord(s string) bool {
	low := strings.ToLower(s)
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

// RetiredNoun and RetiredSegment are the old word itself, published so that the
// tests asserting it is gone can say what they are asserting without saying it.
//
// They outlived the tree-wide guard on purpose. A test that hits
// "/api/silo/v1/repos" to prove the route 404s has to write the route out, and
// building it from these is what stopped the original sweep rewriting those
// assertions into nonsense — each one checking that the new spelling was
// absent from the server that serves it.
const (
	RetiredNoun    = "repo"
	RetiredSegment = "repos"
)
