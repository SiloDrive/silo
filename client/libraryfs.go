package client

// One interface, two implementations.
//
// A caller that wants to list a directory, read a file or write some bytes
// should not have to know whether the server can read what it is storing. The
// two answers are entirely different underneath -- one asks the server to
// resolve a path, the other walks an object graph and decrypts it -- and that
// difference belongs in one choice made per library rather than in a branch at
// every call site. docs/protocol.md § The E2EE client shape asks for exactly
// this, and it asks for it because a client that branches on `e2ee` per call
// grows a plain-only path that quietly does not work on half its libraries.

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/dkam/silo/store"
)

// LibraryFS is one library, addressed by plaintext path.
//
// Every path is relative to the library root, with or without a leading
// slash. Errors that mean "there is nothing there" wrap ErrNotFound, on both
// implementations, because a caller distinguishing those has to be able to do
// it the same way whichever it holds.
type LibraryFS interface {
	// List returns one directory's children.
	List(path string) ([]Node, error)
	// Stat returns the entry naming a path.
	Stat(path string) (Node, error)
	// ReadFile reads a whole file.
	ReadFile(path string) ([]byte, error)
	// ReadAt reads n bytes from off, returning fewer at the end of the file.
	ReadAt(path string, off, n int64) ([]byte, error)

	// WriteFile writes bytes, replacing whatever was there.
	//
	// mtime is the file's own, in unix seconds. It is preserved on an
	// encrypted library, where the client builds the dirent, and NOT on a
	// plain one: PUT entries/{path} stamps the server's clock and takes no
	// mtime from the caller. That is a gap in the path surface rather than a
	// choice here -- silo#31 -- and the parameter stays because dropping it
	// would take the capability away from the half that has it.
	WriteFile(path string, data []byte, mtime int64) error
	// WriteFrom is WriteFile reading from a stream.
	WriteFrom(path string, r io.Reader, mtime int64) error
	// MkdirAll creates a directory and any missing parents, and is content
	// with one that already exists.
	MkdirAll(path string) error
	// Remove deletes one entry, and a directory with whatever it holds.
	Remove(path string) error

	// Refresh drops whatever the implementation is caching about where the
	// library is now, so the next read sees writes that landed elsewhere.
	Refresh()
}

var (
	_ LibraryFS = (*EncryptedLibrary)(nil)
	_ LibraryFS = (*plainLibrary)(nil)
)

// Open returns a handle on a library, choosing the implementation from what
// the library is.
//
// Once, here, rather than at each call site. The listing is the only place the
// server says whether a library is encrypted, and it is also where the head
// comes from, so this is a request either implementation would have made.
func (a *Account) Open(libraryID string) (LibraryFS, error) {
	libraries, err := a.c.ListLibraries()
	if err != nil {
		return nil, err
	}
	for _, lib := range libraries {
		if lib.ID != libraryID {
			continue
		}
		if !lib.Encrypted {
			return &plainLibrary{c: a.c, ID: libraryID}, nil
		}
		return a.OpenEncryptedLibrary(libraryID)
	}
	return nil, fmt.Errorf("%w: library %s", ErrNotFound, libraryID)
}

// plainLibrary answers through entries/{path}, where the server resolves the
// path, chunks the content and builds the tree.
//
// It caches nothing, so Refresh has nothing to do: every answer here is a
// question put to the server, which is the one thing this implementation has
// that the other does not.
type plainLibrary struct {
	c  *APIClient
	ID string
}

func (p *plainLibrary) Refresh() {}

// notFound maps the server's 404 onto the package's own, so a caller can ask
// the same question of either implementation.
func notFound(err error) error {
	if err != nil && isNotFound(err) {
		return fmt.Errorf("%w: %s", ErrNotFound, err)
	}
	return err
}

func (p *plainLibrary) List(dir string) ([]Node, error) {
	entries, err := p.c.ListDir(p.ID, dir)
	if err != nil {
		return nil, notFound(err)
	}
	out := make([]Node, 0, len(entries))
	for _, e := range entries {
		n := Node{Name: e.Name, Mtime: e.Mtime, Type: store.NodeFile}
		if e.Type == "dir" {
			n.Type = store.NodeDir
		}
		// A listing carries an id for everything it names; a malformed one is
		// the server's problem and not worth failing a listing over.
		if id, err := store.ParseID(e.ID); err == nil {
			n.ID = id
		}
		out = append(out, n)
	}
	return out, nil
}

// Stat asks the parent, because the path surface has no stat: a GET on an
// entry is its listing or its bytes, and neither says what the entry is
// without first assuming which one it got.
func (p *plainLibrary) Stat(entry string) (Node, error) {
	segs := segments(entry)
	if len(segs) == 0 {
		return Node{Name: "/", Type: store.NodeDir}, nil
	}
	parent := "/" + strings.Join(segs[:len(segs)-1], "/")
	siblings, err := p.List(parent)
	if err != nil {
		return Node{}, err
	}
	name := segs[len(segs)-1]
	for _, s := range siblings {
		if s.Name == name {
			return s, nil
		}
	}
	return Node{}, fmt.Errorf("%w: %s", ErrNotFound, entry)
}

func (p *plainLibrary) ReadFile(file string) ([]byte, error) {
	body, header, err := p.c.doBytesHeaders("GET", entriesURL(p.ID, file), "", nil, nil)
	if err != nil {
		return nil, notFound(err)
	}
	// A GET on a directory is its listing, and handing that back as file
	// content would be JSON masquerading as bytes.
	if strings.HasPrefix(header.Get("Content-Type"), "application/json") {
		return nil, fmt.Errorf("client: %s is a directory", file)
	}
	return body, nil
}

func (p *plainLibrary) ReadAt(file string, off, n int64) ([]byte, error) {
	if off < 0 || n < 0 {
		return nil, fmt.Errorf("client: read at %d for %d bytes", off, n)
	}
	if n == 0 {
		return []byte{}, nil
	}
	header := http.Header{"Range": []string{fmt.Sprintf("bytes=%d-%d", off, off+n-1)}}
	body, _, err := p.c.doBytesHeaders("GET", entriesURL(p.ID, file), "", header, nil)
	if err != nil {
		// A range that starts past the end of the file is 416, which is the
		// same answer the encrypted implementation gives as no bytes.
		var se *StatusError
		if ok := asStatus(err, &se); ok && se.Code == http.StatusRequestedRangeNotSatisfiable {
			return []byte{}, nil
		}
		return nil, notFound(err)
	}
	return body, nil
}

func (p *plainLibrary) WriteFile(file string, data []byte, mtime int64) error {
	// mtime is not sent: the path surface takes none. See LibraryFS.WriteFile.
	_, err := p.c.doBytes("PUT", entriesURL(p.ID, file), "application/octet-stream", nil, data)
	return notFound(err)
}

// WriteFrom buffers, because doStream's body factory has to be able to produce
// the request a second time after a 401 and an arbitrary reader cannot be read
// twice. A caller with a file on disk wants UploadFile, which streams it.
func (p *plainLibrary) WriteFrom(file string, r io.Reader, mtime int64) error {
	buf, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return p.WriteFile(file, buf, mtime)
}

func (p *plainLibrary) MkdirAll(dir string) error {
	segs := segments(dir)
	for i := range segs {
		err := p.c.Mkdir(p.ID, "/"+strings.Join(segs[:i+1], "/"))
		// Already there is the answer MkdirAll wants, not one it reports.
		var se *StatusError
		if ok := asStatus(err, &se); ok && se.Code == http.StatusConflict {
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *plainLibrary) Remove(entry string) error {
	return notFound(p.c.DeleteFile(p.ID, entry))
}
