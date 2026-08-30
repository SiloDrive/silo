package objstore

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// filledPack appends the given frames to a fresh pack and hands back the pack
// and the ids, so a test can go straight to what it is checking.
func filledPack(t *testing.T, objDir, lib string, frames [][]byte, ids []string) *openPack {
	t.Helper()
	p, err := createPack(objDir, lib)
	if err != nil {
		t.Fatalf("createPack: %v", err)
	}
	for i, f := range frames {
		if _, err := p.append(f, ids[i], true); err != nil {
			t.Fatalf("appending %s: %v", ids[i], err)
		}
	}
	return p
}

// someFrames seals n distinct plaintexts, so the tests work on the bytes a
// pack actually holds.
func someFrames(t *testing.T, key []byte, n int) (ids []string, frames [][]byte) {
	t.Helper()
	for i := 0; i < n; i++ {
		id, frame := framed(t, key, fmt.Sprintf("frame number %d, with a body of its own", i))
		ids = append(ids, id)
		frames = append(frames, frame)
	}
	return ids, frames
}

// --- sealing ------------------------------------------------------------

func TestASealedPackHandsBackEveryFrameItWasGiven(t *testing.T) {
	objDir, lib := packScratch(t)
	key := packKey(t)
	ids, frames := someFrames(t, key, 20)

	p := filledPack(t, objDir, lib, frames, ids)
	packID := p.id
	if err := p.seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}

	s, err := openSealedPack(objDir, lib, packID)
	if err != nil {
		t.Fatalf("openSealedPack: %v", err)
	}

	if s.count() != len(ids) {
		t.Fatalf("the sealed index holds %d records, want %d", s.count(), len(ids))
	}
	for i, id := range ids {
		e, ok := s.lookup(id)
		if !ok {
			t.Fatalf("%s is not in the pack it was written to", id)
		}
		frame, err := s.readFrameAt(e)
		if err != nil {
			t.Fatalf("reading %s: %v", id, err)
		}
		if !bytes.Equal(frame, frames[i]) {
			t.Errorf("%s came back as different bytes", id)
		}
		// And it is still a frame this store can open, which is the point of
		// copying frames into a pack rather than re-sealing them.
		plain, err := openFrame(key, id, frame, nil)
		if err != nil {
			t.Fatalf("opening %s out of a sealed pack: %v", id, err)
		}
		want := fmt.Sprintf("frame number %d, with a body of its own", i)
		if string(plain) != want {
			t.Errorf("%s decrypted to %q, want %q", id, plain, want)
		}
		if e.plaintextLen() != int64(len(want)) {
			t.Errorf("%s: index says %d plaintext bytes, want %d", id, e.plaintextLen(), len(want))
		}
	}
}

func TestASealedIndexIsSortedByID(t *testing.T) {
	objDir, lib := packScratch(t)
	ids, frames := someFrames(t, packKey(t), 30)

	p := filledPack(t, objDir, lib, frames, ids)
	packID := p.id
	if err := p.seal(); err != nil {
		t.Fatal(err)
	}
	s, err := openSealedPack(objDir, lib, packID)
	if err != nil {
		t.Fatal(err)
	}

	got := s.entries()
	for i := 1; i < len(got); i++ {
		if got[i-1].ID >= got[i].ID {
			t.Fatalf("record %d (%s) does not sort before record %d (%s) — a binary search over this index is undefined",
				i-1, got[i-1].ID, i, got[i].ID)
		}
	}
	// Sorting must not have disturbed the offsets: every record still has to
	// name where its frame actually is.
	for _, e := range got {
		frame, err := s.readFrameAt(e)
		if err != nil {
			t.Fatalf("reading %s: %v", e.ID, err)
		}
		if !isFrame(frame) || hex.EncodeToString(frame[offID:offID+32]) != e.ID {
			t.Errorf("the record for %s points at something else", e.ID)
		}
	}
}

// The same chunk appended twice is waste rather than ambiguity — both frames
// hold the same object — but a sealed index with a repeated key in it has no
// defined binary search, so the footer keeps one.
func TestASealedIndexHoldsEachIDOnce(t *testing.T) {
	objDir, lib := packScratch(t)
	key := packKey(t)
	id, frame := framed(t, key, "written twice by two writers that both missed dedup")

	p := filledPack(t, objDir, lib, [][]byte{frame, frame}, []string{id, id})
	packID := p.id
	if err := p.seal(); err != nil {
		t.Fatal(err)
	}
	s, err := openSealedPack(objDir, lib, packID)
	if err != nil {
		t.Fatal(err)
	}

	if s.count() != 1 {
		t.Fatalf("the sealed index holds %d records for one id, want 1", s.count())
	}
	e, ok := s.lookup(id)
	if !ok {
		t.Fatal("the id that was written twice is not in the index at all")
	}
	got, err := s.readFrameAt(e)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, frame) {
		t.Error("the surviving record does not point at the frame")
	}
}

func TestSealingTakesTheSidecarAway(t *testing.T) {
	objDir, lib := packScratch(t)
	ids, frames := someFrames(t, packKey(t), 3)

	p := filledPack(t, objDir, lib, frames, ids)
	packID := p.id
	_, sidePath := packPaths(objDir, lib, packID)
	if _, err := os.Stat(sidePath); err != nil {
		t.Fatalf("an open pack has no sidecar: %v", err)
	}

	if err := p.seal(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sidePath); !os.IsNotExist(err) {
		t.Errorf("the sidecar survived sealing (err = %v) — a sealed pack would look open", err)
	}
	// Which is the whole point: nothing now claims this pack is open.
	open, err := findOpenPack(objDir, lib)
	if err != nil {
		t.Fatal(err)
	}
	if open != "" {
		t.Errorf("findOpenPack still names %s after it was sealed", open)
	}
}

func TestSealingAnEmptyPackIsStillASealedPack(t *testing.T) {
	objDir, lib := packScratch(t)
	p, err := createPack(objDir, lib)
	if err != nil {
		t.Fatal(err)
	}
	packID := p.id
	if err := p.seal(); err != nil {
		t.Fatalf("sealing an empty pack: %v", err)
	}
	s, err := openSealedPack(objDir, lib, packID)
	if err != nil {
		t.Fatalf("opening an empty sealed pack: %v", err)
	}
	if s.count() != 0 {
		t.Errorf("an empty pack holds %d records", s.count())
	}
	if _, ok := s.lookup(strings.Repeat("ab", 32)); ok {
		t.Error("an empty pack claims to hold something")
	}
}

// The interrupted-seal case, and the reason sealing needs no journal of its
// own: the sidecar is still the authority, so recovery throws away however
// much footer was written and the second attempt produces the same bytes the
// first would have.
func TestAnInterruptedSealIsRedoneExactly(t *testing.T) {
	objDir, lib := packScratch(t)
	ids, frames := someFrames(t, packKey(t), 12)

	// One pack sealed without interruption, as the answer to compare against.
	clean := filledPack(t, objDir, lib, frames, ids)
	cleanID := clean.id
	if err := clean.seal(); err != nil {
		t.Fatal(err)
	}
	cleanPath, _ := packPaths(objDir, lib, cleanID)
	want, err := os.ReadFile(cleanPath)
	if err != nil {
		t.Fatal(err)
	}

	// The footer this pack would get, so the test can stop part-way through
	// writing it at several different points.
	footer, err := buildFooter(clean.entries)
	if err != nil {
		t.Fatal(err)
	}

	for _, cut := range []int{0, 1, idxHeaderSize, idxHeaderSize + idxRecordSize + 3, len(footer) - 1} {
		t.Run(fmt.Sprintf("%d bytes of footer written", cut), func(t *testing.T) {
			p := filledPack(t, objDir, lib, frames, ids)
			packID := p.id
			dataEnd := p.size
			if err := p.close(); err != nil {
				t.Fatal(err)
			}

			// The crash: some of the footer reached the pack, and the sidecar
			// is untouched because sealing never writes to it.
			packPath, _ := packPaths(objDir, lib, packID)
			f, err := os.OpenFile(packPath, os.O_WRONLY, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteAt(footer[:cut], dataEnd); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}

			// Restart: the sidecar is still there, so this pack is open.
			openID, err := findOpenPack(objDir, lib)
			if err != nil {
				t.Fatal(err)
			}
			if openID != packID {
				t.Fatalf("findOpenPack named %q, want the interrupted pack %q", openID, packID)
			}
			rp, err := recoverPack(objDir, lib, packID)
			if err != nil {
				t.Fatalf("recovering a pack interrupted mid-seal: %v", err)
			}
			if rp.size != dataEnd {
				t.Errorf("recovery left the pack at %d bytes, want %d — the partial footer stayed",
					rp.size, dataEnd)
			}
			if err := rp.seal(); err != nil {
				t.Fatalf("re-sealing: %v", err)
			}

			got, err := os.ReadFile(packPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("a re-sealed pack is %d bytes and the uninterrupted one is %d; sealing is not idempotent",
					len(got), len(want))
			}

			s, err := openSealedPack(objDir, lib, packID)
			if err != nil {
				t.Fatalf("the re-sealed pack does not open: %v", err)
			}
			for _, id := range ids {
				if _, ok := s.lookup(id); !ok {
					t.Fatalf("%s did not survive the interrupted seal", id)
				}
			}
		})
	}
}

// --- refusals -----------------------------------------------------------

func TestAnOpenPackIsNotASealedOne(t *testing.T) {
	objDir, lib := packScratch(t)
	ids, frames := someFrames(t, packKey(t), 4)
	p := filledPack(t, objDir, lib, frames, ids)
	defer p.close()

	_, err := openSealedPack(objDir, lib, p.id)
	if !errors.Is(err, ErrPackCorrupt) {
		t.Errorf("opening an unsealed pack as sealed gave %v, want ErrPackCorrupt", err)
	}
}

func TestASealedPackWithADamagedTailIsRefused(t *testing.T) {
	objDir, lib := packScratch(t)
	ids, frames := someFrames(t, packKey(t), 6)

	seal := func(t *testing.T) (packPath string, packID string) {
		t.Helper()
		p := filledPack(t, objDir, lib, frames, ids)
		if err := p.seal(); err != nil {
			t.Fatal(err)
		}
		path, _ := packPaths(objDir, lib, p.id)
		return path, p.id
	}

	cases := []struct {
		name   string
		damage func(t *testing.T, path string)
	}{
		{"the closing magic is gone", func(t *testing.T, path string) {
			writeAtEnd(t, path, -4, []byte("XXXX"))
		}},
		{"the pack was truncated after sealing", func(t *testing.T, path string) {
			truncateBy(t, path, 1)
		}},
		{"the footer length is longer than the pack", func(t *testing.T, path string) {
			var b [8]byte
			b[0] = 0x7f
			writeAtEnd(t, path, -int64(packTrailerSize), b[:])
		}},
		{"the index length is longer than the footer", func(t *testing.T, path string) {
			var b [8]byte
			b[6] = 0xff
			writeAtEnd(t, path, -int64(packIndexLenSize+len(packMagic)), b[:])
		}},
		{"the index magic is gone", func(t *testing.T, path string) {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			// The index begins right after the frames; find it by reading the
			// trailer the way a reader would.
			trailer := readAt(t, path, info.Size()-int64(packTrailerSize), packTrailerSize)
			footerLen := int64(0)
			for _, c := range trailer[:8] {
				footerLen = footerLen<<8 | int64(c)
			}
			writeAt(t, path, info.Size()-int64(packTrailerSize)-footerLen, []byte("NOPE"))
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path, packID := seal(t)
			c.damage(t, path)
			s, err := openSealedPack(objDir, lib, packID)
			if err == nil {
				_ = s
				t.Fatalf("a pack with %s opened anyway", c.name)
			}
			if !errors.Is(err, ErrPackCorrupt) && !errors.Is(err, ErrIndexCorrupt) && !errors.Is(err, ErrBloomCorrupt) {
				t.Errorf("err = %v, want one of the corrupt-pack errors", err)
			}
		})
	}
}

func TestASealedPackDoesNotHoldWhatWasNeverWrittenToIt(t *testing.T) {
	objDir, lib := packScratch(t)
	ids, frames := someFrames(t, packKey(t), 50)
	p := filledPack(t, objDir, lib, frames, ids)
	packID := p.id
	if err := p.seal(); err != nil {
		t.Fatal(err)
	}
	s, err := openSealedPack(objDir, lib, packID)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 500; i++ {
		var raw [32]byte
		if _, err := rand.Read(raw[:]); err != nil {
			t.Fatal(err)
		}
		if _, ok := s.lookup(hex.EncodeToString(raw[:])); ok {
			t.Fatalf("the pack claims to hold %x, which was never written to it", raw)
		}
	}
	// And an id that is not one is a miss rather than a panic.
	if _, ok := s.lookup("not an id"); ok {
		t.Error("a malformed id was found in the index")
	}
}

// --- the filter ---------------------------------------------------------

// The only guarantee a bloom filter makes: it never says no to something it
// holds. A false negative here would be an object the store has and cannot
// find.
func TestTheFilterNeverDeniesWhatItHolds(t *testing.T) {
	for _, n := range []int{0, 1, 500, 5000} {
		b := newBloom(n)
		added := make([][]byte, n)
		for i := range added {
			raw := make([]byte, 32)
			if _, err := rand.Read(raw); err != nil {
				t.Fatal(err)
			}
			added[i] = raw
			b.add(raw)
		}
		for _, raw := range added {
			if !b.mayContain(raw) {
				t.Fatalf("n=%d: the filter denies %x, which it holds", n, raw)
			}
		}
	}
}

// The rate has to be set against the number of packs rather than per pack: at
// ~12,000 packs a textbook 1% would mean ~120 confirming searches on every
// lookup. This asserts the sizing is in the right order of magnitude, with
// enough slack that it is not a flaky test about randomness.
func TestTheFilterIsFarBelowAOnePercentRate(t *testing.T) {
	const n = 500
	b := newBloom(n)
	for i := 0; i < n; i++ {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		b.add(raw)
	}

	const trials = 100000
	hits := 0
	for i := 0; i < trials; i++ {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		if b.mayContain(raw) {
			hits++
		}
	}
	// The design target is 1e-5 across the store; the power-of-two rounding
	// puts a 500-entry pack well under it. 1e-4 is the bound that fails loudly
	// if the sizing regresses without failing on luck.
	if rate := float64(hits) / trials; rate > 1e-4 {
		t.Errorf("false-positive rate %g over %d trials, want below 1e-4 (%d bits, k=%d)",
			rate, trials, 1<<b.logBits, b.k)
	}
}

// The probes are disjoint ranges of the id, so k of them have to fit inside
// 256 bits. This is the constraint that caps k, and getting it wrong would
// read past the id.
func TestTheProbesFitInsideAnID(t *testing.T) {
	for _, n := range []int{0, 1, 10, 500, 5000, 100000, 5000000} {
		logBits, k := bloomParams(n)
		if int(k)*int(logBits) > idxIDSize*8 {
			t.Errorf("n=%d: %d probes of %d bits need more than the %d bits an id has",
				n, k, logBits, idxIDSize*8)
		}
		if k < 1 {
			t.Errorf("n=%d: k=%d", n, k)
		}
		if logBits < bloomMinLogBits || logBits > bloomMaxLogBits {
			t.Errorf("n=%d: logBits=%d is outside the range this build reads", n, logBits)
		}
	}
}

func TestAFilterRoundTripsThroughItsEncoding(t *testing.T) {
	b := newBloom(300)
	var held [][]byte
	for i := 0; i < 300; i++ {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		held = append(held, raw)
		b.add(raw)
	}

	got, err := parseBloom(b.encode())
	if err != nil {
		t.Fatalf("parseBloom: %v", err)
	}
	if got.k != b.k || got.logBits != b.logBits || got.count != b.count {
		t.Errorf("parameters changed: got k=%d logBits=%d count=%d, want k=%d logBits=%d count=%d",
			got.k, got.logBits, got.count, b.k, b.logBits, b.count)
	}
	if !bytes.Equal(got.bits, b.bits) {
		t.Error("the bits changed")
	}
	for _, raw := range held {
		if !got.mayContain(raw) {
			t.Fatalf("the decoded filter denies %x", raw)
		}
	}
}

func TestAFilterThatIsNotOneIsRefused(t *testing.T) {
	good := newBloom(10).encode()
	cases := []struct {
		name string
		b    []byte
	}{
		{"nothing", nil},
		{"a header and no bits", good[:bloomHeaderSize]},
		{"bad magic", append([]byte("NOPE"), good[4:]...)},
		{"an unknown version", func() []byte {
			b := append([]byte{}, good...)
			b[len(bloomMagic)] = bloomVersion + 1
			return b
		}()},
		{"an exponent no build reads", func() []byte {
			b := append([]byte{}, good...)
			b[len(bloomMagic)+2] = 63
			return b
		}()},
		{"more probes than an id has bits", func() []byte {
			b := append([]byte{}, good...)
			b[len(bloomMagic)+1] = 200
			return b
		}()},
	}
	for _, c := range cases {
		if _, err := parseBloom(c.b); !errors.Is(err, ErrBloomCorrupt) {
			t.Errorf("%s: err = %v, want ErrBloomCorrupt", c.name, err)
		}
	}
}

// --- the format ---------------------------------------------------------

type footerVector struct {
	Name    string       `json:"name"`
	Entries []indexEntry `json:"entries"`
	Footer  string       `json:"footer"`
}

const footerVectorFile = "testdata/packfooter.json"

// The footer's bytes are pinned for the reason the frame's are, one step
// removed: a pack is written once and read for as long as it exists, and a
// format change that goes unnoticed orphans every pack already sealed. These
// are regenerated with -update and the diff reviewed, never refreshed to make
// a failing test pass.
//
// The entries are literals rather than a real pack's, so the vector does not
// move when frame sizes or ids do.
func TestPackFooterVectors(t *testing.T) {
	cases := []struct {
		name    string
		entries []indexEntry
	}{
		{"empty", nil},
		{"one record", []indexEntry{
			{ID: strings.Repeat("11", 32), Offset: 8, Length: 100},
		}},
		{"out of order, with a duplicate", []indexEntry{
			{ID: strings.Repeat("cc", 32), Offset: 8, Length: 90},
			{ID: strings.Repeat("22", 32), Offset: 98, Length: 73},
			{ID: strings.Repeat("cc", 32), Offset: 171, Length: 90},
			{ID: strings.Repeat("7f", 32), Offset: 261, Length: 4096},
		}},
	}

	var got []footerVector
	for _, c := range cases {
		footer, err := buildFooter(c.entries)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got = append(got, footerVector{
			Name:    c.name,
			Entries: c.entries,
			Footer:  hex.EncodeToString(footer),
		})
	}

	encoded, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')

	if *updateVectors {
		if err := os.MkdirAll(filepath.Dir(footerVectorFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(footerVectorFile, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", footerVectorFile, len(encoded))
		return
	}

	want, err := os.ReadFile(footerVectorFile)
	if err != nil {
		t.Fatalf("reading vectors: %v (regenerate with -update)", err)
	}
	if !bytes.Equal(encoded, want) {
		t.Errorf("pack footer vectors changed.\n"+
			"A sealed pack is immutable, so a footer format change orphans every pack\n"+
			"already written. Regenerate with -update only when that is what was\n"+
			"intended.\n\ngot:\n%s\nwant:\n%s", encoded, want)
	}
}

// --- small file helpers -------------------------------------------------

func readAt(t *testing.T, path string, off int64, n int) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := make([]byte, n)
	if _, err := f.ReadAt(b, off); err != nil {
		t.Fatal(err)
	}
	return b
}

func writeAt(t *testing.T, path string, off int64, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(b, off); err != nil {
		t.Fatal(err)
	}
}

// writeAtEnd writes at a negative offset from the end of the file.
func writeAtEnd(t *testing.T, path string, off int64, b []byte) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	writeAt(t, path, info.Size()+off, b)
}

func truncateBy(t *testing.T, path string, n int64) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, info.Size()-n); err != nil {
		t.Fatal(err)
	}
}
