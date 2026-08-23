package store

import (
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/bits"
)

// ChunkerAlgorithm names the one chunker this version of the format defines.
// It is stored per library rather than assumed, so a second profile can be
// added later without a format change — the plan's decision 2.
const ChunkerAlgorithm = "fastcdc-gear64/v1"

// The default chunker sizes, confirmed by gate G1 against a real 6 TB
// workload: 93% of bytes sit in files over 256 MiB, so the target is chosen
// for read amplification rather than dedup granularity, and 1 MiB is where
// porter's 2 MiB read window converges.
const (
	DefaultMinSize    = 256 << 10
	DefaultTargetSize = 1 << 20
	DefaultMaxSize    = 4 << 20

	// DefaultNormalization is FastCDC's normalisation level: the number of
	// bits by which the pre-target and post-target masks differ from the
	// target's own bit count. Level 2 is the paper's recommendation and
	// concentrates chunk sizes hard around the target.
	DefaultNormalization = 2
)

// InlineThreshold is the size below which a file's bytes live in its manifest
// instead of becoming a chunk. G1 measured a third of all files under this
// size holding 0.01% of all bytes: the threshold removes a third of the chunk
// objects in the store and costs nothing.
const InlineThreshold = 64 << 10

// Params is one library's chunker, as stored in the catalog and served to
// clients in the library listing. Every field is frozen at library creation —
// a client that chunks differently computes different ids, so these are data,
// never constants compiled into a client.
type Params struct {
	Algorithm string
	// Seed keys the gear table. Plain libraries use PlainSeed, which is the
	// same everywhere so cross-library dedup among them survives; E2EE
	// libraries use ChunkerSeed(CK), which is what stops the server
	// fingerprinting known files by their chunk-size sequence.
	Seed          [32]byte
	MinSize       int
	TargetSize    int
	MaxSize       int
	Normalization int
}

// DefaultParams returns the parameters a library is created with, for the
// given seed.
func DefaultParams(seed [32]byte) Params {
	return Params{
		Algorithm:     ChunkerAlgorithm,
		Seed:          seed,
		MinSize:       DefaultMinSize,
		TargetSize:    DefaultTargetSize,
		MaxSize:       DefaultMaxSize,
		Normalization: DefaultNormalization,
	}
}

// PlainSeed is the published constant seed every plain library chunks under.
//
// It is defined as a hash of its own domain string rather than written out as
// 32 hex bytes so that a second implementation can derive it instead of
// copying it, and so that nobody has to wonder where the number came from.
// It is computed once rather than per call because Format.Validate reaches it
// on every repository load, and re-hashing a compile-time constant was most of
// what that validation cost. Returning the array by value keeps it as
// uncopyable-from-outside as it was.
func PlainSeed() [32]byte { return plainSeed }

var plainSeed = sha256.Sum256([]byte("silo/chunker/plain/v1"))

// ChunkerSeed derives an E2EE library's gear seed from its content key.
//
// The seed does two jobs, and the second is easy to miss: manifests publish
// seal_hash over a list of plaintext chunk hashes, which is computable from
// plaintext alone. It is not a content-confirmation oracle only because
// reproducing that list means reproducing the cut points, which needs this
// seed, which needs CK. Moving an E2EE library onto PlainSeed would break
// confidentiality, not just fingerprint resistance.
func ChunkerSeed(ck []byte) [32]byte {
	out, err := hkdf.Key(sha256.New, ck, []byte("silo/chunker/v1"), "", 32)
	if err != nil {
		// hkdf.Key fails only on a length beyond 255 hashes; 32 is not.
		panic("store: chunker seed derivation: " + err.Error())
	}
	var seed [32]byte
	copy(seed[:], out)
	return seed
}

// ErrParams reports parameters no chunker can be built from. It is a sentinel
// because these arrive from the catalog and from the wire: a library row
// carrying a target below its minimum is a server bug or a hostile server,
// and either way the client must refuse rather than chunk something nobody
// else will reproduce.
var ErrParams = errors.New("invalid chunker parameters")

// Validate reports whether the parameters describe a chunker this package can
// build. The bounds are the format's, not this implementation's: a port that
// accepts more accepts libraries no other client can read.
func (p Params) Validate() error {
	if p.Algorithm != ChunkerAlgorithm {
		return fmt.Errorf("%w: unknown algorithm %q", ErrParams, p.Algorithm)
	}
	if p.MinSize < 64 {
		return fmt.Errorf("%w: min size %d below 64", ErrParams, p.MinSize)
	}
	if p.MinSize >= p.TargetSize {
		return fmt.Errorf("%w: min size %d not below target %d", ErrParams, p.MinSize, p.TargetSize)
	}
	if p.TargetSize > p.MaxSize {
		return fmt.Errorf("%w: target size %d above max %d", ErrParams, p.TargetSize, p.MaxSize)
	}
	if p.MaxSize > 1<<30 {
		return fmt.Errorf("%w: max size %d above 1 GiB", ErrParams, p.MaxSize)
	}
	tb := targetBits(p.TargetSize)
	if p.Normalization < 0 || p.Normalization > 3 {
		return fmt.Errorf("%w: normalisation level %d outside 0..3", ErrParams, p.Normalization)
	}
	if tb-p.Normalization < 1 || tb+p.Normalization > 64 {
		return fmt.Errorf("%w: normalisation level %d leaves no mask for a %d-bit target",
			ErrParams, p.Normalization, tb)
	}
	return nil
}

// ValidateFor reports whether these parameters describe the chunker a library
// of this type must use. Validate answers "can a chunker be built from this";
// this answers "is it the right chunker for this library", which is the
// question with a confidentiality answer.
//
// The rule the spec states in bold — an E2EE library must never chunk under
// the plain seed — lives here rather than in a caller, because a rule enforced
// above the shared package is a rule the second implementation of that package
// silently ships without. This one fails quiet: a client that chunks an E2EE
// library under PlainSeed produces manifests that open, verify and sync, and
// hands the server a content-confirmation oracle it needs no key to use. There
// is no error to notice and nothing in the bytes that looks wrong.
//
// ck is what the caller holds, not what the library is: e2ee says which kind
// of library this is, and a nil ck under e2ee is a server, or a client before
// it has unwrapped anything. That case still gets a check — the seed cannot be
// compared against a key there isn't one of, but it can be compared against
// the one value it must never be, which is the value a Config assembled from a
// plain library's parameters and an E2EE flag would carry.
//
// The refusals are pinned in chunker.json's params_refused.
func (p Params) ValidateFor(e2ee bool, ck []byte) error {
	if err := p.Validate(); err != nil {
		return err
	}
	switch {
	case !e2ee && len(ck) > 0:
		return fmt.Errorf("%w: a plain library has no content key", ErrParams)
	case len(ck) > 0:
		if p.Seed != ChunkerSeed(ck) {
			return fmt.Errorf("%w: the seed is not this content key's", ErrParams)
		}
	case !e2ee:
		if p.Seed != PlainSeed() {
			return fmt.Errorf("%w: a plain library must chunk under the published seed", ErrParams)
		}
	default:
		if p.Seed == PlainSeed() {
			return fmt.Errorf("%w: an E2EE library must not chunk under the published seed", ErrParams)
		}
	}
	return nil
}

// targetBits is floor(log2(target)). The target is not required to be a power
// of two — restic's is not — so the rule is stated in terms that hold either
// way, and both masks derive from this one number.
func targetBits(target int) int {
	return bits.Len64(uint64(target)) - 1
}
