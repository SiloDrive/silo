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

// SealChunk seals one chunk under this library's content key.
func (k *Keyring) SealChunk(plaintext []byte) (SealedChunk, error) {
	return SealChunk(k.ck, plaintext)
}

// OpenChunk reverses SealChunk, given the plaintext hash the manifest records.
func (k *Keyring) OpenChunk(hp ID, frame []byte) ([]byte, error) {
	return OpenChunk(k.ck, hp, frame)
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
