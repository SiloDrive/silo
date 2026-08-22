package store

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"testing"
)

var testCK = []byte("a thirty-two byte library key!!!")

// chunkedManifest builds a plain library's manifest for a file of n
// pseudo-random bytes, the way a client would: chunk, hash, record.
func chunkedManifest(t *testing.T, label string, n int) *Manifest {
	t.Helper()
	m := &Manifest{FileSize: int64(n)}
	for _, ch := range chunkAll(t, testParams(), pseudoRandom(label, n)) {
		m.Chunks = append(m.Chunks, ChunkRef{ID: ChunkID(ch.Data), Size: int64(len(ch.Data))})
	}
	return m
}

// sealedManifest is the same file in an E2EE library: every chunk is
// encrypted first, so the id names ciphertext and H_p is the only record of
// what the plaintext was.
func sealedManifest(t *testing.T, label string, n int) *Manifest {
	t.Helper()
	m := &Manifest{FileSize: int64(n)}
	for _, ch := range chunkAll(t, DefaultParams(ChunkerSeed(testCK)), pseudoRandom(label, n)) {
		sc, err := SealChunk(testCK, ch.Data)
		if err != nil {
			t.Fatalf("SealChunk: %v", err)
		}
		m.Chunks = append(m.Chunks, ChunkRef{
			ID:            sc.ID,
			Size:          int64(len(ch.Data)),
			PlaintextHash: sc.PlaintextHash,
		})
	}
	return m
}

func TestPlainManifestRoundTrips(t *testing.T) {
	want := chunkedManifest(t, "manifest/plain", 8<<20)
	encoded, err := want.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := DecodeManifest(encoded)
	if err != nil {
		t.Fatalf("DecodeManifest: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip changed the manifest")
	}
}

func TestSealedManifestRoundTripsWithItsPlaintextHashes(t *testing.T) {
	want := sealedManifest(t, "manifest/sealed", 8<<20)
	encoded, err := want.EncodeSealed(testCK)
	if err != nil {
		t.Fatalf("EncodeSealed: %v", err)
	}
	got, err := DecodeSealedManifest(encoded, testCK)
	if err != nil {
		t.Fatalf("DecodeSealedManifest: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip changed the manifest")
	}
	// The hashes a reader needs to derive chunk keys must not be readable
	// from the object: that is the whole reason for the sealed section.
	for _, c := range want.Chunks {
		if bytes.Contains(encoded, c.PlaintextHash[:]) {
			t.Fatal("a plaintext hash appears in the clear in the encoded manifest")
		}
	}
}

func TestSmallFilesInlineInBothLibraryTypes(t *testing.T) {
	data := pseudoRandom("manifest/inline", 4000)
	want := &Manifest{FileSize: int64(len(data)), Inline: data}

	plain, err := want.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if got, err := DecodeManifest(plain); err != nil || !bytes.Equal(got.Inline, data) {
		t.Fatalf("plain inline round trip: %v", err)
	}

	sealed, err := want.EncodeSealed(testCK)
	if err != nil {
		t.Fatalf("EncodeSealed: %v", err)
	}
	if bytes.Contains(sealed, data[:64]) {
		t.Fatal("an E2EE inline manifest carries the file's bytes in the clear")
	}
	got, err := DecodeSealedManifest(sealed, testCK)
	if err != nil {
		t.Fatalf("DecodeSealedManifest: %v", err)
	}
	if !bytes.Equal(got.Inline, data) {
		t.Fatal("sealed inline round trip lost the bytes")
	}
}

func TestAnEmptyFileIsAnInlineManifest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		encode func(*Manifest) ([]byte, error)
		decode func([]byte) (*Manifest, error)
	}{
		{"plain", (*Manifest).Encode, DecodeManifest},
		{"sealed",
			func(m *Manifest) ([]byte, error) { return m.EncodeSealed(testCK) },
			func(b []byte) (*Manifest, error) { return DecodeSealedManifest(b, testCK) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := tc.encode(&Manifest{})
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got, err := tc.decode(encoded)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.FileSize != 0 || len(got.Chunks) != 0 || len(got.Inline) != 0 {
				t.Fatalf("got %+v, want an empty file", got)
			}
		})
	}
}

// Convergence is the property everything else rests on: identical content
// must produce an identical object, hence an identical id. A random nonce or
// a writer-chosen inlining decision would break it, and the symptom would be
// spurious changes?since= traffic rather than an error.
func TestIdenticalContentEncodesIdentically(t *testing.T) {
	a := sealedManifest(t, "manifest/converge", 6<<20)
	b := sealedManifest(t, "manifest/converge", 6<<20)
	for _, tc := range []struct {
		name   string
		encode func(*Manifest) ([]byte, error)
	}{
		{"plain", (*Manifest).Encode},
		{"sealed", func(m *Manifest) ([]byte, error) { return m.EncodeSealed(testCK) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ea, err := tc.encode(a)
			if err != nil {
				t.Fatal(err)
			}
			eb, err := tc.encode(b)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(ea, eb) {
				t.Fatal("the same file encoded to two different manifests")
			}
			if ObjectID(ea) != ObjectID(eb) {
				t.Fatal("the same file produced two manifest ids")
			}
		})
	}
}

func TestADifferentContentKeyCannotOpenTheManifest(t *testing.T) {
	encoded, err := sealedManifest(t, "manifest/wrongkey", 4<<20).EncodeSealed(testCK)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeSealedManifest(encoded, []byte("some other library's key........")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("got %v, want ErrDecrypt", err)
	}
}

// The public section is authenticated, and the key derives from it — so a
// server editing a chunk id, a size, or the header does not merely fail the
// tag, it fails to produce a key that could ever have made this ciphertext.
func TestEditingThePublicSectionBreaksTheSeal(t *testing.T) {
	m := sealedManifest(t, "manifest/tamper", 6<<20)
	encoded, err := m.EncodeSealed(testCK)
	if err != nil {
		t.Fatal(err)
	}
	// One byte in the first chunk id, one in the header's flags-adjacent
	// size field, and one in the last entry's size.
	for _, at := range []int{0, 1, 3, 10, len(encoded) - TagSize - IDSize - 4} {
		tampered := bytes.Clone(encoded)
		tampered[at] ^= 0x01
		if _, err := DecodeSealedManifest(tampered, testCK); err == nil {
			t.Fatalf("a flipped bit at offset %d was accepted", at)
		}
	}
}

// Ordering and membership integrity live in the seal, not in the chunk list —
// chunks themselves are position-free. Swapping two entries must therefore be
// caught, and it is caught by the key changing, not by a range check.
func TestReorderingChunksBreaksTheSeal(t *testing.T) {
	m := sealedManifest(t, "manifest/reorder", 8<<20)
	// Find two adjacent entries whose sizes encode to the same width, so a
	// swap is a straight byte move and the object stays well-formed.
	var i int
	for i = 0; i < len(m.Chunks)-1; i++ {
		if len(appendUvarint(nil, uint64(m.Chunks[i].Size))) ==
			len(appendUvarint(nil, uint64(m.Chunks[i+1].Size))) {
			break
		}
	}
	if i == len(m.Chunks)-1 {
		t.Skip("no two adjacent chunks share a size width in this sample")
	}
	swapped := &Manifest{FileSize: m.FileSize, Chunks: slices.Clone(m.Chunks)}
	swapped.Chunks[i], swapped.Chunks[i+1] = swapped.Chunks[i+1], swapped.Chunks[i]

	original, err := m.EncodeSealed(testCK)
	if err != nil {
		t.Fatal(err)
	}
	reordered, err := swapped.EncodeSealed(testCK)
	if err != nil {
		t.Fatal(err)
	}
	// Graft the original sealed section onto the reordered public one: the
	// attack a portable sealed blob would allow.
	end := len(original) - (len(m.Chunks)*IDSize + TagSize)
	forged := append(bytes.Clone(reordered[:end]), original[end:]...)
	if _, err := DecodeSealedManifest(forged, testCK); err == nil {
		t.Fatal("a reordered manifest carrying the original seal was accepted")
	}
}

func TestALibraryTypeMismatchIsRefusedBeforeParsing(t *testing.T) {
	m := chunkedManifest(t, "manifest/mismatch", 4<<20)
	plain, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := m.EncodeSealed(testCK)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeSealedManifest(plain, testCK); !errors.Is(err, ErrEncoding) {
		t.Errorf("a plain manifest opened as E2EE: %v", err)
	}
	if _, err := DecodeManifest(sealed); !errors.Is(err, ErrEncoding) {
		t.Errorf("an E2EE manifest opened as plain: %v", err)
	}
}

func TestMalformedManifestsAreRefused(t *testing.T) {
	good, err := chunkedManifest(t, "manifest/malformed", 4<<20).Encode()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"empty":              {},
		"header only":        good[:2],
		"unknown version":    append([]byte{2}, good[1:]...),
		"reserved flag set":  append([]byte{good[0], good[1] | 0x40}, good[2:]...),
		"truncated":          good[:len(good)-9],
		"trailing bytes":     append(bytes.Clone(good), 0),
		"inline flag on big": append([]byte{good[0], good[1] | flagInline}, good[2:]...),
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeManifest(b); !errors.Is(err, ErrEncoding) {
				t.Fatalf("got %v, want ErrEncoding", err)
			}
		})
	}
}

// Non-canonical varints are the smallest field in the format and the one that
// silently mints a second encoding of the same object.
func TestNonCanonicalVarintsAreRefused(t *testing.T) {
	if _, _, err := readUvarint([]byte{0x80, 0x00}); !errors.Is(err, ErrEncoding) {
		t.Error("a padded zero was accepted")
	}
	if _, _, err := readUvarint([]byte{0xff, 0x80, 0x00}); !errors.Is(err, ErrEncoding) {
		t.Error("a padded value was accepted")
	}
	if _, _, err := readUvarint([]byte{0x80}); !errors.Is(err, ErrEncoding) {
		t.Error("a truncated varint was accepted")
	}
	for _, v := range []uint64{0, 1, 127, 128, 300, 1 << 20, 1<<64 - 1} {
		got, n, err := readUvarint(appendUvarint(nil, v))
		if err != nil || got != v || n == 0 {
			t.Errorf("%d round tripped as (%d, %d, %v)", v, got, n, err)
		}
	}
}

// Whether a manifest inlines is a function of the file's size, never a
// writer's choice — two clients disagreeing would mint two ids for one file.
func TestInliningIsNotAWritersChoice(t *testing.T) {
	small := &Manifest{FileSize: 4000, Chunks: []ChunkRef{{Size: 4000}}}
	if err := small.Validate(); !errors.Is(err, ErrEncoding) {
		t.Errorf("a chunked manifest for a %d-byte file was accepted", small.FileSize)
	}
	big := &Manifest{FileSize: 1 << 20, Inline: make([]byte, 1<<20)}
	if err := big.Validate(); !errors.Is(err, ErrEncoding) {
		t.Error("an inline manifest for a 1 MiB file was accepted")
	}
}

func TestChunkSizesMustAccountForTheWholeFile(t *testing.T) {
	m := chunkedManifest(t, "manifest/short", 4<<20)
	m.Chunks[0].Size--
	if err := m.Validate(); !errors.Is(err, ErrEncoding) {
		t.Error("chunks that do not total the file size were accepted")
	}
}
