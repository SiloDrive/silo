// The store's id vocabulary: what an id is, and what checks that bytes match
// one.
//
// Here rather than in a backend because it is the store's contract, not a
// filesystem's. Both halves are consumed from three depths now — validPackID
// by packPath, by frameID and by the seam; verifier by ObjectStore.write —
// and a second backend should inherit these rules by reading them, rather
// than by happening to sit in the same package as the first one.
package objstore

import (
	"crypto/sha256"
	"hash"
)

// verifier returns the digest an id names.
//
// One hash, because there is one id width: every id in the store is the
// SHA-256 of the bytes it names. It stays a function rather than a call to
// sha256.New at each site so that the id and the digest that checks it are
// decided in one place.
//
// Called from ObjectStore.write, above the sealing. Verification has to
// happen over the object, and what reaches a backend is the frame around it,
// so a backend cannot check an id even in principle.
func verifier() hash.Hash { return sha256.New() }

// validPackID reports whether an id is one this store will build a path from.
//
// Lowercase hex, sixty-four characters: the SHA-256 of what is stored under
// it. Sixty-four is more than the two characters the fan-out slices off, which
// is the crash this guards.
//
// One case only. Two spellings of an id are two files holding one object, and
// the second is invisible to every reader looking for the first.
func validPackID(id string) bool {
	if len(id) != 2*sha256.Size {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}
