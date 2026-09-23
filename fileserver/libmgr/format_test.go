package libmgr

import (
	"errors"
	"testing"

	"github.com/SiloDrive/silo/store"
)

func TestTheDefaultFormatIsOneTheChunkerAccepts(t *testing.T) {
	for _, e2ee := range []bool{false, true} {
		f := DefaultFormat(e2ee)
		if err := f.Validate(); err != nil {
			t.Fatalf("e2ee=%v: this build's own default is refused by its own bounds: %v", e2ee, err)
		}
		if f.Chunker != store.ChunkerAlgorithm {
			t.Fatalf("default names chunker %q, want %q", f.Chunker, store.ChunkerAlgorithm)
		}
	}
}

// The catalog row is the contract with every client, so it has to reproduce
// exactly what store.DefaultParams would have built — a column written in the
// wrong order gives a working chunker that cuts in different places.
func TestTheCatalogRowRebuildsTheParametersItStoodFor(t *testing.T) {
	seed := store.PlainSeed()
	got, err := DefaultFormat(false).Params(seed)
	if err != nil {
		t.Fatal(err)
	}
	if want := store.DefaultParams(seed); got != want {
		t.Fatalf("rebuilt %+v, want %+v", got, want)
	}
}

func TestPlainParamsRefusesALibraryTheServerCannotRead(t *testing.T) {
	if _, err := DefaultFormat(true).PlainParams(); !errors.Is(err, ErrNoContentKey) {
		t.Fatalf("the server built a chunker for an E2EE library: %v", err)
	}
	if _, err := DefaultFormat(false).PlainParams(); err != nil {
		t.Fatalf("the server could not build a chunker for a plain library: %v", err)
	}
}

// An E2EE library chunks under a seed derived from its content key, so the
// same catalog row gives different cut points to a member and to the server.
// That is the point of the seed, and it is worth a test that the row does not
// quietly carry the seed too.
func TestTheSeedIsNotStoredInTheCatalog(t *testing.T) {
	f := DefaultFormat(true)
	member, err := f.Params(store.ChunkerSeed([]byte("a thirty-two byte library key!!!")))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := f.Params(store.PlainSeed())
	if err != nil {
		t.Fatal(err)
	}
	if member.Seed == plain.Seed {
		t.Fatal("the content key made no difference to the seed")
	}
	member.Seed, plain.Seed = [32]byte{}, [32]byte{}
	if member != plain {
		t.Fatal("the seed was not the only thing that differed")
	}
}

// The row is data, and data arrives wrong. A library carrying parameters no
// chunker can be built from must be refused rather than chunked to numbers
// nobody else will reproduce.
func TestParametersThatDescribeNoChunkerAreRefused(t *testing.T) {
	for _, tc := range []struct {
		what   string
		mutate func(*Format)
	}{
		{"unknown algorithm", func(f *Format) { f.Chunker = "fastcdc-gear64/v2" }},
		{"empty algorithm", func(f *Format) { f.Chunker = "" }},
		{"min above target", func(f *Format) { f.MinSize = f.TargetSize + 1 }},
		{"min equal to target", func(f *Format) { f.MinSize = f.TargetSize }},
		{"target above max", func(f *Format) { f.TargetSize = f.MaxSize + 1 }},
		{"zeroed row", func(f *Format) { *f = Format{} }},
	} {
		f := DefaultFormat(false)
		tc.mutate(&f)
		if err := f.Validate(); err == nil {
			t.Errorf("%s: accepted", tc.what)
		}
		if _, err := f.Params(store.PlainSeed()); !errors.Is(err, store.ErrParams) {
			t.Errorf("%s: Params did not report ErrParams: %v", tc.what, err)
		}
	}
}

// A zeroed Format is what a loader produces when it forgets to read the
// columns, and it must not look like a usable library.
func TestAZeroedFormatIsNotAUsableLibrary(t *testing.T) {
	var f Format
	if err := f.Validate(); err == nil {
		t.Fatal("a library with no format at all validated")
	}
}
