// The per-pack bloom filter: what lets a lookup skip a pack without opening
// it.
//
// A lookup has to name a pack before it can search one, and the obvious answer
// does not work here. Ids are SHA-256 and therefore uniformly random, so a
// min/max pair per pack partitions nothing — every pack's id range is the
// whole id space. That is the same dead end that put optional bloom filters
// into Parquet for high-cardinality equality, and it is why storage.md reaches
// the same place.
//
// So one filter per sealed pack: "no" is certain and ends the search, "yes"
// costs one binary search over that pack's index to confirm.
//
// The filter is written into the footer rather than rebuilt at open time. It
// could be rebuilt — the index is right there — but writing it is what lets the
// parameters be a property of the pack instead of a compiled-in constant, so a
// later pack can size itself differently without invalidating an earlier one.
package objstore

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
)

// The filter's own header, so that a footer section can be told from the
// garbage a truncated write leaves.
//
//	magic     4  "SILB"
//	version   1
//	k         1  how many bit-ranges are probed
//	logBits   1  log2 of the filter's size in bits
//	count     4  how many ids were added — diagnostics only, never read back
//	           to answer a query
const (
	bloomMagic      = "SILB"
	bloomVersion    = 1
	bloomHeaderSize = len(bloomMagic) + 1 + 1 + 1 + 4
)

// bloomBitsPerEntry is the sizing target, and it is set against the number of
// *packs* rather than per pack. That distinction is the easy thing to get
// wrong here. At a 512 MB target a pack holds ~500 chunks of 1 MiB, so a 6 TB
// store is ~12,000 packs — and a textbook 1% per-pack rate would mean ~120
// false hits, and 120 confirming searches, on every single lookup. 1e-5 is the
// rate that makes that ~0.1, and 24 bits per entry is what buys it: ~1.5 KB
// per pack, ~18 MB across the whole store.
const bloomBitsPerEntry = 24

// bloomMaxK caps the probe count at the optimum for bloomBitsPerEntry —
// ln(2) × 24 ≈ 16.6, rounded up. Sizing rounds the filter up to a power of two
// (see bloomParams), so the real bits-per-entry is between 24 and 48 and the
// true optimum is sometimes higher than this; the extra probes would buy
// fractions of an order of magnitude on a rate that is already past target,
// and each one costs id bits this format does not have to spare.
const bloomMaxK = 17

// bloomMinLogBits floors the filter at 512 bits. A pack with three frames in
// it does not need 72 bits of filter, and a filter smaller than a cache line
// saves nothing worth the special case.
const bloomMinLogBits = 9

// bloomMaxLogBits caps it at a 512 MB filter, which no pack this store writes
// can reach. It exists so that a corrupt logBits read off a disk cannot ask
// for an allocation measured in exabytes.
const bloomMaxLogBits = 32

var ErrBloomCorrupt = errors.New("objstore: pack bloom filter is not readable")

// bloomFilter is one pack's filter, in memory.
type bloomFilter struct {
	logBits uint8
	k       uint8
	count   uint32
	bits    []byte
}

// bloomParams sizes a filter for n entries.
//
// The size is rounded up to a power of two, and that is not tidiness: it makes
// an index into the filter a fixed-width slice of the id's bits, with no
// modulo and no bias. What it costs is up to 2× the bytes, and what that buys
// back is a false-positive rate at or below target even after the probe count
// is capped.
func bloomParams(n int) (logBits, k uint8) {
	want := bloomBitsPerEntry * n
	logBits = bloomMinLogBits
	if want > 1<<bloomMinLogBits {
		logBits = uint8(bits.Len(uint(want - 1)))
		if logBits > bloomMaxLogBits {
			logBits = bloomMaxLogBits
		}
	}

	// The probe count is bounded by the id as well as by the maths: k disjoint
	// ranges of logBits each have to fit inside 256 bits.
	k = bloomMaxK
	if budget := uint8(256 / int(logBits)); k > budget {
		k = budget
	}
	if k < 1 {
		k = 1
	}
	return logBits, k
}

// newBloom builds an empty filter sized for n entries.
func newBloom(n int) *bloomFilter {
	logBits, k := bloomParams(n)
	return &bloomFilter{logBits: logBits, k: k, bits: make([]byte, 1<<logBits/8)}
}

// idWindow is an id with eight bytes of padding after it.
//
// The padding is what lets a probe be one 64-bit read. A range can begin at any
// bit of the id, so the read starts at an arbitrary byte, and without the tail
// the last probe's read would run off the end — making the final probe a
// special case rather than the same arithmetic as the others.
type idWindow [idxIDSize + 8]byte

func newIDWindow(raw []byte) idWindow {
	var w idWindow
	copy(w[:], raw)
	return w
}

// probe is the i'th bit position an id maps to.
//
// There is no hash here and that is the point. An id is already the output of
// SHA-256, so its bits are uniform and independent, and hashing it again would
// buy nothing that taking a slice of it does not already have. Each probe
// takes its own disjoint range, so the k indices are independent for the same
// reason the bits are.
//
// One shift and one mask, rather than a loop over the range's bits. A lookup
// asks every sealed pack — twelve thousand on a full store — and most of them
// answer "no" from the first probe or two, so this constant is multiplied by
// the whole fan-out on every lookup.
//
// The arithmetic stays in bounds by construction: bloomParams caps k×logBits at
// the id's 256 bits, so a range starts at bit 247 at the latest and the bit
// offset within its first byte is under 8 — 7 + 32 is well inside a 64-bit read.
func (b *bloomFilter) probe(w *idWindow, i int) uint64 {
	width := uint(b.logBits)
	start := uint(i) * width
	v := binary.BigEndian.Uint64(w[start>>3:])
	return (v >> (64 - start&7 - width)) & (1<<width - 1)
}

func (b *bloomFilter) add(raw []byte) {
	w := newIDWindow(raw)
	for i := 0; i < int(b.k); i++ {
		p := b.probe(&w, i)
		b.bits[p>>3] |= 1 << (p & 7)
	}
	b.count++
}

// mayContain answers "no" definitively and "yes" probably, which is the whole
// contract. A caller that gets true still has to look.
func (b *bloomFilter) mayContain(raw []byte) bool {
	if len(raw) != idxIDSize {
		return false
	}
	w := newIDWindow(raw)
	for i := 0; i < int(b.k); i++ {
		p := b.probe(&w, i)
		if b.bits[p>>3]&(1<<(p&7)) == 0 {
			return false
		}
	}
	return true
}

func (b *bloomFilter) encode() []byte {
	out := make([]byte, bloomHeaderSize+len(b.bits))
	copy(out, bloomMagic)
	out[len(bloomMagic)] = bloomVersion
	out[len(bloomMagic)+1] = b.k
	out[len(bloomMagic)+2] = b.logBits
	binary.BigEndian.PutUint32(out[len(bloomMagic)+3:], b.count)
	copy(out[bloomHeaderSize:], b.bits)
	return out
}

// parseBloom reads a filter back out of a footer.
//
// Every field is checked against what this build can hold before anything is
// allocated from it, because the bytes come off a disk and logBits is an
// exponent.
func parseBloom(b []byte, name string) (*bloomFilter, error) {
	if err := checkFormatHeader(b, name, bloomMagic, bloomVersion, bloomHeaderSize, ErrBloomCorrupt); err != nil {
		return nil, err
	}
	k := b[len(bloomMagic)+1]
	logBits := b[len(bloomMagic)+2]
	count := binary.BigEndian.Uint32(b[len(bloomMagic)+3:])

	if logBits < bloomMinLogBits || logBits > bloomMaxLogBits {
		return nil, fmt.Errorf("%w: %s asks for %d bits, outside what this build reads", ErrBloomCorrupt, name, logBits)
	}
	if k < 1 || int(k)*int(logBits) > idxIDSize*8 {
		return nil, fmt.Errorf("%w: %s asks for %d probes of %d bits, which do not fit an id", ErrBloomCorrupt, name, k, logBits)
	}
	size := 1 << logBits / 8
	if len(b) != bloomHeaderSize+size {
		return nil, fmt.Errorf("%w: %s holds %d bytes of filter, want %d", ErrBloomCorrupt, name, len(b)-bloomHeaderSize, size)
	}

	bf := &bloomFilter{logBits: logBits, k: k, count: count, bits: make([]byte, size)}
	copy(bf.bits, b[bloomHeaderSize:])
	return bf, nil
}
