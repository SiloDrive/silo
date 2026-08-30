package objstore

import (
	"fmt"
	"strings"
	"testing"
)

// reachAll answers the same thing for every object, which is what most of
// these tests want: the interesting variable is the pack, not the mark.
func reachAll(r Reach) func(string) Reach {
	return func(string) Reach { return r }
}

func TestPackStatsPartitionAPack(t *testing.T) {
	s, dataDir := writeStore(t)

	bodies := []string{"head one", "head two", "history only", "nothing reaches this"}
	ids := make([]string, len(bodies))
	for i, body := range bodies {
		ids[i] = idOf([]byte(body))
		if err := s.WriteVerified(libraryID, ids[i], strings.NewReader(body), true); err != nil {
			t.Fatal(err)
		}
	}
	// Seal, so there is something to measure: an open pack is deliberately not
	// reported.
	if err := s.packs.close(); err != nil {
		t.Fatal(err)
	}
	if sealed, open := packFiles(t, dataDir, TypeChunks); sealed != 1 || open != 0 {
		t.Fatalf("%d sealed and %d open packs, want 1 and 0", sealed, open)
	}

	reach := func(objID string) Reach {
		switch objID {
		case ids[0], ids[1]:
			return ReachedHead
		case ids[2]:
			return ReachedHistory
		default:
			return Unreached
		}
	}

	stats, err := s.PackStats(libraryID, reach)
	if err != nil {
		t.Fatalf("PackStats: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("measured %d packs, want 1", len(stats))
	}
	got := stats[0]

	if got.Objects != int64(len(bodies)) {
		t.Errorf("counted %d objects, want %d", got.Objects, len(bodies))
	}

	// The partition, which is the property this exists for: head is inside
	// live, live plus dead is the whole pack, and nothing is counted twice.
	framed := func(body string) int64 { return int64(len(body) + frameOverhead) }
	wantHead := framed(bodies[0]) + framed(bodies[1])
	wantLive := wantHead + framed(bodies[2])
	wantTotal := wantLive + framed(bodies[3])

	if got.HeadBytes != wantHead {
		t.Errorf("head is %d, want %d", got.HeadBytes, wantHead)
	}
	if got.LiveBytes != wantLive {
		t.Errorf("live is %d, want %d", got.LiveBytes, wantLive)
	}
	if got.FrameBytes != wantTotal {
		t.Errorf("frames total %d, want %d", got.FrameBytes, wantTotal)
	}
	if got.DeadBytes() != framed(bodies[3]) {
		t.Errorf("dead is %d, want %d", got.DeadBytes(), framed(bodies[3]))
	}
	if got.HeadBytes > got.LiveBytes {
		t.Error("head is not inside live")
	}
	if got.LiveBytes+got.DeadBytes() != got.FrameBytes {
		t.Error("live and dead do not add up to the pack")
	}

	// The file is bigger than its frames, by the footer it carries. That is
	// the number an operator reconciles against the disk.
	if got.FileBytes <= got.FrameBytes {
		t.Errorf("the file is %d bytes and its frames are %d — a sealed pack carries a footer",
			got.FileBytes, got.FrameBytes)
	}
}

func TestDeadFractionIsWhatTheThresholdCompares(t *testing.T) {
	cases := []struct {
		name string
		stat PackStat
		want float64
	}{
		{"nothing reaches it", PackStat{FrameBytes: 100}, 1},
		{"all live", PackStat{FrameBytes: 100, LiveBytes: 100}, 0},
		{"half", PackStat{FrameBytes: 100, LiveBytes: 50}, 0.5},
		// An empty pack has nothing dead in it. A NaN here would be compared
		// against the threshold and would silently answer false.
		{"empty", PackStat{}, 0},
	}
	for _, c := range cases {
		if got := c.stat.DeadFraction(); got != c.want {
			t.Errorf("%s: dead fraction %v, want %v", c.name, got, c.want)
		}
	}
}

// The open pack is not measured. Its dead fraction is not a fact yet — it is
// still being appended to — and nothing compacts an open pack in any case.
func TestPackStatsIgnoresTheOpenPack(t *testing.T) {
	s, dataDir := writeStore(t)
	body := "sitting in an open pack"
	if err := s.WriteVerified(libraryID, idOf([]byte(body)), strings.NewReader(body), true); err != nil {
		t.Fatal(err)
	}
	if _, open := packFiles(t, dataDir, TypeChunks); open != 1 {
		t.Fatal("the write did not open a pack")
	}

	stats, err := s.PackStats(libraryID, reachAll(ReachedHead))
	if err != nil {
		t.Fatalf("PackStats: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("measured %d packs while only an open one exists, want 0", len(stats))
	}
}

// A store that has never packed anything measures as no packs, not as an
// error. Every install is in that state until the cutover.
func TestPackStatsOnALooseStoreIsEmpty(t *testing.T) {
	s, _, _ := packedStore(t, TypeChunks, nil, true)
	stats, err := s.PackStats("a-library-that-was-never-written-to", reachAll(ReachedHead))
	if err != nil {
		t.Fatalf("PackStats on an unpacked library: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("measured %d packs, want 0", len(stats))
	}
}

func TestPackStatsSeesEveryPack(t *testing.T) {
	s, dataDir := writeStore(t)

	wasTarget := packTarget
	packTarget = int64(frameOverhead)*2 + 40
	t.Cleanup(func() { packTarget = wasTarget })

	for i := 0; i < 8; i++ {
		body := fmt.Sprintf("object number %d", i)
		if err := s.WriteVerified(libraryID, idOf([]byte(body)), strings.NewReader(body), true); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.packs.close(); err != nil {
		t.Fatal(err)
	}

	sealed, _ := packFiles(t, dataDir, TypeChunks)
	stats, err := s.PackStats(libraryID, reachAll(Unreached))
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != sealed {
		t.Errorf("measured %d packs, but %d are on disk", len(stats), sealed)
	}
	var objects int64
	seen := map[string]bool{}
	for _, st := range stats {
		if seen[st.PackID] {
			t.Errorf("pack %s was measured twice", st.PackID)
		}
		seen[st.PackID] = true
		objects += st.Objects
		if st.DeadFraction() != 1 {
			t.Errorf("pack %s has dead fraction %v with nothing reachable, want 1", st.PackID, st.DeadFraction())
		}
	}
	if objects != 8 {
		t.Errorf("the packs hold %d objects between them, want 8", objects)
	}
}
