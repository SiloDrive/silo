package store

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func testCommit() *Commit {
	return &Commit{
		Root:      id(0x42),
		Parents:   []ID{id(0x01), id(0x02)},
		CreatedAt: 1700000000,
		Author:    "dan@example.org",
		Message:   "moved the photos",
	}
}

func TestCommitRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		name   string
		encode func(*Commit) ([]byte, error)
		decode func([]byte) (*Commit, error)
	}{
		{"plain", (*Commit).Encode, DecodeCommit},
		{"sealed",
			func(c *Commit) ([]byte, error) { return c.EncodeSealed(testCK) },
			func(b []byte) (*Commit, error) { return DecodeSealedCommit(b, testCK) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := testCommit()
			encoded, err := tc.encode(want)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got, err := tc.decode(encoded)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("round trip changed the commit:\n got %+v\nwant %+v", got, want)
			}
		})
	}
}

// The root id is public in both library types, and it has to be: it is the
// first edge of every server-side walk — GC's mark, changes?since= — and a
// sealed root would leave the server unable to trace an E2EE library at all.
func TestTheRootIDIsPublicInBothLibraryTypes(t *testing.T) {
	c := testCommit()
	for _, tc := range []struct {
		name   string
		encode func(*Commit) ([]byte, error)
	}{
		{"plain", (*Commit).Encode},
		{"sealed", func(c *Commit) ([]byte, error) { return c.EncodeSealed(testCK) }},
	} {
		encoded, err := tc.encode(c)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(encoded, c.Root[:]) {
			t.Errorf("%s: the root id is not readable in the object", tc.name)
		}
		for _, p := range c.Parents {
			if !bytes.Contains(encoded, p[:]) {
				t.Errorf("%s: a parent id is not readable in the object", tc.name)
			}
		}
	}
}

func TestSealedCommitsHideWhoDidWhat(t *testing.T) {
	c := testCommit()
	plain, err := c.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(plain, []byte(c.Author)) || !bytes.Contains(plain, []byte(c.Message)) {
		t.Error("a plain commit lost its attribution — history must not go anonymous")
	}
	sealed, err := c.EncodeSealed(testCK)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(c.Author)) || bytes.Contains(sealed, []byte(c.Message)) {
		t.Error("an E2EE commit published its author or message")
	}
}

// The splice: a sealed section lifted from one commit onto another. It worked
// only while the sealed blob was portable between commits, and one tag over
// the root and the parent chain ends it.
func TestASealedSectionIsNotPortableBetweenCommits(t *testing.T) {
	mine := testCommit()
	theirs := testCommit()
	theirs.Root = id(0x99)

	a, err := mine.EncodeSealed(testCK)
	if err != nil {
		t.Fatal(err)
	}
	b, err := theirs.EncodeSealed(testCK)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatal("the two commits differ in length")
	}
	sealedLen := len(appendUvarint(nil, uint64(len(mine.Author)))) + len(mine.Author) +
		len(appendUvarint(nil, uint64(len(mine.Message)))) + len(mine.Message)
	at := len(a) - (sealedLen + TagSize)
	forged := append(bytes.Clone(b[:at]), a[at:]...)
	if _, err := DecodeSealedCommit(forged, testCK); err == nil {
		t.Fatal("a commit carrying another's seal was accepted")
	}
}

func TestACommitWithNoAttributionRoundTrips(t *testing.T) {
	c := &Commit{Root: id(7)}
	for _, tc := range []struct {
		name   string
		encode func(*Commit) ([]byte, error)
		decode func([]byte) (*Commit, error)
	}{
		{"plain", (*Commit).Encode, DecodeCommit},
		{"sealed",
			func(c *Commit) ([]byte, error) { return c.EncodeSealed(testCK) },
			func(b []byte) (*Commit, error) { return DecodeSealedCommit(b, testCK) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := tc.encode(c)
			if err != nil {
				t.Fatal(err)
			}
			got, err := tc.decode(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if got.Author != "" || got.Message != "" || len(got.Parents) != 0 {
				t.Fatalf("got %+v, want an empty commit", got)
			}
		})
	}
}

// Every empty sealed section would share one key if the derivation did not
// hash the public bytes. It does, so two commits with nothing sealed still get
// distinct keys from their distinct roots.
func TestTwoEmptyCommitsDoNotShareAKey(t *testing.T) {
	a, err := (&Commit{Root: id(1)}).EncodeSealed(testCK)
	if err != nil {
		t.Fatal(err)
	}
	b, err := (&Commit{Root: id(2)}).EncodeSealed(testCK)
	if err != nil {
		t.Fatal(err)
	}
	tail := func(b []byte) []byte { return b[len(b)-TagSize:] }
	if bytes.Equal(tail(a), tail(b)) {
		t.Fatal("two commits with empty sealed sections produced the same tag")
	}
}

func TestCommitBoundsAreEnforced(t *testing.T) {
	tooMany := testCommit()
	tooMany.Parents = make([]ID, MaxParents+1)
	if _, err := tooMany.Encode(); !errors.Is(err, ErrEncoding) {
		t.Errorf("parents: got %v, want ErrEncoding", err)
	}
	longAuthor := testCommit()
	longAuthor.Author = strings.Repeat("a", MaxAuthorBytes+1)
	if _, err := longAuthor.Encode(); !errors.Is(err, ErrEncoding) {
		t.Errorf("author: got %v, want ErrEncoding", err)
	}
	longMessage := testCommit()
	longMessage.Message = strings.Repeat("m", MaxMessageBytes+1)
	if _, err := longMessage.Encode(); !errors.Is(err, ErrEncoding) {
		t.Errorf("message: got %v, want ErrEncoding", err)
	}
}

func TestCommitTimestampsClampAndReject(t *testing.T) {
	c := testCommit()
	c.CreatedAt = -1
	encoded, err := c.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeCommit(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt != 0 {
		t.Errorf("a negative timestamp survived as %d", got.CreatedAt)
	}

	c.CreatedAt = 0
	encoded, err = c.Encode()
	if err != nil {
		t.Fatal(err)
	}
	at := 2 + IDSize + 1 + 2*IDSize // version, flags, root, parent count, parents
	tampered := append(bytes.Clone(encoded[:at]), appendUvarint(nil, MaxTimestamp+1)...)
	tampered = append(tampered, encoded[at+1:]...)
	if _, err := DecodeCommit(tampered); !errors.Is(err, ErrEncoding) {
		t.Errorf("got %v, want an out-of-range timestamp rejected", err)
	}
}

func TestMalformedCommitsAreRefused(t *testing.T) {
	good, err := testCommit().Encode()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"empty":             {},
		"header only":       good[:2],
		"unknown version":   append([]byte{4}, good[1:]...),
		"reserved flag set": append([]byte{good[0], flagInline}, good[2:]...),
		"truncated":         good[:len(good)-3],
		"trailing bytes":    append(bytes.Clone(good), 0),
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeCommit(b); !errors.Is(err, ErrEncoding) {
				t.Fatalf("got %v, want ErrEncoding", err)
			}
		})
	}
	if _, err := DecodeSealedCommit(good, testCK); !errors.Is(err, ErrEncoding) {
		t.Errorf("a plain commit opened as E2EE: %v", err)
	}
}
