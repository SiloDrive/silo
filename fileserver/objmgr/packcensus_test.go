package objmgr

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SiloDrive/silo/fileserver/objstore"
	"github.com/SiloDrive/silo/store"
)

// seal closes every open pack in the process, which is what a clean shutdown
// does. Nothing measures an open pack, so a test that wants numbers has to get
// here first.
func seal(t *testing.T) {
	t.Helper()
	if err := objstore.Close(); err != nil {
		t.Fatalf("sealing: %v", err)
	}
}

// expunge removes objects from the store the only way a packed store can:
// seal, then rewrite every pack without them.
//
// Since the cutover there is no other way. Remove refuses an object inside a
// pack with ErrReclaimDeferred, so a test that needs one genuinely gone -- an
// expired commit, a directory that damage has taken -- has to do what retention
// plus a compaction run does, which is this.
func expunge(t *testing.T, s *Store, ids ...store.ID) {
	t.Helper()
	seal(t)
	drop := map[string]bool{}
	for _, id := range ids {
		drop[id.String()] = true
	}
	for _, st := range s.stores() {
		stats, err := st.PackStats(s.storeID, func(string) objstore.Reach { return objstore.ReachedHead })
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range stats {
			if _, err := st.CompactPack(s.storeID, p.PackID, func(id string) bool { return !drop[id] }); err != nil {
				t.Fatalf("rewriting %s without %v: %v", p.PackID, ids, err)
			}
		}
	}
}

// packBytes is every sealed pack of one store, end to end. The tests that care
// what is on the disk read this rather than a file per object.
func packBytes(t *testing.T, dataDir, objType, storeID string) []byte {
	t.Helper()
	dir := objstore.PackDir(dataDir, objType, storeID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var all []byte
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".pack" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, raw...)
	}
	if len(all) == 0 {
		t.Fatalf("no packs under %s", dir)
	}
	return all
}

// The assertion this pair exists for: the packs and the census are two
// attributions of one walk, so they have to agree about what is reachable. If
// they can drift, the scheduler will eventually rewrite a pack the census
// still considers live.
func TestPackCensusAgreesWithTheCensus(t *testing.T) {
	s := plainStore(t)

	root := put(t, s, mustEmpty(t, s), "/a.bin", bytes.Repeat([]byte("a"), 200000))
	first := commitOn(t, s, root)
	// A second commit that supersedes the file, with the first as its parent so
	// the superseded bytes are history rather than unreferenced. Without the
	// parent there is no history at all, and the column this test cares most
	// about is silently empty.
	root = put(t, s, root, "/a.bin", bytes.Repeat([]byte("b"), 200000))
	head := commitOn(t, s, root, first)
	seal(t)

	census := mustCensus(t, s, head)
	if census.History.Objects == 0 {
		t.Fatal("no history, so the head-versus-history attribution is untested")
	}
	if census.Unreferenced.Objects == 0 {
		t.Fatal("nothing unreferenced, so the dead-bytes assertion is untested")
	}

	packs, err := s.PackCensus(head)
	if err != nil {
		t.Fatalf("PackCensus: %v", err)
	}
	if len(packs) == 0 {
		t.Fatal("nothing was packed, so this test asserts nothing")
	}

	var frames, live, headBytes, objects int64
	for _, p := range packs {
		frames += p.FrameBytes
		live += p.LiveBytes
		headBytes += p.HeadBytes
		objects += p.Objects
		if p.HeadBytes > p.LiveBytes {
			t.Errorf("pack %s: head %d exceeds live %d", p.PackID, p.HeadBytes, p.LiveBytes)
		}
		if p.LiveBytes > p.FrameBytes {
			t.Errorf("pack %s: live %d exceeds the pack's %d", p.PackID, p.LiveBytes, p.FrameBytes)
		}
	}

	censusObjects := census.Head.Objects + census.History.Objects + census.Unreferenced.Objects
	if objects != censusObjects {
		t.Fatalf("the packs hold %d objects and the census counted %d — with everything packed these are the same set",
			objects, censusObjects)
	}

	// The census counts the bytes a caller stored; a pack counts the frames
	// around them. They differ by one frame's overhead per object, and that is
	// derived here rather than hard-coded so this does not restate a constant
	// the store owns.
	censusBytes := census.Head.Bytes + census.History.Bytes + census.Unreferenced.Bytes
	if (frames-censusBytes)%objects != 0 {
		t.Fatalf("frames total %d and the census %d, over %d objects — that is not a whole overhead per object",
			frames, censusBytes, objects)
	}
	overhead := (frames - censusBytes) / objects
	if overhead <= 0 {
		t.Fatalf("a frame adds %d bytes to what it holds, want more than nothing", overhead)
	}

	if want := census.Head.Bytes + census.Head.Objects*overhead; headBytes != want {
		t.Errorf("packs report %d head bytes, the census %d plus framing = %d", headBytes, census.Head.Bytes, want)
	}
	if want := census.Head.Bytes + census.History.Bytes +
		(census.Head.Objects+census.History.Objects)*overhead; live != want {
		t.Errorf("packs report %d live bytes, the census head+history plus framing = %d", live, want)
	}
	// And the third column is what a compaction would actually reclaim.
	var dead int64
	for _, p := range packs {
		dead += p.DeadBytes()
	}
	if want := census.Unreferenced.Bytes + census.Unreferenced.Objects*overhead; dead != want {
		t.Errorf("packs report %d dead bytes, the census's unreferenced plus framing = %d", dead, want)
	}
}

// A library with nothing unreferenced has nothing to compact, and says so with
// a zero rather than with an absent answer.
func TestAFreshLibraryHasNoDeadBytes(t *testing.T) {
	s := plainStore(t)
	root := put(t, s, mustEmpty(t, s), "/a.bin", bytes.Repeat([]byte("a"), 200000))
	head := commitOn(t, s, root)
	seal(t)

	packs, err := s.PackCensus(head)
	if err != nil {
		t.Fatal(err)
	}
	if len(packs) == 0 {
		t.Fatal("nothing was packed")
	}
	census := mustCensus(t, s, head)

	var dead int64
	for _, p := range packs {
		dead += p.DeadBytes()
	}
	// The one orphan a tree build always leaves: the empty root that put
	// replaced and nothing ever committed. Census names it, so the packs must
	// too — a compaction scheduler that could not see it would never reclaim
	// the one thing there is to reclaim.
	if census.Unreferenced.Objects == 0 {
		t.Fatal("the census found nothing unreferenced, so this test asserts nothing")
	}
	if dead == 0 {
		t.Error("the packs report nothing dead, but the census found unreferenced objects")
	}
}

// An open pack is deliberately not measured: its dead fraction describes a file
// that is a different size by the time anything acts on it. So a store that has
// written but not sealed reports no packs rather than an error — which is also
// what a store written before the cutover reports, for ever.
func TestAnUnsealedStoreMeasuresNoPacks(t *testing.T) {
	s := plainStore(t)
	root := put(t, s, mustEmpty(t, s), "/a.bin", bytes.Repeat([]byte("a"), 200000))
	head := commitOn(t, s, root)

	packs, err := s.PackCensus(head)
	if err != nil {
		t.Fatalf("PackCensus before anything sealed: %v", err)
	}
	if len(packs) != 0 {
		t.Errorf("measured %d packs, and not one of them is sealed", len(packs))
	}
}

// A PackStat is handed back to whichever ObjectStore holds it, so it has to
// say which one that is. Chunks and objects are separate stores and an id is
// only unique within one of them — a stat that did not carry the distinction
// would send a chunk's pack id to the object store, which would report it as
// not a sealed pack of the library and leave the space where it was.
func TestPackCensusSaysWhichStoreEachPackIsIn(t *testing.T) {
	s := plainStore(t)
	// Large enough to be chunked rather than inlined, so both stores are
	// written: the chunks in one, the manifest, directory and commit in the
	// other.
	root := put(t, s, mustEmpty(t, s), "/a.bin", bytes.Repeat([]byte("a"), 200000))
	head := commitOn(t, s, root)
	seal(t)

	packs, err := s.PackCensus(head)
	if err != nil {
		t.Fatalf("PackCensus: %v", err)
	}

	var chunkPacks, objectPacks int
	for _, p := range packs {
		if p.ObjType == objstore.TypeChunks {
			chunkPacks++
			continue
		}
		objectPacks++
	}
	if chunkPacks == 0 {
		t.Error("no pack reported as holding chunks, but a 200KB file was written")
	}
	if objectPacks == 0 {
		t.Error("no pack reported as holding objects, but a commit was written")
	}
}

// The age guard is on the pack rather than on the frame, because an index
// record carries no time. Every frame in a sealed pack was appended before its
// footer was written, so the file's mtime is a lower bound on every frame's
// age — which is the direction a "do not touch anything recent" guard needs.
func TestPackCensusReportsWhenAPackWasSealed(t *testing.T) {
	s := plainStore(t)
	before := time.Now().Add(-time.Second)
	root := put(t, s, mustEmpty(t, s), "/a.bin", bytes.Repeat([]byte("a"), 200000))
	head := commitOn(t, s, root)
	seal(t)
	after := time.Now().Add(time.Second)

	packs, err := s.PackCensus(head)
	if err != nil {
		t.Fatalf("PackCensus: %v", err)
	}
	if len(packs) == 0 {
		t.Fatal("nothing was packed")
	}
	for _, p := range packs {
		if p.SealedAt.Before(before) || p.SealedAt.After(after) {
			t.Errorf("pack %s reports it was sealed at %v, outside the run's window %v..%v",
				p.PackID, p.SealedAt, before, after)
		}
	}
}
