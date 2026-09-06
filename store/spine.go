package store

import (
	"errors"
	"fmt"
)

// A write rewrites the spine: the chain of directories from the root down to
// the one that changed. Three rules decide the bytes, and all three are here
// rather than in a client, because their only executable statement used to be
// welded to a network transport and a port had prose to conform to.
//
//  1. **The salt is carried forward.** A rewritten directory keeps the salt the
//     object it replaces was created with. RewriteSpine never mints one — the
//     caller reads the current directory first, which it must anyway, and a
//     directory being created arrives with its fresh salt already set.
//  2. **Only the changed directory's mtime moves.** The entry naming a child is
//     stamped with now when that child is at or below the shallowest directory
//     whose entry list actually changed; above that, the entry keeps the mtime
//     it had and takes only its child's new id. A writer that stamps every
//     level reports the whole path modified on every write.
//  3. **The mutation's timestamp is not the file's.** now is when the tree
//     changed. A file's own mtime rides in its own entry, which the caller sets
//     before calling this.
//
// A fourth thing falls out of the encoding being deterministic: a directory
// whose sealed bytes come out at the id it was read at has not changed, so the
// caller can skip storing it. RewriteSpine returns every level's bytes and
// leaves that comparison to the caller, which is the side that knows what it
// read.

// SpineDir is one directory in a root-to-leaf chain, with the plaintext name
// of the next directory down inside it.
type SpineDir struct {
	// Dir is the directory to rewrite. RewriteSpine mutates it — see
	// Directory.Clone for when that matters.
	Dir *Directory
	// Name is the plaintext name of the next level down inside Dir. Empty at
	// the leaf, which has no next level.
	Name string
}

// ErrSpine reports a chain RewriteSpine cannot rewrite.
var ErrSpine = errors.New("store: spine")

// RewriteSpine seals a root-to-leaf chain from the deepest directory up,
// writing each new id into its parent's entry, and returns every level's
// sealed bytes in chain order together with the new root id.
//
// changed is the index in chain of the shallowest directory whose entry list
// the caller altered; any value past the leaf means nothing changed, and no
// entry gets a new mtime. now is unix seconds, and is the mutation's time
// rather than any file's.
//
// The returned slice is aligned with chain, so sealed[i] is the object
// chain[i].Dir became. A caller that knows the id each level was read at skips
// storing the ones whose ObjectID is unchanged; a caller that stores them all
// writes objects that were already there, which is wasteful but not wrong.
// Store them deepest-first, so no stored parent ever names an absent child.
func (k *Keyring) RewriteSpine(chain []SpineDir, changed int, now int64) ([][]byte, ID, error) {
	if len(chain) == 0 {
		return nil, ID{}, fmt.Errorf("%w: the chain is empty", ErrSpine)
	}

	sealed := make([][]byte, len(chain))
	var childID ID
	for i := len(chain) - 1; i >= 0; i-- {
		d := chain[i].Dir
		if d == nil {
			return nil, ID{}, fmt.Errorf("%w: level %d has no directory", ErrSpine, i)
		}

		if i < len(chain)-1 {
			// Update the entry for the child just sealed. Its mtime moves only
			// if the child is at or below the shallowest directory whose entry
			// list changed; above that, nothing about this directory's
			// contents changed but one id.
			names, err := k.NameCipher(d.Salt)
			if err != nil {
				return nil, ID{}, err
			}
			name, err := names.Encrypt(chain[i].Name)
			if err != nil {
				return nil, ID{}, err
			}
			e := DirEntry{ChildID: childID, Type: NodeDir, Name: name, Mode: 0o755}
			if at := d.EntryIndex(name); at >= 0 {
				e.Mtime = d.Entries[at].Mtime
			}
			if i+1 >= changed {
				e.Mtime = now
			}
			d.SetEntry(e)
		}

		encoded, err := k.SealDirectory(d)
		if err != nil {
			return nil, ID{}, err
		}
		sealed[i] = encoded
		childID = ObjectID(encoded)
	}
	return sealed, childID, nil
}
