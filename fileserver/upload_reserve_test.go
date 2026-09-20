package silod

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/SiloDrive/silo/fileserver/option"
)

// max_upload_size has never had a default, and the zero it leaves behind means
// "no limit" — so the compiled-in behaviour of an install that has not written
// that key is the unbounded one. Every deployment that never read the config
// reference is running with no ceiling on a single request.
func TestAnUnconfiguredUploadLimitIsStillALimit(t *testing.T) {
	libraryID, acct := testLibrary(t)

	oldMax, oldDefault := option.MaxUploadSize, option.DefaultMaxUploadSize
	option.MaxUploadSize = 0 // as an unconfigured server has it
	option.DefaultMaxUploadSize = 64
	t.Cleanup(func() {
		option.MaxUploadSize, option.DefaultMaxUploadSize = oldMax, oldDefault
	})

	vars := map[string]string{"libraryid": libraryID, "path": "big.bin"}
	content := bytes.Repeat([]byte("x"), 4096)

	for _, tc := range []struct {
		name string
		opts []func(*http.Request)
	}{
		{"declared length", nil},
		{"chunked", []func(*http.Request){func(r *http.Request) { r.ContentLength = -1 }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, putEntry, acct, http.MethodPut, "/entries/big.bin", vars, content, tc.opts...)
			if w.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("PUT with no configured limit = %d (%s), want 413", w.Code, w.Body.String())
			}
		})
	}
}

// The quota pre-check runs on the declared length, and a chunked request
// declares nothing — so the delta is zero, every ceiling is asked about a write
// of no bytes, and every one of them says yes. The next look is after the body
// has been read in full, which does refuse the file — but the chunks are
// already on the disk by then, and a refusal does not unwrite them. The bytes
// land whatever the answer is; only the path is withheld.
//
// So the reserve has to be watched during the stream. The probe is faked here
// because the alternative is filling a real volume: it reports room when the
// request is admitted and none once the bytes are arriving, which is the shape
// of a disk running out mid-upload. What the test asserts is that the server
// stopped reading, not merely that it said no.
func TestAnUploadIsRefusedWhenTheReserveGoesDuringTheStream(t *testing.T) {
	libraryID, acct := testLibrary(t)

	oldReserve, oldProbe := option.DiskReserve, availableSpace
	option.DiskReserve = 1 << 20
	var probes int
	availableSpace = func(string) (int64, error) {
		probes++
		if probes <= 1 {
			return 1 << 30, nil // room, at the moment the request is admitted
		}
		return 0, nil // and none by the time the bytes are arriving
	}
	t.Cleanup(func() { option.DiskReserve, availableSpace = oldReserve, oldProbe })

	const offered = 256 << 20
	body := &countingReader{remaining: offered}

	vars := map[string]string{"libraryid": libraryID, "path": "flood.bin"}
	w := do(t, putEntry, acct, http.MethodPut, "/entries/flood.bin", vars, nil,
		func(r *http.Request) {
			r.Body = io.NopCloser(body)
			r.ContentLength = -1
		})
	if w.Code != http.StatusInsufficientStorage {
		t.Fatalf("PUT while the disk ran out = %d (%s), want %d", w.Code, w.Body.String(), http.StatusInsufficientStorage)
	}
	// The refusal has to arrive before the disk does. A server that reads the
	// whole 256 MB and then declines it has already spent the space it was
	// declining to spend.
	if body.read == offered {
		t.Errorf("the whole %d-byte body was read before the refusal; nothing stopped the stream", offered)
	} else {
		t.Logf("stopped after %d of %d bytes", body.read, offered)
	}

	g := do(t, getEntry, acct, http.MethodGet, "/entries/flood.bin", vars, nil)
	if g.Code != http.StatusNotFound {
		t.Errorf("GET after the refusal = %d, want 404", g.Code)
	}
}

// countingReader hands out as many bytes as it was asked to and remembers how
// many were taken.
type countingReader struct {
	remaining int
	read      int
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.remaining == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > c.remaining {
		n = c.remaining
	}
	for i := range p[:n] {
		p[i] = 'f'
	}
	c.remaining -= n
	c.read += n
	return n, nil
}
