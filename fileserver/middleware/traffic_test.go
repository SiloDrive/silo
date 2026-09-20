package middleware

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SiloDrive/silo/fileserver/traffic"
)

func TestBothDirectionsAreCounted(t *testing.T) {
	before := traffic.Default.Read()

	body := strings.Repeat("u", 500)
	reply := strings.Repeat("d", 900)

	h := CountTraffic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("draining the body: %v", err)
		}
		_, _ = io.WriteString(w, reply)
	}))

	req := httptest.NewRequest("POST", "/api/silo/v1/libraries/r1/chunks", strings.NewReader(body))
	h.ServeHTTP(httptest.NewRecorder(), req)

	after := traffic.Default.Read()
	gotIn := after.Total["bulk"].In - before.Total["bulk"].In
	gotOut := after.Total["bulk"].Out - before.Total["bulk"].Out
	if gotIn != int64(len(body)) {
		t.Errorf("counted %d bytes in, want %d", gotIn, len(body))
	}
	if gotOut != int64(len(reply)) {
		t.Errorf("counted %d bytes out, want %d", gotOut, len(reply))
	}
}

// A body the handler never reads still crossed the socket, but this counts
// what was read -- so the figure is what the server actually took off the
// wire. Worth pinning either way so a change to it is a decision.
func TestAnUnreadBodyIsNotCounted(t *testing.T) {
	before := traffic.Default.Read()

	h := CountTraffic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	req := httptest.NewRequest("POST", "/api/silo/v1/libraries/r1/chunks", strings.NewReader(strings.Repeat("x", 100)))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got := traffic.Default.Read().Total["bulk"].In - before.Total["bulk"].In; got != 0 {
		t.Errorf("counted %d bytes of a body nothing read", got)
	}
}

// The lane split has to survive the middleware, not just LaneFor.
func TestControlTrafficDoesNotLandInTheBulkLane(t *testing.T) {
	before := traffic.Default.Read()

	h := CountTraffic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "{}")
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/silo/v1/libraries", nil))

	after := traffic.Default.Read()
	if after.Total["bulk"] != before.Total["bulk"] {
		t.Errorf("a listing moved the bulk counters from %+v to %+v", before.Total["bulk"], after.Total["bulk"])
	}
	if after.Total["control"].Out == before.Total["control"].Out {
		t.Error("a listing was not counted in the control lane")
	}
}

// hijackable is a ResponseWriter that can be hijacked, which httptest's
// recorder is not. /notification is a WebSocket and hijacks the connection, so
// a wrapper that swallowed Hijack would break the notification lane rather
// than merely mismeasure it.
type hijackable struct {
	http.ResponseWriter
	hijacked bool
	flushed  bool
}

func (h *hijackable) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	return nil, nil, nil
}
func (h *hijackable) Flush() { h.flushed = true }

func TestHijackAndFlushReachTheWriterUnderneath(t *testing.T) {
	under := &hijackable{ResponseWriter: httptest.NewRecorder()}

	h := CountTraffic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("the wrapped writer is not a Flusher: streaming responses would buffer whole")
		}
		f.Flush()

		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("the wrapped writer is not a Hijacker: the notification WebSocket cannot upgrade")
		}
		if _, _, err := hj.Hijack(); err != nil {
			t.Errorf("Hijack returned %v", err)
		}
	}))
	h.ServeHTTP(under, httptest.NewRequest("GET", "/notification", nil))

	if !under.flushed {
		t.Error("Flush did not reach the writer underneath")
	}
	if !under.hijacked {
		t.Error("Hijack did not reach the writer underneath")
	}
}

// A nil body is what a GET arrives with once the server has finished with it,
// and dereferencing it would turn every read request into a panic.
func TestARequestWithNoBodyDoesNotPanic(t *testing.T) {
	h := CountTraffic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	req := httptest.NewRequest("GET", "/api/silo/v1/server-info", nil)
	req.Body = nil
	h.ServeHTTP(httptest.NewRecorder(), req)
}
