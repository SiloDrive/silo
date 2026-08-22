package store

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
)

// Published AES-SIV vectors, at both the width the RFC documents and the
// width this format uses.
//
// The distinction matters and is easy to skip: RFC 5297's own appendix covers
// only AES-SIV with 128-bit components — a 32-byte total key. Silo's names use
// AES-256 components, a 64-byte key the RFC's vectors never touch. So both
// widths are run here: the 32-byte cases prove the construction against the
// specification, and the 64-byte cases prove the production width against an
// independent implementation's published set (miscreant's
// cross-implementation vectors, which include the 256-bit-subkey cases).
//
// Between them they also cover the two S2V branches and the CTR bit-clearing:
// the deterministic example's 14-byte plaintext takes the short branch, the
// nonce-based example's 47-byte plaintext takes xorend, and every case would
// fail if bits 31 and 63 of the synthetic IV were not cleared before counting.
func TestSIVMatchesPublishedVectors(t *testing.T) {
	cases := []struct {
		name       string
		key        string
		ad         []string
		plaintext  string
		ciphertext string
	}{
		{
			// RFC 5297 A.1
			name:       "rfc5297/deterministic/128-bit subkeys",
			key:        "fffefdfcfbfaf9f8f7f6f5f4f3f2f1f0f0f1f2f3f4f5f6f7f8f9fafbfcfdfeff",
			ad:         []string{"101112131415161718191a1b1c1d1e1f2021222324252627"},
			plaintext:  "112233445566778899aabbccddee",
			ciphertext: "85632d07c6e8f37f950acd320a2ecc9340c02b9690c4dc04daef7f6afe5c",
		},
		{
			// RFC 5297 A.2 — three associated-data items, so the S2V
			// chaining loop actually runs.
			name: "rfc5297/nonce-based/128-bit subkeys",
			key:  "7f7e7d7c7b7a79787776757473727170404142434445464748494a4b4c4d4e4f",
			ad: []string{
				"00112233445566778899aabbccddeeffdeaddadadeaddadaffeeddccbbaa99887766554433221100",
				"102030405060708090a0",
				"09f911029d74e35bd84156c5635688c0",
			},
			plaintext: "7468697320697320736f6d6520706c61696e7465787420746f20656e6372797074207573696e67205349562d414553",
			ciphertext: "7bdb6e3b432667eb06f4d14bff2fbd0fcb900f2fddbe404326601965c889bf17" +
				"dba77ceb094fa663b7a3f748ba8af829ea64ad544a272e9c485b62a3fd5c0d",
		},
		{
			name:       "empty ad and plaintext/128-bit subkeys",
			key:        "fffefdfcfbfaf9f8f7f6f5f4f3f2f1f0f0f1f2f3f4f5f6f7f8f9fafbfcfdfeff",
			ad:         nil,
			plaintext:  "",
			ciphertext: "f2007a5beb2b8900c588a7adf599f172",
		},
		{
			// Exactly on the 16-byte boundary: the first length that takes
			// xorend rather than the pad-and-double branch.
			name:       "block-sized plaintext/128-bit subkeys",
			key:        "fffefdfcfbfaf9f8f7f6f5f4f3f2f1f0f0f1f2f3f4f5f6f7f8f9fafbfcfdfeff",
			ad:         nil,
			plaintext:  "00112233445566778899aabbccddeeff",
			ciphertext: "f304f912863e303d5b540e5057c7010c942ffaf45b0e5ca5fb9a56a5263bb065",
		},
		{
			// The production width. Same construction, AES-256 halves.
			name: "miscreant/deterministic/256-bit subkeys",
			key: "fffefdfcfbfaf9f8f7f6f5f4f3f2f1f06f6e6d6c6b6a696867666564636261" +
				"60f0f1f2f3f4f5f6f7f8f9fafbfcfdfeff000102030405060708090a0b0c0d0e0f",
			ad:         []string{"101112131415161718191a1b1c1d1e1f2021222324252627"},
			plaintext:  "112233445566778899aabbccddee",
			ciphertext: "f125274c598065cfc26b0e71575029088b035217e380cac8919ee800c126",
		},
		{
			name: "miscreant/nonce-based/256-bit subkeys",
			key: "7f7e7d7c7b7a797877767574737271706f6e6d6c6b6a696867666564636261" +
				"60404142434445464748494a4b4c4d4e4f505152535455565758595a5b5b5d5e5f",
			ad: []string{
				"00112233445566778899aabbccddeeffdeaddadadeaddadaffeeddccbbaa99887766554433221100",
				"102030405060708090a0",
				"09f911029d74e35bd84156c5635688c0",
			},
			plaintext: "7468697320697320736f6d6520706c61696e7465787420746f20656e6372797074207573696e67205349562d414553",
			ciphertext: "85b8167310038db7dc4692c0281ca35868181b2762f3c24f2efa5fb80cb14351" +
				"6ce6c434b898a6fd8eb98a418842f51f66fc67de43ac185a66dd72475bbb08",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := newSIV(mustHex(t, tc.key))
			if err != nil {
				t.Fatal(err)
			}
			var ad [][]byte
			for _, a := range tc.ad {
				ad = append(ad, mustHex(t, a))
			}
			got := s.seal(mustHex(t, tc.plaintext), ad...)
			if hex.EncodeToString(got) != tc.ciphertext {
				t.Fatalf("seal:\n got %s\nwant %s", hex.EncodeToString(got), tc.ciphertext)
			}
			back, err := s.open(mustHex(t, tc.ciphertext), ad...)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if !bytes.Equal(back, mustHex(t, tc.plaintext)) {
				t.Fatalf("open returned %x", back)
			}
		})
	}
}

// The short branch of S2V is the hot path for names — most filenames are under
// sixteen bytes — and it is the branch a hand-written implementation is most
// likely to get wrong, because it is the one the RFC's headline example does
// not exercise. Every length either side of the boundary must round-trip and
// must differ from its neighbours.
func TestSIVBothS2VBranchesAcrossTheBoundary(t *testing.T) {
	s, err := newSIV(mustHex(t,
		"fffefdfcfbfaf9f8f7f6f5f4f3f2f1f06f6e6d6c6b6a696867666564636261"+
			"60f0f1f2f3f4f5f6f7f8f9fafbfcfdfeff000102030405060708090a0b0c0d0e0f"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, n := range []int{0, 1, 14, 15, 16, 17, 31, 32, 33, 175} {
		plain := pseudoRandom("siv/boundary", n)
		sealed := s.seal(plain)
		if len(sealed) != n+SIVOverhead {
			t.Fatalf("%d bytes sealed to %d, want %d", n, len(sealed), n+SIVOverhead)
		}
		if prev, ok := seen[string(sealed[:SIVOverhead])]; ok {
			t.Fatalf("lengths %d and %d produced the same synthetic IV", prev, n)
		}
		seen[string(sealed[:SIVOverhead])] = n

		back, err := s.open(sealed)
		if err != nil {
			t.Fatalf("%d bytes: open: %v", n, err)
		}
		if !bytes.Equal(back, plain) {
			t.Fatalf("%d bytes did not round trip", n)
		}
	}
}

// SIV decrypts before it can verify — the synthetic IV authenticates the
// plaintext, which does not exist until CTR has run. That ordering is inherent
// to the mode, so the rule has to be explicit: nothing unauthenticated is
// returned, not even for the caller to look at.
func TestSIVReleasesNoPlaintextOnAFailedTag(t *testing.T) {
	s, err := newSIV(mustHex(t, "fffefdfcfbfaf9f8f7f6f5f4f3f2f1f0f0f1f2f3f4f5f6f7f8f9fafbfcfdfeff"))
	if err != nil {
		t.Fatal(err)
	}
	sealed := s.seal([]byte("quarterly-report.pdf"))
	for _, at := range []int{0, SIVOverhead - 1, SIVOverhead, len(sealed) - 1} {
		tampered := bytes.Clone(sealed)
		tampered[at] ^= 0x40
		got, err := s.open(tampered)
		if !errors.Is(err, ErrDecrypt) {
			t.Errorf("a flipped bit at %d gave %v, want ErrDecrypt", at, err)
		}
		if got != nil {
			t.Errorf("a flipped bit at %d released %d bytes of plaintext", at, len(got))
		}
	}
	if _, err := s.open(sealed[:SIVOverhead-1]); !errors.Is(err, ErrDecrypt) {
		t.Errorf("a truncated ciphertext gave %v, want ErrDecrypt", err)
	}
}

func TestSIVRefusesKeysItCannotSplit(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33, 48 + 1, 63, 65, 128} {
		if _, err := newSIV(make([]byte, n)); err == nil {
			t.Errorf("a %d-byte key was accepted", n)
		}
	}
	for _, n := range []int{32, 48, 64} {
		if _, err := newSIV(make([]byte, n)); err != nil {
			t.Errorf("a %d-byte key was refused: %v", n, err)
		}
	}
}

// The counter masking is the quirk most easily left out, and leaving it out
// produces something that round-trips against itself and interoperates with
// nothing. The published vectors above catch it; this states it directly.
func TestTheCounterClearsTheTwoPinnedBits(t *testing.T) {
	var v [16]byte
	for i := range v {
		v[i] = 0xff
	}
	q := counter(v)
	if q[8] != 0x7f || q[12] != 0x7f {
		t.Fatalf("counter left %#02x at 8 and %#02x at 12", q[8], q[12])
	}
	for i, b := range q {
		if i != 8 && i != 12 && b != 0xff {
			t.Fatalf("counter changed byte %d", i)
		}
	}
}
