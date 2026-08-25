package notif

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/utils"
)

// shortGrace makes the provisional deadline testable, and restores it.
func shortGrace(t *testing.T, d time.Duration) {
	t.Helper()
	orig := provisionalGrace.Load()
	provisionalGrace.Store(int64(d))
	t.Cleanup(func() { provisionalGrace.Store(orig) })
}

// dialSocket opens a notification socket against a server that authenticates
// the request however inject says.
func dialSocket(t *testing.T, inject func(*http.Request) *http.Request) *websocket.Conn {
	t.Helper()
	Init()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if inject != nil {
			r = inject(r)
		}
		Handler(w, r)
	}))
	t.Cleanup(srv.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// subscribeTo sends a well-formed subscribe frame for one library.
func subscribeTo(t *testing.T, conn *websocket.Conn, libraryID string) {
	t.Helper()
	tok, err := utils.GenNotifJWTToken(libraryID, "watcher@example.com", time.Now().Add(time.Hour).Unix())
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	content, err := json.Marshal(subscribeFrame{
		Libraries: []subscribeLibrary{{LibraryID: libraryID, Token: tok}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(&Message{Type: "subscribe", Content: content}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
}

// closedWithin reports whether the server hung up before the deadline. A read
// that times out is the connection still being held, which is the opposite of
// what is being asked.
func closedWithin(t *testing.T, conn *websocket.Conn, d time.Duration) bool {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(d)); err != nil {
		t.Fatal(err)
	}
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return false
			}
			return true
		}
		// A frame arrived -- keep reading until the deadline or the close.
	}
}

// An anonymous connection that never subscribes is closed.
//
// The socket carries library-update and nothing else, so a connection with no
// subscriptions receives nothing, ever. One that also presented no credential
// at the handshake has therefore proved nothing and asked for nothing, and was
// free to hold four goroutines, a connection and its buffers for as long as it
// liked -- indefinitely, since anything that answers pings survives the only
// reaper there was, and every WebSocket library answers pings for you.
//
// Subscribing is what proves a caller: the frame carries a library-scoped JWT
// the server verifies. So this is not a new authentication requirement, it is a
// deadline on the one that was always there.
func TestAnAnonymousConnectionThatNeverSubscribesIsClosed(t *testing.T) {
	shortGrace(t, 200*time.Millisecond)

	conn := dialSocket(t, nil)

	if !closedWithin(t, conn, 5*time.Second) {
		t.Error("a connection that presented no credential and subscribed to nothing " +
			"was still being held after the grace expired")
	}
}

// Subscribing is what earns the connection its keep.
//
// The reaper must not touch a client that has done the one thing the socket is
// for. Getting this wrong turns a security fix into a disconnect loop for every
// working client.
func TestAConnectionThatSubscribesIsKeptPastTheGrace(t *testing.T) {
	const libraryID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	shortGrace(t, 200*time.Millisecond)

	conn := dialSocket(t, nil)
	subscribeTo(t, conn, libraryID)

	// Well past the deadline it would have been closed at.
	time.Sleep(time.Second)

	// Still live: an event sent now arrives.
	const commitID = "0123456789abcdef0123456789abcdef01234567"
	NotifyLibraryUpdate(libraryID, commitID)
	ev := awaitUpdate(t, conn, libraryID)
	if ev.CommitID != commitID {
		t.Errorf("got commit %q, want %q", ev.CommitID, commitID)
	}
}

// A connection that authenticated at the handshake may sit idle.
//
// This is not a nicety, it is the TUI: it opens the socket when you log in and
// subscribes only when you open a library, so between the two it is a
// legitimate connection with nothing subscribed. An account named at the
// handshake is what separates that from an anonymous socket doing the same
// thing for free.
func TestAnAuthenticatedConnectionMayIdleWithoutSubscribing(t *testing.T) {
	shortGrace(t, 200*time.Millisecond)

	acct := &account.Account{Email: "idle@example.com", IsActive: true}
	conn := dialSocket(t, func(r *http.Request) *http.Request {
		return middleware.WithAccount(r, acct)
	})

	if closedWithin(t, conn, time.Second) {
		t.Error("an authenticated connection was closed for not subscribing; " +
			"the TUI holds exactly this connection between login and opening a library")
	}
}
