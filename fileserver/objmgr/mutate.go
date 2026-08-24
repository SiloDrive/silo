// Tree mutation: the inverse of tree.go.
//
// Every object in this format is immutable and named by its content, so
// "changing a file" means building a new manifest, a new directory holding it,
// a new directory holding that, up to a new root — and then a commit pointing
// at it. Nothing is edited in place and nothing is deleted here; what falls
// out of reach is the collector's problem, not this file's.
package objmgr

import (
	"errors"
	"fmt"
	"path"
	"slices"

	"github.com/dkam/silo/store"
)

// ErrExists reports a path that already holds an entry, for the operations
// that refuse to overwrite one.
var ErrExists = errors.New("objmgr: entry already exists")

// ErrIsDir reports a path that names a directory where a non-directory was
// required.
var ErrIsDir = errors.New("objmgr: is a directory")

// ErrInvalidPath reports a path an operation cannot act on at all — the root
// where an entry was needed, or a rename into its own subtree.
var ErrInvalidPath = errors.New("objmgr: invalid path")

// EmptyDir stores an empty directory and returns its id, with a fresh salt in
// an E2EE library. It is where a new library's root and every new subdirectory
// start.
func (s *Store) EmptyDir() (store.ID, error) {
	salt, err := s.NewDirSalt()
	if err != nil {
		return store.ID{}, err
	}
	return s.PutDir(salt, nil)
}

// splitLeaf splits a path into the segments of its parent directory and the
// name of the entry itself. The root has no name, so it is refused: an
// operation that acts on an entry needs one to act on.
//
// It is a method because the key check belongs in front of it rather than
// after: a store with no content key cannot write names at all, and it should
// say so before it says anything about the path it was handed.
func (s *Store) splitLeaf(p string) ([]string, string, error) {
	if s.e2ee && !s.HasKey() {
		return nil, "", ErrNoContentKey
	}
	segments, err := SplitPath(p)
	if err != nil {
		return nil, "", err
	}
	if len(segments) == 0 {
		return nil, "", fmt.Errorf("%w: %q names the root, which has no entry", ErrInvalidPath, p)
	}
	return segments[:len(segments)-1], segments[len(segments)-1], nil
}

// rewrite rebuilds the spine from root down to the directory named by
// segments, applies fn to that directory's entries, and returns the new id of
// root.
//
// It works on stored entries rather than on Nodes, so a mutation encrypts or
// decrypts only the one name it touches — the same reason lookup matches on
// ciphertext. Rewriting a thousand-entry directory to add one file would
// otherwise decrypt a thousand names and re-encrypt them.
//
// Two rules about what the rewrite carries forward:
//
// Each directory keeps the salt of the object it replaces, read from that
// object. A fresh salt would change the directory's id, hence every
// ancestor's, hence the root, and changes?since= would report the whole tree
// modified on every write.
//
// Only the directory whose entry list changed gets a new mtime, and that mtime
// lives one level up, in its parent's entry for it — so exactly one ancestor
// is touched beyond having its child id swapped. That is POSIX's rule:
// creating or removing an entry changes the containing directory's mtime, and
// changes nothing above it. When the mutated directory is the root there is no
// entry to carry it, and the root's mtime is its commit's created_at.
//
// A mutation that changes nothing reproduces the ids it started from, all the
// way up: the encoding is canonical, so an identical entry list is an
// identical object.
func (s *Store) rewrite(root store.ID, segments []string, now int64, fn func(*dirReader) ([]store.DirEntry, error)) (store.ID, error) {
	if s.e2ee && !s.HasKey() {
		return store.ID{}, ErrNoContentKey
	}
	return s.rewriteAt(root, segments, 0, now, fn)
}

func (s *Store) rewriteAt(dirID store.ID, segments []string, depth int, now int64, fn func(*dirReader) ([]store.DirEntry, error)) (store.ID, error) {
	r, err := s.openDir(dirID)
	if err != nil {
		return store.ID{}, err
	}
	if depth == len(segments) {
		entries, err := fn(r)
		if err != nil {
			return store.ID{}, err
		}
		return s.PutDirectory(&store.Directory{Salt: r.dir.Salt, Entries: entries})
	}

	i, err := r.index(segments[depth])
	if err != nil {
		return store.ID{}, err
	}
	here := path.Join(segments[:depth+1]...)
	if i < 0 {
		return store.ID{}, fmt.Errorf("%w: %q", ErrNotFound, here)
	}
	if r.dir.Entries[i].Type != store.NodeDir {
		return store.ID{}, fmt.Errorf("%w: %q", ErrNotDir, here)
	}

	child, err := s.rewriteAt(r.dir.Entries[i].ChildID, segments, depth+1, now, fn)
	if err != nil {
		return store.ID{}, err
	}
	entries := slices.Clone(r.dir.Entries)
	entries[i].ChildID = child
	if depth == len(segments)-1 {
		entries[i].Mtime = now
	}
	return s.PutDirectory(&store.Directory{Salt: r.dir.Salt, Entries: entries})
}

// PutNode places n at path p and returns the new root id.
//
// An existing file at p is replaced; an existing directory is not, because
// replacing a directory with a file would drop its whole subtree as a side
// effect of a write. Remove it first and mean it.
//
// The name comes from the path. n.Name may be empty or may repeat the last
// segment, but a node whose name disagrees with where it is being put is
// refused rather than silently renamed.
//
// now is when the mutation happened, and it is separate from n.Mtime on
// purpose: a client uploading a file preserves that file's mtime, which may be
// years old, while the directory it lands in changed just now.
//
// Writing a file is this, composed: WriteFile for the chunks and the manifest,
// PutManifest for its id, PutNode for the tree, PutCommit for the root.
func (s *Store) PutNode(root store.ID, p string, n Node, now int64) (store.ID, error) {
	parent, name, err := s.splitLeaf(p)
	if err != nil {
		return store.ID{}, err
	}
	if n.Name != "" && n.Name != name {
		return store.ID{}, fmt.Errorf("%w: node named %q cannot be put at %q", ErrInvalidPath, n.Name, p)
	}

	return s.rewrite(root, parent, now, func(r *dirReader) ([]store.DirEntry, error) {
		stored, err := r.stored(name)
		if err != nil {
			return nil, err
		}
		i, err := r.index(name)
		if err != nil {
			return nil, err
		}
		e := store.DirEntry{ChildID: n.ID, Type: n.Type, Name: stored, Mtime: n.Mtime, Mode: n.Mode}
		entries := slices.Clone(r.dir.Entries)
		if i < 0 {
			return append(entries, e), nil
		}
		if entries[i].Type == store.NodeDir {
			return nil, fmt.Errorf("%w: %q", ErrIsDir, p)
		}
		entries[i] = e
		return entries, nil
	})
}

// Mkdir creates an empty directory at p and returns the new root id. The
// parent must exist; an entry already at p is an error, whatever its type.
func (s *Store) Mkdir(root store.ID, p string, mode uint32, now int64) (store.ID, error) {
	parent, name, err := s.splitLeaf(p)
	if err != nil {
		return store.ID{}, err
	}
	empty, err := s.EmptyDir()
	if err != nil {
		return store.ID{}, err
	}

	return s.rewrite(root, parent, now, func(r *dirReader) ([]store.DirEntry, error) {
		i, err := r.index(name)
		if err != nil {
			return nil, err
		}
		if i >= 0 {
			return nil, fmt.Errorf("%w: %q", ErrExists, p)
		}
		stored, err := r.stored(name)
		if err != nil {
			return nil, err
		}
		return append(slices.Clone(r.dir.Entries), store.DirEntry{
			ChildID: empty, Type: store.NodeDir, Name: stored, Mtime: now, Mode: mode,
		}), nil
	})
}

// MkdirAll creates p and every missing directory above it, returning the new
// root id — or root itself, unchanged, if the whole path is already there.
//
// The missing tail is built bottom-up and grafted on with a single spine
// rewrite, rather than one rewrite per level: the new directories have no
// history to carry forward, so there is nothing to read before writing them.
//
// An existing non-directory anywhere along p is an error, as it is for mkdir
// -p.
func (s *Store) MkdirAll(root store.ID, p string, mode uint32, now int64) (store.ID, error) {
	if s.e2ee && !s.HasKey() {
		return store.ID{}, ErrNoContentKey
	}
	segments, err := SplitPath(p)
	if err != nil {
		return store.ID{}, err
	}

	// Walk as far down as the tree already goes.
	missing := 0
	cur := root
	for ; missing < len(segments); missing++ {
		r, err := s.openDir(cur)
		if err != nil {
			return store.ID{}, err
		}
		i, err := r.index(segments[missing])
		if err != nil {
			return store.ID{}, err
		}
		if i < 0 {
			break
		}
		if r.dir.Entries[i].Type != store.NodeDir {
			return store.ID{}, fmt.Errorf("%w: %q", ErrNotDir, path.Join(segments[:missing+1]...))
		}
		cur = r.dir.Entries[i].ChildID
	}
	if missing == len(segments) {
		return root, nil
	}

	id, err := s.EmptyDir()
	if err != nil {
		return store.ID{}, err
	}
	for i := len(segments) - 1; i > missing; i-- {
		salt, err := s.NewDirSalt()
		if err != nil {
			return store.ID{}, err
		}
		id, err = s.PutDir(salt, []Node{{
			ID: id, Type: store.NodeDir, Name: segments[i], Mtime: now, Mode: mode,
		}})
		if err != nil {
			return store.ID{}, err
		}
	}
	return s.PutNode(root, path.Join(segments[:missing+1]...), Node{
		ID: id, Type: store.NodeDir, Name: segments[missing], Mtime: now, Mode: mode,
	}, now)
}

// Remove deletes the entry at p and returns the new root id.
//
// A directory goes with everything under it. There is no rmdir-refuses-a-
// non-empty-directory rule here and there is nowhere to put one: dropping the
// entry is a single write whatever the subtree holds, and the subtree does not
// go anywhere — it stays exactly as reachable from every older commit that
// referenced it, which is what makes history work. The collector reclaims what
// no commit can reach.
func (s *Store) Remove(root store.ID, p string, now int64) (store.ID, error) {
	parent, name, err := s.splitLeaf(p)
	if err != nil {
		return store.ID{}, err
	}

	return s.rewrite(root, parent, now, func(r *dirReader) ([]store.DirEntry, error) {
		i, err := r.index(name)
		if err != nil {
			return nil, err
		}
		if i < 0 {
			return nil, fmt.Errorf("%w: %q", ErrNotFound, p)
		}
		return slices.Delete(slices.Clone(r.dir.Entries), i, i+1), nil
	})
}

// Rename moves the entry at from to to, and returns the new root id.
//
// A destination holding a directory is always refused — replacing it would
// drop its whole subtree as a side effect of a move — and so is moving a
// directory onto an existing file, which the caller has to decide about
// rather than a tree layer picking silently. File onto file is the one
// collision that destroys nothing the caller did not name, and it is
// allowed, replacing the destination: the same rule PutNode already applies
// when a path is written directly.
//
// It also refuses to move a directory into its own subtree, which would
// detach the subtree from the root and leave it referenced only by itself.
//
// The move is a remove and a put, so the tree is rewritten twice. Only the
// second root is ever published: the intermediate is an id nothing points at.
func (s *Store) Rename(root store.ID, from, to string, now int64) (store.ID, error) {
	if s.e2ee && !s.HasKey() {
		return store.ID{}, ErrNoContentKey
	}
	src, err := SplitPath(from)
	if err != nil {
		return store.ID{}, err
	}
	dst, err := SplitPath(to)
	if err != nil {
		return store.ID{}, err
	}
	if len(src) == 0 || len(dst) == 0 {
		return store.ID{}, fmt.Errorf("%w: the root cannot be renamed", ErrInvalidPath)
	}
	// Segment-wise, so that renaming "a/b" to "a/bc" is allowed and renaming
	// it to "a/b/c" is not.
	if len(dst) >= len(src) && slices.Equal(src, dst[:len(src)]) {
		return store.ID{}, fmt.Errorf("%w: %q is inside %q", ErrInvalidPath, to, from)
	}

	node, err := s.Resolve(root, from)
	if err != nil {
		return store.ID{}, err
	}
	if dst, err := s.Resolve(root, to); err == nil {
		switch {
		case dst.IsDir():
			return store.ID{}, fmt.Errorf("%w: %q", ErrIsDir, to)
		case node.IsDir():
			return store.ID{}, fmt.Errorf("%w: %q", ErrExists, to)
		}
	} else if !errors.Is(err, ErrNotFound) {
		return store.ID{}, err
	}

	mid, err := s.Remove(root, from, now)
	if err != nil {
		return store.ID{}, err
	}
	node.Name = dst[len(dst)-1]
	return s.PutNode(mid, to, node, now)
}
