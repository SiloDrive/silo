package store

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The test vectors are part of the spec, not part of this package's tests.
// porter-mac's Swift implementation is checked against the same file, and a
// release ships only when both are green against it — so these are
// regenerated deliberately with -update and the diff is reviewed, never
// refreshed to make a failing test pass.
var updateVectors = flag.Bool("update", false, "regenerate testdata/vectors")

const vectorFile = "testdata/vectors/chunker.json"

// checkVectorFile is the whole -update protocol, in one place for all three
// vector files: with the flag it rewrites the file and reports what it wrote,
// without it the build has to reproduce the committed bytes exactly. drift
// says what a difference would mean for the format, because that is the only
// part that differs between the three.
func checkVectorFile(t *testing.T, file string, doc any, drift string) {
	t.Helper()
	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("encoding vectors: %v", err)
	}
	encoded = append(encoded, '\n')

	if *updateVectors {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", file, len(encoded))
		return
	}

	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading vectors: %v (regenerate with -update)", err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("this build no longer reproduces %s.\n"+
			"That is a format change: %s. If it is deliberate, regenerate "+
			"with -update and review the diff.", file, drift)
	}
}

// loadVectors reads a committed vector file back, for the tests that check a
// port could reproduce it from the file alone.
func loadVectors(t *testing.T, file string, doc any) {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, doc); err != nil {
		t.Fatal(err)
	}
}

type vectorDoc struct {
	Format        string                `json:"format"`
	Note          string                `json:"note"`
	Gear          []gearVector          `json:"gear"`
	Chunks        []cutVector           `json:"chunker"`
	ParamsRefused []paramsRefusedVector `json:"params_refused"`
}

type gearVector struct {
	Name  string   `json:"name"`
	Seed  string   `json:"seed"`
	First []string `json:"first_four"`
	Last  string   `json:"last"`
	Hash  string   `json:"table_sha256"`
}

type cutVector struct {
	Name   string       `json:"name"`
	Params paramsVector `json:"params"`
	Input  inputVector  `json:"input"`
	Count  int          `json:"chunk_count"`
	Chunks []chunkEntry `json:"chunks"`
}

// The seed rejection vectors. Described rather than stored, like the KDF and
// identifier ones: what must not happen is a library chunking at all, so there
// is nothing to commit but the parameters that must be refused.
//
// This is the half a port passes every other vector in this file without. Every
// cut vector above is reproducible whether or not an implementation checks that
// the seed belongs to the library it is chunking, and an implementation that
// skips the check produces correct chunks under the wrong seed — which for an
// E2EE library is a content-confirmation oracle the server needs no key to use.
type paramsRefusedVector struct {
	Name string `json:"name"`
	E2EE bool   `json:"e2ee"`
	// ContentKey is what the client holds, empty for one holding none. It is
	// the same key objects.json uses, spelled out so a port can derive the
	// seeds below rather than copy them.
	ContentKey string `json:"content_key_utf8"`
	Seed       string `json:"seed"`
	Why        string `json:"why"`
}

type paramsVector struct {
	Algorithm     string `json:"algorithm"`
	Seed          string `json:"seed"`
	MinSize       int    `json:"min_size"`
	TargetSize    int    `json:"target_size"`
	MaxSize       int    `json:"max_size"`
	Normalization int    `json:"normalization"`
}

type inputVector struct {
	Generator string `json:"generator"`
	Label     string `json:"label,omitempty"`
	Length    int    `json:"length"`
}

type chunkEntry struct {
	Offset int    `json:"offset"`
	Size   int    `json:"size"`
	ID     string `json:"id"`
}

func (i inputVector) bytes() []byte {
	switch i.Generator {
	case "pseudorandom":
		return pseudoRandom(i.Label, i.Length)
	case "zeros":
		return make([]byte, i.Length)
	default:
		panic("store: unknown vector generator " + i.Generator)
	}
}

func (p paramsVector) params(t *testing.T) Params {
	t.Helper()
	seed, err := hex.DecodeString(p.Seed)
	if err != nil || len(seed) != 32 {
		t.Fatalf("vector seed %q is not 32 hex bytes", p.Seed)
	}
	var s [32]byte
	copy(s[:], seed)
	return Params{
		Algorithm:     p.Algorithm,
		Seed:          s,
		MinSize:       p.MinSize,
		TargetSize:    p.TargetSize,
		MaxSize:       p.MaxSize,
		Normalization: p.Normalization,
	}
}

// seedOf is ChunkerSeed as a slice, for the vector table's hex rendering.
func seedOf(ck []byte) []byte {
	s := ChunkerSeed(ck)
	return s[:]
}

func vectorParams(seed [32]byte) paramsVector {
	p := DefaultParams(seed)
	return paramsVector{
		Algorithm:     p.Algorithm,
		Seed:          hex.EncodeToString(seed[:]),
		MinSize:       p.MinSize,
		TargetSize:    p.TargetSize,
		MaxSize:       p.MaxSize,
		Normalization: p.Normalization,
	}
}

func buildVectors(t *testing.T) vectorDoc {
	t.Helper()
	doc := vectorDoc{
		Format: "silo/store/vectors/v1",
		Note: "Generated by go test ./store -run TestVectors -update. " +
			"Inputs are described, not stored: generator \"pseudorandom\" is the " +
			"concatenation of SHA-256(label || uint64le(i)) for i = 0, 1, 2, ... " +
			"truncated to length; generator \"zeros\" is that many zero bytes.",
	}

	seeds := []struct {
		name string
		seed [32]byte
	}{
		{"plain", PlainSeed()},
		{"e2ee", ChunkerSeed(vectorCK)},
	}
	for _, s := range seeds {
		g := gearTable(s.seed)
		var raw bytes.Buffer
		for _, v := range g {
			fmt.Fprintf(&raw, "%016x", v)
		}
		doc.Gear = append(doc.Gear, gearVector{
			Name: s.name,
			Seed: hex.EncodeToString(s.seed[:]),
			First: []string{
				fmt.Sprintf("%016x", g[0]), fmt.Sprintf("%016x", g[1]),
				fmt.Sprintf("%016x", g[2]), fmt.Sprintf("%016x", g[3]),
			},
			Last: fmt.Sprintf("%016x", g[255]),
			Hash: ObjectID(raw.Bytes()).String(),
		})
	}

	cases := []struct {
		name   string
		params paramsVector
		input  inputVector
	}{
		{"empty", vectorParams(PlainSeed()), inputVector{"pseudorandom", "silo/vector/empty", 0}},
		{"inline-sized", vectorParams(PlainSeed()), inputVector{"pseudorandom", "silo/vector/small", 1000}},
		{"one-chunk", vectorParams(PlainSeed()), inputVector{"pseudorandom", "silo/vector/one", 300000}},
		{"pseudorandom-16MiB", vectorParams(PlainSeed()), inputVector{"pseudorandom", "silo/vector/random", 16 << 20}},
		{"zeros-16MiB", vectorParams(PlainSeed()), inputVector{"zeros", "", 16 << 20}},
		{"pseudorandom-16MiB-e2ee", vectorParams(ChunkerSeed(vectorCK)), inputVector{"pseudorandom", "silo/vector/random", 16 << 20}},
	}
	for _, c := range cases {
		v := cutVector{Name: c.name, Params: c.params, Input: c.input}
		for _, ch := range chunkAll(t, c.params.params(t), c.input.bytes()) {
			v.Chunks = append(v.Chunks, chunkEntry{
				Offset: int(ch.Offset),
				Size:   len(ch.Data),
				ID:     ChunkID(ch.Data).String(),
			})
		}
		v.Count = len(v.Chunks)
		doc.Chunks = append(doc.Chunks, v)
	}

	otherCK := []byte("a second library's content key!!!")
	plain, derived := PlainSeed(), ChunkerSeed(vectorCK)
	doc.ParamsRefused = []paramsRefusedVector{
		{"e2ee-under-the-plain-seed", true, "", hex.EncodeToString(plain[:]),
			"the one the spec states in bold: seal_hash becomes a CK-free content-confirmation " +
				"oracle, and every other vector here still reproduces"},
		{"e2ee-with-key-under-the-plain-seed", true, string(vectorCK), hex.EncodeToString(plain[:]),
			"the same library with the key in hand; holding CK is not permission to ignore the seed"},
		{"e2ee-under-another-librarys-seed", true, string(vectorCK),
			hex.EncodeToString(seedOf(otherCK)),
			"a real derived seed, derived from the wrong key — the case a client hits by " +
				"reusing a Config across libraries"},
		{"plain-under-a-derived-seed", false, "", hex.EncodeToString(derived[:]),
			"the other direction: a plain library off the published seed dedups against nothing"},
		{"plain-with-a-content-key", false, string(vectorCK), hex.EncodeToString(plain[:]),
			"the seed is right and the pair is not; a plain library has no content key"},
	}
	return doc
}

func TestVectors(t *testing.T) {
	checkVectorFile(t, vectorFile, buildVectors(t),
		"every existing library's ids move, and porter-mac stops interoperating")
}

// What a port has to pass on top of reproducing every cut above: refuse every
// parameter set the file says must be refused. Nothing else in this file
// notices an implementation that omits the seed check — the cuts reproduce
// either way, which is exactly why this is committed separately.
func TestTheCommittedParameterRefusalsAreRefused(t *testing.T) {
	var doc vectorDoc
	loadVectors(t, vectorFile, &doc)
	if len(doc.ParamsRefused) == 0 {
		t.Fatal("the file commits no refused parameters")
	}
	for _, v := range doc.ParamsRefused {
		raw, err := hex.DecodeString(v.Seed)
		if err != nil || len(raw) != 32 {
			t.Fatalf("%s: seed is not 32 hex-encoded bytes", v.Name)
		}
		p := DefaultParams([32]byte(raw))
		var ck []byte
		if v.ContentKey != "" {
			ck = []byte(v.ContentKey)
		}
		if err := p.ValidateFor(v.E2EE, ck); !errors.Is(err, ErrParams) {
			t.Errorf("%s: accepted, and the vector says it must not (%s): %v", v.Name, v.Why, err)
		}
		// The shape rules pass on all of these, which is the point: Validate
		// cannot see what is wrong here.
		if err := p.Validate(); err != nil {
			t.Errorf("%s: refused by the shape check, so it proves nothing about the seed rule: %v", v.Name, err)
		}
	}
}
