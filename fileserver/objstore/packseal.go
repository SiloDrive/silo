// Sealing a pack, and reading a sealed one.
//
// An open pack is a file plus a sidecar; a sealed pack is one file that
// carries everything needed to read it. Sealing is the step between, and it is
// deliberately the only step that is not append-and-index: it sorts the index
// it has been accumulating, writes it into the pack as a footer, and takes the
// sidecar away.
//
//	SILP <version> <reserved×3>   the header createPack wrote
//	<frame> <frame> …             SILF frames, in arrival order
//	SILX <version> <records>      the index, in id order
//	SILB <params> <bits>          the bloom filter
//	<footer length> <index length>
//	SILP                          the magic again
//
// The index cannot be a header. A pack does not know its frame offsets until
// the frames are written, so putting the index first would need a second pass
// or reserved space to seek back into. Parquet arrives at the same layout from
// the same constraint, and the magic at both ends is worth copying with it: a
// truncated pack is the *ordinary* crash outcome here rather than an exotic
// one, and a missing tail magic says so in four bytes, before any offset read
// out of that same tail is trusted.
//
// One file rather than a file and a sidecar is what this buys on a durable
// tier: half the PUTs, half the LIST entries, and no way for a pack and its
// index to be separated, to upload out of order, or for one to outlive the
// other.
package objstore

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// The trailer, at a known offset from the end so that one read finds it.
//
//	footer length  8  index and bloom together — where the footer starts
//	index length   8  where the index ends and the bloom begins
//	magic          4
//
// Two lengths rather than one, because the reader has to split the footer and
// neither section can say where it ends on its own: the index is a run of
// fixed-width records with no count, and the bloom's size is a function of a
// parameter stored at *its* start. Putting both lengths in the tail keeps the
// index section byte-identical to a sidecar, which is the property the whole
// one-format argument rests on.
const (
	packFooterLenSize = 8
	packIndexLenSize  = 8
	packTrailerSize   = packFooterLenSize + packIndexLenSize + len(packMagic)
)

// seal finishes a pack: it writes the footer, fsyncs, and removes the sidecar.
//
// The order is what makes an interrupted seal survivable, and it is the same
// argument append makes. Until the sidecar is gone the pack is still open, and
// findOpenPack will hand it back to recoverPack, which truncates it to the last
// frame the sidecar indexes — discarding however much of a footer got written.
// Sealing again from the same records produces the same bytes, because sorting
// is deterministic and so is the filter, so re-doing the step is not merely
// safe but exact.
//
// The sidecar is removed rather than emptied. It is named after its pack, and
// pack ids are random, so there is no next pack to hand an emptied one to —
// and "a sidecar exists" is then an exact answer to "was this pack open",
// with no second piece of state anywhere that could disagree with it.
func (p *openPack) seal() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	footer, err := buildFooter(p.entries)
	if err != nil {
		return fmt.Errorf("sealing pack %s: %v", p.id, err)
	}
	// WriteAt rather than Write: the offset is the one the index agrees with,
	// rather than wherever the file position happens to have been left by a
	// recovery seek or a read.
	if _, err := p.f.WriteAt(footer, p.size); err != nil {
		return fmt.Errorf("writing the footer of pack %s: %v", p.id, err)
	}
	if err := p.f.Sync(); err != nil {
		return fmt.Errorf("syncing the footer of pack %s: %v", p.id, err)
	}

	sidePath := p.side.path
	if err := p.side.close(); err != nil {
		return fmt.Errorf("closing the index of pack %s: %v", p.id, err)
	}
	if err := os.Remove(sidePath); err != nil {
		return fmt.Errorf("removing the index of pack %s: %v", p.id, err)
	}
	if err := syncDir(filepath.Dir(p.path)); err != nil {
		return fmt.Errorf("publishing pack %s: %v", p.id, err)
	}
	return p.f.Close()
}

// buildFooter lays out everything after the last frame.
func buildFooter(entries []indexEntry) ([]byte, error) {
	sorted := dedupedIndex(sortedIndex(entries))

	var buf bytes.Buffer
	buf.Write(indexHeader())
	rec := make([]byte, idxRecordSize)
	for _, e := range sorted {
		if err := encodeIndexRecord(rec, e); err != nil {
			return nil, err
		}
		buf.Write(rec)
	}
	indexLen := buf.Len()

	filter, err := buildBloom(sorted)
	if err != nil {
		return nil, err
	}
	buf.Write(filter.encode())
	footerLen := buf.Len()

	trailer := make([]byte, packTrailerSize)
	binary.BigEndian.PutUint64(trailer, uint64(footerLen))
	binary.BigEndian.PutUint64(trailer[packFooterLenSize:], uint64(indexLen))
	copy(trailer[packFooterLenSize+packIndexLenSize:], packMagic)
	buf.Write(trailer)

	return buf.Bytes(), nil
}

// dedupedIndex drops repeats of an id, keeping the first.
//
// Two records for one id should not happen — dedup above this layer is what
// stops the same chunk being stored twice — but two writers that both miss it
// concurrently would each append a frame, and both frames hold the same
// object, because an id is the hash of what is under it. So the duplicate is
// waste rather than ambiguity, and the footer records one of them; compaction
// is what reclaims the other. What must not happen is a sealed index with a
// repeated key in it, because a binary search over one has no defined answer.
//
// The input must already be sorted.
func dedupedIndex(sorted []indexEntry) []indexEntry {
	out := sorted[:0:0]
	for i, e := range sorted {
		if i > 0 && e.ID == sorted[i-1].ID {
			continue
		}
		out = append(out, e)
	}
	return out
}

// sealedPack is a pack that will not change again: its frames, its index and
// its filter, all in one file.
type sealedPack struct {
	id   string
	path string
	f    *os.File

	// index is the footer's index section with its header sliced off, so that
	// it is exactly a run of records and record i starts at i*idxRecordSize.
	// It is searched as bytes rather than decoded into a slice of entries,
	// which is what lets an mmap be dropped in here later without the search
	// changing: 12,000 packs' worth of decoded indexes is a resident set worth
	// handing to the page cache instead, and that is a decision step 3 makes
	// when the numbers arrive.
	index []byte
	bloom *bloomFilter
}

// openSealedPack reads a sealed pack's footer and leaves the file open for
// reads.
//
// The read order is the one a tier can afford: the tail first, because on a
// backend with no local copy it is one ranged GET, and everything else is
// found from what it says.
func openSealedPack(objDir, libraryID, packID string) (*sealedPack, error) {
	packPath, _ := packPaths(objDir, libraryID, packID)
	f, err := os.Open(packPath)
	if err != nil {
		return nil, err
	}
	closeOnErr := func(e error) (*sealedPack, error) {
		_ = f.Close()
		return nil, e
	}

	info, err := f.Stat()
	if err != nil {
		return closeOnErr(err)
	}
	size := info.Size()
	if size < int64(packHeaderSize+packTrailerSize) {
		return closeOnErr(fmt.Errorf("%w: %s is %d bytes, too short to be sealed",
			ErrPackCorrupt, packPath, size))
	}

	header := make([]byte, packHeaderSize)
	if _, err := f.ReadAt(header, 0); err != nil {
		return closeOnErr(fmt.Errorf("%w: %s: reading the header: %v", ErrPackCorrupt, packPath, err))
	}
	if err := checkPackHeader(header, packPath); err != nil {
		return closeOnErr(err)
	}

	trailer := make([]byte, packTrailerSize)
	if _, err := f.ReadAt(trailer, size-int64(packTrailerSize)); err != nil {
		return closeOnErr(fmt.Errorf("%w: %s: reading the trailer: %v", ErrPackCorrupt, packPath, err))
	}
	// The closing magic first: an open pack, or one truncated mid-seal, fails
	// here rather than being asked to explain a length read out of its frames.
	if string(trailer[packFooterLenSize+packIndexLenSize:]) != packMagic {
		return closeOnErr(fmt.Errorf("%w: %s does not end %q — it is unsealed or truncated",
			ErrPackCorrupt, packPath, packMagic))
	}
	footerLen := int64(binary.BigEndian.Uint64(trailer))
	indexLen := int64(binary.BigEndian.Uint64(trailer[packFooterLenSize:]))

	footerStart := size - int64(packTrailerSize) - footerLen
	switch {
	case footerLen < 0 || indexLen < 0 || indexLen > footerLen:
		return closeOnErr(fmt.Errorf("%w: %s claims a %d-byte footer holding a %d-byte index",
			ErrPackCorrupt, packPath, footerLen, indexLen))
	case footerStart < int64(packHeaderSize):
		return closeOnErr(fmt.Errorf("%w: %s claims a %d-byte footer, more than the %d bytes it has",
			ErrPackCorrupt, packPath, footerLen, size))
	}

	footer := make([]byte, footerLen)
	if _, err := f.ReadAt(footer, footerStart); err != nil {
		return closeOnErr(fmt.Errorf("%w: %s: reading the footer: %v", ErrPackCorrupt, packPath, err))
	}
	if err := checkIndexHeader(footer[:indexLen], packPath); err != nil {
		return closeOnErr(err)
	}
	index := footer[idxHeaderSize:indexLen]
	if len(index)%idxRecordSize != 0 {
		return closeOnErr(fmt.Errorf("%w: %s holds %d bytes of index, not a whole number of %d-byte records",
			ErrPackCorrupt, packPath, len(index), idxRecordSize))
	}
	filter, err := parseBloom(footer[indexLen:])
	if err != nil {
		return closeOnErr(fmt.Errorf("%s: %w", packPath, err))
	}

	return &sealedPack{id: packID, path: packPath, f: f, index: index, bloom: filter}, nil
}

// checkPackHeader gates a pack on its opening magic and version.
func checkPackHeader(header []byte, path string) error {
	if len(header) < packHeaderSize {
		return fmt.Errorf("%w: %s is too short to hold a header", ErrPackCorrupt, path)
	}
	if string(header[:len(packMagic)]) != packMagic {
		return fmt.Errorf("%w: %s does not begin %q", ErrPackCorrupt, path, packMagic)
	}
	if header[len(packMagic)] != packVersion {
		return fmt.Errorf("%w: %s is version %d, and this build reads %d",
			ErrPackCorrupt, path, header[len(packMagic)], packVersion)
	}
	return nil
}

// count is how many objects the pack holds.
func (s *sealedPack) count() int { return len(s.index) / idxRecordSize }

// record returns the i'th index record, raw.
func (s *sealedPack) record(i int) []byte {
	return s.index[i*idxRecordSize : (i+1)*idxRecordSize]
}

// lookup finds an object in this pack, asking the filter first.
//
// The filter is asked even though the search that follows it is cheap, because
// the point of it is not this pack: it is the other eleven thousand, where the
// same call returns false without touching an index at all.
func (s *sealedPack) lookup(objID string) (indexEntry, bool) {
	raw, err := hex.DecodeString(objID)
	if err != nil || len(raw) != idxIDSize {
		return indexEntry{}, false
	}
	if !s.bloom.mayContain(raw) {
		return indexEntry{}, false
	}

	n := s.count()
	i := sort.Search(n, func(i int) bool {
		return bytes.Compare(s.record(i)[:idxIDSize], raw) >= 0
	})
	if i == n || !bytes.Equal(s.record(i)[:idxIDSize], raw) {
		return indexEntry{}, false
	}
	return decodeIndexRecord(s.record(i)), true
}

// readFrameAt reads one frame out of the pack.
func (s *sealedPack) readFrameAt(e indexEntry) ([]byte, error) {
	buf := make([]byte, e.Length)
	if _, err := s.f.ReadAt(buf, e.Offset); err != nil {
		return nil, fmt.Errorf("reading %s from pack %s: %v", e.ID, s.id, err)
	}
	return buf, nil
}

// entries decodes the whole index, in id order. Compaction and the ingest scan
// want it; a lookup never does.
func (s *sealedPack) entries() []indexEntry {
	out := make([]indexEntry, 0, s.count())
	for i := 0; i < s.count(); i++ {
		out = append(out, decodeIndexRecord(s.record(i)))
	}
	return out
}

func (s *sealedPack) close() error { return s.f.Close() }
