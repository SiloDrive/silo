// Package doclint holds the rules docs/ is checked against.
//
// A document that cites code by line number is wrong within a week: the
// line moves and the citation does not, and a reader who follows it lands on
// something unrelated and trusts it less next time. A symbol name survives
// every edit that does not delete the thing it names, and when it is deleted
// the name fails to grep, which is the right way for a citation to die. The
// review that produced this rule found forty-five line-number citations in
// docs/ and nearly all of them dead.
package doclint

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// lineRef matches a Go source citation with a line number: fileop.go:1991,
// `api/api.go:439`, schema.go:255. It does not match a bare file name, which
// is the form the rule asks for instead.
var lineRef = regexp.MustCompile(`[A-Za-z0-9_/.-]+\.go:\d+`)

func TestDocsCiteSymbolsNotLineNumbers(t *testing.T) {
	root := filepath.Join("..", "..", "docs")
	var hits []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for n := 1; sc.Scan(); n++ {
			if m := lineRef.FindString(sc.Text()); m != "" {
				rel, _ := filepath.Rel(root, p)
				hits = append(hits, rel+":"+itoa(n)+"  "+m)
			}
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		t.Error(h)
	}
	if len(hits) > 0 {
		t.Errorf("%d line-number citations; name the symbol instead", len(hits))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
