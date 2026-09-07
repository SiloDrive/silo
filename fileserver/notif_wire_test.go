package silod

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

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
	enableNotifications(t)

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

// A rename rings an account-scoped socket, and moves no head.
//
// End to end, because the thing that was missing was a producer: the rename
// handler is one UPDATE and mints no commit, so nothing on the commit path
// ever announced it. The socket here is opened the way a client opens it --
// the session credential in the header, one frame saying "everything" -- and
// the ring is what arrives.
func TestARenameRingsAnAccountScopedSocket(t *testing.T) {
	enableNotifications(t)

	base, token := wire(t)
	libraryID := makeLibrary(t, base, token)
	headBefore := libraryHead(t, base, token, libraryID)

	header := http.Header{"Authorization": {"Bearer " + token}}
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/notification", header)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{"type": "subscribe", "content": map[string]any{"account": true}}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	frames := readFrames(conn)

	// The subscribe is handled on the server's read loop, so a rename made
	// before it lands rings nobody. Rename until one comes back; every one of
	// them is a rename that must ring, so the loop is the assertion.
	rung := false
	for i := 0; i < 100 && !rung; i++ {
		code, body := call(t, "PATCH", base+"/api/silo/v1/libraries/"+libraryID, token,
			`{"name":"renamed `+strconv.Itoa(i)+`"}`)
		if code != http.StatusOK {
			t.Fatalf("rename: status %d, body %s", code, body)
		}
		select {
		case msg := <-frames:
			if msg.Type != notif.EventTypeAccountUpdate {
				t.Fatalf("an account socket was sent %s, want %s", msg.Type, notif.EventTypeAccountUpdate)
			}
			rung = true
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !rung {
		t.Fatal("a rename never rang the account socket")
	}
	if after := libraryHead(t, base, token, libraryID); after != headBefore {
		t.Errorf("the rename moved the head from %s to %s; it must mint no commit", headBefore, after)
	}
}

// libraryHead reads a library's head commit id off the listing.
func libraryHead(t *testing.T, base, token, libraryID string) string {
	t.Helper()
	code, body := call(t, "GET", base+"/api/silo/v1/libraries", token, "")
	if code != http.StatusOK {
		t.Fatalf("listing: status %d, body %s", code, body)
	}
	var rows []struct {
		ID   string `json:"id"`
		Head string `json:"head_commit_id"`
	}
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatalf("decode listing: %v", err)
	}
	for _, r := range rows {
		if r.ID == libraryID {
			return r.Head
		}
	}
	t.Fatalf("library %s is not in the listing", libraryID)
	return ""
}

// The token lane is gone, and the three things a client can observe about that
// are pinned here.
//
// Each one is a sentence in the docs -- `protocol.md` § Removed in 0.5.0, and
// `upgrading-to-0.5.0.md` -- and a client author will act on all three: delete
// the minting call, move the credential onto the upgrade, and stop sending
// `jwt_token`. A removal has no handler to test, so the assertion has to be
// that the surface is absent in the exact shape the documentation claims,
// rather than absent in some shape.
func TestTheNotifyTokenLaneIsGone(t *testing.T) {
	enableNotifications(t)

	base, token := wire(t)
	libraryID := makeLibrary(t, base, token)

	// The mint answers 404 -- indistinguishable from notifications being
	// switched off, which is why the docs send clients to the feature name
	// instead of to a fallback on this status.
	if code, body := call(t, "POST", base+"/api/silo/v1/libraries/"+libraryID+"/notify-token", token, ""); code != http.StatusNotFound {
		t.Errorf("POST notify-token: status %d, want %d; body %s", code, http.StatusNotFound, body)
	}

	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/notification"

	// The handshake requires a credential now: no header, and a bad one, are
	// both refused before the upgrade rather than downgraded to anonymous.
	for _, tc := range []struct {
		name   string
		header http.Header
	}{
		{"no credential", nil},
		{"a bad credential", http.Header{"Authorization": {"Bearer not-a-credential"}}},
	} {
		conn, resp, err := websocket.DefaultDialer.Dial(wsURL, tc.header)
		if err == nil {
			conn.Close()
			t.Errorf("a socket with %s was upgraded; it must be refused", tc.name)
			continue
		}
		if resp == nil {
			t.Errorf("dial with %s failed without a response: %v", tc.name, err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("dial with %s: status %d, want %d", tc.name, resp.StatusCode, http.StatusUnauthorized)
		}
	}

	// And a subscribe entry that still carries jwt_token is granted on the
	// credential: the field is ignored, not refused, so a client mid-migration
	// is not broken by the one line it forgot to delete.
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer " + token}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	frame := map[string]any{"type": "subscribe", "content": map[string]any{
		"libraries": []map[string]any{{"id": libraryID, "jwt_token": "a token nothing issues any more"}},
	}}
	if err := conn.WriteJSON(frame); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	frames := readFrames(conn)

	// Commit until an update comes back, since the subscribe lands on the
	// server's read loop; a subscribe-denied at any point is the failure.
	granted := false
	for i := 0; i < 100 && !granted; i++ {
		code, body := call(t, "PUT", base+"/api/silo/v1/libraries/"+libraryID+"/entries/probe"+strconv.Itoa(i)+"?type=dir", token, "")
		if code != http.StatusOK && code != http.StatusCreated {
			t.Fatalf("mkdir: status %d, body %s", code, body)
		}
		select {
		case msg := <-frames:
			if msg.Type == notif.EventTypeSubscribeDenied {
				t.Fatalf("a subscribe carrying jwt_token was denied; the field must be ignored: %s", msg.Content)
			}
			if msg.Type == notif.EventTypeLibraryUpdate {
				granted = true
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !granted {
		t.Error("a subscribe carrying a stale jwt_token never received an update")
	}
}

// enableNotifications turns the socket on for one test and puts the package
// back the way it was.
func enableNotifications(t *testing.T) {
	t.Helper()
	orig := option.EnableNotification
	t.Cleanup(func() { option.EnableNotification = orig })
	option.EnableNotification = true
	notif.Init()
}

// readFrames pumps a socket onto a channel.
//
// A goroutine rather than a read deadline: a read that times out poisons the
// connection for every read after it, so a polling loop over deadlines probes
// once and then fails instantly forever.
func readFrames(conn *websocket.Conn) <-chan notif.Message {
	frames := make(chan notif.Message, 8)
	go func() {
		for {
			var msg notif.Message
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			frames <- msg
		}
	}()
	return frames
}
