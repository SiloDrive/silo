package silod

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/dkam/silo/fileserver/fsmgr"
	"github.com/dkam/silo/fileserver/repomgr"
)

// commitsWithoutGCCheck lists the GenNewCommit call sites allowed to pass
// checkGC false, by the function containing them. Both write no fs object of
// their own — they commit a root they were handed — so there is nothing a
// collector running alongside them could reclaim.
//
// Everything else must opt in. That is the whole point of this test: the gap
// closed in mkdirWithParents, updateDir, mkdirHandler, deleteFileHandler and
// moveOrCopy was not a wrong value, it was a default nobody revisited when
// each new handler was written. A handler added next year will trip this
// rather than quietly rejoin the bug.
var commitsWithoutGCCheck = map[string]string{
	"renameRepo": "commits head.RootID unchanged; a library rename writes no object",
	"updateDir":  "the dirPath == \"/\" branch commits the root it was given, which the client uploaded before the call",
}

// TestEveryCommitEitherChecksGCOrSaysWhyNot reads the package's own source.
// A GC that starts between building a tree and committing it can reclaim the
// dirents the commit is about to name, which leaves a live commit pointing at
// objects that are gone -- a library that reports fine and reads broken.
func TestEveryCommitEitherChecksGCOrSaysWhyNot(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("failed to read the package directory: %v", err)
	}

	fset := token.NewFileSet()
	var unchecked []string
	sites := 0

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("failed to parse %s: %v", name, err)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				ident, ok := call.Fun.(*ast.Ident)
				if !ok || ident.Name != "GenNewCommit" {
					return true
				}
				sites++

				// checkGC is the last parameter. A literal is the only form
				// this test can judge, and the only form worth writing: a
				// variable here hides the decision from every future reader
				// as effectively as it hides it from this test.
				last, ok := call.Args[len(call.Args)-1].(*ast.Ident)
				if !ok {
					t.Errorf("%s: GenNewCommit in %s passes a non-literal checkGC; write true or false so the choice is readable",
						fset.Position(call.Pos()), fn.Name.Name)
					return true
				}
				switch last.Name {
				case "true":
					return true
				case "false":
					if _, allowed := commitsWithoutGCCheck[fn.Name.Name]; !allowed {
						unchecked = append(unchecked,
							fset.Position(call.Pos()).String()+" in "+fn.Name.Name)
					}
					return true
				default:
					t.Errorf("%s: GenNewCommit in %s passes %q as checkGC", fset.Position(call.Pos()), fn.Name.Name, last.Name)
					return true
				}
			})
		}
	}

	if sites == 0 {
		t.Fatal("found no GenNewCommit call sites, so this test proves nothing -- did the function get renamed?")
	}
	if len(unchecked) > 0 {
		sort.Strings(unchecked)
		t.Errorf("these commits skip the gc check:\n\t%s\n\nRead repomgr.GetCurrentGCID before building the tree and pass it with checkGC true. "+
			"If the commit genuinely writes no new object, add the function to commitsWithoutGCCheck with the reason.",
			strings.Join(unchecked, "\n\t"))
	}
}

// TestCommitThatRacedGCIsRejected is the other half: the call sites above opt
// into a check that has to actually reject. Without it the opting-in is
// decoration.
func TestCommitThatRacedGCIsRejected(t *testing.T) {
	head := contendedRepoTestDB(t)
	repo := repomgr.Get(contendedRepo)
	if repo == nil {
		t.Fatal("failed to load the seeded repo")
	}

	dbExec(t, "INSERT INTO GCID (repo_id, gc_id) VALUES (?, ?)", contendedRepo, "gc-before")

	newRoot, err := fsmgr.NewSeafdir(1, nil)
	if err != nil {
		t.Fatalf("failed to build a root: %v", err)
	}
	if err := fsmgr.SaveSeafdir(contendedRepo, newRoot); err != nil {
		t.Fatalf("failed to save a root: %v", err)
	}

	// A collector ran after the handler read the gc id: what it holds no
	// longer describes the store it is about to commit against.
	if _, err := GenNewCommit(repo, head, newRoot.DirID, repoOwner, "Raced a gc", false, "gc-stale", true); !errors.Is(err, ErrGCConflict) {
		t.Fatalf("committing with a stale gc id returned %v, want ErrGCConflict", err)
	}

	// The same commit, with the id the store actually has, goes through --
	// so the rejection above is the gc check and not the commit path failing
	// for some unrelated reason.
	if _, err := GenNewCommit(repo, head, newRoot.DirID, repoOwner, "Did not race a gc", false, "gc-before", true); err != nil {
		t.Fatalf("committing with the current gc id failed: %v", err)
	}
}
