package silod

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"testing"

	"github.com/dkam/silo/store"
)

// A body that ends before its Content-Length is not an upload, and the answer
// to it must not be a success. The handler used to return without writing a
// status when the body read failed, on the reasoning that a client that hung
// up has not asked for an answer -- but a half-close, a proxy cutting the
// upstream, or an HTTP/2 stream ending early all read the same way, and in
// each the client is still listening. net/http then sent 200 for an object
// that was never stored, and a client that believed it moved the head over
// the hole.
//
// Over a raw socket, because no HTTP client library will send a short body on
// purpose.
func TestATruncatedObjectPUTIsNotAnswered200(t *testing.T) {
	base, token := wire(t)
	libraryID := makeLibrary(t, base, token)
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}

	full := bytes.Repeat([]byte("cut me short. "), 20)
	for kind, id := range map[string]string{
		"objects": store.ObjectID(full).String(),
		"chunks":  store.ChunkID(full).String(),
	} {
		t.Run(kind, func(t *testing.T) {
			conn, err := net.Dial("tcp", u.Host)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			path := fmt.Sprintf("/api/silo/v1/libraries/%s/%s/%s", libraryID, kind, id)
			fmt.Fprintf(conn, "PUT %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n\r\n",
				path, u.Host, token, len(full))
			if _, err := conn.Write(full[:len(full)/2]); err != nil {
				t.Fatal(err)
			}
			if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
				t.Fatal(err)
			}

			// No response at all is an acceptable answer: a client that reads
			// EOF has not been told anything. A success is not.
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err == nil {
				defer resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					t.Fatalf("a PUT with half its body was answered %d", resp.StatusCode)
				}
			}

			code, _ := call(t, "GET", base+path, token, "")
			if code != http.StatusNotFound {
				t.Fatalf("GET after the truncated PUT = %d, want 404: something was stored", code)
			}
		})
	}
}
