// The pack index: what says where a frame is, in the one format that serves
// three jobs.
//
// It is the sidecar an open pack accumulates, it is the footer a sealed pack
// carries, and it is what recovery reads to decide where a torn pack ends.
// Those could each have had a format and the temptation to give them one is
// real — the sidecar is written a record at a time and the footer is written
// once, sorted — but they hold the same records, and three parsers is three
// chances for one of them to disagree with the others about a pack nobody can
// read any more.
//
// So: one record, one encoder, one decoder. What differs between the sidecar
// and the footer is the *order* of the records and nothing else, and sealing
// is a sort.
package objstore

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
)

// An index record is fixed width, which is the property the whole design leans
// on: the footer is binary-searched in place without being parsed, and a torn
// sidecar is truncated to a record boundary by arithmetic rather than by
// scanning for one.
//
//	id      32  the frame's object id, raw rather than hex — half the bytes,
//	            and the comparison a binary search does is on these anyway
//	offset   8  where the frame starts in the pack
//	length   4  the frame's length, header and tag included
//
// The plaintext length is deliberately absent. It is the frame length minus
// frameOverhead, exactly as Stat already derives it from a file size, and a
// second copy of a derived number is a second thing that can disagree.
//
// Four bytes for a length caps a frame at 4 GiB, which is three orders of
// magnitude past the largest chunk this store will hold. Eight for an offset
// leaves the pack size a tuning decision rather than a format one.
const (
	idxIDSize     = 32
	idxOffsetSize = 8
	idxLengthSize = 4
	idxRecordSize = idxIDSize + idxOffsetSize + idxLengthSize
)

// The sidecar names itself, for the reason the frame does: a file that says
// what it is can be told from a file that was truncated to nothing, and from
// somebody else's file that happens to be in the way.
const (
	idxMagic      = "SILX"
	idxVersion    = 1
	idxHeaderSize = len(idxMagic) + 1
)

// ErrIndexCorrupt reports a sidecar that is not one — bad magic, an unknown
// version, or a header too short to hold either.
//
// A torn *tail* is deliberately not this error. A sidecar is appended to and
// fsynced, so a crash between those two leaves a partial trailing record, and
// that is an ordinary thing to find rather than corruption: the records before
// it are complete and the pack is recoverable from them. Reporting it as
// damage would turn every unclean shutdown into an operator's problem.
var ErrIndexCorrupt = errors.New("objstore: pack index is not readable")

// indexEntry is one record, decoded.
type indexEntry struct {
	// ID is hex, because that is what every caller above this holds and the
	// conversion has to happen somewhere. The record stores raw bytes.
	ID     string
	Offset int64
	Length int64
}

// end is the first offset past this frame — where the next one starts, and
// where a pack holding only indexed frames ends.
func (e indexEntry) end() int64 { return e.Offset + e.Length }

// plaintextLen is what Stat answers for this object.
func (e indexEntry) plaintextLen() int64 {
	if e.Length <= int64(frameOverhead) {
		return 0
	}
	return e.Length - int64(frameOverhead)
}

func encodeIndexRecord(dst []byte, e indexEntry) error {
	raw, err := hex.DecodeString(e.ID)
	if err != nil || len(raw) != idxIDSize {
		return fmt.Errorf("index record for %q: id is not %d hex-encoded bytes", e.ID, idxIDSize)
	}
	if e.Offset < 0 {
		return fmt.Errorf("index record for %s: negative offset %d", e.ID, e.Offset)
	}
	if e.Length < 0 || e.Length > 1<<32-1 {
		return fmt.Errorf("index record for %s: length %d does not fit the record", e.ID, e.Length)
	}
	copy(dst, raw)
	binary.BigEndian.PutUint64(dst[idxIDSize:], uint64(e.Offset))
	binary.BigEndian.PutUint32(dst[idxIDSize+idxOffsetSize:], uint32(e.Length))
	return nil
}

func decodeIndexRecord(b []byte) indexEntry {
	return indexEntry{
		ID:     hex.EncodeToString(b[:idxIDSize]),
		Offset: int64(binary.BigEndian.Uint64(b[idxIDSize:])),
		Length: int64(binary.BigEndian.Uint32(b[idxIDSize+idxOffsetSize:])),
	}
}

// Big-endian, so that the raw id bytes sort the same way a byte-wise memcmp
// sorts them and the same way their hex sorts. A binary search over the footer
// compares ids, and having three orderings that agree means a test can assert
// against whichever is convenient to write.

// sidecar is the index of a pack that is still open. It appends and it is
// read back whole; it is never searched on disk, because the writer holds the
// same records in memory.
type sidecar struct {
	path string
	f    *os.File
	buf  [idxRecordSize]byte
}

// createSidecar starts a new one, replacing anything at the path. Sealing
// truncates rather than deletes, so an existing file here is either a fresh
// truncation or debris from a crash before one, and both want the same
// treatment.
func createSidecar(path string) (*sidecar, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	header := append([]byte(idxMagic), idxVersion)
	if _, err := f.Write(header); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &sidecar{path: path, f: f}, nil
}

// openSidecar opens an existing one for appending, at its end.
func openSidecar(path string) (*sidecar, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &sidecar{path: path, f: f}, nil
}

// append writes one record. It does not sync: the caller decides when, because
// the ordering that makes a crash survivable is the caller's to hold — the
// pack is fsynced before this is written, and this is fsynced before anything
// is acknowledged.
func (s *sidecar) append(e indexEntry) error {
	if err := encodeIndexRecord(s.buf[:], e); err != nil {
		return err
	}
	_, err := s.f.Write(s.buf[:])
	return err
}

func (s *sidecar) sync() error  { return s.f.Sync() }
func (s *sidecar) close() error { return s.f.Close() }

// truncate empties the sidecar back to its header, which is what sealing does
// to it once its records are safely inside the pack's footer.
func (s *sidecar) truncate() error {
	if err := s.f.Truncate(int64(idxHeaderSize)); err != nil {
		return err
	}
	if _, err := s.f.Seek(int64(idxHeaderSize), io.SeekStart); err != nil {
		return err
	}
	return s.f.Sync()
}

// readSidecar returns every complete record in a sidecar, in the order they
// were appended.
//
// A partial trailing record is dropped, and that is the point of the function
// rather than an edge case in it: the last thing a crashing writer does is
// leave one. The records before it describe frames that are already durable in
// the pack, so they are the truth about what the pack holds, and the partial
// one describes a frame the writer never acknowledged.
func readSidecar(path string) ([]indexEntry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) < idxHeaderSize {
		return nil, fmt.Errorf("%w: %s is %d bytes, shorter than its header", ErrIndexCorrupt, path, len(b))
	}
	if string(b[:len(idxMagic)]) != idxMagic {
		return nil, fmt.Errorf("%w: %s does not begin %q", ErrIndexCorrupt, path, idxMagic)
	}
	if b[len(idxMagic)] != idxVersion {
		return nil, fmt.Errorf("%w: %s is version %d, and this build reads %d",
			ErrIndexCorrupt, path, b[len(idxMagic)], idxVersion)
	}

	body := b[idxHeaderSize:]
	n := len(body) / idxRecordSize
	entries := make([]indexEntry, 0, n)
	for i := 0; i < n; i++ {
		entries = append(entries, decodeIndexRecord(body[i*idxRecordSize:]))
	}
	return entries, nil
}

// sortedIndex returns the records ordered by id, which is the order a sealed
// pack's footer is written in so that a reader can binary-search it.
//
// It copies rather than sorting in place. The caller's slice is in arrival
// order, which is the order the pack's frames are in, and something that wants
// to walk a pack sequentially still needs it.
func sortedIndex(entries []indexEntry) []indexEntry {
	out := make([]indexEntry, len(entries))
	copy(out, entries)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
