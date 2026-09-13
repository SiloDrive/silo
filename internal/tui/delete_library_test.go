package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/dkam/silo/client"
)

// fakeLibrary is a server over a fixed tree that also remembers what was asked
// of it: which directories were listed, and which libraries were deleted. The
// deletions are the point — a confirmation that is too easy to give shows up
// here as a DELETE nobody meant to send.
type fakeLibrary struct {
	tree map[string][]client.DirEntry

	mu      sync.Mutex
	listed  []string
	deleted []string
}

func (f *fakeLibrary) listings() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.listed...)
}

func (f *fakeLibrary) deletions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

func fakeLibraryServer(t *testing.T, tree map[string][]client.DirEntry) (*client.APIClient, *fakeLibrary) {
	t.Helper()
	f := &fakeLibrary{tree: tree}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method == http.MethodDelete {
			f.mu.Lock()
			f.deleted = append(f.deleted, path.Base(r.URL.Path))
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}

		_, rest, isListing := strings.Cut(r.URL.Path, "/entries")
		if !isListing {
			_, _ = w.Write([]byte(`[{"id":"library","name":"Photos"}]`))
			return
		}

		dir := path.Clean("/" + strings.Trim(rest, "/"))
		f.mu.Lock()
		f.listed = append(f.listed, dir)
		f.mu.Unlock()

		entries := f.tree[dir]
		if entries == nil {
			entries = []client.DirEntry{}
		}
		_ = json.NewEncoder(w).Encode(entries)
	}))
	t.Cleanup(srv.Close)
	return client.NewClient(srv.URL), f
}

func file(name string) client.DirEntry { return client.DirEntry{Name: name, Type: "file"} }
func dir(name string) client.DirEntry  { return client.DirEntry{Name: name, Type: "dir"} }

// run executes a command the way the Bubble Tea loop would — unpacking a
// batch — and feeds each message back into the model. The commands those
// messages produce in turn are not run: one of them is the cursor blink, and
// following it would tick forever.
func run(t *testing.T, m model, cmd tea.Cmd) model {
	t.Helper()
	if cmd == nil {
		return m
	}
	switch msg := cmd().(type) {
	case nil:
		return m
	case tea.BatchMsg:
		for _, c := range msg {
			m = run(t, m, c)
		}
		return m
	default:
		next, _ := m.Update(msg)
		updated, ok := next.(model)
		if !ok {
			t.Fatalf("Update returned %T, want tui.model", next)
		}
		return updated
	}
}

// confirmingDelete is the library list with one library on it, with the delete
// confirmation open over it and whatever it went to find already back.
func confirmingDelete(t *testing.T, tree map[string][]client.DirEntry) (model, *fakeLibrary) {
	t.Helper()
	api, fake := fakeLibraryServer(t, tree)
	// initialModel rather than a hand-rolled struct, so that a field this
	// screen needs configured is configured here the way it ships.
	m := initialModel("http://localhost:8082", "", "")
	m.api = api
	m.view = viewLibraries
	m.libraries = []client.Library{{ID: "library", Name: "Photos"}}
	m.width, m.height = 80, 24

	next, cmd := m.Update(key("d"))
	updated, ok := next.(model)
	if !ok {
		t.Fatalf("Update returned %T, want tui.model", next)
	}
	if updated.view != viewConfirm {
		t.Fatalf("after 'd': view %q, want %q", updated.view, viewConfirm)
	}
	return run(t, updated, cmd), fake
}

// full is a library with something in it: four files, two of them down a
// directory the top-level listing does not show the size of.
var full = map[string][]client.DirEntry{
	"/":          {dir("2024"), file("cover.jpg"), file("notes.txt")},
	"/2024":      {dir("june"), file("a.jpg")},
	"/2024/june": {file("b.jpg")},
}

// The bug: 'd' then 'y' deletes a library holding four files as readily as an
// empty one, and 'y' is one key away from 'd' under the same finger.
func TestAFullLibraryIsNotDeletedByASingleKeystroke(t *testing.T) {
	m, fake := confirmingDelete(t, full)

	next, cmd := m.Update(key("y"))
	m = next.(model)
	m = run(t, m, cmd)

	if got := fake.deletions(); len(got) > 0 {
		t.Fatalf("'y' alone deleted %v — a library with files in it must take more than one keystroke", got)
	}
	if m.view != viewConfirm {
		t.Errorf("view is %q, want %q: the confirmation should still be up", m.view, viewConfirm)
	}
}

// What does delete it: the library's own name, typed back.
func TestTypingTheNameDeletesAFullLibrary(t *testing.T) {
	m, fake := confirmingDelete(t, full)

	m = press(t, m, strings.Split("Photos", "")...)
	next, cmd := m.Update(key("enter"))
	m = next.(model)
	m = run(t, m, cmd)

	if got := fake.deletions(); len(got) != 1 || got[0] != "library" {
		t.Fatalf("deletions %v, want one for %q", got, "library")
	}
}

// A near miss is not a confirmation, and it says so rather than deleting.
func TestAMistypedNameDeletesNothing(t *testing.T) {
	m, fake := confirmingDelete(t, full)

	m = press(t, m, strings.Split("photos", "")...)
	next, cmd := m.Update(key("enter"))
	m = next.(model)
	m = run(t, m, cmd)

	if got := fake.deletions(); len(got) > 0 {
		t.Fatalf("a mistyped name deleted %v", got)
	}
	if !strings.Contains(m.View(), "Nothing was deleted") {
		t.Errorf("the screen does not say the deletion did not happen:\n%s", m.View())
	}
}

// The warning has to say what is at stake, or it is just another prompt to
// dismiss: the counts come from a walk of the whole library, not of its root.
func TestTheConfirmationSaysWhatTheLibraryHolds(t *testing.T) {
	m, fake := confirmingDelete(t, full)

	view := m.View()
	if !strings.Contains(view, "4 files") {
		t.Errorf("the screen does not name the four files:\n%s", view)
	}
	if !strings.Contains(view, "2 folders") {
		t.Errorf("the screen does not name the two folders:\n%s", view)
	}

	// Every directory, not just the root — the case this exists for is one
	// top-level folder holding everything.
	for _, want := range []string{"/", "/2024", "/2024/june"} {
		if !contains(fake.listings(), want) {
			t.Errorf("%q was never listed; the walk stopped short: %v", want, fake.listings())
		}
	}
}

// An empty library is not worth the ceremony: it stays a y/n.
func TestAnEmptyLibraryIsStillDeletedWithY(t *testing.T) {
	m, fake := confirmingDelete(t, map[string][]client.DirEntry{"/": {}})

	next, cmd := m.Update(key("y"))
	m = next.(model)
	m = run(t, m, cmd)

	if got := fake.deletions(); len(got) != 1 {
		t.Fatalf("deletions %v, want one: an empty library takes a plain y", got)
	}
}

// Escape backs out from either shape of the confirmation.
func TestEscapeLeavesTheLibraryAlone(t *testing.T) {
	m, fake := confirmingDelete(t, full)

	next, cmd := m.Update(key("esc"))
	m = next.(model)
	m = run(t, m, cmd)

	if got := fake.deletions(); len(got) > 0 {
		t.Fatalf("escape deleted %v", got)
	}
	if m.view != viewLibraries {
		t.Errorf("view is %q, want %q", m.view, viewLibraries)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// The walk goes to the bottom of the tree, and reports what it found there.
func TestCountTreeCountsEveryDirectory(t *testing.T) {
	c := countTree(func(at string) ([]client.DirEntry, error) { return full[at], nil })

	if c.err != nil {
		t.Fatalf("walk failed: %v", c.err)
	}
	if c.files != 4 || c.dirs != 2 {
		t.Errorf("counted %d files in %d folders, want 4 in 2", c.files, c.dirs)
	}
	if c.partial {
		t.Error("a tree of three directories was reported as a partial count")
	}
}

// A library big enough to be worth warning about is also big enough that
// counting all of it would keep the screen waiting. The walk stops, and says
// that it stopped rather than reporting its floor as a total.
func TestCountTreeStopsAtItsBudget(t *testing.T) {
	// Deep and wide both: one directory per listing, each holding ten files
	// and the next directory down, so neither cap is reached before the other
	// has had its chance.
	requests := 0
	c := countTree(func(string) ([]client.DirEntry, error) {
		requests++
		entries := []client.DirEntry{dir("next")}
		for i := 0; i < 10; i++ {
			entries = append(entries, file(fmt.Sprintf("f%d", i)))
		}
		return entries, nil
	})

	if !c.partial {
		t.Error("an endless tree was reported as a complete count")
	}
	if requests > countRequestCap {
		t.Errorf("made %d requests, want at most %d", requests, countRequestCap)
	}
	if c.files > countFileCap+10 {
		t.Errorf("counted %d files, want the walk to stop near %d", c.files, countFileCap)
	}
	if !strings.HasPrefix(c.phrase(), "at least ") {
		t.Errorf("phrase is %q, want it to read as a floor", c.phrase())
	}
}

// A listing that fails leaves the contents unknown, which is not the same
// answer as empty.
func TestCountTreeReportsAFailedListing(t *testing.T) {
	c := countTree(func(string) ([]client.DirEntry, error) {
		return nil, errors.New("connection refused")
	})

	if c.err == nil {
		t.Fatal("a failed listing was reported as a successful walk")
	}
	if c.empty() {
		t.Error("a library whose contents could not be read was reported as empty")
	}
}

// The walk takes a round trip per directory, so the confirmation is on screen
// before the answer is. Until it lands, neither confirmation is live: a 'y'
// held over from the last library must not delete this one the moment the
// count says it is empty.
func TestTheConfirmationAcceptsNothingUntilItHasCounted(t *testing.T) {
	api, fake := fakeLibraryServer(t, full)
	m := initialModel("http://localhost:8082", "", "")
	m.api = api
	m.view = viewLibraries
	m.libraries = []client.Library{{ID: "library", Name: "Photos"}}
	m.width, m.height = 80, 24

	// 'd' only: the count command is deliberately left un-run.
	next, _ := m.Update(key("d"))
	m = next.(model)

	for _, k := range []string{"y", "enter"} {
		next, cmd := m.Update(key(k))
		m = next.(model)
		m = run(t, m, cmd)
		if got := fake.deletions(); len(got) > 0 {
			t.Fatalf("%q deleted %v before the count came back", k, got)
		}
	}
	if !strings.Contains(m.View(), "Checking") {
		t.Errorf("the screen does not say it is still checking:\n%s", m.View())
	}
}

// A count that could not be taken asks for the name anyway. An unreachable
// server is a reason for more care, not less.
func TestAnUncountableLibraryStillAsksForTheName(t *testing.T) {
	m, fake := confirmingDelete(t, full)
	failed := libraryContents{err: errors.New("connection refused")}
	m.deleteContents = &failed

	next, cmd := m.Update(key("y"))
	m = next.(model)
	m = run(t, m, cmd)

	if got := fake.deletions(); len(got) > 0 {
		t.Fatalf("'y' deleted %v on a library nobody could count", got)
	}
	if view := m.View(); !strings.Contains(view, "Could not check") {
		t.Errorf("the screen does not say the check failed:\n%s", view)
	}
}
