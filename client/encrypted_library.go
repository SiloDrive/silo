package client

// Creating an end-to-end encrypted library.
//
// The server cannot mint one. Its initial root directory and initial commit
// are both sealed under a content key the server never holds, and the content
// key itself has to be wrapped to somebody before it is worth having -- so all
// four arrive with the request, and the client is the only party that can
// build them.
//
// The library id arrives with the request too, which is the part worth naming:
// store.WrapCK binds the library id into the wrap as associated data, so the
// id has to exist before the key can be wrapped to anybody. The alternative is
// two requests with a window in between holding a library whose key nobody
// stored. docs/storage.md § What the server can read is the owning document.

import (
	"crypto/rand"
	"fmt"
	"time"

	"github.com/dkam/silo/store"

	"github.com/google/uuid"
)

// EncryptedSeed is the initial state of an encrypted library, as it goes on
// the wire: the two objects the server stores without being able to read
// them, the id they belong to, and the only copy of the key that opens them.
//
// It is a named type rather than four arguments to one request because it is
// also the unit the server validates -- see libmgr.EncryptedSeed -- and
// because building it is separable from sending it, which is what lets a test
// exercise the server's refusals against seeds this code actually produces.
type EncryptedSeed struct {
	LibraryID  string `json:"library_id"`
	Root       []byte `json:"root"`
	Commit     []byte `json:"commit"`
	WrappedKey []byte `json:"wrapped_key"`
}

// NewEncryptedSeed mints a content key for a library that will be created
// under libraryID, and builds the initial objects sealed under it.
//
// The id is a parameter rather than minted here because the wrap binds it:
// whatever id the library ends up with has to be the one passed in, or the
// wrap does not open.
func NewEncryptedSeed(libraryID string, recipient [store.X25519KeySize]byte) (EncryptedSeed, *store.Keyring, error) {
	kr, err := store.GenerateKeyring()
	if err != nil {
		return EncryptedSeed{}, nil, err
	}

	// A fresh salt per directory, so that two directories holding a file of
	// the same name do not produce the same encrypted entry name.
	var salt [store.DirSaltSize]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return EncryptedSeed{}, nil, fmt.Errorf("client: generating the root directory's salt: %w", err)
	}
	root, err := kr.SealDirectory(&store.Directory{Salt: salt})
	if err != nil {
		return EncryptedSeed{}, nil, fmt.Errorf("client: sealing the root directory: %w", err)
	}
	commit, err := kr.SealCommit(&store.Commit{
		Root:      store.ObjectID(root),
		CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		return EncryptedSeed{}, nil, fmt.Errorf("client: sealing the initial commit: %w", err)
	}
	wrapped, err := kr.WrapTo(recipient, libraryID)
	if err != nil {
		return EncryptedSeed{}, nil, fmt.Errorf("client: wrapping the content key: %w", err)
	}

	return EncryptedSeed{LibraryID: libraryID, Root: root, Commit: commit, WrappedKey: wrapped}, kr, nil
}

// HeadCommitID is the commit the library starts at: the id of the seed's own
// initial commit, which is what the server will publish as the head.
func (s EncryptedSeed) HeadCommitID() string { return store.ObjectID(s.Commit).String() }

// CreateEncryptedLibrary mints a library the server cannot read, and returns
// it along with the keyring that opens it.
//
// The keyring comes back because this is the only moment it exists for free.
// It can be had again with OpenLibrary, but only by an account that published
// an identity key before this call and still has the password that opens it;
// there is nothing on the server that can reconstruct it otherwise, and no
// second copy anywhere. A caller that drops it has not lost a cache.
func (a *Account) CreateEncryptedLibrary(name string) (*Library, *store.Keyring, error) {
	seed, kr, err := NewEncryptedSeed(uuid.New().String(), a.Identity.Public())
	if err != nil {
		return nil, nil, err
	}

	var created Library
	err = a.c.doRequest("POST", "/api/silo/v1/libraries", struct {
		Name string `json:"name"`
		E2EE bool   `json:"e2ee"`
		EncryptedSeed
	}{name, true, seed}, &created)
	if err != nil {
		return nil, nil, err
	}
	// The id is checked rather than taken, because the wrap is bound to the
	// one that was sent: a library stored under any other id holds a key
	// nothing can open, and it would not be discovered until the first read.
	if created.ID != seed.LibraryID {
		return nil, nil, fmt.Errorf(
			"client: the server created library %s, but the content key is wrapped to %s",
			created.ID, seed.LibraryID)
	}

	// The create response says id and name; the rest is what the client
	// already knows to be true of what it just built.
	created.Encrypted = true
	created.HeadCommitID = seed.HeadCommitID()
	return &created, kr, nil
}
