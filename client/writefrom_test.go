package client

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
)

// zeros is a source with a size and no backing array, so that a test can ask
// for more bytes than it would like to see allocated.
type zeros struct{ left int64 }

func (z *zeros) Read(p []byte) (int, error) {
	if z.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > z.left {
		p = p[:z.left]
	}
	for i := range p {
		p[i] = 0
	}
	z.left -= int64(len(p))
	return len(p), nil
}
func (z *zeros) Close() error { return nil }

// WriteFrom is the streaming write, and a streaming write that reads its
// source into memory first is only streaming from the server's point of view.
// plainLibrary buffered because io.Reader cannot satisfy doStream's replay
// contract -- the 401 retry has to send the body a second time, and a drained
// reader cannot -- so the parameter had to become a factory before this could
// be true.
//
// Measured in total allocation rather than peak, which is the conservative
// direction: io.ReadAll of 64 MiB allocates at least that much and usually
// twice it while the slice grows, and a copy through the transport's buffer
// allocates a rounding error either way. The margin is four-fold so that this
// fails on a regression rather than on a garbage collector.
func TestPlainWriteFromDoesNotBufferTheWholeFile(t *testing.T) {
	const size = 64 << 20

	var got int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		got = n
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	lib := &plainLibrary{c: NewClient(srv.URL), ID: "r1"}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	err := lib.WriteFrom("big.bin", func() (io.ReadCloser, int64, error) {
		return &zeros{left: size}, size, nil
	}, 0)

	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("WriteFrom returned %v", err)
	}
	if got != size {
		t.Fatalf("the server received %d bytes, want %d", got, size)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > size/4 {
		t.Errorf("WriteFrom allocated %d bytes to send %d: it is buffering rather than streaming",
			allocated, size)
	}
}

// The reason the parameter is a factory and not a reader: doStream re-sends
// the body after a 401, so a source that can only be read once cannot be
// retried. This is the property buffering was there to provide, and it has to
// survive the change that removed the buffering.
func TestWriteFromReplaysItsBodyAfterAnUnauthorized(t *testing.T) {
	const content = "the second attempt has to send this too"

	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/silo/v1/auth/login" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"t2"}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL)
	if err := c.Login("someone@example.com", "secret"); err != nil {
		t.Fatalf("login: %v", err)
	}
	lib := &plainLibrary{c: c, ID: "r1"}

	if err := lib.WriteFrom("retried.bin", func() (io.ReadCloser, int64, error) {
		return io.NopCloser(bytes.NewReader([]byte(content))), int64(len(content)), nil
	}, 0); err != nil {
		t.Fatalf("WriteFrom returned %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("the server saw %d write(s), want 2 -- a 401 and the retry", len(bodies))
	}
	for i, b := range bodies {
		if b != content {
			t.Errorf("attempt %d sent %q, want %q", i+1, b, content)
		}
	}
}
