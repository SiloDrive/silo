package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// IDSize is the length of an object id in bytes.
const IDSize = sha256.Size

// ID names everything in the store: chunks, manifests, directory objects,
// commits. It is the dedup key, the integrity proof, the wire identifier and
// the cache key, and it is the only name anything outside the store uses.
// Pack offsets are lookup results; they are never identities.
type ID [IDSize]byte

// String renders an id as lowercase hex, which is how it travels on the wire
// and appears in every log line.
func (id ID) String() string { return hex.EncodeToString(id[:]) }

// IsZero reports whether the id is the zero value — never a real id, since it
// would have to be a SHA-256 preimage of zero.
func (id ID) IsZero() bool { return id == ID{} }

// ParseID reads an id from its hex spelling. It rejects uppercase, so one
// spelling reaches the wire and a case-folding hop cannot turn one id into
// two cache entries.
func ParseID(s string) (ID, error) {
	var id ID
	if len(s) != 2*IDSize {
		return id, fmt.Errorf("store: id %q is %d characters, want %d", s, len(s), 2*IDSize)
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return id, fmt.Errorf("store: id %q: %w", s, err)
	}
	if hex.EncodeToString(b) != s {
		return id, fmt.Errorf("store: id %q is not lowercase hex", s)
	}
	copy(id[:], b)
	return id, nil
}

// ChunkID is the id of a chunk, computed over the bytes the client sent —
// plaintext for a plain library, the client's ciphertext for an E2EE one.
//
// The distinction is the guardrail the whole encryption design rests on: the
// server only ever hashes what it stores. It never computes an id from
// plaintext, which means it cannot, so verification and ETags work identically
// for both library types and no future refactor can quietly acquire the
// ability.
func ChunkID(stored []byte) ID { return sha256.Sum256(stored) }

// ObjectID is the id of an encoded manifest, directory or commit — the same
// hash over the same kind of thing, named separately because the argument is
// an encoded object rather than content, and callers confusing the two would
// be a bug worth making visible.
func ObjectID(encoded []byte) ID { return sha256.Sum256(encoded) }

// PlaintextHash is H_p: the hash of a chunk's plaintext, which under E2EE is
// what the per-chunk key derives from and what a reader verifies against
// after decrypting.
//
// It is not an id and never appears as one. A chunk id hashes ciphertext, and
// there is no path from one to the other — which is exactly why manifests
// carry a sealed section: a reader holding only ids could not derive a key.
func PlaintextHash(plaintext []byte) ID { return sha256.Sum256(plaintext) }
