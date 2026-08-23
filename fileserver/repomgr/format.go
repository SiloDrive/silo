package repomgr

import (
	"errors"
	"fmt"

	"github.com/dkam/silo/store"
)

// ErrNoContentKey reports a server-side caller asking for the chunker of an
// end-to-end encrypted library. The server holds no content key, so it cannot
// build one — which is the correct answer rather than a missing feature, since
// there is nothing in such a library for a server to chunk.
var ErrNoContentKey = errors.New("library is end-to-end encrypted; the server holds no content key")

// Format is how a library's bytes are made: the chunker it was created with,
// and whether its content is end-to-end encrypted.
//
// Both are frozen at creation, and both are per-library data rather than
// server configuration. A client that chunks differently computes different
// ids for the same file, so these values are part of the library's identity —
// serving them to clients is how every implementation agrees on where the cut
// points are. Changing one is `silo convert`, which rewrites every object.
type Format struct {
	Chunker       string
	MinSize       int
	TargetSize    int
	MaxSize       int
	Normalization int
	E2EE          bool
}

// DefaultFormat is what a new library is created with.
//
// e2ee is a parameter rather than a constant because both answers are
// reachable and the default is the encrypted one: "make this library public"
// needs the other, and so does anything that has to be readable by a client
// that cannot hold a key.
func DefaultFormat(e2ee bool) Format {
	return Format{
		Chunker:       store.ChunkerAlgorithm,
		MinSize:       store.DefaultMinSize,
		TargetSize:    store.DefaultTargetSize,
		MaxSize:       store.DefaultMaxSize,
		Normalization: store.DefaultNormalization,
		E2EE:          e2ee,
	}
}

// Params turns a catalog row into chunker parameters, validated.
//
// The seed is a parameter because it is not always the server's to compute: a
// server-readable library chunks under store.PlainSeed, but an E2EE library's
// seed is derived from its content key, which only its members hold. Server
// code wants PlainParams; client code holding CK passes store.ChunkerSeed(ck).
//
// Validation happens on the way out rather than on the way in, every time. The
// row is data, and a row carrying a target below its minimum is either a bug
// upstream or a database somebody edited — in both cases the answer is to
// refuse rather than to chunk something no other client will reproduce.
func (f Format) Params(seed [32]byte) (store.Params, error) {
	p := store.Params{
		Algorithm:     f.Chunker,
		Seed:          seed,
		MinSize:       f.MinSize,
		TargetSize:    f.TargetSize,
		MaxSize:       f.MaxSize,
		Normalization: f.Normalization,
	}
	if err := p.Validate(); err != nil {
		return store.Params{}, err
	}
	return p, nil
}

// PlainParams is Params for a library the server can read, and refuses one it
// cannot.
func (f Format) PlainParams() (store.Params, error) {
	if f.E2EE {
		return store.Params{}, ErrNoContentKey
	}
	return f.Params(store.PlainSeed())
}

// ServerParams is Params as the server can know them, for either library type.
//
// A plain library chunks under the published seed and the server chunks it, so
// that case is PlainParams. An E2EE library's seed is HKDF over its content
// key, which the server never holds — so there is no honest value to put here,
// and the seed is left zero. That is safe and deliberate rather than a
// placeholder: the server never chunks an E2EE library (WriteFile refuses
// without the key), so the seed is never read; and zero is specifically not
// the plain seed, so store.Params.ValidateFor's rule that an E2EE library must
// never chunk under the published seed still bites if this value ever reaches
// a chunker.
//
// Use this to open a library for the work the server actually does — serving
// bytes by id, tracing the tree, reading public sections. Anything needing
// real chunking in an E2EE library is a client operation.
func (f Format) ServerParams() (store.Params, error) {
	if !f.E2EE {
		return f.PlainParams()
	}
	return f.Params([32]byte{})
}

// Validate reports whether the format describes a library this build can work
// with at all, without needing a seed to say so.
func (f Format) Validate() error {
	// Any seed validates the same bounds; the seed is not part of them.
	if _, err := f.Params(store.PlainSeed()); err != nil {
		return fmt.Errorf("library format: %w", err)
	}
	return nil
}
