package silod

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkam/silo/fileserver/objstore"
	"github.com/dkam/silo/internal/format"
)

// sealedPackCount is how many sealed packs a library holds across both stores.
//
// By name on disk rather than through objstore, because the question is whether
// the write path produced packs at all -- and an API that answered "none" for a
// store it had not been asked to pack would answer it the same way either side
// of the cutover.
func sealedPackCount(t *testing.T, libraryID string) int {
	t.Helper()
	n := 0
	for _, objType := range objstore.Types {
		entries := packFiles(t, objType, libraryID)
		open := map[string]bool{}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".idx") {
				open[strings.TrimSuffix(e.Name(), ".idx")] = true
			}
		}
		for _, e := range entries {
			if filepath.Ext(e.Name()) == ".pack" && !open[strings.TrimSuffix(e.Name(), ".pack")] {
				n++
			}
		}
	}
	return n
}

// The cutover's acceptance test, and the one storage.md has been promising
// since packs landed: **space comes back**, on a store nobody told to pack.
//
// It is written against the default write path on purpose. Before the flip it
// fails at the first assertion -- the server writes one file per object, so
// there is no pack to compact and the reclaim chain has no last link. After it,
// the whole chain runs: expiry names the cut, the sweep drops what it can, and
// compaction rewrites the packs without the rest.
func TestSpaceComesBackOnADefaultStore(t *testing.T) {
	sqliteTestDB(t)

	// Three commits, each overwriting the same file, so two versions of it are
	// history and only the newest is at head.
	// Sealed, because a clean shutdown is what seals the open pack and gc is an
	// offline operation -- the state it runs against.
	libraryID, _ := packedHistoryFixture(t, 40*24*time.Hour, 30*24*time.Hour, 1*24*time.Hour)

	if sealedPackCount(t, libraryID) == 0 {
		t.Fatal("the default write path produced no packs, so there is nothing for compaction to reclaim")
	}

	before := diskUsage(t, libraryID)

	// The full chain, in the order RunGC runs it.
	window := 14 * 24 * time.Hour
	if _, err := expireHistory(libraryID, window, true); err != nil {
		t.Fatal(err)
	}
	if _, err := sweepOrphans(libraryID, 0, true); err != nil {
		t.Fatal(err)
	}
	if _, err := compactLibrary(libraryID, compactOpts{
		threshold: 0.0, minAge: 0, expire: true, expireWindow: window, del: true,
	}); err != nil {
		t.Fatal(err)
	}

	_, st, head, err := openLibraryAtHead(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	c, err := st.Census(head)
	if err != nil {
		t.Fatal(err)
	}
	if c.Head.Bytes == 0 {
		t.Fatal("the head is empty after the reclaim, which is a different bug entirely")
	}
	if c.History.Bytes != 0 || c.Unreferenced.Bytes != 0 {
		t.Errorf("history=%+v unreferenced=%+v after the full chain, want both empty", c.History, c.Unreferenced)
	}

	after := diskUsage(t, libraryID)
	if after >= before {
		t.Fatalf("the store held %s and now holds %s — nothing came back",
			format.Bytes(before), format.Bytes(after))
	}
	// Within a footer of the head's census: the frames the head needs, one
	// frame's overhead each, and the pack's own header, index and filter. The
	// slack is generous rather than exact because the footer's size is the
	// store's business and asserting it here would restate a constant objstore
	// owns -- what matters is that the leftovers are overhead and not data.
	if slack := c.Head.Bytes/10 + 64*1024; after > c.Head.Bytes+slack {
		t.Errorf("the store holds %s and the head needs %s — more than a footer's worth is left over",
			format.Bytes(after), format.Bytes(c.Head.Bytes))
	}
}
