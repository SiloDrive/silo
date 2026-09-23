package tui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SiloDrive/silo/client"
)

// notifyServer answers the two listings a refresh can ask for, so a reload
// driven by an event has something to come back with.
func notifyServer(t *testing.T) *client.APIClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/entries"):
			_, _ = w.Write([]byte(`[{"name":"only.txt","type":"file"}]`))
		default:
			_, _ = w.Write([]byte(`[{"id":"library","name":"Library"},{"id":"other","name":"Other"}]`))
		}
	}))
	t.Cleanup(srv.Close)
	return client.NewClient(srv.URL)
}

// The reported bug: a file deleted in one TUI left the other showing it.
func TestBrowseReloadsOnUpdateToTheBrowsedLibrary(t *testing.T) {
	m := browseModel(3, 80, 24)
	m.api = notifyServer(t)

	_, cmd := m.Update(libraryUpdatedMsg{LibraryUpdate: client.LibraryUpdate{LibraryID: "library"}})
	if cmd == nil {
		t.Fatal("no command: an update to the browsed library must reload its listing")
	}
	msg := cmd()
	loaded, ok := msg.(dirLoadedMsg)
	if !ok {
		t.Fatalf("got %T, want dirLoadedMsg", msg)
	}
	if loaded.err != nil {
		t.Fatalf("reload failed: %v", loaded.err)
	}
	if len(loaded.entries) != 1 || loaded.entries[0].Name != "only.txt" {
		t.Errorf("reloaded %+v, want the server's one entry", loaded.entries)
	}
}

// An update about a library that is not on screen is not a reason to re-fetch
// the one that is.
func TestBrowseIgnoresUpdateToAnotherLibrary(t *testing.T) {
	m := browseModel(3, 80, 24)
	m.api = notifyServer(t)

	if _, cmd := m.Update(libraryUpdatedMsg{LibraryUpdate: client.LibraryUpdate{LibraryID: "elsewhere"}}); cmd != nil {
		t.Error("an update to an unwatched library triggered a reload")
	}
}

func TestLibraryListReloadsOnAnyUpdate(t *testing.T) {
	m := browseModel(3, 80, 24)
	m.api = notifyServer(t)
	m.view = viewLibraries

	_, cmd := m.Update(libraryUpdatedMsg{LibraryUpdate: client.LibraryUpdate{LibraryID: "other"}})
	if cmd == nil {
		t.Fatal("no command: the library list must reload when a library moves")
	}
	msg := cmd()
	loaded, ok := msg.(librariesLoadedMsg)
	if !ok {
		t.Fatalf("got %T, want librariesLoadedMsg", msg)
	}
	if len(loaded.libraries) != 2 {
		t.Errorf("reloaded %d libraries, want 2", len(loaded.libraries))
	}
}

// A form on screen is holding half-typed input. Reloading under it would move
// the ground it is standing on, so an update waits.
func TestFormViewsAreNotReloadedUnderTheUser(t *testing.T) {
	for _, view := range []string{viewRename, viewMkdir, viewUpload, viewMove, viewConfirmDelete, viewLogin} {
		m := browseModel(3, 80, 24)
		m.api = notifyServer(t)
		m.view = view

		if _, cmd := m.Update(libraryUpdatedMsg{LibraryUpdate: client.LibraryUpdate{LibraryID: "library"}}); cmd != nil {
			t.Errorf("view %q: an update reloaded under an open form", view)
		}
	}
}

// A command fires once. If handling an update did not arm the next wait, the
// first push would be the only one that ever landed.
func TestUpdateReArmsTheWaitForTheNextEvent(t *testing.T) {
	m := browseModel(3, 80, 24)
	m.api = notifyServer(t)
	// Nothing listening on this port, which is fine: the watcher is here to be
	// waited on, not to deliver.
	m.watcher = client.NewClient("http://127.0.0.1:9").Watch()
	t.Cleanup(m.watcher.Close)
	// A form view refreshes nothing, so the only command left to return is the
	// re-arm.
	m.view = viewRename

	if _, cmd := m.Update(libraryUpdatedMsg{LibraryUpdate: client.LibraryUpdate{LibraryID: "library"}}); cmd == nil {
		t.Fatal("handling an update returned no command, so nothing waits for the next one")
	}
}
