package store

import (
	"strings"
	"testing"
)

// Every entry point that takes a bare content key refuses one of the wrong
// width.
//
// The width used to be enforced at two boundaries — NewKeyring and WrapCK —
// and nowhere in between, so everything below took any non-empty []byte. HKDF
// absorbs any length, so a wrong-width key derived good subkeys and produced
// stable ids for a key no library can hold. That is not a decryption failure a
// caller would notice; it is a whole library keyed on something the format
// says cannot exist, and the one that got built this way was the published
// vector key itself.
//
// Zero stays refused, as it always was. The widths either side of 32 are the
// ones a hand-written key lands on.
func TestEveryEntryPointRefusesAWrongWidthContentKey(t *testing.T) {
	sealedManifest, err := (&Manifest{}).EncodeSealed(testCK)
	if err != nil {
		t.Fatal(err)
	}
	sealedDir, err := (&Directory{Salt: saltA}).EncodeSealed(testCK)
	if err != nil {
		t.Fatal(err)
	}
	sealedCommit, err := (&Commit{Root: id(7)}).EncodeSealed(testCK)
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := SealChunk(testCK, []byte("some content"))
	if err != nil {
		t.Fatal(err)
	}

	calls := map[string]func(ck []byte) error{
		"SealChunk": func(ck []byte) error {
			_, err := SealChunk(ck, []byte("some content"))
			return err
		},
		"OpenChunk": func(ck []byte) error {
			_, err := OpenChunk(ck, chunk.PlaintextHash, chunk.Frame)
			return err
		},
		"NameKey": func(ck []byte) error {
			_, err := NameKey(ck, saltA)
			return err
		},
		"Manifest.EncodeSealed": func(ck []byte) error {
			_, err := (&Manifest{}).EncodeSealed(ck)
			return err
		},
		"Directory.EncodeSealed": func(ck []byte) error {
			_, err := (&Directory{Salt: saltA}).EncodeSealed(ck)
			return err
		},
		"Commit.EncodeSealed": func(ck []byte) error {
			_, err := (&Commit{Root: id(7)}).EncodeSealed(ck)
			return err
		},
		"DecodeSealedManifest": func(ck []byte) error {
			_, err := DecodeSealedManifest(sealedManifest, ck)
			return err
		},
		"DecodeSealedDirectory": func(ck []byte) error {
			_, err := DecodeSealedDirectory(sealedDir, ck)
			return err
		},
		"DecodeSealedCommit": func(ck []byte) error {
			_, err := DecodeSealedCommit(sealedCommit, ck)
			return err
		},
	}

	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			// The legal key still works, so the test cannot pass by refusing
			// everything.
			if err := call(testCK); err != nil {
				t.Fatalf("a %d-byte key was refused: %v", CKSize, err)
			}
			for _, n := range []int{0, 1, 16, CKSize - 1, CKSize + 1, 64} {
				if err := call(make([]byte, n)); err == nil {
					t.Errorf("a %d-byte content key was accepted", n)
				} else if !strings.Contains(err.Error(), "content key") {
					t.Errorf("a %d-byte key failed for the wrong reason: %v", n, err)
				}
			}
		})
	}
}

// Params.ValidateFor is the seam objmgr.New goes through, so the width check
// there is what keeps a Store from ever being built on a key the format does
// not allow — and what lets Store.HasKey stay a non-empty test rather than a
// second, drifting copy of this rule.
//
// A nil key stays legal here and only here: that is a server holding an E2EE
// library, or a client before it has unwrapped anything, which is the ordinary
// case rather than an error.
func TestValidateForRefusesAWrongWidthContentKey(t *testing.T) {
	p := DefaultParams(ChunkerSeed(testCK))

	if err := p.ValidateFor(true, testCK); err != nil {
		t.Fatalf("the key these params were derived from was refused: %v", err)
	}
	// A keyless caller passes when the seed is not the plain one: it cannot be
	// compared against a key there isn't one of, only against the value it
	// must never be.
	if err := p.ValidateFor(true, nil); err != nil {
		t.Fatalf("a server holding an E2EE library was refused: %v", err)
	}
	if err := DefaultParams(PlainSeed()).ValidateFor(true, nil); err == nil {
		t.Fatal("an E2EE library chunking under the plain seed was accepted")
	}

	for _, n := range []int{1, 16, CKSize - 1, CKSize + 1, 64} {
		err := p.ValidateFor(true, make([]byte, n))
		if err == nil {
			t.Errorf("a %d-byte content key was accepted", n)
			continue
		}
		if !strings.Contains(err.Error(), "content key") {
			t.Errorf("a %d-byte key failed for the wrong reason: %v", n, err)
		}
	}
}
