package client

// Writing an end-to-end encrypted library.
//
// Everything a write needs, the server cannot do: it cannot chunk a file it
// cannot read, cannot name an entry, cannot rewrite a directory and cannot
// merge two of them. So a write here is the whole spine -- a new manifest, a
// new parent, a new grandparent, up to a new root and a new commit -- computed
// on the client and stored verbatim. docs/protocol.md § A write rewrites the
// spine is the owning document, and its three rules are what this file exists
// to get right:
//
//   - a rewritten directory carries the salt of the object it replaces, or its
//     id changes, so every ancestor's does, so changes?since= reports the whole
//     library modified on every commit;
//   - only the directory whose entry list changed gets a new mtime, and that
//     mtime lives one level up, in its parent's entry for it;
//   - the mutation's timestamp is not the file's mtime, which may be years old.
//
// PUT head is a compare-and-swap with no merge behind it, so the last step is
// a loop: lose the swap, re-read, rebuild on the new root, try again.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/dkam/silo/store"
)

// headAttempts bounds the rebuild loop.
//
// A bound rather than a deadline because each attempt is a full rebuild and
// losing repeatedly means a library under sustained write from somewhere else;
// a client that spun on that forever would look like a hang rather than a
// contended write. Losing ten times in a row is a report, not a retry.
const headAttempts = 10

// WriteFile writes bytes to a path, replacing whatever was there.
//
// mtime is the file's own, in unix seconds -- it is preserved and is not the
// time of this write. The parent directory must exist; MkdirAll makes one.
//
// The new head is not returned because it is already known: a write updates
// this library's cached head, so Head answers with it and costs nothing.
func (l *EncryptedLibrary) WriteFile(p string, data []byte, mtime int64) error {
	return l.WriteFrom(p, bytes.NewReader(data), mtime)
}

// WriteFrom is WriteFile reading from a stream.
//
// Bounded memory however large the file: chunks are sealed and sent in
// batches, so what is held is one batch rather than one file. What is not
// bounded is the chunk list, which is the manifest and has to be complete
// before the manifest can be sealed.
func (l *EncryptedLibrary) WriteFrom(p string, r io.Reader, mtime int64) error {
	segs := segments(p)
	if len(segs) == 0 {
		return errors.New("client: the root is not a file")
	}
	manifest, err := l.writeContent(r)
	if err != nil {
		return err
	}
	name := segs[len(segs)-1]
	return l.mutate(segs[:len(segs)-1], false, func(sp *spine, now int64) error {
		return sp.set(store.DirEntry{
			ChildID: manifest, Type: store.NodeFile,
			Mtime: mtime, Mode: 0o644,
		}, name)
	})
}

// MkdirAll creates a directory and any missing parents, and is content with
// one that already exists.
func (l *EncryptedLibrary) MkdirAll(p string) error {
	segs := segments(p)
	if len(segs) == 0 {
		_, err := l.Root()
		return err
	}
	return l.mutate(segs, true, func(sp *spine, now int64) error { return nil })
}

// Remove deletes one entry. A directory is removed with whatever it holds:
// nothing else names those objects, and the next collection reclaims them.
func (l *EncryptedLibrary) Remove(p string) error {
	segs := segments(p)
	if len(segs) == 0 {
		return errors.New("client: the root cannot be removed")
	}
	name := segs[len(segs)-1]
	return l.mutate(segs[:len(segs)-1], false, func(sp *spine, now int64) error {
		return sp.remove(name)
	})
}

// spine is one root-to-leaf chain of directories being rewritten, root first.
//
// It holds copies rather than the cached objects: the cache is keyed by object
// id and its entries are shared, so mutating one would corrupt every reader
// that had already resolved through it.
type spine struct {
	dirs []*store.Directory
	// names[i] is the plaintext name of dirs[i+1] inside dirs[i].
	names []string
	// changed is the shallowest directory whose entry list actually changed.
	// Everything from there down gets a fresh mtime in its parent's entry;
	// everything above it only gets its child's new id, because its own entry
	// list is the same list naming the same names.
	changed int
	// ciphers[i] names dirs[i]'s children.
	ciphers []*store.NameCipher
}

// leaf is the directory a mutation acts on.
func (sp *spine) leaf() *store.Directory { return sp.dirs[len(sp.dirs)-1] }

// set inserts or replaces one entry by plaintext name.
//
// The error is the name the format refuses -- empty, too long, or holding a
// separator. It is returned rather than worked around: the only fallback for a
// name that will not encrypt is to store it in the clear, which would put a
// plaintext filename inside an encrypted library.
func (sp *spine) set(e store.DirEntry, name string) error {
	enc, err := sp.ciphers[len(sp.dirs)-1].Encrypt(name)
	if err != nil {
		return err
	}
	sp.markChanged(len(sp.dirs) - 1)
	e.Name = enc
	d := sp.leaf()
	for i := range d.Entries {
		if bytes.Equal(d.Entries[i].Name, enc) {
			d.Entries[i] = e
			return nil
		}
	}
	d.Entries = append(d.Entries, e)
	return nil
}

// remove deletes one entry by plaintext name.
func (sp *spine) remove(name string) error {
	enc, err := sp.ciphers[len(sp.dirs)-1].Encrypt(name)
	if err != nil {
		return err
	}
	d := sp.leaf()
	for i := range d.Entries {
		if bytes.Equal(d.Entries[i].Name, enc) {
			d.Entries = append(d.Entries[:i], d.Entries[i+1:]...)
			sp.markChanged(len(sp.dirs) - 1)
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrNotFound, name)
}

func (sp *spine) markChanged(i int) {
	if i < sp.changed {
		sp.changed = i
	}
}

// mutate applies one change to the tree and publishes it, rebuilding and
// retrying if the head moves underneath.
//
// segs names the directory the change acts on. create makes the missing part
// of that path rather than refusing it.
func (l *EncryptedLibrary) mutate(segs []string, create bool, apply func(*spine, int64) error) error {
	var last error
	for attempt := 0; attempt < headAttempts; attempt++ {
		head, root, err := l.at()
		if err != nil {
			return err
		}
		now := l.Now()

		sp, err := l.openSpine(root, segs, create, now)
		if err != nil {
			return err
		}
		if err := apply(sp, now); err != nil {
			return err
		}
		newRoot, err := l.publish(sp, now)
		if err != nil {
			return err
		}
		// Nothing changed: an already-existing MkdirAll, or a write of the
		// bytes that were there. Publishing an empty commit would move the
		// head and report every watcher a change that did not happen.
		if newRoot == root {
			return nil
		}

		commit, err := l.kr.SealCommit(&store.Commit{
			Root: newRoot, Parents: []store.ID{head}, CreatedAt: now,
		})
		if err != nil {
			return err
		}
		commitID := store.ObjectID(commit)
		if err := l.c.PutObject(l.ID, commitID, commit); err != nil {
			return fmt.Errorf("client: storing the commit: %w", err)
		}

		switch err := l.c.PutHead(l.ID, commitID, head); {
		case err == nil:
			l.mu.Lock()
			l.head, l.root, l.haveHead = commitID, newRoot, true
			l.mu.Unlock()
			return nil
		case errors.Is(err, ErrHeadMoved):
			// Somebody else committed. The objects just written are not
			// wasted -- they are addressed by content, so the rebuild reuses
			// every one whose bytes are unchanged, and the collection reclaims
			// the rest.
			last = err
			l.Refresh()
		default:
			return err
		}
	}
	return fmt.Errorf("client: the head moved on %d attempts in a row: %w", headAttempts, last)
}

// openSpine reads the directories from the root down to segs, as copies that
// can be rewritten.
func (l *EncryptedLibrary) openSpine(root store.ID, segs []string, create bool, now int64) (*spine, error) {
	sp := &spine{changed: len(segs) + 1}
	at := root
	for depth := 0; ; depth++ {
		var dir *store.Directory
		var names *store.NameCipher
		if at == (store.ID{}) {
			// A directory that does not exist yet: a fresh salt, because it is
			// a new object rather than a rewrite of one.
			salt, err := newDirSalt()
			if err != nil {
				return nil, err
			}
			dir = &store.Directory{Salt: salt}
			if names, err = l.kr.NameCipher(salt); err != nil {
				return nil, err
			}
			// A new directory is a change to its parent's entry list, and the
			// parent is one shallower.
			sp.markChanged(depth - 1)
		} else {
			d, err := l.directory(at)
			if err != nil {
				return nil, err
			}
			dir, names = cloneDir(d.dir), d.names
		}
		sp.dirs = append(sp.dirs, dir)
		sp.ciphers = append(sp.ciphers, names)
		if depth == len(segs) {
			break
		}

		seg := segs[depth]
		child, kind, found, err := lookup(dir, names, seg)
		switch {
		case err != nil:
			return nil, err
		case found && kind != store.NodeDir:
			return nil, fmt.Errorf("client: %s is not a directory", strings.Join(segs[:depth+1], "/"))
		case found:
			at = child
		case create:
			at = store.ID{}
		default:
			return nil, fmt.Errorf("%w: %s", ErrNotFound, strings.Join(segs[:depth+1], "/"))
		}
		sp.names = append(sp.names, seg)
	}
	return sp, nil
}

// publish seals the spine from the deepest directory up, writing each new id
// into its parent's entry, and returns the new root id.
func (l *EncryptedLibrary) publish(sp *spine, now int64) (store.ID, error) {
	var childID store.ID
	for i := len(sp.dirs) - 1; ; i-- {
		if i < len(sp.dirs)-1 {
			// Update the entry for the child just sealed. Its mtime moves only
			// if the child is at or below the shallowest directory whose entry
			// list changed; above that, nothing about this directory's
			// contents changed but one id.
			name, err := sp.ciphers[i].Encrypt(sp.names[i])
			if err != nil {
				return store.ID{}, err
			}
			d := sp.dirs[i]
			at := -1
			for j := range d.Entries {
				if bytes.Equal(d.Entries[j].Name, name) {
					at = j
					break
				}
			}
			e := store.DirEntry{ChildID: childID, Type: store.NodeDir, Name: name, Mode: 0o755}
			if at >= 0 {
				e.Mtime = d.Entries[at].Mtime
			}
			if i+1 >= sp.changed {
				e.Mtime = now
			}
			if at >= 0 {
				d.Entries[at] = e
			} else {
				d.Entries = append(d.Entries, e)
			}
		}

		encoded, err := l.kr.SealDirectory(sp.dirs[i])
		if err != nil {
			return store.ID{}, err
		}
		childID = store.ObjectID(encoded)
		if err := l.c.PutObject(l.ID, childID, encoded); err != nil {
			return store.ID{}, fmt.Errorf("client: storing a directory: %w", err)
		}
		if i == 0 {
			return childID, nil
		}
	}
}

// writeContent turns a stream into a stored manifest and returns its id.
func (l *EncryptedLibrary) writeContent(r io.Reader) (store.ID, error) {
	// Whether a file inlines is a function of its size alone, so the first
	// question is whether it reaches the threshold -- which is answered by
	// reading that much and seeing whether the stream ends.
	head := make([]byte, store.InlineThreshold)
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return store.ID{}, err
	}
	head = head[:n]

	m := &store.Manifest{}
	if n < store.InlineThreshold {
		m.FileSize, m.Inline = int64(n), head
	} else {
		refs, size, err := l.uploadChunksOf(io.MultiReader(bytes.NewReader(head), r))
		if err != nil {
			return store.ID{}, err
		}
		m.FileSize, m.Chunks = size, refs
	}

	encoded, err := l.kr.SealManifest(m)
	if err != nil {
		return store.ID{}, err
	}
	id := store.ObjectID(encoded)
	if err := l.c.PutObject(l.ID, id, encoded); err != nil {
		return store.ID{}, fmt.Errorf("client: storing the manifest: %w", err)
	}
	return id, nil
}

// uploadChunksOf chunks a stream under the library's own seed, seals each
// chunk and sends the ones the server does not already hold.
//
// Batched, so memory is one batch rather than one file, and asked about before
// sent: a chunk the server holds is a chunk nobody has to transfer, which is
// the whole reason the ids are content hashes.
func (l *EncryptedLibrary) uploadChunksOf(r io.Reader) ([]store.ChunkRef, int64, error) {
	chunker, err := store.NewChunker(l.kr.Params(), r)
	if err != nil {
		return nil, 0, err
	}

	var (
		refs    []store.ChunkRef
		size    int64
		pending []string
		bytesIn int64
		sources = map[string]chunkSource{}
	)
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		missing, err := l.c.MissingChunks(l.ID, pending)
		if err != nil {
			return err
		}
		if err := l.c.sendChunks(l.ID, missing, sources, func(int, int64) {}); err != nil {
			return err
		}
		pending, bytesIn, sources = nil, 0, map[string]chunkSource{}
		return nil
	}

	for {
		c, err := chunker.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, 0, err
		}
		sealed, err := l.kr.SealChunk(c.Data)
		if err != nil {
			return nil, 0, err
		}
		refs = append(refs, store.ChunkRef{
			ID: sealed.ID, Size: int64(len(c.Data)), PlaintextHash: sealed.PlaintextHash,
		})
		size += int64(len(c.Data))

		// A repeated chunk is sent once and named as often as it occurs.
		id := sealed.ID.String()
		if _, seen := sources[id]; !seen {
			sources[id] = fromBytes(sealed.Frame)
			pending = append(pending, id)
			bytesIn += int64(len(sealed.Frame))
		}
		if len(pending) >= maxChunksPerUpload || bytesIn >= maxUploadBatchSize {
			if err := flush(); err != nil {
				return nil, 0, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, 0, err
	}
	return refs, size, nil
}

// lookup finds one child by plaintext name. SIV is deterministic, so the
// wanted name encrypts to exactly the bytes the object holds.
func lookup(d *store.Directory, names *store.NameCipher, seg string) (store.ID, store.NodeType, bool, error) {
	want, err := names.Encrypt(seg)
	if err != nil {
		return store.ID{}, 0, false, err
	}
	for _, e := range d.Entries {
		if bytes.Equal(e.Name, want) {
			return e.ChildID, e.Type, true, nil
		}
	}
	return store.ID{}, 0, false, nil
}

// cloneDir copies a directory deeply enough to be rewritten: the cache hands
// out shared pointers, and an entry slice appended to in place would be
// appended to under every reader that had already resolved through it.
func cloneDir(d *store.Directory) *store.Directory {
	out := &store.Directory{Salt: d.Salt, Entries: make([]store.DirEntry, len(d.Entries))}
	copy(out.Entries, d.Entries)
	return out
}
