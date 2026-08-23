package store

import (
	"bytes"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

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

// ValidName returns an error if a plaintext name is not one this format will
// carry, and nil if it is.
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

// NameCipher encrypts and decrypts the entry names of one directory.
//
// It exists as a prepared value because the alternative shape — a free
// function taking the key — hides two AES-256 key schedules and a CMAC subkey
// derivation inside every call, and names are encrypted once per directory
// entry. A thousand-entry listing would pay for two thousand key schedules
// instead of two. Build one per directory and reuse it across the listing.
//
// It is also the only place the format's key width is enforced: newSIV accepts
// the RFC's narrower widths so the RFC's own vectors run against this code, and
// without this check a caller who derived a 32-byte key would get working,
// self-consistent names that no conforming client could read.
type NameCipher struct {
	siv *siv
}

// NewNameCipher prepares the cipher for a directory whose name key is
// nameKey, which must be SIVKeySize bytes — what NameKey returns.
func NewNameCipher(nameKey []byte) (*NameCipher, error) {
	if len(nameKey) != SIVKeySize {
		return nil, fmt.Errorf("store: name key is %d bytes, want %d", len(nameKey), SIVKeySize)
	}
	s, err := newSIV(nameKey)
	if err != nil {
		return nil, err
	}
	return &NameCipher{siv: s}, nil
}

// Encrypt encrypts one path segment. The result goes into a directory entry's
// Name field as raw bytes — never base64, which is the URL encoding only.
func (c *NameCipher) Encrypt(name string) ([]byte, error) {
	plain := []byte(name)
	if err := ValidName(plain); err != nil {
		return nil, err
	}
	if len(plain) > MaxPlainNameBytes {
		return nil, fmt.Errorf("%w: %d bytes, above the %d an E2EE library can carry",
			ErrName, len(plain), MaxPlainNameBytes)
	}
	return c.siv.seal(plain), nil
}

// Decrypt reverses Encrypt, and applies ValidName to what comes back.
func (c *NameCipher) Decrypt(ciphertext []byte) (string, error) {
	if len(ciphertext) > MaxNameCTBytes {
		return "", fmt.Errorf("%w: ciphertext is %d bytes, above %d",
			ErrName, len(ciphertext), MaxNameCTBytes)
	}
	plain, err := c.siv.open(ciphertext)
	if err != nil {
		return "", err
	}
	if err := ValidName(plain); err != nil {
		return "", err
	}
	return string(plain), nil
}

// EncryptName encrypts a single name under a key, for callers with one name to
// encrypt. Anything encrypting a directory's worth should hold a NameCipher.
func EncryptName(nameKey []byte, name string) ([]byte, error) {
	c, err := NewNameCipher(nameKey)
	if err != nil {
		return nil, err
	}
	return c.Encrypt(name)
}

// DecryptName is the one-name counterpart to EncryptName.
func DecryptName(nameKey, ciphertext []byte) (string, error) {
	c, err := NewNameCipher(nameKey)
	if err != nil {
		return "", err
	}
	return c.Decrypt(ciphertext)
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
	if len(b) < SIVOverhead || len(b) > MaxNameCTBytes {
		return nil, fmt.Errorf("%w: %d bytes decoded", ErrName, len(b))
	}
	return b, nil
}
