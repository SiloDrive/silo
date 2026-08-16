package silod

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/fsmgr"
)

func TestCheckEntryName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"report.txt", true},
		{"a directory", true},
		{"日本語.txt", true},
		{".hidden", true},
		{"..hidden", true},
		{"", false},
		{".", false},
		{"..", false},
		{"../../../.ssh/authorized_keys", false},
		{"..\x2f..", false},
		{"sub/dir", false},
		{"/", false},
		{"lead/..", false},
		{strings.Repeat("a", 256), false},
		{"bad\xff\xfeutf8", false},
	}

	for _, c := range cases {
		w := httptest.NewRecorder()
		if got := checkEntryName(w, c.name); got != c.want {
			t.Errorf("checkEntryName(%q) = %v, want %v", c.name, got, c.want)
			continue
		}
		if !c.want && w.Code != 400 {
			t.Errorf("checkEntryName(%q) rejected with status %d, want 400", c.name, w.Code)
		}
	}
}

// addNewEntries is the last chokepoint before a dirent is written into a tree,
// so it must reject a traversal name even if a caller forgets to validate.
func TestAddNewEntriesRejectsInvalidName(t *testing.T) {
	var oldDents []*fsmgr.SeafDirent
	var names []string
	dent := fsmgr.NewDirent(fsmgr.EmptySha1, "../../../.ssh/authorized_keys", 0644, time.Now().Unix(), "", 0)

	err := addNewEntries(nil, "user@example.com", &oldDents, []*fsmgr.SeafDirent{dent}, false, &names)
	if err == nil {
		t.Fatal("addNewEntries accepted a traversal name, want error")
	}
	if len(oldDents) != 0 || len(names) != 0 {
		t.Errorf("addNewEntries mutated the tree on rejection: dents=%v names=%v", oldDents, names)
	}
}

func TestAddNewEntriesAcceptsValidName(t *testing.T) {
	var oldDents []*fsmgr.SeafDirent
	var names []string
	dent := fsmgr.NewDirent(fsmgr.EmptySha1, "report.txt", 0644, time.Now().Unix(), "", 0)

	if err := addNewEntries(nil, "user@example.com", &oldDents, []*fsmgr.SeafDirent{dent}, false, &names); err != nil {
		t.Fatalf("addNewEntries rejected a valid name: %v", err)
	}
	if len(oldDents) != 1 || oldDents[0].Name != "report.txt" {
		t.Errorf("addNewEntries did not add the entry: %v", oldDents)
	}
}
