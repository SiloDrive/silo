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

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/middleware"
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

// dialRefused opens a socket that is expected to be refused, and returns the
// handshake's status.
func dialRefused(t *testing.T, inject func(*http.Request) *http.Request) int {
	t.Helper()
	Init()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if inject != nil {
			r = inject(r)
		}
		Handler(w, r)
	}))
	t.Cleanup(srv.Close)

	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err == nil {
		_ = conn.Close()
		t.Fatal("the handshake was accepted")
	}
	if resp == nil {
		t.Fatalf("dial failed without a response: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp.StatusCode
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

// A socket that presents no credential is refused at the handshake.
//
// It used to be accepted and put on a clock. A subscribe frame could carry a
// library-scoped JWT, which was the one thing an anonymous connection could
// prove anything with, so it was given provisionalGrace to do that in and
// reaped if it did not -- and the socket was free to hold four goroutines, a
// connection and its buffers until then, since anything that answers pings
// survives a ping reaper and every WebSocket library answers pings for you.
//
// With the token lane gone there is nothing such a connection could ever be
// subscribed to, so the deadline became a slow way of saying no. This is the
// fast way, and it is the same answer every other route gives.
func TestASocketWithNoCredentialIsRefusedAtTheHandshake(t *testing.T) {
	if got := dialRefused(t, nil); got != http.StatusUnauthorized {
		t.Errorf("the handshake answered %d, want %d", got, http.StatusUnauthorized)
	}
}

// An account on the request is not a credential, and does not open the socket.
//
// The two are separate fields for a reason -- the account is who the socket
// belongs to, the credential is what that holder may reach -- and a subscribe
// is answered from the credential alone. A socket admitted on the account
// would be one whose every subscribe is denied.
func TestASocketWithAnAccountButNoCredentialIsRefused(t *testing.T) {
	acct := &account.Account{Email: "idle@example.com", IsActive: true}
	got := dialRefused(t, func(r *http.Request) *http.Request {
		return middleware.WithAccount(r, acct)
	})
	if got != http.StatusUnauthorized {
		t.Errorf("the handshake answered %d, want %d", got, http.StatusUnauthorized)
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
