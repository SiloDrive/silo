package store

import (
	"crypto/aes"
	"encoding/hex"
	"testing"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex in test: %v", err)
	}
	return b
}

// The subkeys are checked separately from the tags, and on purpose. A bug in
// dbl — the shift, or the Rb fold when the top bit was set — produces a CMAC
// that is perfectly self-consistent, so it survives any round-trip test and
// any vector generated from the same code. RFC 4493 publishes the
// intermediate values precisely so that cannot happen.
//
// RFC 4493 section 4, subkey generation example.
func TestCMACSubkeysMatchRFC4493(t *testing.T) {
	block, err := aes.NewCipher(mustHex(t, "2b7e151628aed2a6abf7158809cf4f3c"))
	if err != nil {
		t.Fatal(err)
	}
	var l [16]byte
	block.Encrypt(l[:], l[:])
	if got := hex.EncodeToString(l[:]); got != "7df76b0c1ab899b33e42f047b91b546f" {
		t.Fatalf("L = %s", got)
	}
	c := newCMAC(block)
	if got := hex.EncodeToString(c.k1[:]); got != "fbeed618357133667c85e08f7236a8de" {
		t.Errorf("K1 = %s", got)
	}
	if got := hex.EncodeToString(c.k2[:]); got != "f7ddac306ae266ccf90bc11ee46d513b" {
		t.Errorf("K2 = %s", got)
	}
}

// RFC 4493 section 4 examples 1–4 (AES-128) and NIST SP 800-38B's AES-256
// examples, as published in miscreant's cross-implementation vector set.
//
// The four AES-128 cases are the four shapes CMAC has: an empty message, one
// whole block, a partial final block, and several whole blocks. Between them
// they exercise both padding branches and the chaining loop.
func TestCMACMatchesPublishedVectors(t *testing.T) {
	const (
		k128 = "2b7e151628aed2a6abf7158809cf4f3c"
		k256 = "603deb1015ca71be2b73aef0857d77811f352c073b6108d72d9810a30914dff4"
		m16  = "6bc1bee22e409f96e93d7e117393172a"
		m40  = "6bc1bee22e409f96e93d7e117393172aae2d8a571e03ac9c9eb76fac45af8e5130c81c46a35ce411"
		m64  = "6bc1bee22e409f96e93d7e117393172aae2d8a571e03ac9c9eb76fac45af8e51" +
			"30c81c46a35ce411e5fbc1191a0a52eff69f2445df4f9b17ad2b417be66c3710"
	)
	cases := []struct{ name, key, msg, tag string }{
		{"aes128/empty", k128, "", "bb1d6929e95937287fa37d129b756746"},
		{"aes128/one block", k128, m16, "070a16b46b4d4144f79bdd9dd04a287c"},
		{"aes128/partial block", k128, m40, "dfa66747de9ae63030ca32611497c827"},
		{"aes128/four blocks", k128, m64, "51f0bebf7e3b9d92fc49741779363cfe"},
		{"aes256/empty", k256, "", "028962f61b7bf89efc6b551f4667d983"},
		{"aes256/one block", k256, m16, "28a7023f452e8f82bd4bf28d8c37c35c"},
		{"aes256/partial block", k256, m40, "aaf3d8f1de5640c232f5b169b9c911e6"},
		{"aes256/four blocks", k256, m64, "e1992190549f6ed5696a2c056c315410"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			block, err := aes.NewCipher(mustHex(t, tc.key))
			if err != nil {
				t.Fatal(err)
			}
			got := newCMAC(block).sum(mustHex(t, tc.msg))
			if hex.EncodeToString(got[:]) != tc.tag {
				t.Fatalf("tag %s, want %s", hex.EncodeToString(got[:]), tc.tag)
			}
		})
	}
}
