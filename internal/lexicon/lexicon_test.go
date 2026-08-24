package lexicon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// historical are the files that say the old word on purpose, because they
// record something that happened rather than something that is.
//
// Kept deliberately short. Every entry is a place the guard cannot help, so
// each one has to earn itself: a bug report describing a route by the name it
// had when it broke is history, and rewriting it would make it a report of a
// bug that was never filed.
var historical = map[string]string{
	"docs/README.md": "states the rule, which means naming the word it retires",
}

func TestNoGoSourceSaysRepo(t *testing.T) {
	offenders := scan(t, ".go")
	if len(offenders) > 0 {
		t.Errorf("the Go source still says repo in %d place(s):\n%s",
			count(offenders), render(offenders))
	}
}

func TestNoDocSaysRepo(t *testing.T) {
	offenders := scan(t, ".md")
	if len(offenders) > 0 {
		t.Errorf("the docs still say repo in %d place(s):\n%s",
			count(offenders), render(offenders))
	}
}

// TestNoFilenameSaysRepo covers the half a content scan cannot see. A package
// directory named repomgr and a bug report named repos-returns-null.md are
// both the word, and both survive a sweep that only edits file bodies.
func TestNoFilenameSaysRepo(t *testing.T) {
	root := repoRoot(t)
	var bad []string
	walk(t, root, func(rel string, _ []byte) {
		if SaysOldWord(filepath.Base(rel)) {
			bad = append(bad, rel)
		}
		if dir := filepath.Dir(rel); SaysOldWord(dir) {
			bad = append(bad, dir+string(filepath.Separator))
		}
	})
	if len(bad) > 0 {
		t.Errorf("these paths still say repo:\n  %s", strings.Join(dedupe(bad), "\n  "))
	}
}

// hit is one line that still says the old word.
type hit struct {
	line int
	text string
}

func scan(t *testing.T, ext string) map[string][]hit {
	t.Helper()
	offenders := map[string][]hit{}
	walk(t, repoRoot(t), func(rel string, body []byte) {
		if filepath.Ext(rel) != ext {
			return
		}
		if _, ok := historical[filepath.ToSlash(rel)]; ok {
			return
		}
		for i, line := range strings.Split(string(body), "\n") {
			if SaysOldWord(line) {
				offenders[rel] = append(offenders[rel], hit{i + 1, strings.TrimSpace(line)})
			}
		}
	})
	return offenders
}

// walk visits every file in the tree that a human wrote. Skipping .git is
// obvious; skipping this package is not, and matters — this is where the old
// word is deliberately written down, so a guard that scanned itself could
// never pass.
func walk(t *testing.T, root string, visit func(rel string, body []byte)) {
	t.Helper()
	self := filepath.Join("internal", "lexicon")
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor":
				return filepath.SkipDir
			}
			if rel == self {
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(rel) {
		case ".go", ".md":
		default:
			return nil
		}
		body, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		visit(rel, body)
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
}

// repoRoot climbs to the directory holding go.mod, so the guard runs the same
// from `go test ./...` at the root as from this package's own directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}

func count(offenders map[string][]hit) int {
	n := 0
	for _, hits := range offenders {
		n += len(hits)
	}
	return n
}

// render shows a few lines per file and says how many it did not show. A guard
// over three thousand identifiers has to fail readably or it gets skipped.
func render(offenders map[string][]hit) string {
	const perFile = 3
	files := make([]string, 0, len(offenders))
	for f := range offenders {
		files = append(files, f)
	}
	sortStrings(files)

	var b strings.Builder
	for _, f := range files {
		hits := offenders[f]
		for i, h := range hits {
			if i == perFile {
				b.WriteString("  " + f + ": … and " + itoa(len(hits)-perFile) + " more\n")
				break
			}
			b.WriteString("  " + f + ":" + itoa(h.line) + ": " + trim(h.text, 90) + "\n")
		}
	}
	return b.String()
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func dedupe(s []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sortStrings(out)
	return out
}
