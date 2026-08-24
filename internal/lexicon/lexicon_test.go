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
	"docs/README.md":             "states the rule, which means naming the word it retires",
	"docs/upgrading-to-0.5.0.md": "tells a client author what to grep for, which means writing it out",
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

// TestTheSweepDidNotEatACapitalS guards the defect the rename actually shipped.
//
// The sweep matched the old word with an optional trailing "s" under a
// case-insensitive flag, so "repo" followed by a capital S had the S eaten as
// if it were the plural: repoSelect became librarieselect, RepoStatus became
// Librariestatus, RepoSharePerm became LibrariesharePerm. Twenty-two
// identifiers, all of which compiled, none of which any test could notice —
// they are names, and a name means nothing to a compiler.
//
// "libraries" followed by a lowercase letter is the signature. It cannot occur
// in English, where the word ends and a space follows, and it cannot occur in
// an identifier, where the next component starts with a capital. So every
// match is a casualty.
func TestTheSweepDidNotEatACapitalS(t *testing.T) {
	var bad []string
	walk(t, repoRoot(t), func(rel string, body []byte) {
		for i, line := range strings.Split(string(body), "\n") {
			for _, w := range mangled(line) {
				bad = append(bad, rel+":"+itoa(i+1)+": "+w)
			}
		}
	})
	if len(bad) > 0 {
		t.Errorf("the sweep ate a capital S in %d place(s):\n  %s",
			len(bad), strings.Join(dedupe(bad), "\n  "))
	}
}

// mangled returns the "libraries"+lowercase runs on one line.
func mangled(line string) []string {
	var out []string
	low := strings.ToLower(line)
	const w = "libraries"
	for i := 0; i+len(w) < len(low); i++ {
		if low[i:i+len(w)] != w {
			continue
		}
		// The original case, not the lowered copy. LibrariesHandler is a
		// perfectly good name and lowercases to something indistinguishable
		// from a casualty — the first draft of this test flagged sixty-two of
		// them.
		c := line[i+len(w)]
		if c < 'a' || c > 'z' {
			continue
		}
		end := i + len(w)
		for end < len(line) && line[end] >= 'a' && line[end] <= 'z' {
			end++
		}
		out = append(out, line[i:end])
	}
	return out
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
