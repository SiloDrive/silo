// The sealed storage frame: what an object looks like on disk, on every
// backend, in every library.
//
// This is the *outer* of the two encryptions a byte can carry, and it is not
// the one in store/. That package's sealing is content crypto under a
// library's content key, which the server does not hold for an E2EE library.
// This one is under storage.key, which the server holds and no client ever
// sees, and it applies uniformly — both library types, all backends —
// so that an E2EE chunk simply gets a second wrap and the backend stays
// ignorant of libraries. docs/storage.md § Storage encryption, universal is
// the owning document.
//
// The frame lives here rather than in store/ for the reason store/doc.go
// gives: storage-layer encryption is the server's business, and a client that
// reimplements the store format has no use for code it can never run. The
// vectors in testdata/frame.json pin the bytes anyway, because storage.key is
// unrotatable and so is the format under it.
//
// It is applied above the backend seam, not inside one. Every backend stores
// identical bytes — that is what makes replication a file copy — so a backend
// that had to seal for itself would be a second place for the answer to
// differ.
package objstore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
)

// The frame's fixed header, in order:
//
//	magic       4    "SILF"
//	version     1    frameVersion
//	id         32    the object's id — SHA-256 of the bytes sealed here
//	ct_len      8    length of ciphertext ‖ tag, little-endian
//	nonce      12    fresh per write
//	ct ‖ tag    n
//
// Every width is fixed, and that is forced rather than tidy: Stat answers
// with the plaintext length, derived as the file size minus frameOverhead, so
// that a Content-Length costs one os.Stat and never opens the object. A
// varint length would make the overhead a function of the content and break
// the subtraction.
//
// The magic and the version are not in the tuple docs/storage.md first gave.
// Without the magic a frame cannot say it is one, so a truncated or foreign
// file would be decrypted rather than rejected; without the version a format
// change means rewriting every frame on every tier, which is the operation
// storage.key is defined as not supporting.
const (
	frameMagic      = "SILF"
	frameVersion    = 1
	frameNonceSize  = 12
	frameTagSize    = 16
	frameHeaderSize = len(frameMagic) + 1 + 32 + 8 + frameNonceSize

	// frameOverhead is what a frame adds to the bytes it holds.
	frameOverhead = frameHeaderSize + frameTagSize

	offVersion = len(frameMagic)
	offID      = offVersion + 1
	offLen     = offID + 32
	offNonce   = offLen + 8
)

// StorageKeySize is the length of storage.key.
const StorageKeySize = 32

// ErrFrameCorrupt reports a frame that did not authenticate: a wrong key, a
// truncated write, a bit rot, or a frame moved to another object's path. They
// are indistinguishable by construction and the answer is the same for all of
// them — this object is not readable, and the id it was stored under does not
// name what is there.
var ErrFrameCorrupt = errors.New("objstore: stored frame failed to authenticate")

// isFrame reports whether b begins a sealed frame.
//
// A magic check, and openFrame's first gate. It is not a choice between two
// readers: everything in the store is a frame, so bytes that fail this are
// corruption and are reported as such. Anything that reaches here and is not
// a frame has already got past a server that refuses to start on a store it
// has no key for.
func isFrame(b []byte) bool {
	return len(b) >= len(frameMagic) && string(b[:len(frameMagic)]) == frameMagic
}

// frameAEAD builds the cipher for one frame.
func frameAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != StorageKeySize {
		return nil, fmt.Errorf("objstore: storage key is %d bytes, want %d", len(key), StorageKeySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("objstore: storage cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

// sealFrame seals plaintext under key as the object stored at id, with a
// fresh random nonce.
//
// Non-convergent on purpose: the same plaintext sealed twice gives different
// bytes. That is the opposite of the rule store.SealChunk follows, and it is
// right here for the reason that rule is right there — an id is minted over
// the bytes handed to the store, before this runs, so dedup, ETags and
// changes?since= never see what happens underneath them. A deterministic
// nonce would buy byte-identical rewrites of an object that is never
// rewritten, at the cost of the one thing GCM asks of a caller.
func sealFrame(key []byte, id string, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, frameNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("objstore: no randomness for a frame nonce: %w", err)
	}
	return sealFrameNonce(key, id, plaintext, nonce)
}

// sealFrameNonce is sealFrame with the nonce supplied. Only the vectors call
// it directly; a caller that picks its own nonce is one that can repeat one.
func sealFrameNonce(key []byte, id string, plaintext, nonce []byte) ([]byte, error) {
	aead, err := frameAEAD(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != frameNonceSize {
		return nil, fmt.Errorf("objstore: frame nonce is %d bytes, want %d", len(nonce), frameNonceSize)
	}
	raw, err := frameID(id)
	if err != nil {
		return nil, err
	}

	frame := make([]byte, frameHeaderSize, frameHeaderSize+len(plaintext)+frameTagSize)
	copy(frame, frameMagic)
	frame[offVersion] = frameVersion
	copy(frame[offID:], raw)
	binary.LittleEndian.PutUint64(frame[offLen:], uint64(len(plaintext)+frameTagSize))
	copy(frame[offNonce:], nonce)

	// The header is the associated data, so the id, the length and the nonce
	// are all authenticated. Without that the id in the header is decoration:
	// nothing would stop a frame being relabelled, or moved to another
	// object's path, and opening happily under the same key.
	return aead.Seal(frame, nonce, plaintext, frame[:frameHeaderSize]), nil
}

// openFrame reverses sealFrame, checking that the frame is the one stored at
// id, and appends the plaintext to dst.
//
// dst is an optional scratch buffer: with enough capacity the object is
// decrypted straight into it and the read allocates nothing, which is what
// lets a batch fetch reuse one buffer down its whole loop. A nil dst is a
// fresh allocation, so callers with nothing to reuse pass nil.
func openFrame(key []byte, id string, frame, dst []byte) ([]byte, error) {
	aead, err := frameAEAD(key)
	if err != nil {
		return nil, err
	}
	raw, err := frameID(id)
	if err != nil {
		return nil, err
	}
	if len(frame) < frameOverhead || !isFrame(frame) {
		return nil, ErrFrameCorrupt
	}
	if frame[offVersion] != frameVersion {
		return nil, fmt.Errorf("%w: frame version %d, want %d", ErrFrameCorrupt, frame[offVersion], frameVersion)
	}
	// The id is checked here rather than left to the AEAD, and it is the one
	// header field of which that is true. The associated data is the header
	// as it was read from the file, not one rebuilt from the id asked for, so
	// a whole frame moved to another object's path authenticates perfectly
	// and would hand back the wrong object's bytes under the requested name.
	// The AEAD stops the id being edited in place; this stops the frame being
	// moved. Both are needed and neither substitutes for the other.
	//
	// ct_len gets no such check: Open never reads it, and any disagreement
	// between it and the bytes present is already a failed tag, either
	// because the ciphertext was cut or because the header covering it
	// changed. See ErrFrameCorrupt on why one answer is the right number.
	if string(frame[offID:offID+32]) != string(raw) {
		return nil, fmt.Errorf("%w: frame holds another object", ErrFrameCorrupt)
	}

	plain, err := aead.Open(dst[:0], frame[offNonce:frameHeaderSize], frame[frameHeaderSize:], frame[:frameHeaderSize])
	if err != nil {
		return nil, ErrFrameCorrupt
	}
	return plain, nil
}

// frameID decodes an object id into the 32 bytes the header carries.
//
// The header holds the raw digest rather than its hex, which halves it. The
// width check is validPackID's, restated as a decode: an id that is not
// sixty-four lowercase hex characters never named anything in this store.
func frameID(id string) ([]byte, error) {
	if !validPackID(id) {
		return nil, fmt.Errorf("invalid object id %q", id)
	}
	return hex.DecodeString(id)
}
