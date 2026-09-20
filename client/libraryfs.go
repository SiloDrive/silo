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
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/SiloDrive/silo/store"
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
	//
	// body is a factory rather than an io.Reader because a write is retried
	// after a 401 and a drained reader cannot be sent twice. It returns the
	// stream, the number of bytes it will produce, and any error opening it;
	// it may be called more than once and must produce the same bytes each
	// time. See BytesBody and FileBody for the two ordinary sources.
	WriteFrom(path string, body func() (io.ReadCloser, int64, error), mtime int64) error
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
	lib, err := a.c.Library(libraryID)
	if err != nil {
		return nil, err
	}
	if !lib.Encrypted {
		return &plainLibrary{c: a.c, ID: libraryID}, nil
	}
	return a.OpenEncryptedLibrary(libraryID)
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
//
// Only the JSON helpers need it: doBytesHeaders already maps 404 on the way
// out, so wrapping a second time would only lengthen the message.
func notFound(err error) error {
	if isNotFound(err) && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
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
		out = append(out, node(e))
	}
	return out, nil
}

// node decodes one listing row. A listing carries an id for everything it
// names; a malformed one is the server's problem and not worth failing a
// listing over.
func node(e DirEntry) Node {
	n := Node{Name: e.Name, Mtime: e.Mtime, Type: store.NodeFile}
	if e.Type == "dir" {
		n.Type = store.NodeDir
	}
	if id, err := store.ParseID(e.ID); err == nil {
		n.ID = id
	}
	return n
}

// Stat asks the parent, because the path surface has no stat: a GET on an
// entry is its listing or its bytes, and neither says what the entry is
// without first assuming which one it got.
func (p *plainLibrary) Stat(entry string) (Node, error) {
	segs, err := segments(entry)
	if err != nil {
		return Node{}, err
	}
	if len(segs) == 0 {
		return Node{Name: "/", Type: store.NodeDir}, nil
	}
	parent := "/" + strings.Join(segs[:len(segs)-1], "/")
	siblings, err := p.c.ListDir(p.ID, parent)
	if err != nil {
		return Node{}, notFound(err)
	}
	// The raw listing rather than p.List, so that statting one entry in a large
	// directory does not convert every sibling into a Node to throw away.
	name := segs[len(segs)-1]
	for _, s := range siblings {
		if s.Name == name {
			return node(s), nil
		}
	}
	return Node{}, fmt.Errorf("%w: %s", ErrNotFound, entry)
}

func (p *plainLibrary) ReadFile(file string) ([]byte, error) {
	body, header, err := p.c.doBytesHeaders("GET", entriesURL(p.ID, file), "", nil, nil)
	if err != nil {
		return nil, err
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
		if hasStatus(err, http.StatusRequestedRangeNotSatisfiable) {
			return []byte{}, nil
		}
		return nil, err
	}
	return body, nil
}

func (p *plainLibrary) WriteFile(file string, data []byte, mtime int64) error {
	// mtime is not sent: the path surface takes none. See LibraryFS.WriteFile.
	_, err := p.c.doBytes("PUT", entriesURL(p.ID, file), "application/octet-stream", nil, data)
	return err
}

// WriteFrom streams: the factory is handed straight to doStream, which reads
// it into the request body rather than into memory. It used to buffer, and the
// reason was the parameter type -- an io.Reader cannot satisfy the replay
// contract, so io.ReadAll was the only way to be able to send the body twice
// after a 401. A factory can be called again, so the buffer went with it.
//
// mtime is not sent: the path surface takes none. See LibraryFS.WriteFile.
func (p *plainLibrary) WriteFrom(file string, body func() (io.ReadCloser, int64, error), mtime int64) error {
	resp, err := p.c.doStream("PUT", entriesURL(p.ID, file), "application/octet-stream", body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("client: writing %s: %s: %s", file, resp.Status, string(msg))
	}
	return nil
}

func (p *plainLibrary) MkdirAll(dir string) error {
	segs, err := segments(dir)
	if err != nil {
		return err
	}
	for i := range segs {
		err := p.c.Mkdir(p.ID, "/"+strings.Join(segs[:i+1], "/"))
		// Already there is the answer MkdirAll wants, not one it reports.
		if hasStatus(err, http.StatusConflict) {
			continue
		}
		if err != nil {
			return notFound(err)
		}
	}
	return nil
}

func (p *plainLibrary) Remove(entry string) error {
	return notFound(p.c.DeleteFile(p.ID, entry))
}

// BytesBody is a WriteFrom source over bytes already in memory.
func BytesBody(b []byte) func() (io.ReadCloser, int64, error) {
	return func() (io.ReadCloser, int64, error) {
		return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
	}
}

// FileBody is a WriteFrom source over a file on disk.
//
// It opens on every call rather than seeking one handle back to the start,
// because a retry can happen after the first attempt has already closed it,
// and a factory that hands back a closed file is worse than one that fails.
func FileBody(localPath string) func() (io.ReadCloser, int64, error) {
	return func() (io.ReadCloser, int64, error) {
		f, err := os.Open(localPath)
		if err != nil {
			return nil, 0, err
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, 0, err
		}
		return f, info.Size(), nil
	}
}
