package silod

import (
	"bytes"
	"testing"
)

func testCrypt(version int) *seafileCrypt {
	return &seafileCrypt{
		key:     []byte("0123456789abcdef0123456789abcdef"),
		iv:      []byte("fedcba9876543210"),
		version: version,
	}
}

func TestCryptRoundTrip(t *testing.T) {
	for _, version := range []int{2, 3} {
		crypt := testCrypt(version)
		for _, plaintext := range [][]byte{
			[]byte(""),
			[]byte("a"),
			[]byte("exactly sixteen!"),
			bytes.Repeat([]byte("x"), 1000),
		} {
			encrypted, err := crypt.encrypt(plaintext)
			if err != nil {
				t.Fatalf("v%d: encrypt(%d bytes) returned %v", version, len(plaintext), err)
			}
			decrypted, err := crypt.decrypt(encrypted)
			if err != nil {
				t.Fatalf("v%d: decrypt of %d bytes returned %v", version, len(plaintext), err)
			}
			if !bytes.Equal(decrypted, plaintext) {
				t.Errorf("v%d: round trip of %d bytes produced %q", version, len(plaintext), decrypted)
			}
		}
	}
}

// The input is a stored object decrypted with a key that may be the wrong
// one, so the result is noise and its last byte an arbitrary number. Every
// one of these used to panic or slice out of range, and a panic in a request
// goroutine takes the whole fileserver down rather than failing one request.
func TestDecryptRejectsMalformedInputWithoutPanicking(t *testing.T) {
	for _, version := range []int{2, 3} {
		crypt := testCrypt(version)
		for name, input := range map[string][]byte{
			"empty":            {},
			"one byte":         {0x00},
			"partial block":    bytes.Repeat([]byte{0x01}, 15),
			"block and a half": bytes.Repeat([]byte{0x02}, 24),
		} {
			out, err := crypt.decrypt(input)
			if err == nil {
				t.Errorf("v%d: decrypt(%s) returned %q, want an error", version, name, out)
			}
		}
	}
}

// A well-formed block of ciphertext whose plaintext has nonsense padding —
// which is what decrypting with the wrong key produces — must be an error,
// not a slice out of range.
func TestPkcs7UnPaddingRejectsBadPadding(t *testing.T) {
	const blockSize = 16

	bad := map[string][]byte{
		"empty":               {},
		"not a whole block":   bytes.Repeat([]byte{0x01}, 17),
		"zero padding length": append(bytes.Repeat([]byte{0xAA}, 15), 0x00),
		"padding past the end": append(bytes.Repeat([]byte{0xAA}, 15),
			byte(blockSize+1)),
		"padding longer than the buffer": append(bytes.Repeat([]byte{0xAA}, 15), 0xFF),
		"inconsistent padding bytes": append(bytes.Repeat([]byte{0xAA}, 13),
			0x01, 0x02, 0x03),
	}
	for name, input := range bad {
		out, err := pkcs7UnPadding(input, blockSize)
		if err == nil {
			t.Errorf("pkcs7UnPadding(%s) returned %q, want an error", name, out)
		}
	}

	// A full block of padding is valid PKCS#7 and must not be rejected.
	full := bytes.Repeat([]byte{byte(blockSize)}, blockSize)
	out, err := pkcs7UnPadding(full, blockSize)
	if err != nil {
		t.Errorf("pkcs7UnPadding(a full block of padding) returned %v", err)
	}
	if len(out) != 0 {
		t.Errorf("pkcs7UnPadding(a full block of padding) = %q, want empty", out)
	}
}
