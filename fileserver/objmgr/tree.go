package objmgr

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/dkam/silo/store"
)

// ErrNotFound reports a path or entry that is not in the tree. It is distinct
// from objstore's missing-object error on purpose: "there is no such file" is
// an answer, and "the object this tree references is gone" is damage.
var ErrNotFound = errors.New("objmgr: no such entry")

// ErrNotDir reports a path that tried to descend through a file.
var ErrNotDir = errors.New("objmgr: not a directory")

// Node is one entry in a tree, with its name already in the clear.
type Node struct {
	ID    store.ID
	Type  store.NodeType
	Name  string
	Mtime int64
	Mode  uint32
}

// IsDir reports whether the node is a directory.
func (n Node) IsDir() bool { return n.Type == store.NodeDir }

// dirReader is a directory plus whatever is needed to read its entry names.
//
// The name cipher is built once per directory rather than once per entry: it
// holds two AES-256 key schedules and a CMAC subkey derivation, and a
// thousand-entry listing that built one per name would pay for two thousand
// key schedules instead of two.
type dirReader struct {
	dir    *store.Directory
	cipher *store.NameCipher
}

func (s *Store) openDir(id store.ID) (*dirReader, error) {
	dir, err := s.GetDirectory(id)
	if err != nil {
		return nil, err
	}
	r := &dirReader{dir: dir}
	if !s.e2ee {
		return r, nil
	}
	key, err := store.NameKey(s.ck, dir.Salt)
	if err != nil {
		return nil, err
	}
	if r.cipher, err = store.NewNameCipher(key); err != nil {
		return nil, err
	}
	return r, nil
}

// name returns one entry's name in the clear.
func (r *dirReader) name(e store.DirEntry) (string, error) {
	if r.cipher == nil {
		return string(e.Name), nil
	}
	return r.cipher.Decrypt(e.Name)
}

// stored returns the bytes a name has inside the object: the name itself in a
// plain library, its SIV ciphertext in an E2EE one.
func (r *dirReader) stored(name string) ([]byte, error) {
	if r.cipher == nil {
		return []byte(name), nil
	}
	return r.cipher.Encrypt(name)
}

// index returns the position of the entry with the given plaintext name, or -1
// if the directory has no such entry.
//
// In an E2EE library it encrypts the name and matches on ciphertext rather
// than decrypting every entry to compare. Both are correct — AES-SIV is
// deterministic, which is the whole reason `entries/{path}` can route on
// ciphertext — but one is a single encryption and the other is one decryption
// per entry in the directory.
func (r *dirReader) index(name string) (int, error) {
	want, err := r.stored(name)
	if err != nil {
		return -1, err
	}
	for i, e := range r.dir.Entries {
		if bytes.Equal(e.Name, want) {
			return i, nil
		}
	}
	return -1, nil
}

// lookup finds an entry by its plaintext name.
func (r *dirReader) lookup(name string) (store.DirEntry, bool, error) {
	i, err := r.index(name)
	if err != nil || i < 0 {
		return store.DirEntry{}, false, err
	}
	return r.dir.Entries[i], true, nil
}

// List returns a directory's entries, with names in the clear and in the
// object's own order — which is bytewise by stored name, so a plain library
// lists alphabetically. A caller wanting a display order sorts; the format
// will not pretend to provide one it cannot.
//
// It needs the content key, so on the server it serves plain libraries only.
// The sentence this comment used to carry — that an E2EE library "lists in an
// order that means nothing" — described the client's view, where the key is
// present and the ciphertext ordering is what survives; on this side the guard
// below refuses first and no order is reached at all. docs/storage.md § The
// wire still describes path-addressed listing working against a sealed library
// on the strength of deterministic name encryption. That is a design, not this
// function: nothing sets Config.CK server-side, so HasKey is always false here
// and every E2EE structural call is refused.
func (s *Store) List(dirID store.ID) ([]Node, error) {
	if s.e2ee && !s.HasKey() {
		return nil, ErrNoContentKey
	}
	r, err := s.openDir(dirID)
	if err != nil {
		return nil, err
	}

	out := make([]Node, 0, len(r.dir.Entries))
	for _, e := range r.dir.Entries {
		name, err := r.name(e)
		if err != nil {
			return nil, fmt.Errorf("directory %s: %w", dirID, err)
		}
		out = append(out, Node{
			ID: e.ChildID, Type: e.Type, Name: name, Mtime: e.Mtime, Mode: e.Mode,
		})
	}
	return out, nil
}

// SplitPath turns a path into its segments, rejecting anything that is not a
// path this format will carry.
//
// Leading and trailing slashes are tolerated and empty segments collapse, so
// "/a/b/" and "a/b" are the same path — that much is ordinary. What is not
// tolerated is "." or "..": they are refused rather than resolved, because
// resolving them here would let a client reach outside the tree it named, and
// the format already refuses to store an entry called either.
func SplitPath(p string) ([]string, error) {
	var out []string
	for _, seg := range strings.Split(p, "/") {
		if seg == "" {
			continue
		}
		if seg == "." || seg == ".." {
			return nil, fmt.Errorf("%w: %q", store.ErrName, seg)
		}
		out = append(out, seg)
	}
	return out, nil
}

// Resolve walks a path from a root directory and returns the node it names.
//
// The root itself resolves to a directory node with an empty name. It has no
// entry anywhere to carry its own mode and mtime — nothing points at the root
// — so those come from the format's stated constants rather than from a
// parent that does not exist.
//
// Under E2EE this is N sequential object fetches for a depth-N path, and that
// is inherent rather than an implementation shortcut: encrypting each segment
// needs its parent's salt, so the parent must be read before the child can be
// named. Clients cache the salt map beside their local index; a cold resolve
// is a tree walk.
func (s *Store) Resolve(root store.ID, p string) (Node, error) {
	if s.e2ee && !s.HasKey() {
		return Node{}, ErrNoContentKey
	}
	segments, err := SplitPath(p)
	if err != nil {
		return Node{}, err
	}

	node := Node{ID: root, Type: store.NodeDir, Mode: store.RootMode}
	for i, seg := range segments {
		if node.Type != store.NodeDir {
			return Node{}, fmt.Errorf("%w: %q", ErrNotDir, path.Join(segments[:i]...))
		}
		r, err := s.openDir(node.ID)
		if err != nil {
			return Node{}, err
		}
		e, ok, err := r.lookup(seg)
		if err != nil {
			return Node{}, err
		}
		if !ok {
			return Node{}, fmt.Errorf("%w: %q", ErrNotFound, path.Join(segments[:i+1]...))
		}
		node = Node{ID: e.ChildID, Type: e.Type, Name: seg, Mtime: e.Mtime, Mode: e.Mode}
	}
	return node, nil
}

// ListPath resolves a path and lists the directory it names.
func (s *Store) ListPath(root store.ID, p string) ([]Node, error) {
	node, err := s.Resolve(root, p)
	if err != nil {
		return nil, err
	}
	if !node.IsDir() {
		return nil, fmt.Errorf("%w: %q", ErrNotDir, p)
	}
	return s.List(node.ID)
}

// OpenPath resolves a path to a file and returns its manifest.
func (s *Store) OpenPath(root store.ID, p string) (*store.Manifest, error) {
	node, err := s.Resolve(root, p)
	if err != nil {
		return nil, err
	}
	if node.IsDir() {
		return nil, fmt.Errorf("%w: %q is a directory", ErrNotDir, p)
	}
	return s.GetManifest(node.ID)
}

// ReadPath writes the content of the file at p to w.
func (s *Store) ReadPath(root store.ID, p string, w io.Writer) error {
	m, err := s.OpenPath(root, p)
	if err != nil {
		return err
	}
	return s.ReadFile(m, w)
}

// Walk calls fn for every node under root, depth-first, with the path each was
// reached by. A directory is visited before its contents. fn's error stops the
// walk and is returned.
//
// This is the shape the tracing collector needs — reachability from a commit —
// and it is deliberately not a listing API: it holds one directory at a time
// and never accumulates the tree.
func (s *Store) Walk(root store.ID, fn func(p string, n Node) error) error {
	if s.e2ee && !s.HasKey() {
		return ErrNoContentKey
	}
	return s.walk(root, "", fn)
}

func (s *Store) walk(dirID store.ID, prefix string, fn func(string, Node) error) error {
	entries, err := s.List(dirID)
	if err != nil {
		return err
	}
	for _, n := range entries {
		p := path.Join(prefix, n.Name)
		if err := fn(p, n); err != nil {
			return err
		}
		if n.IsDir() {
			if err := s.walk(n.ID, p, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// NewDirSalt returns the salt a newly created directory carries: sixteen
// random bytes in an E2EE library, and zero in a plain one, where there is
// nothing to key.
//
// It is generated once, at creation, and every rewrite of that directory
// copies it forward — the writer has to read the current object anyway. A
// fresh salt per write would change the directory's id on every write, hence
// every ancestor's, hence the root, and changes?since= would report the whole
// tree modified on every commit.
func (s *Store) NewDirSalt() ([store.DirSaltSize]byte, error) {
	var salt [store.DirSaltSize]byte
	if !s.e2ee {
		return salt, nil
	}
	if !s.HasKey() {
		return salt, ErrNoContentKey
	}
	if _, err := rand.Read(salt[:]); err != nil {
		return salt, fmt.Errorf("objmgr: directory salt: %w", err)
	}
	return salt, nil
}

// PutDir builds a directory object from nodes carrying plaintext names and
// stores it, returning its id. It is the inverse of List.
//
// Names are encrypted here in an E2EE library, which is the one place that
// happens on the way in — the directory codec treats the name as opaque bytes
// so that one encoder serves both library types, so somebody above it has to
// know. This is that somebody.
//
// Order does not matter: the codec sorts by stored name and refuses
// duplicates. Under E2EE that sort is over ciphertext, so it carries no
// information about the plaintext names, which is the point.
func (s *Store) PutDir(salt [store.DirSaltSize]byte, nodes []Node) (store.ID, error) {
	if s.e2ee && !s.HasKey() {
		return store.ID{}, ErrNoContentKey
	}
	var cipher *store.NameCipher
	if s.e2ee {
		key, err := store.NameKey(s.ck, salt)
		if err != nil {
			return store.ID{}, err
		}
		if cipher, err = store.NewNameCipher(key); err != nil {
			return store.ID{}, err
		}
	}

	d := &store.Directory{Salt: salt, Entries: make([]store.DirEntry, 0, len(nodes))}
	for _, n := range nodes {
		name := []byte(n.Name)
		if cipher != nil {
			ct, err := cipher.Encrypt(n.Name)
			if err != nil {
				return store.ID{}, err
			}
			name = ct
		}
		d.Entries = append(d.Entries, store.DirEntry{
			ChildID: n.ID, Type: n.Type, Name: name, Mtime: n.Mtime, Mode: n.Mode,
		})
	}
	return s.PutDirectory(d)
}
