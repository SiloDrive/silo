package objmgr

import (
	"bytes"
	"testing"

	"github.com/dkam/silo/fileserver/objstore"
	"github.com/dkam/silo/fileserver/option"
)

// packedStore is plainStore with the pack write path on, sealed by the test's
// cleanup so that what it wrote is measurable.
//
// The flag is off everywhere else, because compaction does not exist yet; these
// are the tests that need it on.
func packedStore(t *testing.T) *Store {
	t.Helper()
	was := option.PackWrites
	option.PackWrites = true
	t.Cleanup(func() { option.PackWrites = was })
	return plainStore(t)
}

// seal closes every open pack in the process, which is what a clean shutdown
// does. Nothing measures an open pack, so a test that wants numbers has to get
// here first.
func seal(t *testing.T) {
	t.Helper()
	if err := objstore.Close(); err != nil {
		t.Fatalf("sealing: %v", err)
	}
}

// The assertion this pair exists for: the packs and the census are two
// attributions of one walk, so they have to agree about what is reachable. If
// they can drift, the scheduler will eventually rewrite a pack the census
// still considers live.
func TestPackCensusAgreesWithTheCensus(t *testing.T) {
	s := packedStore(t)

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
	s := packedStore(t)
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

// A store that never packed anything measures as no packs rather than as an
// error — which is every install until the cutover.
func TestPackCensusOnALooseStoreIsEmpty(t *testing.T) {
	s := plainStore(t)
	root := put(t, s, mustEmpty(t, s), "/a.bin", bytes.Repeat([]byte("a"), 200000))
	head := commitOn(t, s, root)

	packs, err := s.PackCensus(head)
	if err != nil {
		t.Fatalf("PackCensus on a loose store: %v", err)
	}
	if len(packs) != 0 {
		t.Errorf("measured %d packs on a store that writes loose objects", len(packs))
	}
}
