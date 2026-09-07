package notif

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

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

// A credentialed connection may sit idle.
//
// This is not a nicety, it is the TUI: it opens the socket when you log in and
// subscribes only when you open a library, so between the two it is a
// legitimate connection with nothing subscribed. Nothing reaps it now that
// nothing anonymous gets this far, and this says so -- a reaper reintroduced
// for idleness alone would disconnect the TUI on every login.
func TestACredentialedConnectionMayIdleWithoutSubscribing(t *testing.T) {
	conn := dialWithCredential(t, testCredential())

	if closedWithin(t, conn, time.Second) {
		t.Error("a credentialed connection was closed for not subscribing; " +
			"the TUI holds exactly this connection between login and opening a library")
	}
}
