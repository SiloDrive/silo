package store

import (
	"reflect"
	"testing"
)

// A manifest whose chunks are 10, 20, 30, 40 bytes: file offsets 0-9, 10-29,
// 30-59, 60-99. Sizes differ so an off-by-one in the prefix sum cannot hide
// behind a uniform stride.
func rangesManifest() *Manifest {
	m := &Manifest{FileSize: 100}
	for i, size := range []int64{10, 20, 30, 40} {
		m.Chunks = append(m.Chunks, ChunkRef{ID: ChunkID([]byte{byte(i)}), Size: size})
	}
	return m
}

func TestChunksCoveringPicksTheChunksARangeTouches(t *testing.T) {
	m := rangesManifest()
	for _, tc := range []struct {
		name   string
		ranges []ByteRange
		want   []int
	}{
		{"the first byte", []ByteRange{{0, 1}}, []int{0}},
		{"inside one chunk", []ByteRange{{35, 5}}, []int{2}},
		{"across a boundary", []ByteRange{{9, 2}}, []int{0, 1}},
		{"ending exactly on a boundary", []ByteRange{{0, 10}}, []int{0}},
		{"starting exactly on a boundary", []ByteRange{{10, 1}}, []int{1}},
		{"the last byte", []ByteRange{{99, 1}}, []int{3}},
		{"the whole file", []ByteRange{{0, 100}}, []int{0, 1, 2, 3}},
		{"past the end is clamped away", []ByteRange{{95, 1000}}, []int{3}},
		{"wholly past the end touches nothing", []ByteRange{{100, 10}}, nil},
		{"a negative length runs to the end", []ByteRange{{60, -1}}, []int{3}},
		// Two reads a preview makes: a header and a tail. The point of the
		// endpoint is that neither drags the middle of the file with it.
		{"head and tail, not the middle", []ByteRange{{0, 4}, {96, 4}}, []int{0, 3}},
		// Overlapping and out-of-order ranges are one answer, ascending and
		// without repeats: a client assembling ranges from a seek pattern has
		// no reason to sort them first.
		{"out of order", []ByteRange{{60, 4}, {0, 4}}, []int{0, 3}},
		{"overlapping", []ByteRange{{0, 15}, {5, 10}}, []int{0, 1}},
		// Two ranges landing in one chunk name it once. Without the sweep
		// carrying its position across spans this is the case that repeats.
		{"two ranges in one chunk", []ByteRange{{31, 1}, {50, 1}}, []int{2}},
		{"adjacent ranges sharing a chunk", []ByteRange{{28, 4}, {32, 4}}, []int{1, 2}},
		{"zero length touches nothing", []ByteRange{{10, 0}}, nil},
		{"a negative offset is clamped to the start", []ByteRange{{-5, 12}}, []int{0, 1}},
		{"no ranges", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := m.ChunksCovering(tc.ranges, 0)
			if !ok {
				t.Fatalf("ChunksCovering(%v) reported over the limit with no limit set", tc.ranges)
			}
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ChunksCovering(%v) = %v, want %v", tc.ranges, got, tc.want)
			}
		})
	}
}

// An inline manifest has no chunk list, so nothing covers anything: the bytes
// are in the manifest the caller already holds.
func TestChunksCoveringOfAnInlineManifestIsEmpty(t *testing.T) {
	m := &Manifest{FileSize: 4, Inline: []byte("abcd")}
	got, ok := m.ChunksCovering([]ByteRange{{0, 4}}, 0)
	if !ok || len(got) != 0 {
		t.Errorf("ChunksCovering = %v, %v; want empty and within the limit", got, ok)
	}
}

// The limit refuses rather than truncating, and refuses before it has built
// the list — a truncated answer here would have the caller send frames for a
// range it did not cover and believe it had.
func TestChunksCoveringRefusesPastItsLimit(t *testing.T) {
	m := rangesManifest()
	if _, ok := m.ChunksCovering([]ByteRange{{0, 100}}, 3); ok {
		t.Error("four chunks passed a limit of three")
	}
	if got, ok := m.ChunksCovering([]ByteRange{{0, 100}}, 4); !ok || len(got) != 4 {
		t.Errorf("four chunks at a limit of four = %v, %v; want all four", got, ok)
	}
}
