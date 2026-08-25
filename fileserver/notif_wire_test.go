package silod

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/dkam/silo/client"
	"github.com/dkam/silo/fileserver/notif"
	"github.com/dkam/silo/fileserver/option"
)

// The whole chain, end to end: one client deletes a file, and a second client
// that is only watching hears about it without asking. This is the loop the TUI
// rides on, and every link in it lives in a different package — the commit
// announcing, the socket fanning out, the client resubscribing — so it is worth
// one test that owns none of them.
func TestADeleteReachesAWatchingClient(t *testing.T) {
	origEnabled, origKey := option.EnableNotification, option.JWTPrivateKey
	t.Cleanup(func() { option.EnableNotification, option.JWTPrivateKey = origEnabled, origKey })
	option.EnableNotification = true
	option.JWTPrivateKey = "test-secret-key-for-wire-tests"
	notif.Init()

	base, _ := wire(t)

	// The account wire() created.
	c := client.NewClient(base)
	if err := c.Login("wire@example.com", "correct horse battery staple"); err != nil {
		t.Fatalf("login: %v", err)
	}
	library, err := c.CreateLibrary("watched")
	if err != nil {
		t.Fatalf("create library: %v", err)
	}

	watcher := c.Watch()
	defer watcher.Close()
	watcher.Subscribe(library.ID)

	// Subscribing is asynchronous, and a commit made before the server has
	// recorded the subscription reaches nobody. Probe with commits that don't
	// matter until one comes back, so what follows is racing nothing.
	live := false
	for i := 0; i < 100 && !live; i++ {
		if err := c.Mkdir(library.ID, "/probe"+strconv.Itoa(i)); err != nil {
			t.Fatalf("probe mkdir: %v", err)
		}
		select {
		case <-watcher.Events():
			live = true
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !live {
		t.Fatal("never received an event for a subscribed library")
	}
	// Drain whatever the probes left queued, so the next event read is the
	// delete's and not a leftover.
	for draining := true; draining; {
		select {
		case <-watcher.Events():
		case <-time.After(200 * time.Millisecond):
			draining = false
		}
	}

	local := filepath.Join(t.TempDir(), "doomed.txt")
	if err := os.WriteFile(local, []byte("here for now"), 0o600); err != nil {
		t.Fatalf("write local file: %v", err)
	}
	if err := c.UploadFile(library.ID, "/", local); err != nil {
		t.Fatalf("upload: %v", err)
	}
	select {
	case <-watcher.Events():
	case <-time.After(5 * time.Second):
		t.Fatal("no event for the upload")
	}

	// The other client is showing this listing.
	before, err := c.ListDir(library.ID, "/")
	if err != nil {
		t.Fatalf("list before: %v", err)
	}
	if !hasEntry(before, "doomed.txt") {
		t.Fatalf("uploaded file is not in the listing: %+v", before)
	}

	if err := c.DeleteFile(library.ID, "/doomed.txt"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	select {
	case ev := <-watcher.Events():
		if ev.LibraryID != library.ID {
			t.Errorf("event for library %s, want %s", ev.LibraryID, library.ID)
		}
		if ev.CommitID == "" {
			t.Error("event carried no commit id")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deleting a file did not reach the watching client")
	}

	// And the reload the event prompts shows the deletion.
	after, err := c.ListDir(library.ID, "/")
	if err != nil {
		t.Fatalf("list after: %v", err)
	}
	if hasEntry(after, "doomed.txt") {
		t.Errorf("deleted file still listed: %+v", after)
	}
}

func hasEntry(entries []client.DirEntry, name string) bool {
	for _, e := range entries {
		if e.Name == name {
			return true
		}
	}
	return false
}
