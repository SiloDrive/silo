package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/SiloDrive/silo/fileserver/notif"
)

const watchLibraryID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

// A server that offers notifications but not the credential lane is left
// alone.
//
// This is the version skew the feature name exists for. Such a server wants a
// jwt_token in the subscribe frame and answers a frame without one with
// jwt-expired, which means "re-mint and try again" -- so a client that dialled
// it would ask forever for a token this one no longer knows how to fetch.
func TestWatcherStopsWhenTheServerOffersNoCredentialLane(t *testing.T) {
	dials := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/silo/v1/server-info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"0.5.0","features":["batch","notifications"]}`))
	})
	mux.HandleFunc("/notification", func(w http.ResponseWriter, r *http.Request) {
		dials++
		http.Error(w, "not found", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	w := NewClient(srv.URL).Watch()
	defer w.Close()
	w.Subscribe(watchLibraryID)

	select {
	case _, ok := <-w.Events():
		if ok {
			t.Error("an event arrived from a server that does not offer the credential lane")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the watcher is still going against a server that wants a token it cannot mint")
	}
	if dials != 0 {
		t.Errorf("dialled the notification endpoint %d times on a server that wants a jwt_token", dials)
	}
}

// stubNotifSocket stands up a notification endpoint that hands each connection
// to onConn, numbered from one.
//
// These tests used to drive the real notif.Handler, on the grounds that a
// client tested against a mirror of its own assumptions is a client that has
// tested nothing. The real handler now answers a subscribe by asking whether
// the socket's credential reaches the library, and that question needs a
// database this package has never had -- so what it would answer here is "no"
// to everything, which tests less than a stub does.
//
// Two things carry the weight the real handler used to.
// TestMessageTypesMatchTheServers below pins every frame name against the
// server's own constants, so a rename cannot pass. And
// fileserver/notif_wire_test.go runs this client against the real server, real
// database and real permission check, end to end -- which is more of the stack
// than these tests ever reached.
func stubNotifSocket(t *testing.T, onConn func(n int, conn *websocket.Conn)) *APIClient {
	t.Helper()
	var mu sync.Mutex
	n := 0
	up := websocket.Upgrader{}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/silo/v1/server-info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"0.5.0","features":["notifications","notifications-credential"]}`))
	})
	mux.HandleFunc("/notification", func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		mu.Lock()
		n++
		which := n
		mu.Unlock()
		onConn(which, conn)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL)
}

// readSubscribe reads one subscribe frame and returns the libraries it names.
func readSubscribe(t *testing.T, conn *websocket.Conn) []string {
	t.Helper()
	for {
		var msg wireMessage
		if err := conn.ReadJSON(&msg); err != nil {
			return nil
		}
		if msg.Type != msgSubscribe {
			continue
		}
		var frame wireSubscribe
		if err := json.Unmarshal(msg.Content, &frame); err != nil {
			t.Errorf("bad subscribe frame: %v", err)
			return nil
		}
		ids := make([]string, 0, len(frame.Libraries))
		for _, lib := range frame.Libraries {
			ids = append(ids, lib.LibraryID)
		}
		return ids
	}
}

// writeFrame sends one server frame whose content is a library id and, for an
// update, a commit.
func writeFrame(conn *websocket.Conn, typ, libraryID, commitID string) error {
	content, err := json.Marshal(map[string]string{"library_id": libraryID, "commit_id": commitID})
	if err != nil {
		return err
	}
	return conn.WriteJSON(wireMessage{Type: typ, Content: content})
}

// A subscribe frame names the library and nothing else, and an update for it
// reaches the caller.
//
// The frame carrying no jwt_token is the whole of what changed here: the field
// is gone from the wire, and the server answers from the credential the
// handshake carried.
func TestWatcherDeliversLibraryUpdate(t *testing.T) {
	const commitID = "0123456789abcdef0123456789abcdef01234567"

	sent := make(chan []string, 4)
	c := stubNotifSocket(t, func(n int, conn *websocket.Conn) {
		libs := readSubscribe(t, conn)
		sent <- libs
		for _, id := range libs {
			_ = writeFrame(conn, msgLibraryUpdate, id, commitID)
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})

	w := c.Watch()
	defer w.Close()
	w.Subscribe(watchLibraryID)

	select {
	case libs := <-sent:
		if len(libs) != 1 || libs[0] != watchLibraryID {
			t.Errorf("subscribed to %v, want [%s]", libs, watchLibraryID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no subscribe frame arrived")
	}

	select {
	case ev := <-w.Events():
		if ev.LibraryID != watchLibraryID || ev.CommitID != commitID {
			t.Errorf("got %+v, want library=%s commit=%s", ev, watchLibraryID, commitID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a library-update event")
	}
}

// A refused subscribe is retried, and the retry is what the client does
// instead of re-minting.
//
// jwt-expired used to say "the token you sent is stale, fetch another", and
// the client answered with an HTTP round trip. subscribe-denied says something
// the client cannot fix by fetching anything -- so the only useful answer is
// to back off and ask again, in case a share was re-granted under a socket
// that is still up. What must not happen is the client sitting quietly on a
// subscription it does not have.
func TestWatcherRetriesAfterSubscribeDenied(t *testing.T) {
	const commitID = "0123456789abcdef0123456789abcdef01234567"

	attempts := make(chan int, 8)
	c := stubNotifSocket(t, func(n int, conn *websocket.Conn) {
		for attempt := 1; ; attempt++ {
			libs := readSubscribe(t, conn)
			if len(libs) == 0 {
				return
			}
			attempts <- attempt
			for _, id := range libs {
				if attempt == 1 {
					_ = writeFrame(conn, msgSubscribeDenied, id, "")
					continue
				}
				_ = writeFrame(conn, msgLibraryUpdate, id, commitID)
			}
		}
	})

	w := c.Watch()
	defer w.Close()
	w.Subscribe(watchLibraryID)

	select {
	case ev := <-w.Events():
		if ev.LibraryID != watchLibraryID || ev.CommitID != commitID {
			t.Errorf("got %+v, want library=%s commit=%s", ev, watchLibraryID, commitID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a refused subscribe was never retried")
	}
	if n := len(attempts); n < 2 {
		t.Errorf("the client sent %d subscribe frame(s), want at least 2 (the refused one and its retry)", n)
	}
}

// Close is what the TUI calls on the way out; it must stop the watcher without
// leaving the caller blocked on a channel that will never speak again.
func TestWatcherCloseEndsEvents(t *testing.T) {
	c := stubNotifSocket(t, func(n int, conn *websocket.Conn) {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})

	w := c.Watch()
	w.Subscribe(watchLibraryID)
	w.Close()
	w.Close() // idempotent: the TUI may close on both quit paths

	select {
	case _, ok := <-w.Events():
		if ok {
			t.Error("got an event after Close")
		}
	case <-time.After(2 * time.Second):
		t.Error("Events channel still open two seconds after Close")
	}
}

func TestNotifyEndpoint(t *testing.T) {
	cases := []struct {
		base string
		want string
	}{
		{"http://localhost:8080", "ws://localhost:8080/notification"},
		{"https://silo.example.com", "wss://silo.example.com/notification"},
		// A trailing slash must not double up, and a server mounted under a
		// prefix keeps it.
		{"https://example.com/", "wss://example.com/notification"},
		{"https://example.com/silo", "wss://example.com/silo/notification"},
		{"https://example.com/silo/", "wss://example.com/silo/notification"},
	}
	for _, c := range cases {
		got, err := notifyEndpoint(c.base)
		if err != nil {
			t.Errorf("notifyEndpoint(%q): %v", c.base, err)
			continue
		}
		if got != c.want {
			t.Errorf("notifyEndpoint(%q) = %q, want %q", c.base, got, c.want)
		}
	}

	if _, err := notifyEndpoint("ftp://example.com"); err == nil {
		t.Error("notifyEndpoint accepted an ftp:// server URL")
	}
}

// The client restates the server's vocabulary rather than importing its
// package, so nothing but this asserts the two still agree: rename an event
// type on the server and both sides still compile, while no event ever lands.
func TestMessageTypesMatchTheServers(t *testing.T) {
	if msgLibraryUpdate != notif.EventTypeLibraryUpdate {
		t.Errorf("client sends %q, server sends %q", msgLibraryUpdate, notif.EventTypeLibraryUpdate)
	}
	if msgSubscribeDenied != notif.EventTypeSubscribeDenied {
		t.Errorf("client reads %q, server sends %q", msgSubscribeDenied, notif.EventTypeSubscribeDenied)
	}
	if msgSubscribe != "subscribe" {
		t.Errorf("client sends %q, server reads %q", msgSubscribe, "subscribe")
	}
}

// A server built without the notification endpoint is worth asking about once,
// rather than dialling for the life of the session.
func TestWatcherStopsWhenTheServerOffersNoNotifications(t *testing.T) {
	dials := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/silo/v1/server-info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"0.5.0","features":["batch","chunks"]}`))
	})
	mux.HandleFunc("/notification", func(w http.ResponseWriter, r *http.Request) {
		dials++
		http.Error(w, "not found", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	w := NewClient(srv.URL).Watch()
	defer w.Close()
	w.Subscribe(watchLibraryID)

	select {
	case _, ok := <-w.Events():
		if ok {
			t.Error("an event arrived from a server that advertises no notifications")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the watcher is still going against a server that advertises no notifications")
	}
	if dials != 0 {
		t.Errorf("dialled the notification endpoint %d times on a server that does not offer it", dials)
	}
}

// A connection that drops takes with it every event the server sent while it
// was down, and the caller has no way to ask for them again. So the socket
// coming back is itself the news: every watched library may have moved, and
// the watcher says so rather than leaving a stale listing on screen.
func TestWatcherResyncsAfterAReconnect(t *testing.T) {
	subscribed := make(chan int, 4)
	c := stubNotifSocket(t, func(n int, conn *websocket.Conn) {
		// Wait for the subscription before doing anything: a connection
		// dropped before the client has said what it wants proves nothing.
		readSubscribe(t, conn)
		subscribed <- n
		if n == 1 {
			return // drop it, and let the watcher find its way back
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})

	w := c.Watch()
	defer w.Close()
	w.Subscribe(watchLibraryID)

	if got := <-subscribed; got != 1 {
		t.Fatalf("first connection numbered %d", got)
	}

	select {
	case ev := <-w.Events():
		if ev.LibraryID != watchLibraryID {
			t.Errorf("resynced %q, want %q", ev.LibraryID, watchLibraryID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the socket came back and nothing told the caller its listing might be stale")
	}
}

// The first connection of a watcher's life has missed nothing: the caller is
// about to load the library for the first time anyway, and a resync there is a
// second load of what it already has.
func TestWatcherDoesNotResyncOnItsFirstConnection(t *testing.T) {
	subscribed := make(chan int, 4)
	c := stubNotifSocket(t, func(n int, conn *websocket.Conn) {
		readSubscribe(t, conn)
		subscribed <- n
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})

	w := c.Watch()
	defer w.Close()
	w.Subscribe(watchLibraryID)
	<-subscribed

	select {
	case ev := <-w.Events():
		t.Errorf("the first connection resynced %+v", ev)
	case <-time.After(500 * time.Millisecond):
	}
}
