package objmgr

import (
	"bytes"
	"runtime"
	"testing"

	"github.com/SiloDrive/silo/store"
)

// An inline manifest holds its own bytes, not the buffer they were read into.
//
// WriteFile reads store.InlineThreshold bytes before it can know whether a
// file inlines, and a short read leaves that 64 KiB array alive behind the
// small slice the manifest keeps. The manifest is the return value, so it is
// the caller's to hold for as long as it likes -- and a caller holding ten
// thousand of them holds 640 MB against a few MB of actual content.
//
// Measured rather than asserted structurally: whether a slice retains its
// backing array is exactly the kind of thing a later refactor changes by
// accident, and the only honest test is how much heap survives a GC with the
// manifests still reachable.
func TestAnInlineManifestDoesNotRetainTheReadBuffer(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates enough to be worth skipping under -short")
	}
	s := plainStore(t)

	const files = 10000
	const size = 100
	content := bytes.Repeat([]byte("x"), size)

	held := make([]*store.Manifest, 0, files)

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	for i := 0; i < files; i++ {
		m, err := s.WriteFile(bytes.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		if len(m.Inline) != size {
			t.Fatalf("file %d did not inline: %d bytes", i, len(m.Inline))
		}
		held = append(held, m)
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(held)

	perFile := (after.HeapAlloc - before.HeapAlloc) / files
	t.Logf("%d retained inline manifests of %d bytes: %d bytes of heap each",
		files, size, perFile)

	// The content is 100 bytes and a Manifest is a handful of words. The bound
	// is far above that and far below InlineThreshold, so it answers one
	// question only: is the read buffer still attached?
	if perFile >= store.InlineThreshold/8 {
		t.Errorf("each retained manifest holds %d bytes for %d bytes of content — "+
			"the %d-byte read buffer is still attached",
			perFile, size, store.InlineThreshold)
	}
}
