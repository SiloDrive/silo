package objstore

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// The frame vectors pin a format that cannot be changed after the first byte
// is written: storage.key is unrotatable, so every frame ever sealed under it
// has to stay readable by this code. They are regenerated deliberately with
// -update and the diff reviewed, never refreshed to make a failing test pass.
//
// Unlike store/testdata/vectors these are not a cross-implementation
// contract — no client holds storage.key, so nothing else implements this —
// which is why they live here rather than in the spec.
var updateVectors = flag.Bool("update", false, "regenerate testdata/frame.json")

const frameVectorFile = "testdata/frame.json"

func testKey(t *testing.T) []byte {
	t.Helper()
	key, err := hex.DecodeString("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// idOf is the id a plaintext would be stored under: objstore ids are the
// SHA-256 of the stored bytes, in lowercase hex.
func idOf(b []byte) string {
	h := verifierFor("")
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

func TestFrameRoundTrip(t *testing.T) {
	key := testKey(t)
	for _, plain := range [][]byte{
		{},
		[]byte("a"),
		[]byte("the quick brown fox"),
		bytes.Repeat([]byte{0xff}, 100000),
	} {
		id := idOf(plain)
		frame, err := sealFrame(key, id, plain)
		if err != nil {
			t.Fatalf("sealing %d bytes: %v", len(plain), err)
		}
		if len(frame) != len(plain)+frameOverhead {
			t.Errorf("frame is %d bytes for %d of plaintext, want %d more",
				len(frame), len(plain), frameOverhead)
		}
		if !isFrame(frame) {
			t.Error("sealed frame is not recognised as one")
		}
		got, err := openFrame(key, id, frame)
		if err != nil {
			t.Fatalf("opening %d bytes: %v", len(plain), err)
		}
		if !bytes.Equal(got, plain) {
			t.Errorf("round trip changed the bytes")
		}
	}
}

// A fresh nonce per write is what makes the storage frame non-convergent, and
// that is deliberate: ids are minted over plaintext before sealing, so dedup
// and changes?since= never see what the storage layer does underneath them.
func TestFrameNonceIsFreshPerWrite(t *testing.T) {
	key := testKey(t)
	plain := []byte("written twice")
	id := idOf(plain)
	a, err := sealFrame(key, id, plain)
	if err != nil {
		t.Fatal(err)
	}
	b, err := sealFrame(key, id, plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Error("two seals of one plaintext produced identical frames: the nonce is not fresh")
	}
}

// Every byte of the frame is covered: the header by the AEAD's associated
// data, the body by its tag. A frame that opens after any single-byte change
// is a frame whose header is decoration.
func TestFrameTamperIsRejected(t *testing.T) {
	key := testKey(t)
	plain := []byte("tamper with me")
	id := idOf(plain)
	frame, err := sealFrame(key, id, plain)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		at   int
	}{
		{"magic", 0},
		{"version", 4},
		{"id", 5},
		{"ct_len", 37},
		{"nonce", 45},
		{"ciphertext", frameHeaderSize},
		{"tag", len(frame) - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := bytes.Clone(frame)
			bad[tc.at] ^= 0x01
			if _, err := openFrame(key, id, bad); !errors.Is(err, ErrFrameCorrupt) {
				t.Errorf("flipping a bit in %s at %d gave %v, want ErrFrameCorrupt", tc.name, tc.at, err)
			}
		})
	}
}

// A frame is bound to the id it was sealed under, so it cannot be moved to
// another object's path and opened there.
func TestFrameIsBoundToItsID(t *testing.T) {
	key := testKey(t)
	plain := []byte("mine")
	frame, err := sealFrame(key, idOf(plain), plain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openFrame(key, idOf([]byte("yours")), frame); !errors.Is(err, ErrFrameCorrupt) {
		t.Errorf("a frame opened under another id: %v", err)
	}
}

func TestFrameWrongKey(t *testing.T) {
	key := testKey(t)
	plain := []byte("secret")
	id := idOf(plain)
	frame, err := sealFrame(key, id, plain)
	if err != nil {
		t.Fatal(err)
	}
	other := bytes.Clone(key)
	other[0] ^= 0x01
	if _, err := openFrame(other, id, frame); !errors.Is(err, ErrFrameCorrupt) {
		t.Errorf("a frame opened under the wrong key: %v", err)
	}
}

// A truncated frame is the torn-write case, and it must be an error rather
// than a short read: the length in the header is what says the frame is whole.
func TestFrameTruncated(t *testing.T) {
	key := testKey(t)
	plain := bytes.Repeat([]byte("x"), 1000)
	id := idOf(plain)
	frame, err := sealFrame(key, id, plain)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 1, frameHeaderSize - 1, frameHeaderSize, len(frame) - 1} {
		if _, err := openFrame(key, id, frame[:n]); !errors.Is(err, ErrFrameCorrupt) {
			t.Errorf("a frame truncated to %d bytes opened: %v", n, err)
		}
	}
}

// isFrame is what the transitional plaintext fallback turns on, so it has to
// be honest about bytes that were never a frame.
func TestIsFrame(t *testing.T) {
	key := testKey(t)
	frame, err := sealFrame(key, idOf([]byte("x")), []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if !isFrame(frame) {
		t.Error("a frame is not recognised as one")
	}
	for _, plain := range [][]byte{
		{},
		[]byte("SIL"),
		[]byte("an ordinary plaintext object"),
		bytes.Repeat([]byte{0}, 200),
	} {
		if isFrame(plain) {
			t.Errorf("%q was mistaken for a frame", plain)
		}
	}
	// Long enough, and it starts with the magic, and it is still not a frame.
	almost := bytes.Repeat([]byte{0}, 200)
	copy(almost, frameMagic)
	if _, err := openFrame(key, idOf([]byte("x")), almost); !errors.Is(err, ErrFrameCorrupt) {
		t.Errorf("plaintext beginning with the magic opened as a frame: %v", err)
	}
}

func TestFrameKeyMustBe32Bytes(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33} {
		if _, err := sealFrame(make([]byte, n), idOf(nil), nil); err == nil {
			t.Errorf("sealing under a %d-byte key succeeded", n)
		}
	}
}

func TestFrameRejectsBadID(t *testing.T) {
	key := testKey(t)
	for _, id := range []string{"", "abc", "ZZ" + idOf(nil)[2:]} {
		if _, err := sealFrame(key, id, nil); err == nil {
			t.Errorf("sealing under id %q succeeded", id)
		}
	}
}

// frameVector is one committed (key, id, nonce, plaintext) → frame.
type frameVector struct {
	Name      string `json:"name"`
	Key       string `json:"key"`
	ID        string `json:"id"`
	Nonce     string `json:"nonce"`
	Plaintext string `json:"plaintext"`
	Frame     string `json:"frame"`
}

func TestFrameVectors(t *testing.T) {
	key := testKey(t)
	nonce, err := hex.DecodeString("0b0a09080706050403020100")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		plain []byte
	}{
		{"empty", []byte{}},
		{"one byte", []byte("a")},
		{"a sentence", []byte("the quick brown fox")},
		{"a kilobyte of zeroes", make([]byte, 1024)},
	}

	var got []frameVector
	for _, c := range cases {
		id := idOf(c.plain)
		frame, err := sealFrameNonce(key, id, c.plain, nonce)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		// Every vector must also open, or a committed vector could pin a
		// frame this code cannot read.
		plain, err := openFrame(key, id, frame)
		if err != nil {
			t.Fatalf("%s: opening what we just sealed: %v", c.name, err)
		}
		if !bytes.Equal(plain, c.plain) {
			t.Fatalf("%s: round trip changed the bytes", c.name)
		}
		got = append(got, frameVector{
			Name:      c.name,
			Key:       hex.EncodeToString(key),
			ID:        id,
			Nonce:     hex.EncodeToString(nonce),
			Plaintext: hex.EncodeToString(c.plain),
			Frame:     hex.EncodeToString(frame),
		})
	}

	encoded, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')

	if *updateVectors {
		if err := os.MkdirAll(filepath.Dir(frameVectorFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(frameVectorFile, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", frameVectorFile, len(encoded))
		return
	}

	want, err := os.ReadFile(frameVectorFile)
	if err != nil {
		t.Fatalf("reading vectors: %v (regenerate with -update)", err)
	}
	if !bytes.Equal(encoded, want) {
		t.Errorf("frame vectors changed.\n"+
			"The storage key is unrotatable, so a frame format change orphans every\n"+
			"frame already written under it. Regenerate with -update only when that\n"+
			"is what was intended.\n\ngot:\n%s\nwant:\n%s", encoded, want)
	}
}

// A frame sealed with a random nonce is what production writes; this checks
// the nonce actually reaches the header, rather than being generated and
// dropped.
func TestFrameNonceIsInTheHeader(t *testing.T) {
	key := testKey(t)
	plain := []byte("nonce me")
	id := idOf(plain)
	frame, err := sealFrame(key, id, plain)
	if err != nil {
		t.Fatal(err)
	}
	nonce := frame[frameHeaderSize-frameNonceSize : frameHeaderSize]
	again, err := sealFrameNonce(key, id, plain, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(frame, again) {
		t.Error("resealing under the frame's own nonce gave different bytes")
	}
	if bytes.Equal(nonce, make([]byte, frameNonceSize)) {
		t.Error("the nonce is all zeroes")
	}
	_ = rand.Read
}
