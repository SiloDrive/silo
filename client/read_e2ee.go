package client

// Reading an end-to-end encrypted library.
//
// The server cannot resolve a path here -- names are AES-SIV ciphertext under
// a key derived from each directory's own salt -- so the client walks the
// object graph itself: head commit, root directory, one directory per path
// segment, a manifest, then chunks. docs/protocol.md § The E2EE client shape
// is the owning document.
//
// Resolving a depth-N path costs N sequential fetches the first time and no
// cleverness removes it: a segment's name key comes from its parent's salt, so
// the parent has to be read before the child can even be named. What makes the
// second resolve cheap is the cache below, and it is a cache that cannot go
// stale: directory objects are content-addressed, so an id names one immutable
// set of bytes forever. Only the head moves, and moving it is Refresh.

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dkam/silo/store"
)

// Node is one entry as the tree holds it, with its name in the clear.
type Node struct {
	ID    store.ID
	Name  string
	Type  store.NodeType
	Mtime int64
	Mode  uint32
}

// IsDir reports whether the node is a directory.
func (n Node) IsDir() bool { return n.Type == store.NodeDir }

// EncryptedLibrary reads one library through the id-addressed surface.
//
// Safe for concurrent use: the cache is the only shared state and it is
// guarded. Two goroutines resolving the same cold path will both fetch it,
// which costs a duplicate request and never a wrong answer.
type EncryptedLibrary struct {
	ID string

	// Now is the clock a write stamps a mutation with, in unix seconds.
	//
	// A field rather than a call to time.Now because "the mutation's timestamp
	// is not the file's mtime" is a rule worth being able to pin, and because a
	// caller building a reproducible tree wants to decide it.
	Now func() int64

	c  *APIClient
	kr *store.Keyring

	mu       sync.Mutex
	head     store.ID
	root     store.ID
	haveHead bool
	dirs     map[store.ID]*directory
}

// directory is one decoded directory object and the cipher its children's
// names are under. The cipher is kept because deriving a name key is an HKDF
// and an AES setup, and a resolve does one per segment per call.
type directory struct {
	dir   *store.Directory
	names *store.NameCipher
}

// OpenEncryptedLibrary opens a library this account holds a content key for.
//
// store.ErrNoKeyring means there is no wrap for this account, which is what a
// plain library and one nobody has shared both look like.
func (a *Account) OpenEncryptedLibrary(libraryID string) (*EncryptedLibrary, error) {
	kr, err := a.OpenLibrary(libraryID)
	if err != nil {
		return nil, err
	}
	return NewEncryptedLibrary(a.c, libraryID, kr), nil
}

// NewEncryptedLibrary builds a reader over a keyring the caller already has --
// the one CreateEncryptedLibrary handed back, say, rather than one fetched
// again.
func NewEncryptedLibrary(c *APIClient, libraryID string, kr *store.Keyring) *EncryptedLibrary {
	return &EncryptedLibrary{
		ID: libraryID, c: c, kr: kr,
		Now:  func() int64 { return time.Now().Unix() },
		dirs: map[store.ID]*directory{},
	}
}

// Keyring is the library's content key and what is derived from it.
func (l *EncryptedLibrary) Keyring() *store.Keyring { return l.kr }

// Refresh forgets the head, so the next read sees writes that have landed
// since.
//
// The directory cache is kept: what it holds is addressed by id, and an id
// that was reachable from one commit and is not reachable from the next still
// names the same bytes.
func (l *EncryptedLibrary) Refresh() {
	l.mu.Lock()
	l.haveHead = false
	l.mu.Unlock()
}

// Head returns the head this library is being read at: the cached one if there
// is one, and otherwise whatever the server reports now.
func (l *EncryptedLibrary) Head() (store.ID, error) {
	head, _, err := l.at()
	return head, err
}

// Root returns the root directory of that head.
func (l *EncryptedLibrary) Root() (store.ID, error) {
	_, root, err := l.at()
	return root, err
}

// at returns the head and its root, reading them once and remembering both
// until Refresh.
//
// Both together, because a head and a root read separately can straddle
// somebody else's commit -- and a write that names one head and builds on
// another root is a write that silently discards whatever landed in between.
func (l *EncryptedLibrary) at() (head, root store.ID, err error) {
	l.mu.Lock()
	if l.haveHead {
		head, root = l.head, l.root
		l.mu.Unlock()
		return head, root, nil
	}
	l.mu.Unlock()

	head, err = l.serverHead()
	if err != nil {
		return store.ID{}, store.ID{}, err
	}
	b, err := l.c.Object(l.ID, head)
	if err != nil {
		return store.ID{}, store.ID{}, fmt.Errorf("client: reading the head commit: %w", err)
	}
	commit, err := l.kr.OpenCommit(b)
	if err != nil {
		return store.ID{}, store.ID{}, fmt.Errorf("client: opening the head commit: %w", err)
	}

	l.mu.Lock()
	l.head, l.root, l.haveHead = head, commit.Root, true
	l.mu.Unlock()
	return head, commit.Root, nil
}

// serverHead asks the server where the library is now, ignoring the cache.
func (l *EncryptedLibrary) serverHead() (store.ID, error) {
	libraries, err := l.c.ListLibraries()
	if err != nil {
		return store.ID{}, err
	}
	for _, lib := range libraries {
		if lib.ID != l.ID {
			continue
		}
		if lib.HeadCommitID == "" {
			return store.ID{}, fmt.Errorf("client: library %s reports no head commit", l.ID)
		}
		return store.ParseID(lib.HeadCommitID)
	}
	return store.ID{}, fmt.Errorf("%w: library %s", ErrNotFound, l.ID)
}

// directory fetches and decodes one directory object, or returns the cached
// one. Content-addressed, so a hit is always correct.
func (l *EncryptedLibrary) directory(id store.ID) (*directory, error) {
	l.mu.Lock()
	if d, ok := l.dirs[id]; ok {
		l.mu.Unlock()
		return d, nil
	}
	l.mu.Unlock()

	b, err := l.c.Object(l.ID, id)
	if err != nil {
		return nil, fmt.Errorf("client: reading directory %s: %w", id, err)
	}
	dir, err := l.kr.OpenDirectory(b)
	if err != nil {
		return nil, fmt.Errorf("client: opening directory %s: %w", id, err)
	}
	names, err := l.kr.NameCipher(dir.Salt)
	if err != nil {
		return nil, err
	}
	d := &directory{dir: dir, names: names}

	l.mu.Lock()
	l.dirs[id] = d
	l.mu.Unlock()
	return d, nil
}

// segments splits a library path. The root is no segments rather than one
// empty one, and "a//b" is "a/b" -- a doubled separator is a typo in a path,
// not a nameless directory between them.
func segments(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" && s != "." {
			out = append(out, s)
		}
	}
	return out
}

// Stat resolves a path to the entry that names it.
//
// The root has no entry of its own -- nothing names it but the commit -- so it
// is reported as a directory called "/" with no mtime or mode.
func (l *EncryptedLibrary) Stat(p string) (Node, error) {
	root, err := l.Root()
	if err != nil {
		return Node{}, err
	}
	at := Node{ID: root, Name: "/", Type: store.NodeDir}

	for i, seg := range segments(p) {
		if at.Type != store.NodeDir {
			return Node{}, fmt.Errorf("%w: %s is not a directory",
				ErrNotFound, strings.Join(segments(p)[:i], "/"))
		}
		d, err := l.directory(at.ID)
		if err != nil {
			return Node{}, err
		}
		// SIV is deterministic, so the wanted name encrypts to exactly the
		// bytes the object holds and the match is a comparison rather than a
		// decrypt of every entry.
		want, err := d.names.Encrypt(seg)
		if err != nil {
			return Node{}, err
		}
		found := false
		for _, e := range d.dir.Entries {
			if bytes.Equal(e.Name, want) {
				at = Node{ID: e.ChildID, Name: seg, Type: e.Type, Mtime: e.Mtime, Mode: e.Mode}
				found = true
				break
			}
		}
		if !found {
			return Node{}, fmt.Errorf("%w: %s", ErrNotFound, p)
		}
	}
	return at, nil
}

// List returns one directory's children, names in the clear.
func (l *EncryptedLibrary) List(p string) ([]Node, error) {
	at, err := l.Stat(p)
	if err != nil {
		return nil, err
	}
	if at.Type != store.NodeDir {
		return nil, fmt.Errorf("client: %s is not a directory", p)
	}
	d, err := l.directory(at.ID)
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(d.dir.Entries))
	for _, e := range d.dir.Entries {
		name, err := d.names.Decrypt(e.Name)
		if err != nil {
			return nil, fmt.Errorf("client: an entry of %s does not decrypt: %w", p, err)
		}
		out = append(out, Node{ID: e.ChildID, Name: name, Type: e.Type, Mtime: e.Mtime, Mode: e.Mode})
	}
	return out, nil
}

// DecryptPath turns a path as changes?since= reports it into a plaintext one.
//
// An encrypted library's changes carry each segment as unpadded base64url of
// its AES-SIV ciphertext, because the server builds that answer out of public
// directory sections and has nothing else to say. Decrypting it needs a walk:
// each segment is under its parent's salt, so the parents have to be read in
// order, which is the same cost as resolving the path and shares the same
// cache.
//
// It resolves against the current head, so a path whose parent directories
// have since been removed cannot be decrypted -- there is no directory left to
// hold the key. That is a real limit on a client that lets its anchor get old,
// and the answer is to read changes more often rather than to keep a key map
// the format does not have.
func (l *EncryptedLibrary) DecryptPath(p string) (string, error) {
	segs := segments(p)
	if len(segs) == 0 {
		return "/", nil
	}
	at, err := l.Root()
	if err != nil {
		return "", err
	}
	out := make([]string, 0, len(segs))
	for i, seg := range segs {
		ct, err := store.NameFromURL(seg)
		if err != nil {
			return "", fmt.Errorf("client: segment %d of %s: %w", i, p, err)
		}
		d, err := l.directory(at)
		if err != nil {
			return "", err
		}
		name, err := d.names.Decrypt(ct)
		if err != nil {
			return "", fmt.Errorf("client: segment %d of %s does not decrypt: %w", i, p, err)
		}
		out = append(out, name)
		if i == len(segs)-1 {
			break
		}
		child, kind, found, err := lookup(d.dir, d.names, name)
		switch {
		case err != nil:
			return "", err
		case !found:
			return "", fmt.Errorf("%w: %s is no longer in the tree", ErrNotFound, strings.Join(out, "/"))
		case kind != store.NodeDir:
			return "", fmt.Errorf("client: %s is not a directory", strings.Join(out, "/"))
		}
		at = child
	}
	return "/" + strings.Join(out, "/"), nil
}

// Directory reads the directory object a path names.
//
// Exported because the salt is part of it, and a caller rewriting a directory
// has to carry that salt forward -- which is a rule it cannot follow without
// being able to see it.
func (l *EncryptedLibrary) Directory(p string) (*store.Directory, error) {
	at, err := l.Stat(p)
	if err != nil {
		return nil, err
	}
	if at.Type != store.NodeDir {
		return nil, fmt.Errorf("client: %s is not a directory", p)
	}
	d, err := l.directory(at.ID)
	if err != nil {
		return nil, err
	}
	return d.dir, nil
}

// Manifest reads the manifest a path names.
func (l *EncryptedLibrary) Manifest(p string) (*store.Manifest, error) {
	at, err := l.Stat(p)
	if err != nil {
		return nil, err
	}
	if at.Type == store.NodeDir {
		return nil, fmt.Errorf("client: %s is a directory", p)
	}
	b, err := l.c.Object(l.ID, at.ID)
	if err != nil {
		return nil, fmt.Errorf("client: reading the manifest for %s: %w", p, err)
	}
	m, err := l.kr.OpenManifest(b)
	if err != nil {
		return nil, fmt.Errorf("client: opening the manifest for %s: %w", p, err)
	}
	return m, nil
}

// ReadFile reads a whole file.
func (l *EncryptedLibrary) ReadFile(p string) ([]byte, error) {
	m, err := l.Manifest(p)
	if err != nil {
		return nil, err
	}
	return l.read(m, 0, m.FileSize)
}

// ReadAt reads n bytes from off.
//
// A ranged read on an encrypted library is arithmetic over the manifest and
// nothing else: chunk sizes there are plaintext lengths, so the chunks a range
// touches are known before a byte is fetched, and only those are. A short
// answer means the range ran past the end of the file, exactly as it would on
// a plain one.
func (l *EncryptedLibrary) ReadAt(p string, off, n int64) ([]byte, error) {
	m, err := l.Manifest(p)
	if err != nil {
		return nil, err
	}
	return l.read(m, off, n)
}

func (l *EncryptedLibrary) read(m *store.Manifest, off, n int64) ([]byte, error) {
	if off < 0 || n < 0 {
		return nil, fmt.Errorf("client: read at %d for %d bytes", off, n)
	}
	end := min(off+n, m.FileSize)
	if off >= end {
		return []byte{}, nil
	}
	if len(m.Chunks) == 0 {
		return bytes.Clone(m.Inline[off:end]), nil
	}

	// Which chunks the range touches, and where each one starts. Sizes are
	// plaintext, so this is settled before anything is fetched.
	type span struct {
		ref   store.ChunkRef
		start int64
	}
	var want []span
	var pos int64
	for _, ref := range m.Chunks {
		if pos >= end {
			break
		}
		if pos+ref.Size > off {
			want = append(want, span{ref: ref, start: pos})
		}
		pos += ref.Size
	}

	ids := make([]store.ID, 0, len(want))
	seen := make(map[store.ID]bool, len(want))
	for _, s := range want {
		if !seen[s.ref.ID] {
			seen[s.ref.ID] = true
			ids = append(ids, s.ref.ID)
		}
	}
	frames, err := l.c.FetchChunks(l.ID, ids)
	if err != nil {
		return nil, err
	}

	out := make([]byte, 0, end-off)
	opened := make(map[store.ID][]byte, len(ids))
	for _, s := range want {
		plain, ok := opened[s.ref.ID]
		if !ok {
			frame, held := frames[s.ref.ID]
			if !held {
				return nil, fmt.Errorf("%w: chunk %s", ErrNotFound, s.ref.ID)
			}
			// The plaintext hash comes from the manifest, not from the id: the
			// id is SHA-256 of the ciphertext and the chunk key is derived from
			// SHA-256 of the plaintext, and there is no path between them.
			plain, err = l.kr.OpenChunk(s.ref.PlaintextHash, frame)
			if err != nil {
				return nil, fmt.Errorf("client: opening chunk %s: %w", s.ref.ID, err)
			}
			if int64(len(plain)) != s.ref.Size {
				return nil, fmt.Errorf("client: chunk %s is %d bytes, and its manifest says %d",
					s.ref.ID, len(plain), s.ref.Size)
			}
			opened[s.ref.ID] = plain
		}
		lo := max(off-s.start, 0)
		hi := min(end-s.start, s.ref.Size)
		out = append(out, plain[lo:hi]...)
	}
	if int64(len(out)) != end-off {
		return nil, fmt.Errorf("client: assembled %d bytes for a %d-byte range", len(out), end-off)
	}
	return out, nil
}
