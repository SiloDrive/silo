package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/subtle"
	"fmt"
)

// AES-CMAC-SIV, RFC 5297.
//
// SIV is used here for one thing only — encrypting the name of a single
// directory entry — and it is used because that job needs a cipher that is
// *deterministic by design*: the same name under the same directory key must
// give the same ciphertext, or `entries/{path}` could not route on it. That
// determinism is Option A's stated and accepted equality leak, closed across
// directories by the per-directory key derivation rather than by a nonce.
//
// There is therefore no nonce parameter anywhere in this file's exported
// surface, and there should never be one: a caller who wanted randomness would
// be asking for a different scheme, not a different argument.

// sivKeySizes are the total key lengths this implementation accepts. RFC 5297
// splits the key into equal halves — the left half keys S2V, the right half
// keys CTR — so a 64-byte key means AES-256 on both, which is what this format
// uses. The narrower widths exist so the RFC's own test vectors, which are all
// 128-bit, can be run against the same code the format uses.
var sivKeySizes = map[int]bool{32: true, 48: true, 64: true}

// SIVKeySize is the key length this format's names use: AES-256 for both
// halves. It is stated as a constant rather than implied by the derivation,
// so a port cannot arrive at 32 by reading only the RFC's examples.
const SIVKeySize = 64

// SIVOverhead is how much longer a SIV ciphertext is than its plaintext: the
// synthetic IV, which is also the tag.
const SIVOverhead = 16

type siv struct {
	mac *cmac        // over the left half of the key
	ctr cipher.Block // the right half
}

func newSIV(key []byte) (*siv, error) {
	if !sivKeySizes[len(key)] {
		return nil, fmt.Errorf("store: SIV key is %d bytes, want 32, 48 or 64", len(key))
	}
	half := len(key) / 2
	s2vKey, err := aes.NewCipher(key[:half])
	if err != nil {
		return nil, fmt.Errorf("store: SIV S2V cipher: %w", err)
	}
	ctrKey, err := aes.NewCipher(key[half:])
	if err != nil {
		return nil, fmt.Errorf("store: SIV CTR cipher: %w", err)
	}
	return &siv{mac: newCMAC(s2vKey), ctr: ctrKey}, nil
}

// s2v is RFC 5297's string-to-vector construction: a CMAC chain over the
// associated-data items and the plaintext, producing the synthetic IV.
//
// The final component takes one of two branches, and which one is not an
// implementation detail — they produce different output. A component of 16
// bytes or more is XORed into its *end* (xorend); a shorter one is padded and
// XORed into a doubled accumulator. Silo's names are routinely under 16 bytes,
// so the short branch is the hot path, and it is the branch a hand-written
// implementation is most likely to get wrong. Both are covered by vectors
// either side of the boundary.
func (s *siv) s2v(components ...[]byte) [16]byte {
	if len(components) == 0 {
		// Unreachable from seal and open, which always pass the plaintext as
		// a component. Present because the RFC defines it.
		var one [16]byte
		one[15] = 1
		return s.mac.sum(one[:])
	}

	var zero [16]byte
	d := s.mac.sum(zero[:])
	for _, c := range components[:len(components)-1] {
		d = dbl(d)
		m := s.mac.sum(c)
		subtle.XORBytes(d[:], d[:], m[:])
	}

	last := components[len(components)-1]
	if len(last) >= 16 {
		t := make([]byte, len(last))
		copy(t, last)
		subtle.XORBytes(t[len(t)-16:], t[len(t)-16:], d[:])
		return s.mac.sum(t)
	}
	d = dbl(d)
	var padded [16]byte
	copy(padded[:], last)
	padded[len(last)] = 0x80
	subtle.XORBytes(d[:], d[:], padded[:])
	return s.mac.sum(d[:])
}

// counter derives the CTR starting block from the synthetic IV.
//
// The two cleared bits are the quirk of SIV that is easiest to miss and
// hardest to notice: they exist so that the counter cannot carry out of the
// low 32 bits of either half-word on a long message, which would let two
// different IVs generate overlapping keystreams. Miss them and everything
// round-trips against itself while interoperating with nothing.
func counter(v [16]byte) [16]byte {
	v[8] &= 0x7f
	v[12] &= 0x7f
	return v
}

// seal returns V ‖ CTR(P). The variadic associated data exists so the RFC's
// own test vectors run against this code; this format's own use passes none.
func (s *siv) seal(plaintext []byte, ad ...[]byte) []byte {
	v := s.s2v(append(append([][]byte(nil), ad...), plaintext)...)
	out := make([]byte, SIVOverhead+len(plaintext))
	copy(out, v[:])
	q := counter(v)
	cipher.NewCTR(s.ctr, q[:]).XORKeyStream(out[SIVOverhead:], plaintext)
	return out
}

// open reverses seal.
//
// SIV decrypts before it can verify — the synthetic IV is a MAC over the
// *plaintext*, so there is nothing to check until the plaintext exists. That
// ordering is inherent to the mode and is the one place its shape invites a
// leak that GCM's does not, so the rule is explicit: on a mismatch the
// recovered buffer is zeroed and nothing is returned. No caller ever gets to
// see unauthenticated plaintext, not even to inspect it.
func (s *siv) open(sealed []byte, ad ...[]byte) ([]byte, error) {
	if len(sealed) < SIVOverhead {
		return nil, ErrDecrypt
	}
	var v [16]byte
	copy(v[:], sealed[:SIVOverhead])

	plain := make([]byte, len(sealed)-SIVOverhead)
	q := counter(v)
	cipher.NewCTR(s.ctr, q[:]).XORKeyStream(plain, sealed[SIVOverhead:])

	want := s.s2v(append(append([][]byte(nil), ad...), plain)...)
	if subtle.ConstantTimeCompare(want[:], v[:]) != 1 {
		for i := range plain {
			plain[i] = 0
		}
		return nil, ErrDecrypt
	}
	return plain, nil
}
