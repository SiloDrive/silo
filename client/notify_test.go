package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/dkam/silo/fileserver/notif"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/utils"
)

const watchLibraryID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

// newNotifServer stands up the real notification endpoint and the real
// token-minting shape in front of it, so these tests exercise the wire
// protocol rather than a client talking to a mirror of its own assumptions.
//
// mintToken is called for each notify-token request, so a test can hand out a
// token the server will reject and watch the client recover.
func newNotifServer(t *testing.T, mintToken func(n int) string) (*APIClient, *int) {
	t.Helper()
	option.JWTPrivateKey = "test-secret-key-for-watch-tests"
	notif.Init()

	mints := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/notification", notif.Handler)
	mux.HandleFunc("/api/silo/v1/libraries/"+watchLibraryID+"/notify-token", func(w http.ResponseWriter, r *http.Request) {
		mints++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jwt_token":  mintToken(mints),
			"expires_at": time.Now().Add(72 * time.Hour).Unix(),
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL), &mints
}

func goodToken(t *testing.T) string {
	t.Helper()
	tok, err := utils.GenNotifJWTToken(watchLibraryID, "alice@example.com", time.Now().Add(72*time.Hour).Unix())
	if err != nil {
		t.Fatalf("GenNotifJWTToken: %v", err)
	}
	return tok
}

// awaitUpdate re-announces until the event lands, because subscribing is
// asynchronous: an announcement made before the server has recorded the
// subscription reaches nobody, and that is the announcement's design.
func awaitUpdate(t *testing.T, w *Watcher, commitID string) LibraryUpdate {
	t.Helper()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		notif.NotifyLibraryUpdate(watchLibraryID, commitID)
		select {
		case ev := <-w.Events():
			return ev
		case <-tick.C:
		case <-deadline:
			t.Fatal("timed out waiting for a library-update event")
		}
	}
}

func TestWatcherDeliversLibraryUpdate(t *testing.T) {
	c, _ := newNotifServer(t, func(int) string { return goodToken(t) })

	w := c.Watch()
	defer w.Close()
	w.Subscribe(watchLibraryID)

	const commitID = "0123456789abcdef0123456789abcdef01234567"
	ev := awaitUpdate(t, w, commitID)
	if ev.LibraryID != watchLibraryID || ev.CommitID != commitID {
		t.Errorf("got %+v, want library=%s commit=%s", ev, watchLibraryID, commitID)
	}
}

// A token the server refuses comes back as jwt-expired, which is the client's
// cue to mint a fresh one rather than to sit on a subscription it does not have.
func TestWatcherRemintsAfterJWTExpired(t *testing.T) {
	c, mints := newNotifServer(t, func(n int) string {
		if n == 1 {
			return "not-a-jwt"
		}
		return goodToken(t)
	})

	w := c.Watch()
	defer w.Close()
	w.Subscribe(watchLibraryID)

	ev := awaitUpdate(t, w, "0000000000000000000000000000000000000000")
	if ev.LibraryID != watchLibraryID {
		t.Errorf("got %+v, want library=%s", ev, watchLibraryID)
	}
	if *mints < 2 {
		t.Errorf("minted %d tokens, want at least 2 (the rejected one and its replacement)", *mints)
	}
}

// Close is what the TUI calls on the way out; it must stop the watcher without
// leaving the caller blocked on a channel that will never speak again.
func TestWatcherCloseEndsEvents(t *testing.T) {
	c, _ := newNotifServer(t, func(int) string { return goodToken(t) })

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
	if msgJWTExpired != notif.EventTypeJWTExpired {
		t.Errorf("client reads %q, server sends %q", msgJWTExpired, notif.EventTypeJWTExpired)
	}
}

// A server built without the notification endpoint is worth asking about once,
// rather than dialling for the life of the session.
func TestWatcherStopsWhenTheServerOffersNoNotifications(t *testing.T) {
	dials := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/silo/v1/server-info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"0.5.0","features":["batch","blocks"]}`))
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

// stubNotifSocket stands up a notification endpoint that hands each connection
// to onConn, numbered from one, along with the subscribe frame that connection
// carried. It exists because the real handler has no seam for dropping a
// connection out from under a client, which is the thing under test here.
func stubNotifSocket(t *testing.T, onConn func(n int, conn *websocket.Conn)) *APIClient {
	t.Helper()
	var mu sync.Mutex
	n := 0
	up := websocket.Upgrader{}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/silo/v1/libraries/"+watchLibraryID+"/notify-token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jwt_token":  "any-token-this-server-does-not-check",
			"expires_at": time.Now().Add(72 * time.Hour).Unix(),
		})
	})
	mux.HandleFunc("/notification", func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		mu.Lock()
		n++
		which := n
		mu.Unlock()
		// Wait for the subscription before doing anything: a connection dropped
		// before the client has said what it wants proves nothing.
		var msg wireMessage
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		onConn(which, conn)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL)
}

// A connection that drops takes with it every event the server sent while it
// was down, and the caller has no way to ask for them again. So the socket
// coming back is itself the news: every watched library may have moved, and
// the watcher says so rather than leaving a stale listing on screen.
func TestWatcherResyncsAfterAReconnect(t *testing.T) {
	subscribed := make(chan int, 4)
	c := stubNotifSocket(t, func(n int, conn *websocket.Conn) {
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
