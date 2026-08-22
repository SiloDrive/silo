package store

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"io"
)

// gearTableBytes is the HKDF output length behind the 256-entry gear table:
// 256 entries of 8 bytes.
const gearTableBytes = 256 * 8

// gearTable expands a library's seed into the 256 random uint64 the rolling
// hash adds per byte.
//
// A PRF rather than a hardcoded table because E2EE libraries need a secret
// table and there is no reason for plain libraries to reach it a different
// way. Little-endian is pinned here because it is the only choice a port can
// get wrong silently: a big-endian reader produces a working chunker that
// cuts in entirely different places.
func gearTable(seed [32]byte) [256]uint64 {
	out, err := hkdf.Key(sha256.New, seed[:], []byte("silo/gear/v1"), "", gearTableBytes)
	if err != nil {
		panic("store: gear table derivation: " + err.Error())
	}
	var g [256]uint64
	for i := range g {
		g[i] = binary.LittleEndian.Uint64(out[i*8:])
	}
	return g
}

// topMask returns the n most significant bits of a uint64.
//
// The masks take the high bits because of how the gear hash accumulates: with
// h = (h<<1) + gear[b], bit j of h has been fed by the last j+1 bytes, so the
// low bits are decided by a handful of bytes and the high bits by a window of
// sixty-odd. A mask over low bits would cut on the last byte alone and lose
// the content-defined property entirely.
//
// This is the one place the format departs from the FastCDC paper, which
// publishes hand-picked masks for an 8 KiB average with their set bits spread
// through the middle of the word. Those numbers do not generalise to another
// target size, and a rule that does is worth more here than matching a table:
// two implementations must agree, and "the top n bits" is a sentence, not a
// constant to be transcribed.
func topMask(n int) uint64 {
	if n <= 0 {
		return 0
	}
	if n >= 64 {
		return ^uint64(0)
	}
	return ^uint64(0) << (64 - n)
}

// Chunker splits a byte stream into content-defined chunks.
//
// Chunking is inherently sequential — where a boundary falls depends on every
// byte before it — so this type is a stream, not a function over a buffer.
// Hashing the chunks it yields is what parallelises.
type Chunker struct {
	p     Params
	gear  [256]uint64
	maskS uint64
	maskL uint64

	r       io.Reader
	buf     []byte
	off     int64
	eof     bool
	pending int
}

// Chunk is one cut piece of the stream.
//
// Data aliases the chunker's internal buffer and is only valid until the next
// call to Next. Callers that keep chunks — building a manifest, queueing
// uploads — must copy or consume before advancing. The alternative is one
// allocation per chunk on a path that runs at disk speed over terabytes.
type Chunk struct {
	Offset int64
	Data   []byte
}

// NewChunker returns a chunker reading from r under the given parameters.
func NewChunker(p Params, r io.Reader) (*Chunker, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	tb := targetBits(p.TargetSize)
	return &Chunker{
		p:     p,
		gear:  gearTable(p.Seed),
		maskS: topMask(tb + p.Normalization),
		maskL: topMask(tb - p.Normalization),
		r:     r,
		buf:   make([]byte, 0, p.MaxSize),
	}, nil
}

// Next returns the next chunk, or io.EOF when the stream is exhausted.
//
// A zero-length stream yields no chunks at all rather than one empty chunk:
// an empty file is inlined and has no chunk list, and a chunk of no bytes
// would otherwise become a real id every empty file in the library referenced.
func (c *Chunker) Next() (Chunk, error) {
	if err := c.fill(); err != nil {
		return Chunk{}, err
	}
	if len(c.buf) == 0 {
		return Chunk{}, io.EOF
	}
	n := c.Cut(c.buf)
	ch := Chunk{Offset: c.off, Data: c.buf[:n]}
	c.off += int64(n)
	c.pending = n
	return ch, nil
}

// fill drops the chunk the last Next handed out and tops the buffer back up
// to MaxSize.
//
// The drop happens here, at the start of the next call, rather than at the
// end of Next — which is what keeps the Data slice Next returned valid for
// exactly as long as its documentation promises.
func (c *Chunker) fill() error {
	if c.pending > 0 {
		c.buf = c.buf[:copy(c.buf, c.buf[c.pending:])]
		c.pending = 0
	}
	for !c.eof && len(c.buf) < c.p.MaxSize {
		n, err := c.r.Read(c.buf[len(c.buf):c.p.MaxSize])
		c.buf = c.buf[:len(c.buf)+n]
		if err == io.EOF {
			c.eof = true
			break
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Cut returns the length of the first chunk in data, given that data holds
// everything remaining in the stream up to at least MaxSize.
//
// This is the whole of the boundary rule, exported because it is what the
// test vectors pin and what a second implementation has to reproduce exactly.
//
// The shape is FastCDC's normalised chunking with sub-minimum cut-point
// skipping: bytes before the minimum are not hashed at all, then a strict
// mask applies until the target and a lax one after it, which pulls the size
// distribution in around the target from both sides. The two off-by-ones that
// a port can get wrong silently are pinned here: hashing begins at
// data[MinSize-1], so the shortest chunk is exactly MinSize, and a byte that
// satisfies the mask ends the chunk it is part of, so a hit at index i yields
// a chunk of i+1 bytes.
func (c *Chunker) Cut(data []byte) int {
	n := len(data)
	if n <= c.p.MinSize {
		return n
	}
	if n > c.p.MaxSize {
		n = c.p.MaxSize
	}
	normal := c.p.TargetSize
	if normal > n {
		normal = n
	}

	var h uint64
	i := c.p.MinSize - 1
	for ; i < normal; i++ {
		h = (h << 1) + c.gear[data[i]]
		if h&c.maskS == 0 {
			return i + 1
		}
	}
	for ; i < n; i++ {
		h = (h << 1) + c.gear[data[i]]
		if h&c.maskL == 0 {
			return i + 1
		}
	}
	return n
}
