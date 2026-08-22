package store

import "errors"

// ErrEncoding reports an object this format cannot have produced: a
// non-canonical varint, a count that disagrees with the list it prefixes, a
// reserved flag bit, a field past a pinned bound. It is a sentinel because
// these objects arrive from a server the threat model calls actively
// malicious, and every caller's answer is the same — refuse the object, do
// not repair it.
var ErrEncoding = errors.New("malformed store object")

// appendUvarint appends v in LEB128: seven bits per byte, little-endian, high
// bit set on every byte but the last.
func appendUvarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// readUvarint reads one varint and returns it with the number of bytes
// consumed.
//
// It rejects non-canonical encodings — a value padded with continuation bytes
// that add nothing — which encoding/binary accepts. Without that, two writers
// produce different bytes for the same object, hence different ids and
// different sealing keys for identical content: the silent divergence the
// whole format exists to prevent, arriving through the smallest field in it.
func readUvarint(b []byte) (uint64, int, error) {
	var v uint64
	var shift uint
	for i := 0; i < len(b); i++ {
		c := b[i]
		if i == 9 && c > 1 {
			return 0, 0, ErrEncoding
		}
		v |= uint64(c&0x7f) << shift
		if c < 0x80 {
			if i > 0 && c == 0 {
				return 0, 0, ErrEncoding
			}
			return v, i + 1, nil
		}
		shift += 7
		if i == 9 {
			return 0, 0, ErrEncoding
		}
	}
	return 0, 0, ErrEncoding
}
