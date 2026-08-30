// The keyring: a library's content key and everything derived from it, in one
// object, so that no caller has to hold a bare CK.
//
// CK is not a key anything uses directly. Chunking runs under a seed derived
// from it, entry names under a per-directory key derived from it, and chunks,
// manifests, directories and commits each under their own derivation. A client
// that passes CK from function to function is a client where every one of
// those derivations is a call site that can be got wrong, and where the one
// secret that opens the whole library is the value most often in a variable.
//
// So the keyring is what OpenLibrary returns and what everything downstream
// takes. It holds CK unexported, and the things a caller actually wants are
// methods.
package store

import (
	"crypto/rand"
	"errors"
	"fmt"
)

// Keyring holds one library's content key and the parameters it implies.
type Keyring struct {
	// ck never leaves this struct except through the methods below.
	ck []byte
	// params is derived once. ChunkerSeed is an HKDF per call and the chunker
	// is built per file, so deriving it at every write would be a hash of the
	// content key on a path that runs for every file in the library.
	params Params
}

// NewKeyring builds the keyring for a content key.
//
// The parameters are the published defaults under this library's own chunker
// seed, which is what makes an encrypted library's chunk boundaries unguessable
// to anyone without CK — see ChunkerSeed and docs/chunking.md.
func NewKeyring(ck []byte) (*Keyring, error) {
	if len(ck) != CKSize {
		return nil, fmt.Errorf("store: a content key is %d bytes, got %d", CKSize, len(ck))
	}
	k := &Keyring{ck: make([]byte, len(ck))}
	copy(k.ck, ck)
	k.params = DefaultParams(ChunkerSeed(k.ck))
	return k, nil
}

// GenerateKeyring mints a content key for a new library.
//
// The key is generated here rather than by the caller so that the only way to
// come by one is to come by a keyring: a bare CK on the client is a value that
// can be logged, copied into a config file, or passed to the wrong derivation,
// and there is no recovering from any of those.
func GenerateKeyring() (*Keyring, error) {
	ck := make([]byte, CKSize)
	if _, err := rand.Read(ck); err != nil {
		return nil, fmt.Errorf("store: generating a content key: %w", err)
	}
	return NewKeyring(ck)
}

// OpenKeyring unwraps a library's content key with an identity and returns the
// keyring for it.
//
// The pair to WrapTo, and the reason this exists rather than leaving callers to
// compose UnwrapCK with NewKeyring: that composition puts a bare content key in
// a variable in whichever package writes it, which is the one thing the top of
// this file says must not happen. NewKeyring stays exported for the vectors and
// for a caller that already has a key by some other route.
func OpenKeyring(id *Identity, library string, wrapped []byte) (*Keyring, error) {
	ck, err := UnwrapCK(id, library, wrapped)
	if err != nil {
		return nil, err
	}
	k, err := NewKeyring(ck)
	// NewKeyring copies, so the intermediate is ours to clear either way.
	for i := range ck {
		ck[i] = 0
	}
	return k, err
}

// Params is the chunker configuration for this library.
func (k *Keyring) Params() Params { return k.params }

// NameCipher returns the cipher for one directory's entry names.
//
// Per directory, because the name key is derived under that directory's salt:
// two directories holding a file of the same name produce different
// ciphertext, so the store does not leak that they match.
func (k *Keyring) NameCipher(salt [DirSaltSize]byte) (*NameCipher, error) {
	nk, err := NameKey(k.ck, salt)
	if err != nil {
		return nil, err
	}
	return NewNameCipher(nk)
}

// WrapTo wraps this library's content key to a recipient's identity key.
//
// The library id is bound into the wrap as associated data, so it has to be
// the id the library will actually have -- a wrap made against one spelling of
// it does not open under another.
func (k *Keyring) WrapTo(recipient [X25519KeySize]byte, library string) ([]byte, error) {
	return WrapCK(recipient, library, k.ck)
}

// SealChunk seals one chunk under this library's content key.
func (k *Keyring) SealChunk(plaintext []byte) (SealedChunk, error) {
	return SealChunk(k.ck, plaintext)
}

// OpenChunk reverses SealChunk, given the plaintext hash the manifest records.
func (k *Keyring) OpenChunk(hp ID, frame []byte) ([]byte, error) {
	return OpenChunk(k.ck, hp, frame)
}

// SealManifest, SealDirectory and SealCommit are the writing halves of the
// three opens below.
func (k *Keyring) SealManifest(m *Manifest) ([]byte, error) {
	return m.EncodeSealed(k.ck)
}

func (k *Keyring) SealDirectory(d *Directory) ([]byte, error) {
	return d.EncodeSealed(k.ck)
}

func (k *Keyring) SealCommit(c *Commit) ([]byte, error) {
	return c.EncodeSealed(k.ck)
}

// OpenManifest, OpenDirectory and OpenCommit open the three object types a
// walk of an encrypted library reads.
func (k *Keyring) OpenManifest(b []byte) (*Manifest, error) {
	return DecodeSealedManifest(b, k.ck)
}

func (k *Keyring) OpenDirectory(b []byte) (*Directory, error) {
	return DecodeSealedDirectory(b, k.ck)
}

func (k *Keyring) OpenCommit(b []byte) (*Commit, error) {
	return DecodeSealedCommit(b, k.ck)
}

// ErrNoKeyring reports an operation that needs a content key on a library the
// caller holds no wrap for.
var ErrNoKeyring = errors.New("store: no content key for this library")
