package store

import "sort"

// Byte ranges to chunks: the arithmetic that turns "I want these bytes" into
// "I need these chunks".
//
// It lives in `store` rather than in the server for the reason the chunker and
// the manifest codec do. Both sides need it and they must agree: a client asks
// for the chunks a range touches, and the server answers a QUERY on
// entries/{path} by working out the same set. Two implementations of one
// prefix sum is two places for an off-by-one, and the failure it produces is
// a read that returns the wrong bytes rather than an error.
//
// A manifest earns its keep here: chunk sizes are recorded as PLAINTEXT
// lengths, so the run of chunks a range touches is computable from the
// manifest alone, without reading a single chunk. That is why this is
// arithmetic rather than I/O planning.

// ByteRange is a span of a file's bytes: Offset is where it starts, Length how
// far it runs. A negative Length means the rest of the file, which is the
// convention the reader already uses; an Offset past the end names nothing.
//
// It is half-open — Offset to Offset+Length — rather than the inclusive
// first-byte/last-byte pair an HTTP Range header carries. The header's
// spelling is the wire's, and it is inclusive because it has to name the last
// byte of a file whose length the sender may not know. A caller that has the
// manifest knows the length, so the arithmetic that follows is cleaner in the
// form the rest of the language counts in.
type ByteRange struct {
	Offset int64
	Length int64
}

// ChunksCovering reports which chunks the given ranges touch, as indices into
// m.Chunks: ascending, and each index once however many ranges reach it.
//
// Ranges need not be sorted or disjoint. A client assembling them from a seek
// pattern has no reason to order them, and two reads landing either side of
// one chunk boundary must not name the shared chunk twice — that repeat is
// exactly the bandwidth a batched fetch exists to save.
//
// Out-of-range input is clamped rather than refused, because this is
// arithmetic and the caller is the one holding a contract to enforce. A
// negative offset starts at the beginning, a range running past the end stops
// at it, and a range wholly past the end contributes nothing. The caller that
// answers a request validates first; see queryEntry in the file server.
//
// limit bounds the answer: ranges touching more than limit chunks stop the
// walk and report false, rather than building a list the caller is about to
// refuse. That matters because the list is the one allocation here that scales
// with the manifest rather than with the request — a two-line body naming one
// range over a large file would otherwise size a slice from the file. A limit
// of zero means no bound.
//
// The walk is a single forward sweep of the chunk list, not a search per
// range. A binary search would need a prefix-sum table the manifest does not
// carry, and building one costs the pass this avoids.
func (m *Manifest) ChunksCovering(ranges []ByteRange, limit int) ([]int, bool) {
	if len(m.Chunks) == 0 || len(ranges) == 0 {
		return nil, true
	}

	// Clamped to the file first, so the merge below compares spans that are
	// all real. The copy is deliberate: sorting the caller's slice would
	// reorder a list it may still be using to lay the answer back out.
	spans := make([]ByteRange, 0, len(ranges))
	for _, r := range ranges {
		lo := r.Offset
		if lo < 0 {
			lo = 0
		}
		if lo >= m.FileSize {
			continue
		}
		// m.FileSize-lo cannot overflow: lo is non-negative and below it. Going
		// the other way round — lo+r.Length — can, on a length a caller chose.
		length := r.Length
		if length < 0 || length > m.FileSize-lo {
			length = m.FileSize - lo
		}
		if length == 0 {
			continue
		}
		spans = append(spans, ByteRange{Offset: lo, Length: length})
	}
	if len(spans) == 0 {
		return nil, true
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].Offset < spans[j].Offset })

	var idx []int
	// i and pos are the sweep: the next chunk to consider and the file offset
	// it begins at. They are carried across spans rather than reset, which is
	// both what makes this one pass and what keeps a chunk two spans share
	// from being named twice — after a span is answered they point past the
	// last chunk emitted, so the next span can only add to the list.
	i, pos := 0, int64(0)
	for _, s := range spans {
		lo, hi := s.Offset, s.Offset+s.Length
		for i < len(m.Chunks) && pos+m.Chunks[i].Size <= lo {
			pos += m.Chunks[i].Size
			i++
		}
		for i < len(m.Chunks) && pos < hi {
			idx = append(idx, i)
			if limit > 0 && len(idx) > limit {
				return nil, false
			}
			pos += m.Chunks[i].Size
			i++
		}
	}
	return idx, true
}
