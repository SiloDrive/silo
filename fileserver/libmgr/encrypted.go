package libmgr

// Creating an end-to-end encrypted library.
//
// It is a separate function from CreateLibrary rather than a flag on it,
// because for an E2EE library almost nothing CreateLibrary does is available.
// The root directory and the initial commit are sealed under a content key the
// server never holds, so they arrive with the request; and the library id
// arrives with them, because store.WrapCK binds the library id into the wrap
// as associated data and so the id has to exist before the key can be wrapped
// to anybody.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/option"
	storefmt "github.com/dkam/silo/store"
)

// ErrBadSeed reports initial objects this server will not build a library
// from. Every case is a library that would exist, load cleanly and be
// unreadable -- which is strictly worse than one that was never created, since
// a client has no way to tell it apart from one whose key it has merely
// mislaid.
var ErrBadSeed = errors.New("invalid initial objects for an encrypted library")

// ErrLibraryExists reports an id already in use.
var ErrLibraryExists = errors.New("a library with that id already exists")

// MaxSeedObjectBytes bounds each of the two objects that arrive with the
// request. An empty sealed directory and an initial commit are a few hundred
// bytes each; this is that with a wide margin, and it is a bound rather than a
// trust because these are the only object writes on this server that happen
// before there is a library to charge them to.
const MaxSeedObjectBytes = 64 << 10

// EncryptedSeed is what a client must supply to create an encrypted library:
// the two initial objects, exactly as they are to be stored, and the content
// key wrapped to the creator's own identity key.
type EncryptedSeed struct {
	// LibraryID is the id the client has already bound into WrappedKey. It
	// must be a canonical lower-case hyphenated UUID -- the same spelling
	// store.WrapCK requires -- because a wrap made against one spelling does
	// not open under another.
	LibraryID string

	// Root is the encoded, sealed empty root directory object.
	Root []byte

	// Commit is the encoded, sealed initial commit object. It must name Root
	// and have no parents.
	Commit []byte

	// WrappedKey is the library's content key wrapped to the creator's
	// published X25519 public key. The server stores it and cannot open it.
	WrappedKey []byte
}

// Validate refuses every seed that would produce an unreadable library.
//
// The checks are exactly the ones the server can make without a content key,
// and no others -- it verifies that each object's id is the SHA-256 of its
// bytes and that it decodes, which is the same rule PUT objects/{id} already
// states, plus the two structural facts that make these objects an *initial*
// state rather than an arbitrary pair.
func (s EncryptedSeed) Validate() (rootID, commitID storefmt.ID, err error) {
	id, err := uuid.Parse(s.LibraryID)
	if err != nil || id.String() != s.LibraryID {
		return rootID, commitID, fmt.Errorf(
			"%w: library_id must be a canonical lower-case hyphenated UUID", ErrBadSeed)
	}
	for _, o := range []struct {
		what string
		b    []byte
	}{{"root", s.Root}, {"commit", s.Commit}} {
		if len(o.b) == 0 {
			return rootID, commitID, fmt.Errorf("%w: no %s object", ErrBadSeed, o.what)
		}
		if len(o.b) > MaxSeedObjectBytes {
			return rootID, commitID, fmt.Errorf("%w: the %s object is %d bytes",
				ErrBadSeed, o.what, len(o.b))
		}
	}
	if len(s.WrappedKey) == 0 || len(s.WrappedKey) > MaxSeedObjectBytes {
		return rootID, commitID, fmt.Errorf(
			"%w: wrapped_key is %d bytes, and the content key must have somewhere to live",
			ErrBadSeed, len(s.WrappedKey))
	}

	dir, err := storefmt.DecodeDirectoryPublic(s.Root)
	if err != nil {
		return rootID, commitID, fmt.Errorf("%w: the root does not decode as a directory: %v", ErrBadSeed, err)
	}
	if !dir.E2EE {
		return rootID, commitID, fmt.Errorf("%w: the root is a plain directory, not a sealed one", ErrBadSeed)
	}
	// Empty, because every entry would name an object that is not here: there
	// is no library yet to have uploaded one into.
	if len(dir.Entries) != 0 {
		return rootID, commitID, fmt.Errorf("%w: the root holds %d entries, and a new library starts empty",
			ErrBadSeed, len(dir.Entries))
	}

	commit, err := storefmt.DecodeCommitPublic(s.Commit)
	if err != nil {
		return rootID, commitID, fmt.Errorf("%w: the commit does not decode: %v", ErrBadSeed, err)
	}
	if !commit.E2EE {
		return rootID, commitID, fmt.Errorf("%w: the commit is a plain one, not a sealed one", ErrBadSeed)
	}
	if len(commit.Parents) != 0 {
		return rootID, commitID, fmt.Errorf("%w: the initial commit has %d parents",
			ErrBadSeed, len(commit.Parents))
	}

	rootID = storefmt.ObjectID(s.Root)
	commitID = storefmt.ObjectID(s.Commit)
	if commit.Root != rootID {
		return rootID, commitID, fmt.Errorf(
			"%w: the commit names root %s, and the root object sent is %s", ErrBadSeed, commit.Root, rootID)
	}
	return rootID, commitID, nil
}

// CreateEncryptedLibrary makes an end-to-end encrypted library from objects the
// client sealed, and records the content key wrapped to the owner.
//
// The order is objects first, rows second. Both objects are content-addressed
// and their writes are idempotent, so a failure after them leaves two
// unreferenced objects and no library -- which the tracing collector reclaims,
// and which is the harmless direction. The other order would leave a library
// whose head names an object that is not there, and that is a library
// GetWithReason reports as corrupted.
//
// The owner must already have published an identity key. Refusing otherwise is
// the whole reason this could not be built before: a content key wrapped to
// nothing lives on the device that generated it and dies with it.
func CreateEncryptedLibrary(name string, owner *account.Account, format Format, seed EncryptedSeed) (string, error) {
	if !format.E2EE {
		return "", fmt.Errorf("CreateEncryptedLibrary called with a plain format")
	}
	if err := format.Validate(); err != nil {
		return "", err
	}
	rootID, commitID, err := seed.Validate()
	if err != nil {
		return "", err
	}

	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	// Asked before anything is written. The id is the client's, so a collision
	// is a thing that happens to a caller rather than an internal error, and
	// it must not be answered by writing objects into another library's store.
	taken, err := libraryExists(ctx, seed.LibraryID)
	if err != nil {
		return "", err
	}
	if taken {
		return "", fmt.Errorf("%w: %s", ErrLibraryExists, seed.LibraryID)
	}

	st, err := OpenStore(seed.LibraryID, format)
	if err != nil {
		return "", fmt.Errorf("failed to open store for new library: %w", err)
	}
	if err := st.PutObject(rootID, seed.Root); err != nil {
		return "", fmt.Errorf("failed to store the root directory: %w", err)
	}
	if err := st.PutObject(commitID, seed.Commit); err != nil {
		return "", fmt.Errorf("failed to store the initial commit: %w", err)
	}

	tx, err := writeDB.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("failed to begin transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Unix()
	if err := insertLibraryRows(ctx, tx, libraryRows{
		LibraryID: seed.LibraryID, Name: name, Owner: owner, Format: format,
		CommitID: commitID.String(), RootID: rootID.String(), Now: now,
	}); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		dbutil.InsertOrReplace("LibraryKeyWrap", "library_id, account_id, wrapped_key, ctime"),
		seed.LibraryID, owner.ID, seed.WrappedKey, now); err != nil {
		return "", fmt.Errorf("failed to store the content key wrap: %v", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("failed to commit transaction: %v", err)
	}
	return seed.LibraryID, nil
}

// libraryExists reports whether the catalog already holds this id.
//
// It reads Library rather than Branch, because a library whose head row is
// missing is still an id in use -- handing it to a second creator would put
// two libraries in one object store.
func libraryExists(ctx context.Context, libraryID string) (bool, error) {
	var one int
	err := readDB.QueryRowContext(ctx,
		"SELECT 1 FROM Library WHERE library_id = ?", libraryID).Scan(&one)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, fmt.Errorf("failed to look for library %s: %v", libraryID, err)
}

// ContentKeyWrap returns the content key of an encrypted library, wrapped to
// one member's identity key, or ErrNoContentKeyWrap when there is none.
//
// The server cannot open it and has no use for it; this exists so a new device
// that has just unwrapped its identity key can unwrap the libraries that
// identity holds.
func ContentKeyWrap(ctx context.Context, libraryID string, holder account.ID) ([]byte, error) {
	var wrapped []byte
	err := readDB.QueryRowContext(ctx,
		"SELECT wrapped_key FROM LibraryKeyWrap WHERE library_id = ? AND account_id = ?",
		libraryID, holder).Scan(&wrapped)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoContentKeyWrap
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read the content key wrap for %s: %v", libraryID, err)
	}
	return wrapped, nil
}

// ErrNoContentKeyWrap means this account holds no wrap for this library --
// because the library is a plain one, or because nobody has shared it with
// them.
var ErrNoContentKeyWrap = errors.New("no content key wrap for this account and library")
