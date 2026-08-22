package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
)

// The sealing domains. One per container type, mandatory: without them the
// containers under a library's content key would share one key space.
const (
	domainManifest = "silo/manifest/v1"
	domainDir      = "silo/dir/v1"
	domainCommit   = "silo/commit/v1"
	domainChunk    = "silo/chunk/v1"
)

// TagSize is the AEAD tag every sealed frame carries. Frames are
// `ciphertext ‖ tag` with the nonce implicit — never CryptoKit's combined
// form, which prepends the nonce and would make every frame twelve bytes
// longer and every id different.
const TagSize = 16

// ErrDecrypt reports a sealed section that did not authenticate: the wrong
// key, or a server that altered the object. The two are indistinguishable by
// construction and the client's answer is the same either way.
var ErrDecrypt = errors.New("sealed section failed to authenticate")

// sealKey derives the key for one sealed container.
//
// One derivation, everywhere a sealed section exists:
//
//	HKDF-SHA256(secret, salt=<domain>, info=SHA-256(AD), L=32)
//
// info is the hash of exactly the byte range the AEAD authenticates — raw 32
// bytes, no length prefix. That is what licenses the zero nonce. Because the
// public section carries seal_hash, the AD commits to the sealed plaintext
// too, so no two distinct (AD, plaintext) pairs can ever meet the same key —
// the condition GCM requires and the reason this is one function rather than
// an argument repeated per container.
//
// Hashing anything smaller — the public section without the header, say —
// would key a strict subset of what the tag covers, and two objects differing
// only in version or flags over unchanged sealed content would meet one key,
// one nonce and two ADs: the forbidden case, reintroduced.
func sealKey(secret []byte, domain string, ad []byte) ([]byte, error) {
	h := sha256.Sum256(ad)
	key, err := hkdf.Key(sha256.New, secret, []byte(domain), string(h[:]), 32)
	if err != nil {
		return nil, fmt.Errorf("store: deriving %s key: %w", domain, err)
	}
	return key, nil
}

func sealAEAD(secret []byte, domain string, ad []byte) (cipher.AEAD, error) {
	key, err := sealKey(secret, domain, ad)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("store: %s cipher: %w", domain, err)
	}
	return cipher.NewGCM(block)
}

// zeroNonce is the 96-bit nonce every convergent frame uses. It is safe only
// because each key encrypts exactly one plaintext, ever — see sealKey. A
// random nonce here would be the more cautious-looking choice and the wrong
// one: identical content would mint a new id on every write, turning copies,
// re-uploads after index loss, and the same file arriving from two members
// into spurious changes?since= traffic.
var zeroNonce [12]byte

// sealSection encrypts plaintext under the container's derived key, binding
// ad as associated data. The result is ciphertext ‖ tag.
func sealSection(secret []byte, domain string, ad, plaintext []byte) ([]byte, error) {
	aead, err := sealAEAD(secret, domain, ad)
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, zeroNonce[:], plaintext, ad), nil
}

// openSection reverses sealSection.
func openSection(secret []byte, domain string, ad, sealed []byte) ([]byte, error) {
	aead, err := sealAEAD(secret, domain, ad)
	if err != nil {
		return nil, err
	}
	out, err := aead.Open(nil, zeroNonce[:], sealed, ad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return out, nil
}

// SealedChunk is one chunk of an E2EE library: the bytes that go on the wire
// and into a pack, and the two hashes the manifest records for them.
type SealedChunk struct {
	// ID is SHA-256 of the frame — the ciphertext, not the plaintext. It is
	// the chunk's only name: the dedup key, the wire identifier, and what the
	// server verifies an upload against.
	ID ID
	// PlaintextHash is H_p. It goes in the manifest's sealed section,
	// because a reader holding only ID cannot derive the key from it.
	PlaintextHash ID
	// Frame is ciphertext ‖ tag, TagSize bytes longer than the plaintext.
	Frame []byte
}

// SealChunk encrypts one chunk under its library's content key.
//
//	H_p   = SHA-256(plaintext)
//	K_c   = HKDF-SHA256(CK, salt="silo/chunk/v1", info=H_p)
//	frame = AES-256-GCM(K_c, zero nonce, plaintext)     -- no associated data
//	id    = SHA-256(frame)
//
// The derivation is convergent on purpose: identical plaintext under the same
// content key yields an identical frame and an identical id, so delta sync and
// within-library dedup work exactly as they do in a plain library. What that
// costs is a content-confirmation oracle — but only for holders of CK, which
// is to say the library's own members, and it is the price of the property
// everything else in the sync design depends on.
//
// Nonce reuse is safe here for the reason it is safe nowhere else: K_c is a
// function of the plaintext, so each key encrypts exactly one plaintext, ever.
// A chunk frame carries no associated data — there is no public section to
// bind, and distinct plaintexts already give distinct keys. Do not invent one
// to make this look like the sealed containers.
//
// Cross-library and cross-user dedup end for E2EE libraries, since a different
// CK gives a different frame. That was always the price of E2EE.
func SealChunk(ck, plaintext []byte) (SealedChunk, error) {
	if len(ck) == 0 {
		return SealedChunk{}, errors.New("store: sealing a chunk needs a content key")
	}
	hp := PlaintextHash(plaintext)
	aead, err := chunkAEAD(ck, hp)
	if err != nil {
		return SealedChunk{}, err
	}
	frame := aead.Seal(nil, zeroNonce[:], plaintext, nil)
	return SealedChunk{ID: ChunkID(frame), PlaintextHash: hp, Frame: frame}, nil
}

// OpenChunk decrypts a chunk frame, given the plaintext hash its manifest
// carried for it, and verifies the plaintext against that hash.
func OpenChunk(ck []byte, hp ID, frame []byte) ([]byte, error) {
	if len(ck) == 0 {
		return nil, errors.New("store: opening a chunk needs a content key")
	}
	aead, err := chunkAEAD(ck, hp)
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, zeroNonce[:], frame, nil)
	if err != nil {
		return nil, ErrDecrypt
	}
	if PlaintextHash(plain) != hp {
		return nil, ErrDecrypt
	}
	return plain, nil
}

func chunkAEAD(ck []byte, hp ID) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, ck, []byte(domainChunk), string(hp[:]), 32)
	if err != nil {
		return nil, fmt.Errorf("store: deriving chunk key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("store: chunk cipher: %w", err)
	}
	return cipher.NewGCM(block)
}
