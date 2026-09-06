package store

import (
	"bytes"
	"errors"
	"testing"
)

func TestChunkSealRoundTrips(t *testing.T) {
	plain := pseudoRandom("chunk/roundtrip", 1<<20)
	sc, err := SealChunk(testCK, plain)
	if err != nil {
		t.Fatalf("SealChunk: %v", err)
	}
	if len(sc.Frame) != len(plain)+TagSize {
		t.Errorf("frame is %d bytes for %d of plaintext, want %d more", len(sc.Frame), len(plain), TagSize)
	}
	if sc.ID != ChunkID(sc.Frame) {
		t.Error("the id does not name the frame")
	}
	if sc.PlaintextHash != PlaintextHash(plain) {
		t.Error("the plaintext hash is not H_p")
	}
	got, err := OpenChunk(testCK, sc.PlaintextHash, sc.Frame)
	if err != nil {
		t.Fatalf("OpenChunk: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("the chunk came back different")
	}
}

// The property the whole sync design rests on: the same bytes encrypt to the
// same frame, so an unchanged chunk keeps its id across re-uploads, copies,
// and two members uploading the same file.
func TestChunkSealIsConvergent(t *testing.T) {
	plain := pseudoRandom("chunk/converge", 300000)
	a, err := SealChunk(testCK, plain)
	if err != nil {
		t.Fatal(err)
	}
	b, err := SealChunk(testCK, bytes.Clone(plain))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || !bytes.Equal(a.Frame, b.Frame) {
		t.Fatal("the same chunk encrypted to two different frames")
	}
}

func TestAnotherLibraryCannotReadTheChunk(t *testing.T) {
	plain := pseudoRandom("chunk/otherkey", 100000)
	mine, err := SealChunk(testCK, plain)
	if err != nil {
		t.Fatal(err)
	}
	other, err := SealChunk([]byte("another library's content key!!!"), plain)
	if err != nil {
		t.Fatal(err)
	}
	if mine.ID == other.ID {
		t.Fatal("two libraries produced the same chunk id for the same content")
	}
	if _, err := OpenChunk(testCK, other.PlaintextHash, other.Frame); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("got %v, want ErrDecrypt", err)
	}
}

// H_p is what the key derives from, so the wrong one is the wrong key. This
// is also the reader's verification that the plaintext is the plaintext the
// manifest promised.
func TestTheWrongPlaintextHashDoesNotOpenTheChunk(t *testing.T) {
	sc, err := SealChunk(testCK, pseudoRandom("chunk/hp", 50000))
	if err != nil {
		t.Fatal(err)
	}
	wrong := sc.PlaintextHash
	wrong[0] ^= 1
	if _, err := OpenChunk(testCK, wrong, sc.Frame); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("got %v, want ErrDecrypt", err)
	}
}

func TestATamperedFrameDoesNotOpen(t *testing.T) {
	sc, err := SealChunk(testCK, pseudoRandom("chunk/tamper", 50000))
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []int{0, len(sc.Frame) / 2, len(sc.Frame) - 1} {
		frame := bytes.Clone(sc.Frame)
		frame[at] ^= 0x80
		if _, err := OpenChunk(testCK, sc.PlaintextHash, frame); !errors.Is(err, ErrDecrypt) {
			t.Errorf("a flipped bit at %d was accepted: %v", at, err)
		}
	}
}

// Every sealed container has its own domain, or they share one key space
// under a single content key. This checks the domains are actually distinct
// inputs rather than three spellings that happen to collide.
func TestSealingDomainsDoNotShareKeys(t *testing.T) {
	ad := []byte("some public section")
	plain := []byte("some sealed payload")
	seen := map[string]string{}
	for _, d := range []string{domainManifest, domainDir, domainCommit} {
		frame, err := sealSection(testCK, d, ad, plain)
		if err != nil {
			t.Fatal(err)
		}
		if prev, ok := seen[string(frame)]; ok {
			t.Fatalf("%s and %s produced the same frame", prev, d)
		}
		seen[string(frame)] = d
		if _, err := openSection(testCK, domainChunk, ad, frame); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("a %s frame opened under the chunk domain", d)
		}
	}
}

func TestSealingWithoutAContentKeyIsRefused(t *testing.T) {
	if _, err := SealChunk(nil, []byte("x")); err == nil {
		t.Error("SealChunk accepted an empty key")
	}
	if _, err := OpenChunk(nil, ID{}, []byte("x")); err == nil {
		t.Error("OpenChunk accepted an empty key")
	}
	if _, err := (&Manifest{}).EncodeSealed(nil); err == nil {
		t.Error("EncodeSealed accepted an empty key")
	}
	if _, err := DecodeSealedManifest(nil, nil); err == nil {
		t.Error("DecodeSealedManifest accepted an empty key")
	}
}
