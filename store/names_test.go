package store

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func testNameKey(t *testing.T, salt [DirSaltSize]byte) []byte {
	t.Helper()
	k, err := NameKey(testCK, salt)
	if err != nil {
		t.Fatal(err)
	}
	if len(k) != SIVKeySize {
		t.Fatalf("name key is %d bytes, want %d", len(k), SIVKeySize)
	}
	return k
}

var saltA = [DirSaltSize]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
var saltB = [DirSaltSize]byte{16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}

func TestNameRoundTrips(t *testing.T) {
	key := testNameKey(t, saltA)
	for _, name := range []string{
		"a", "notes.txt", "Ünïcödé — naïve.pdf", "a name with spaces",
		strings.Repeat("x", MaxPlainNameBytes),
	} {
		ct, err := EncryptName(key, name)
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		got, err := DecryptName(key, ct)
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		if got != name {
			t.Fatalf("got %q, want %q", got, name)
		}
	}
}

// Deterministic by design, and it has to be: entries/{path} routes on the
// ciphertext, so the client must be able to compute the same bytes the
// directory object holds. This is Option A's stated equality leak, accepted.
func TestNameEncryptionIsDeterministic(t *testing.T) {
	key := testNameKey(t, saltA)
	a, err := EncryptName(key, "taxes.pdf")
	if err != nil {
		t.Fatal(err)
	}
	b, err := EncryptName(key, "taxes.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("the same name encrypted to two different ciphertexts")
	}
}

// And the leak is confined to one directory, which is the whole reason the key
// is derived per directory rather than per library: with one library-wide key,
// Enc("taxes.pdf") would be a constant the server could look for everywhere.
func TestTheSameNameInTwoDirectoriesIsUnrelated(t *testing.T) {
	here, err := EncryptName(testNameKey(t, saltA), "taxes.pdf")
	if err != nil {
		t.Fatal(err)
	}
	there, err := EncryptName(testNameKey(t, saltB), "taxes.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(here, there) {
		t.Fatal("one filename encrypts to one ciphertext library-wide")
	}
}

func TestAnotherLibraryCannotReadTheName(t *testing.T) {
	ct, err := EncryptName(testNameKey(t, saltA), "payroll.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	other, err := NameKey([]byte("a different library's content key"), saltA)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecryptName(other, ct)
	if !errors.Is(err, ErrDecrypt) {
		t.Fatalf("got %v, want ErrDecrypt", err)
	}
	if got != "" {
		t.Fatalf("a failed decrypt returned %q", got)
	}
}

// A separator or a NUL inside a name is how a directory entry becomes a path
// traversal on whichever client writes it to disk. Refused in both directions:
// a client must not be able to write one, and must not act on one it is
// handed.
func TestPathBytesAreRefusedInBothDirections(t *testing.T) {
	key := testNameKey(t, saltA)
	for _, bad := range []string{"", ".", "..", "../etc/passwd", "a/b", "a\x00b"} {
		if _, err := EncryptName(key, bad); !errors.Is(err, ErrName) {
			t.Errorf("EncryptName(%q) = %v, want ErrName", bad, err)
		}
		// And the same name arriving from a directory object is refused, even
		// though its tag verifies — a buggy writer is not a reason to act on a
		// path.
		s, err := newSIV(key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecryptName(key, s.seal([]byte(bad))); !errors.Is(err, ErrName) {
			t.Errorf("DecryptName(%q) = %v, want ErrName", bad, err)
		}
	}
}

func TestNameLengthCeiling(t *testing.T) {
	key := testNameKey(t, saltA)
	ok := strings.Repeat("n", MaxPlainNameBytes)
	ct, err := EncryptName(key, ok)
	if err != nil {
		t.Fatalf("a %d-byte name was refused: %v", len(ok), err)
	}
	// The ceiling exists so the URL form fits the 255-byte name field.
	if url := NameToURL(ct); len(url) > MaxNameBytes {
		t.Fatalf("the longest name renders to %d URL characters, above %d", len(url), MaxNameBytes)
	}
	if _, err := EncryptName(key, ok+"n"); !errors.Is(err, ErrName) {
		t.Fatalf("a %d-byte name was accepted", len(ok)+1)
	}
}

// base64url unpadded is the URL encoding and nothing else. Directory objects
// carry raw SIV bytes; a port that base64s a name into the object produces a
// different sealing key and a different object id for an identical tree.
func TestURLEncodingIsUnpaddedBase64URL(t *testing.T) {
	key := testNameKey(t, saltA)
	ct, err := EncryptName(key, "quarterly report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	url := NameToURL(ct)
	if strings.ContainsAny(url, "=+/") {
		t.Fatalf("%q is not unpadded base64url", url)
	}
	back, err := NameFromURL(url)
	if err != nil || !bytes.Equal(back, ct) {
		t.Fatalf("URL round trip: %v", err)
	}
	if _, err := NameFromURL(base64.URLEncoding.EncodeToString(ct)); err == nil {
		if len(ct)%3 != 0 {
			t.Error("a padded encoding was accepted")
		}
	}
	if _, err := NameFromURL("not base64!"); !errors.Is(err, ErrName) {
		t.Error("a non-base64 segment was accepted")
	}
	if _, err := NameFromURL(NameToURL(ct[:4])); !errors.Is(err, ErrName) {
		t.Error("a too-short ciphertext was accepted")
	}
}

// The whole path, as a client walks it: encrypt names, put them in a directory
// object, seal it, read it back, decrypt the names.
func TestNamesTravelThroughASealedDirectory(t *testing.T) {
	key := testNameKey(t, saltA)
	want := []string{"archive", "notes.txt", "Ünïcödé.pdf"}

	d := &Directory{Salt: saltA}
	for i, name := range want {
		ct, err := EncryptName(key, name)
		if err != nil {
			t.Fatal(err)
		}
		d.Entries = append(d.Entries, DirEntry{
			ChildID: id(byte(i + 1)), Type: NodeFile, Name: ct,
			Mtime: 1700000000 + int64(i), Mode: 0o644,
		})
	}

	encoded, err := d.EncodeSealed(testCK)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range want {
		if bytes.Contains(encoded, []byte(name)) {
			t.Fatalf("the name %q is readable in the encoded directory", name)
		}
	}

	got, err := DecodeSealedDirectory(encoded, testCK)
	if err != nil {
		t.Fatal(err)
	}
	readKey, err := NameKey(testCK, got.Salt)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range got.Entries {
		name, err := DecryptName(readKey, e.Name)
		if err != nil {
			t.Fatalf("decrypting an entry name: %v", err)
		}
		names = append(names, name)
	}
	// Entry order is by ciphertext, which is unrelated to plaintext order, so
	// compare as sets.
	seen := map[string]bool{}
	for _, n := range names {
		seen[n] = true
	}
	for _, n := range want {
		if !seen[n] {
			t.Fatalf("%q did not survive the round trip; got %q", n, names)
		}
	}
}

func TestNameKeyNeedsAContentKey(t *testing.T) {
	if _, err := NameKey(nil, saltA); err == nil {
		t.Error("NameKey accepted an empty content key")
	}
}
