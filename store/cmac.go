package store

import (
	"crypto/cipher"
	"crypto/subtle"
)

// AES-CMAC, RFC 4493. It exists here only because AES-SIV needs it: neither
// Go's standard library nor x/crypto ships CMAC, and neither does CryptoKit,
// so the Swift port is hand-written whichever way this went. An owned
// implementation with the RFC's own vectors is worth more to a frozen format
// than an unmaintained dependency.

// rb is the representation of the GF(2^128) reduction polynomial for a
// 128-bit block, per RFC 4493 section 2.3.
const rb = 0x87

// cmac holds a block cipher and the two subkeys derived from it.
type cmac struct {
	b      cipher.Block
	k1, k2 [16]byte
}

// newCMAC derives the subkeys. b must have a 16-byte block size; every caller
// here passes AES.
func newCMAC(b cipher.Block) *cmac {
	var l [16]byte
	b.Encrypt(l[:], l[:])
	c := &cmac{b: b}
	c.k1 = dbl(l)
	c.k2 = dbl(c.k1)
	return c
}

// dbl doubles a 128-bit value in GF(2^128): a left shift by one bit, with rb
// folded in when the bit shifted off the top was set.
//
// This is the operation a hand-rolled CMAC most often gets wrong, and a
// subkey bug survives end-to-end tests written against one's own output —
// which is why cmac_test.go checks the subkeys and tags against RFC 4493
// rather than only checking that SIV round-trips.
//
// The fold is arithmetic rather than an if, so the branch does not depend on
// key material.
func dbl(in [16]byte) [16]byte {
	var out [16]byte
	carry := in[0] >> 7
	for i := 0; i < 15; i++ {
		out[i] = in[i]<<1 | in[i+1]>>7
	}
	out[15] = in[15]<<1 ^ carry*rb
	return out
}

// sum computes the CMAC tag over msg.
func (c *cmac) sum(msg []byte) [16]byte {
	// The final block is XORed with k1 when the message is a whole number of
	// blocks, and padded and XORed with k2 otherwise. An empty message takes
	// the padded branch with an empty tail, which is what makes it one block
	// rather than none.
	var n int
	var last [16]byte
	if len(msg) > 0 && len(msg)%16 == 0 {
		n = len(msg) / 16
		copy(last[:], msg[(n-1)*16:])
		subtle.XORBytes(last[:], last[:], c.k1[:])
	} else {
		n = len(msg)/16 + 1
		tail := msg[(n-1)*16:]
		copy(last[:], tail)
		last[len(tail)] = 0x80
		subtle.XORBytes(last[:], last[:], c.k2[:])
	}

	var x, y [16]byte
	for i := 0; i < n-1; i++ {
		subtle.XORBytes(y[:], x[:], msg[i*16:(i+1)*16])
		c.b.Encrypt(x[:], y[:])
	}
	subtle.XORBytes(y[:], x[:], last[:])
	c.b.Encrypt(x[:], y[:])
	return x
}
