package silod

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// A reader that goes away mid-body is not a server fault.
//
// The status is already on the wire by the time the body is written, so a
// failed write cannot become a 500 and all that is left to do is log it. Both
// stream sites logged at ERROR, and every instance in practice was a client
// cancelling a download -- a Quick Look preview closing, a fetch the File
// Provider daemon withdrew. EPIPE and ECONNRESET on a body write mean the
// reader went away, which is the ordinary shape of that traffic; an operator
// scanning for ERROR wants the writes that failed for some other reason.

// failingWriter is a ResponseWriter whose body writes fail, the way a socket
// whose peer has gone does.
type failingWriter struct {
	header http.Header
	code   int
	err    error
}

func (w *failingWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *failingWriter) WriteHeader(code int) { w.code = code }

func (w *failingWriter) Write([]byte) (int, error) { return 0, w.err }

// goneErr is what the kernel hands back when the peer has closed its end and
// the server writes anyway, wrapped the way net does it.
func goneErr(errno syscall.Errno) error {
	return &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", errno)}
}

// captureDebug collects what the package logs for the rest of the test.
func captureDebug(t *testing.T) *bytes.Buffer {
	t.Helper()

	buf := &bytes.Buffer{}
	prevOut, prevLevel := log.StandardLogger().Out, log.GetLevel()
	log.SetOutput(buf)
	log.SetLevel(log.DebugLevel)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetLevel(prevLevel)
	})
	return buf
}

// streamedFile puts one file big enough to be chunked, and returns the library
// it lives in with the account that owns it.
func streamedFile(t *testing.T) (string, *account.Account) {
	t.Helper()

	libraryID, acct := testLibrary(t)
	content := make([]byte, 3<<20)
	for i := range content {
		content[i] = byte(i * 7 % 251)
	}
	w := do(t, putEntry, acct, http.MethodPut, "/entries/big.bin",
		map[string]string{"libraryid": libraryID, "path": "big.bin"}, content)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT = %d (%s), want 201", w.Code, w.Body.String())
	}
	return libraryID, acct
}

// readInto runs one GET through getEntry against a writer of the test's
// choosing, and returns what the handler logged.
func readInto(t *testing.T, w http.ResponseWriter, libraryID string, acct *account.Account, opts ...func(*http.Request)) string {
	t.Helper()

	r := httptest.NewRequest(http.MethodGet, "/entries/big.bin", nil)
	r = mux.SetURLVars(r, map[string]string{"libraryid": libraryID, "path": "big.bin"})
	r = middleware.WithCredential(r, testCredential(acct), acct)
	for _, opt := range opts {
		opt(r)
	}

	buf := captureDebug(t)
	getEntry(w, r)
	return buf.String()
}

// A whole-file read whose reader has gone is not something an operator has to
// act on.
func TestAStreamToAGoneClientIsNotAnError(t *testing.T) {
	libraryID, acct := streamedFile(t)

	for _, errno := range []syscall.Errno{syscall.EPIPE, syscall.ECONNRESET} {
		line := readInto(t, &failingWriter{err: goneErr(errno)}, libraryID, acct)

		if strings.Contains(line, "[ERROR]") {
			t.Errorf("%v on a body write logged at ERROR: %s", errno, line)
		}
		if !strings.Contains(line, "[DEBUG]") || !strings.Contains(line, libraryID) {
			t.Errorf("%v on a body write left no debug line naming the library: %s", errno, line)
		}
	}
}

// A ranged read takes the same view: same client, same reason.
func TestARangedStreamToAGoneClientIsNotAnError(t *testing.T) {
	libraryID, acct := streamedFile(t)

	line := readInto(t, &failingWriter{err: goneErr(syscall.EPIPE)}, libraryID, acct,
		withHeader("Range", "bytes=0-1023"))
	if strings.Contains(line, "[ERROR]") {
		t.Errorf("a broken pipe on a ranged read logged at ERROR: %s", line)
	}
	if !strings.Contains(line, "[DEBUG]") || !strings.Contains(line, libraryID) {
		t.Errorf("a broken pipe on a ranged read left no debug line naming the library: %s", line)
	}
}

// Anything else that stops a body being written is still an error.
func TestAStreamThatFailsForAnotherReasonIsStillAnError(t *testing.T) {
	libraryID, acct := streamedFile(t)

	line := readInto(t, &failingWriter{err: errors.New("the disk gave up")}, libraryID, acct)
	if !strings.Contains(line, "[ERROR]") {
		t.Errorf("a genuine write failure was demoted: %s", line)
	}
}
