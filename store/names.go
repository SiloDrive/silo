package store

import (
	"bytes"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// MaxPlainNameBytes is the longest filename an E2EE library can hold.
//
// The arithmetic, so nobody has to rediscover it: a directory entry's name
// field is capped at 255 bytes, SIV prepends a 16-byte synthetic IV, and the
// URL form is base64url of the whole thing — ceil(4(16+n)/3) <= 255 gives
// n <= 175. Plain libraries keep the full 255, because their names are not
// wrapped in anything.
const MaxPlainNameBytes = 175

// ErrName reports a name this format will not carry: too long, empty, or
// holding a byte that would make it a path rather than a name.
var ErrName = errors.New("invalid entry name")

// NameKey derives the key a directory's entry names are encrypted under.
//
//	HKDF-SHA256(CK, salt="silo/names/v1", info=dir_salt, L=64)
//
// Keyed per directory, not per library. The job is the equality leak: with one
// library-wide key, Enc("taxes.pdf") is the same ciphertext in every directory
// it appears in, and the server learns the shape of a filesystem it cannot
// read. Per-directory keys make those ciphertexts unrelated.
//
// The honest cost is that path construction becomes stateful: a cold resolve
// of a depth-N path is N sequential fetches, because encrypting each segment
// needs its parent's salt. Steady state is fine — clients cache the salt map
// beside the local index — but the cold start is a tree walk. Moving an entry
// re-encrypts one name; renaming an ancestor re-encrypts nothing.
func NameKey(ck []byte, salt [DirSaltSize]byte) ([]byte, error) {
	if len(ck) == 0 {
		return nil, errors.New("store: deriving a name key needs a content key")
	}
	key, err := hkdf.Key(sha256.New, ck, []byte("silo/names/v1"), string(salt[:]), SIVKeySize)
	if err != nil {
		return nil, fmt.Errorf("store: deriving name key: %w", err)
	}
	return key, nil
}

// ValidName reports whether a plaintext name is one this format will carry.
//
// The path bytes are refused rather than escaped, in both directions. A
// separator or a NUL inside a name is how a directory entry becomes a path
// traversal on whichever client writes it to disk, and "." and ".." are the
// same attack spelled differently. In a plain library the server writes these
// names, and the threat model calls it actively malicious for integrity; in an
// E2EE library it cannot write them, but a buggy client could, and the answer
// is the same either way.
func ValidName(name []byte) error {
	switch {
	case len(name) == 0:
		return fmt.Errorf("%w: empty", ErrName)
	case bytes.ContainsAny(name, "/\x00"):
		return fmt.Errorf("%w: %q holds a separator or a NUL", ErrName, name)
	case bytes.Equal(name, []byte(".")), bytes.Equal(name, []byte("..")):
		return fmt.Errorf("%w: %q", ErrName, name)
	}
	return nil
}

// EncryptName encrypts one path segment for an E2EE library. The result goes
// into a directory entry's Name field as raw bytes — never base64, which is
// the URL encoding only.
func EncryptName(nameKey []byte, name string) ([]byte, error) {
	if err := ValidName([]byte(name)); err != nil {
		return nil, err
	}
	if len(name) > MaxPlainNameBytes {
		return nil, fmt.Errorf("%w: %d bytes, above the %d an E2EE library can carry",
			ErrName, len(name), MaxPlainNameBytes)
	}
	s, err := newSIV(nameKey)
	if err != nil {
		return nil, err
	}
	return s.seal([]byte(name)), nil
}

// DecryptName reverses EncryptName, and applies ValidName to what comes back.
func DecryptName(nameKey, ciphertext []byte) (string, error) {
	if len(ciphertext) > SIVOverhead+MaxPlainNameBytes {
		return "", fmt.Errorf("%w: ciphertext is %d bytes, above %d",
			ErrName, len(ciphertext), SIVOverhead+MaxPlainNameBytes)
	}
	s, err := newSIV(nameKey)
	if err != nil {
		return "", err
	}
	plain, err := s.open(ciphertext)
	if err != nil {
		return "", err
	}
	if err := ValidName(plain); err != nil {
		return "", err
	}
	return string(plain), nil
}

// NameToURL renders an encrypted name for the entries/{path} route.
//
// base64url unpadded, RFC 4648 §5, and this is the ONLY place base64 appears
// in the format. A port that base64s a name into a directory object produces
// different bytes, a different sealing key, and a different object id for an
// identical tree.
func NameToURL(ciphertext []byte) string {
	return base64.RawURLEncoding.EncodeToString(ciphertext)
}

// NameFromURL reverses NameToURL.
func NameFromURL(s string) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not unpadded base64url", ErrName, s)
	}
	if len(b) < SIVOverhead || len(b) > SIVOverhead+MaxPlainNameBytes {
		return nil, fmt.Errorf("%w: %d bytes decoded", ErrName, len(b))
	}
	return b, nil
}
