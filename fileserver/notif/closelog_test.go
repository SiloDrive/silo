package notif

import (
	"bytes"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"

	"github.com/dkam/silo/fileserver/credential"
)

// A socket ends in one of two ways, and the log has to say which.
//
// readLoop logged every exit as a read error, so a client that shut its socket
// down politely -- the macOS container app closing its notification socket
// because a File Provider domain was being removed -- read the same in the
// journal as one whose connection broke. Correlating a client-side incident
// then needed the client's own log to tell the two apart.

// logBuffer collects log output written from another goroutine. readLoop logs
// from the connection's own goroutine, so the test cannot share a bare
// bytes.Buffer with it.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureDebug collects what the package logs for the rest of the test.
func captureDebug(t *testing.T) *logBuffer {
	t.Helper()

	buf := &logBuffer{}
	prevOut, prevLevel := log.StandardLogger().Out, log.GetLevel()
	log.SetOutput(buf)
	log.SetLevel(log.DebugLevel)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetLevel(prevLevel)
	})
	return buf
}

// awaitLine waits for a line mentioning the client's socket, and returns it.
func awaitLine(t *testing.T, buf *logBuffer) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if out := buf.String(); strings.Contains(out, "notif: client") {
			return out
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the socket ended and nothing was logged about it: %s", buf.String())
	return ""
}

// A client that says it is going away is logged as having disconnected.
func TestAPoliteCloseIsNotLoggedAsAReadError(t *testing.T) {
	Init()
	authorizeReturning(t, func(*credential.Credential, string) bool { return true })

	buf := captureDebug(t)
	conn := dialWithCredential(t, testCredential())

	// 1001, RFC 6455's "going away": what a client sends when it is shutting
	// down deliberately.
	frame := websocket.FormatCloseMessage(websocket.CloseGoingAway, "")
	if err := conn.WriteMessage(websocket.CloseMessage, frame); err != nil {
		t.Fatalf("sending the close frame: %v", err)
	}

	line := awaitLine(t, buf)
	if !strings.Contains(line, "disconnected") {
		t.Errorf("a clean close is not logged as a disconnect: %s", line)
	}
	if strings.Contains(line, "read error") {
		t.Errorf("a clean close is logged as a read error: %s", line)
	}
	if !strings.Contains(line, "1001") {
		t.Errorf("the line does not carry the close code: %s", line)
	}
}

// A socket that breaks is still a read error, which is the distinction the
// separate wording exists to draw.
func TestABrokenSocketIsStillLoggedAsAReadError(t *testing.T) {
	Init()
	authorizeReturning(t, func(*credential.Credential, string) bool { return true })

	buf := captureDebug(t)
	conn := dialWithCredential(t, testCredential())

	// No close frame: the TCP connection goes away underneath the websocket,
	// which is what a dropped link looks like from the server's side. RST
	// rather than FIN, so the server reads a failure rather than an orderly
	// end of stream.
	if tcp, ok := conn.UnderlyingConn().(*net.TCPConn); ok {
		if err := tcp.SetLinger(0); err != nil {
			t.Fatalf("SetLinger: %v", err)
		}
	}
	if err := conn.UnderlyingConn().Close(); err != nil {
		t.Fatalf("closing the socket: %v", err)
	}

	line := awaitLine(t, buf)
	if !strings.Contains(line, "read error") {
		t.Errorf("a broken socket is not logged as a read error: %s", line)
	}
}
