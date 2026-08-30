package objstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// packScratch is a library directory to write packs into, and the objDir they
// hang off.
func packScratch(t *testing.T) (objDir, libraryID string) {
	t.Helper()
	return t.TempDir(), "lib-under-test"
}

// framed seals some bytes into a real frame, so the tests exercise the thing a
// pack actually holds rather than a stand-in for it. The id is the hash of the
// plaintext, which is what the store guarantees everywhere else.
func framed(t *testing.T, key []byte, plaintext string) (id string, frame []byte) {
	t.Helper()
	sum := sha256.Sum256([]byte(plaintext))
	id = hex.EncodeToString(sum[:])
	frame, err := sealFrame(key, id, []byte(plaintext))
	if err != nil {
		t.Fatalf("sealing %q: %v", plaintext, err)
	}
	return id, frame
}

func packKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, StorageKeySize)
	for i := range k {
		k[i] = byte(i * 7)
	}
	return k
}

// --- the record ---------------------------------------------------------

func TestAnIndexRecordRoundTrips(t *testing.T) {
	want := indexEntry{
		ID:     strings.Repeat("ab", 32),
		Offset: 1 << 40,
		Length: 1<<32 - 1,
	}
	var buf [idxRecordSize]byte
	if err := encodeIndexRecord(buf[:], want); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if got := decodeIndexRecord(buf[:]); got != want {
		t.Errorf("round trip gave %+v, want %+v", got, want)
	}
}

func TestAnIndexRecordRefusesWhatItCannotHold(t *testing.T) {
	var buf [idxRecordSize]byte
	cases := []struct {
		name string
		e    indexEntry
	}{
		{"a short id", indexEntry{ID: "abcd", Offset: 0, Length: 1}},
		{"an id that is not hex", indexEntry{ID: strings.Repeat("zz", 32), Offset: 0, Length: 1}},
		{"a negative offset", indexEntry{ID: strings.Repeat("ab", 32), Offset: -1, Length: 1}},
		{"a length past four bytes", indexEntry{ID: strings.Repeat("ab", 32), Offset: 0, Length: 1 << 32}},
	}
	for _, c := range cases {
		if err := encodeIndexRecord(buf[:], c.e); err == nil {
			t.Errorf("%s was encoded rather than refused", c.name)
		}
	}
}

// The plaintext length is derived rather than stored, so it has to agree with
// what Stat derives from a file size for the same frame.
func TestAnIndexEntryReportsThePlaintextLength(t *testing.T) {
	key := packKey(t)
	id, frame := framed(t, key, "nine byte")

	e := indexEntry{ID: id, Offset: 0, Length: int64(len(frame))}
	if got, want := e.plaintextLen(), int64(len("nine byte")); got != want {
		t.Errorf("plaintextLen = %d, want %d", got, want)
	}
	if got, want := e.plaintextLen(), objectSize(int64(len(frame))); got != want {
		t.Errorf("the index says %d and a file size says %d for the same frame", got, want)
	}
}

// --- the sidecar --------------------------------------------------------

func TestASidecarReadsBackWhatItWrote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.idx")
	s, err := createSidecar(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []indexEntry{
		{ID: strings.Repeat("11", 32), Offset: 8, Length: 100},
		{ID: strings.Repeat("22", 32), Offset: 108, Length: 250},
		{ID: strings.Repeat("00", 32), Offset: 358, Length: 7},
	}
	for _, e := range want {
		if err := s.append(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.sync(); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}

	got, err := readSidecar(path)
	if err != nil {
		t.Fatalf("readSidecar: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("read %d records, wrote %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("record %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Arrival order is preserved, because a pack's frames are in it. Sorting
	// is what sealing does, and it does not disturb the caller's slice.
	sorted := sortedIndex(got)
	if sorted[0].ID != want[2].ID {
		t.Errorf("sortedIndex did not sort by id: first is %s", sorted[0].ID[:4])
	}
	if got[0] != want[0] {
		t.Error("sortedIndex sorted the caller's slice in place")
	}
}

// A crash between the write and the fsync leaves a partial record. That is the
// ordinary shape of an interrupted write here, not damage, and the records
// before it are still the truth about the pack.
func TestASidecarDropsATornTrailingRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.idx")
	s, err := createSidecar(path)
	if err != nil {
		t.Fatal(err)
	}
	whole := indexEntry{ID: strings.Repeat("33", 32), Offset: 8, Length: 40}
	if err := s.append(whole); err != nil {
		t.Fatal(err)
	}
	if err := s.sync(); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}

	for _, torn := range []int{1, idxRecordSize / 2, idxRecordSize - 1} {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(bytes.Repeat([]byte{0xAA}, torn)); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()

		got, err := readSidecar(path)
		if err != nil {
			t.Fatalf("a %d-byte partial record was read as corruption: %v", torn, err)
		}
		if len(got) != 1 || got[0] != whole {
			t.Errorf("with %d torn bytes, read %+v, want just %+v", torn, got, whole)
		}

		if err := os.Truncate(path, int64(idxHeaderSize+idxRecordSize)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestASidecarThatIsNotOneIsRefused(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		body []byte
	}{
		{"empty", nil},
		{"shorter than a header", []byte("SI")},
		{"wrong magic", append([]byte("NOPE"), idxVersion)},
		{"a version this build does not read", append([]byte(idxMagic), 99)},
	}
	for _, c := range cases {
		path := filepath.Join(dir, c.name+".idx")
		if err := os.WriteFile(path, c.body, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := readSidecar(path)
		if !errors.Is(err, ErrIndexCorrupt) {
			t.Errorf("%s: err = %v, want ErrIndexCorrupt", c.name, err)
		}
	}
}

// --- the pack -----------------------------------------------------------

func TestAPackAppendsAndReadsBackEveryFrame(t *testing.T) {
	objDir, lib := packScratch(t)
	key := packKey(t)

	p, err := createPack(objDir, lib)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.close() }()

	bodies := []string{"first", "second, a little longer", "third", strings.Repeat("x", 5000)}
	ids := make([]string, len(bodies))
	for i, body := range bodies {
		id, frame := framed(t, key, body)
		ids[i] = id
		if _, err := p.append(frame, id, true); err != nil {
			t.Fatalf("appending %q: %v", body, err)
		}
	}

	for i, id := range ids {
		e, ok := p.lookup(id)
		if !ok {
			t.Fatalf("%q is not in the pack that just took it", bodies[i])
		}
		frame, err := p.readFrameAt(e)
		if err != nil {
			t.Fatal(err)
		}
		plain, err := openFrame(key, id, frame, nil)
		if err != nil {
			t.Fatalf("the frame at the indexed offset did not open: %v", err)
		}
		if string(plain) != bodies[i] {
			t.Errorf("frame %d holds %q, want %q", i, plain, bodies[i])
		}
	}

	// A pack opens with its magic, and the first frame sits directly after it.
	head := make([]byte, packHeaderSize)
	if _, err := p.f.ReadAt(head, 0); err != nil {
		t.Fatal(err)
	}
	if string(head[:len(packMagic)]) != packMagic {
		t.Errorf("a pack does not begin %q", packMagic)
	}
	if first, _ := p.lookup(ids[0]); first.Offset != int64(packHeaderSize) {
		t.Errorf("the first frame is at %d, want %d", first.Offset, packHeaderSize)
	}
}

func TestAPackKnowsWhenItIsFull(t *testing.T) {
	objDir, lib := packScratch(t)
	key := packKey(t)

	restore := packTarget
	packTarget = 1 << 12
	t.Cleanup(func() { packTarget = restore })

	p, err := createPack(objDir, lib)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.close() }()

	if p.full() {
		t.Error("an empty pack reports itself full")
	}
	id, frame := framed(t, key, strings.Repeat("y", int(packTarget)))
	if _, err := p.append(frame, id, true); err != nil {
		t.Fatal(err)
	}
	if !p.full() {
		t.Errorf("a pack of %d bytes is not full at a target of %d", p.size, packTarget)
	}
}

// Two writers appending at once must not be handed the same offset. Without
// the lock each would compute its own end-of-file, and one would index bytes
// the other wrote.
func TestConcurrentAppendsGetDistinctOffsets(t *testing.T) {
	objDir, lib := packScratch(t)
	key := packKey(t)

	p, err := createPack(objDir, lib)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.close() }()

	const writers = 16
	var wg sync.WaitGroup
	ids := make([]string, writers)
	for i := 0; i < writers; i++ {
		body := fmt.Sprintf("writer %d", i)
		id, frame := framed(t, key, body)
		ids[i] = id
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.append(frame, id, false); err != nil {
				t.Errorf("append: %v", err)
			}
		}()
	}
	wg.Wait()

	seen := map[int64]string{}
	for i, id := range ids {
		e, ok := p.lookup(id)
		if !ok {
			t.Fatalf("writer %d's frame is missing", i)
		}
		if other, clash := seen[e.Offset]; clash {
			t.Fatalf("two frames claim offset %d: %s and %s", e.Offset, other[:8], id[:8])
		}
		seen[e.Offset] = id

		frame, err := p.readFrameAt(e)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := openFrame(key, id, frame, nil); err != nil {
			t.Errorf("writer %d's frame did not open at its indexed offset: %v", i, err)
		}
	}
}

// --- recovery -----------------------------------------------------------

// The interrupt matrix. A pack is written to, the process is imagined to stop
// at each point in append's ordering, and recovery has to produce the same
// store every time: every acknowledged frame present and readable, and nothing
// half-written left where a reader could reach it.
func TestRecoveryTruncatesToTheLastIndexedFrame(t *testing.T) {
	key := packKey(t)

	// Each case leaves the pack and sidecar in the state a crash at that
	// point would, starting from one durable frame followed by an attempt at
	// a second.
	cases := []struct {
		name string
		// stop describes how far the second append got.
		writeFrame  bool
		writeRecord bool
	}{
		{"before the second frame was written", false, false},
		{"after the frame, before the record", true, false},
		{"after both", true, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			objDir, lib := packScratch(t)
			p, err := createPack(objDir, lib)
			if err != nil {
				t.Fatal(err)
			}
			packID := p.id

			firstID, firstFrame := framed(t, key, "the durable one")
			if _, err := p.append(firstFrame, firstID, true); err != nil {
				t.Fatal(err)
			}

			secondID, secondFrame := framed(t, key, "the interrupted one")
			if c.writeFrame {
				if _, err := p.f.Write(secondFrame); err != nil {
					t.Fatal(err)
				}
				if err := p.f.Sync(); err != nil {
					t.Fatal(err)
				}
			}
			if c.writeRecord {
				e := indexEntry{ID: secondID, Offset: p.size, Length: int64(len(secondFrame))}
				if err := p.side.append(e); err != nil {
					t.Fatal(err)
				}
				if err := p.side.sync(); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.close(); err != nil {
				t.Fatal(err)
			}

			r, err := recoverPack(objDir, lib, packID)
			if err != nil {
				t.Fatalf("recovering: %v", err)
			}
			defer func() { _ = r.close() }()

			// The acknowledged frame is always there and always readable.
			e, ok := r.lookup(firstID)
			if !ok {
				t.Fatal("recovery lost a frame that had been fsynced and indexed")
			}
			frame, err := r.readFrameAt(e)
			if err != nil {
				t.Fatal(err)
			}
			if plain, err := openFrame(key, firstID, frame, nil); err != nil {
				t.Errorf("the recovered frame did not open: %v", err)
			} else if string(plain) != "the durable one" {
				t.Errorf("the recovered frame holds %q", plain)
			}

			// The second is present exactly when its record was written, and
			// the file ends where the index says either way.
			_, second := r.lookup(secondID)
			if second != c.writeRecord {
				t.Errorf("the interrupted frame is indexed = %v, want %v", second, c.writeRecord)
			}

			info, err := os.Stat(r.path)
			if err != nil {
				t.Fatal(err)
			}
			want := int64(packHeaderSize) + int64(len(firstFrame))
			if c.writeRecord {
				want += int64(len(secondFrame))
			}
			if info.Size() != want {
				t.Errorf("the recovered pack is %d bytes, want %d", info.Size(), want)
			}
			if r.size != want {
				t.Errorf("the recovered pack appends at %d, want %d", r.size, want)
			}
		})
	}
}

// Recovery leaves a pack that can be appended to, and the frame that follows
// lands where the truncation left off rather than after the discarded bytes.
func TestAPackKeepsTakingFramesAfterRecovery(t *testing.T) {
	objDir, lib := packScratch(t)
	key := packKey(t)

	p, err := createPack(objDir, lib)
	if err != nil {
		t.Fatal(err)
	}
	packID := p.id
	firstID, firstFrame := framed(t, key, "kept")
	if _, err := p.append(firstFrame, firstID, true); err != nil {
		t.Fatal(err)
	}
	// A frame that reached the pack and never got a record.
	if _, err := p.f.Write([]byte("garbage that was never indexed")); err != nil {
		t.Fatal(err)
	}
	if err := p.close(); err != nil {
		t.Fatal(err)
	}

	r, err := recoverPack(objDir, lib, packID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.close() }()

	nextID, nextFrame := framed(t, key, "written after the recovery")
	e, err := r.append(nextFrame, nextID, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(packHeaderSize) + int64(len(firstFrame)); e.Offset != want {
		t.Errorf("the frame after recovery is at %d, want %d — it was written past the discarded bytes", e.Offset, want)
	}

	frame, err := r.readFrameAt(e)
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := openFrame(key, nextID, frame, nil); err != nil {
		t.Errorf("the frame written after recovery did not open: %v", err)
	} else if string(plain) != "written after the recovery" {
		t.Errorf("it holds %q", plain)
	}
}

// A pack shorter than its index cannot be produced by the write order, so it
// is reported rather than repaired: truncating further would turn a
// detectable problem into a silent one.
func TestAPackShorterThanItsIndexIsRefused(t *testing.T) {
	objDir, lib := packScratch(t)
	key := packKey(t)

	p, err := createPack(objDir, lib)
	if err != nil {
		t.Fatal(err)
	}
	packID := p.id
	id, frame := framed(t, key, "indexed and then lost")
	if _, err := p.append(frame, id, true); err != nil {
		t.Fatal(err)
	}
	if err := p.close(); err != nil {
		t.Fatal(err)
	}

	if err := os.Truncate(p.path, int64(packHeaderSize)+4); err != nil {
		t.Fatal(err)
	}

	_, err = recoverPack(objDir, lib, packID)
	if !errors.Is(err, ErrPackCorrupt) {
		t.Fatalf("err = %v, want ErrPackCorrupt", err)
	}
}

func TestAPackThatIsNotOneIsRefused(t *testing.T) {
	objDir, lib := packScratch(t)

	p, err := createPack(objDir, lib)
	if err != nil {
		t.Fatal(err)
	}
	packID := p.id
	if err := p.close(); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		body []byte
	}{
		{"too short for a header", []byte("SIL")},
		{"wrong magic", append([]byte("NOPE"), packVersion, 0, 0, 0)},
		{"a version this build does not read", append([]byte(packMagic), 99, 0, 0, 0)},
	}
	for _, c := range cases {
		if err := os.WriteFile(p.path, c.body, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := recoverPack(objDir, lib, packID)
		if !errors.Is(err, ErrPackCorrupt) {
			t.Errorf("%s: err = %v, want ErrPackCorrupt", c.name, err)
		}
	}
}

// An empty pack — created and never appended to — recovers to itself rather
// than to an error, which is the state a crash right after createPack leaves.
func TestAnEmptyPackRecovers(t *testing.T) {
	objDir, lib := packScratch(t)
	p, err := createPack(objDir, lib)
	if err != nil {
		t.Fatal(err)
	}
	packID := p.id
	if err := p.close(); err != nil {
		t.Fatal(err)
	}

	r, err := recoverPack(objDir, lib, packID)
	if err != nil {
		t.Fatalf("an empty pack did not recover: %v", err)
	}
	defer func() { _ = r.close() }()
	if r.size != int64(packHeaderSize) {
		t.Errorf("an empty recovered pack appends at %d, want %d", r.size, packHeaderSize)
	}
}

// The sidecar on disk is the only marker for "this pack was open", so finding
// it is how a restart knows what to recover.
func TestFindOpenPackNamesTheOneWithASidecar(t *testing.T) {
	objDir, lib := packScratch(t)

	if id, err := findOpenPack(objDir, lib); err != nil || id != "" {
		t.Fatalf("a library with no packs named %q (err %v)", id, err)
	}

	p, err := createPack(objDir, lib)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.close(); err != nil {
		t.Fatal(err)
	}

	id, err := findOpenPack(objDir, lib)
	if err != nil {
		t.Fatal(err)
	}
	if id != p.id {
		t.Errorf("findOpenPack named %q, want %q", id, p.id)
	}

	// Sealing removes the sidecar, and then there is no open pack. That step
	// is not built yet; removing the file is what it will do.
	_, sidePath := packPaths(objDir, lib, p.id)
	if err := os.Remove(sidePath); err != nil {
		t.Fatal(err)
	}
	if id, err := findOpenPack(objDir, lib); err != nil || id != "" {
		t.Errorf("after the sidecar went, findOpenPack named %q (err %v)", id, err)
	}
}

// Packs must be invisible to the loose store's walk, or every pack would be
// reported as an object whose id is its filename. backend_fs.list skips any
// fan-out entry whose name is not two characters, and "packs" is five.
func TestTheLooseWalkDoesNotSeePacks(t *testing.T) {
	dataDir := t.TempDir()
	b, err := newFSBackend(dataDir, TypeChunks)
	if err != nil {
		t.Fatal(err)
	}
	key := packKey(t)
	const lib = "coexisting"

	// One loose object, written through the backend.
	looseID, looseFrame := framed(t, key, "a loose object")
	if err := b.write(lib, looseID, bytes.NewReader(looseFrame), false); err != nil {
		t.Fatal(err)
	}

	// And a pack beside it.
	p, err := createPack(b.objDir, lib)
	if err != nil {
		t.Fatal(err)
	}
	packedID, packedFrame := framed(t, key, "a packed object")
	if _, err := p.append(packedFrame, packedID, true); err != nil {
		t.Fatal(err)
	}
	if err := p.close(); err != nil {
		t.Fatal(err)
	}

	var seen []string
	if err := b.list(lib, func(info packInfo) error {
		seen = append(seen, info.id)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != looseID {
		t.Errorf("the loose walk reported %v, want just the loose object %s", seen, looseID)
	}
}
