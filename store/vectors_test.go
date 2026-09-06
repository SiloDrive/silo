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
// silo-drive's Swift implementation is checked against the same file, and a
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
	Masks         []maskVector          `json:"masks"`
	Chunks        []cutVector           `json:"chunker"`
	ParamsInvalid []paramsInvalidVector `json:"params_invalid"`
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
	Name string `json:"name"`
	// Why says what this case pins that the others do not, because a port
	// that fails one row needs to know which rule it just broke.
	Why    string       `json:"why"`
	Params paramsVector `json:"params"`
	Input  inputVector  `json:"input"`
	Count  int          `json:"chunk_count"`
	Chunks []chunkEntry `json:"chunks"`
}

// maskVector pins the two masks a parameter set derives, separately from any
// cut. The derivation is the one rule in this format that generalises rather
// than being a constant to transcribe — the paper publishes hand-picked masks
// for one target size and this does not — so it is the rule a port is most
// likely to shortcut into "target_bits = 20, mask_s = the top 22 bits". Those
// constants pass every default-parameter cut vector in this file and mis-cut
// every library created with anything else.
type maskVector struct {
	Name          string `json:"name"`
	MinSize       int    `json:"min_size"`
	TargetSize    int    `json:"target_size"`
	MaxSize       int    `json:"max_size"`
	Normalization int    `json:"normalization"`
	TargetBits    int    `json:"target_bits"`
	MaskS         string `json:"mask_s"`
	MaskL         string `json:"mask_l"`
	Why           string `json:"why"`
}

// paramsInvalidVector is a parameter set no chunker may be built from, for the
// same reason params_refused exists: the bounds are the format's, not one
// implementation's, and a port that accepts more accepts libraries no other
// client can read. These fail the shape check, where params_refused passes it.
type paramsInvalidVector struct {
	Name   string       `json:"name"`
	Params paramsVector `json:"params"`
	Why    string       `json:"why"`
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

// customParams is vectorParams for the cases that depart from the defaults.
// Every one of them exists because DefaultParams is not the format: a library
// is created with whatever its server chose, and those numbers travel to the
// client in the library listing.
func customParams(seed [32]byte, min, target, max, norm int) paramsVector {
	p := vectorParams(seed)
	p.MinSize, p.TargetSize, p.MaxSize, p.Normalization = min, target, max, norm
	return p
}

// maskRow renders one parameter set's derived masks.
func maskRow(name string, p paramsVector, why string) maskVector {
	tb := targetBits(p.TargetSize)
	return maskVector{
		Name:          name,
		MinSize:       p.MinSize,
		TargetSize:    p.TargetSize,
		MaxSize:       p.MaxSize,
		Normalization: p.Normalization,
		TargetBits:    tb,
		MaskS:         fmt.Sprintf("%016x", topMask(tb+p.Normalization)),
		MaskL:         fmt.Sprintf("%016x", topMask(tb-p.Normalization)),
		Why:           why,
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

	plainDefaults := vectorParams(PlainSeed())
	doc.Masks = []maskVector{
		maskRow("default", plainDefaults,
			"the parameters every library is created with today: floor(log2(1048576)) = 20, "+
				"so the strict mask is the top 22 bits and the lax one the top 18"),
		maskRow("normalization-0", customParams(PlainSeed(), 262144, 1<<20, 4<<20, 0),
			"level 0 collapses the two loops into one: mask_s == mask_l, and a port that "+
				"special-cases the second loop away must still cut in the same places"),
		maskRow("normalization-3", customParams(PlainSeed(), 262144, 1<<20, 4<<20, 3),
			"the far edge of the permitted range, 0..3"),
		maskRow("target-not-a-power-of-two", customParams(PlainSeed(), 196608, 786432, 3145728, 2),
			"the row that separates floor(log2(t)) from round(log2(t)): 786432 has bit length "+
				"20, so target_bits is 19, while a port taking log2 in floating point and "+
				"rounding gets 20 and every mask off by one"),
		maskRow("target-just-over-a-power-of-two", customParams(PlainSeed(), 262144, 1572864, 4<<20, 2),
			"the same trap in the other direction: 1.5 MiB rounds up to 21 and floors to 20, "+
				"and the masks here are the default's — so only the switch point moves"),
		maskRow("small-target", customParams(PlainSeed(), 64, 1024, 8192, 2),
			"a parameter set nothing like the default, for a port that hardcoded the default"),
		maskRow("narrowest-permitted", customParams(PlainSeed(), 64, 65, 4096, 3),
			"the smallest target the bounds admit above the 64-byte minimum: target_bits 6, "+
				"so the lax mask is the top 3 bits and cuts land almost immediately"),
		maskRow("largest-permitted", customParams(PlainSeed(), 64, 1<<30, 1<<30, 3),
			"target_bits 30 and a strict mask of the top 33 bits: the row that catches a port "+
				"deriving masks in a 32-bit word"),
	}

	cases := []struct {
		name   string
		why    string
		params paramsVector
		input  inputVector
	}{
		{"empty", "a zero-length stream yields no chunks at all, not one empty chunk",
			plainDefaults, inputVector{"pseudorandom", "silo/vector/empty", 0}},
		{"inline-sized", "under the inline threshold, so a real client never chunks it; one chunk if it did",
			plainDefaults, inputVector{"pseudorandom", "silo/vector/small", 1000}},
		{"one-chunk", "shorter than min_size, so the cut loop never runs and no byte is hashed",
			plainDefaults, inputVector{"pseudorandom", "silo/vector/one", 300000}},
		{"pseudorandom-16MiB", "the ordinary case: thirteen cuts under the default parameters",
			plainDefaults, inputVector{"pseudorandom", "silo/vector/random", 16 << 20}},
		{"zeros-16MiB", "content with no boundary in it, forced to cut at max_size four times",
			plainDefaults, inputVector{"zeros", "", 16 << 20}},
		{"pseudorandom-16MiB-e2ee", "the same bytes under a derived seed: every boundary moves",
			vectorParams(ChunkerSeed(vectorCK)), inputVector{"pseudorandom", "silo/vector/random", 16 << 20}},

		{"min-size-exactly", "a stream of exactly min_size: n <= min_size returns n, and nothing is hashed",
			plainDefaults, inputVector{"pseudorandom", "silo/vector/min", 262144}},
		{"min-size-minus-one", "one byte below the minimum, still one chunk",
			plainDefaults, inputVector{"pseudorandom", "silo/vector/min", 262143}},
		{"min-size-plus-one", "one byte above it, so exactly one byte is hashed — data[min_size-1] — " +
			"and misses the mask, as a random byte does with probability 1 - 2^-22",
			plainDefaults, inputVector{"pseudorandom", "silo/vector/min", 262145}},
		{"max-size-exactly", "a stream ending exactly on the forced cut: one chunk, no empty second one",
			plainDefaults, inputVector{"zeros", "", 4 << 20}},
		{"max-size-plus-one", "one byte past it: the forced cut, then a final chunk of one byte — " +
			"the last chunk is the only one allowed below min_size",
			plainDefaults, inputVector{"zeros", "", (4 << 20) + 1}},

		{"normalization-0", "the same bytes as pseudorandom-16MiB's first half with one mask instead of two",
			customParams(PlainSeed(), 262144, 1<<20, 4<<20, 0), inputVector{"pseudorandom", "silo/vector/random", 8 << 20}},
		{"normalization-3", "and at the top of the permitted range, where sizes crowd hard onto the target",
			customParams(PlainSeed(), 262144, 1<<20, 4<<20, 3), inputVector{"pseudorandom", "silo/vector/random", 8 << 20}},
		{"target-not-a-power-of-two", "min, target and max all off a power of two, and target_bits 19 " +
			"rather than 20: a port that rounds log2 instead of flooring it fails here and nowhere else",
			customParams(PlainSeed(), 196608, 786432, 3145728, 2), inputVector{"pseudorandom", "silo/vector/random", 8 << 20}},
		{"small-parameters", "a 1 KiB target over the same stream: nothing about the default sizes is a constant",
			customParams(PlainSeed(), 64, 1024, 8192, 2), inputVector{"pseudorandom", "silo/vector/random", 32768}},
		{"min-size-cut", "the only case here that catches hashing from data[min_size] instead of " +
			"data[min_size-1]. The gear hash forgets where it started after 64 bytes, so the two " +
			"differ only when a cut lands inside the first 64 hashed bytes — impossible to observe " +
			"at the default min_size, and routine at 64. This label was chosen so the output holds " +
			"both a chunk of exactly min_size and one of exactly max_size",
			customParams(PlainSeed(), 64, 256, 1024, 0), inputVector{"pseudorandom", "silo/vector/min-cut/44", 4096}},
	}
	for _, c := range cases {
		v := cutVector{Name: c.name, Why: c.why, Params: c.params, Input: c.input}
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

	badParams := func(mutate func(*paramsVector)) paramsVector {
		p := vectorParams(PlainSeed())
		mutate(&p)
		return p
	}
	doc.ParamsInvalid = []paramsInvalidVector{
		{"unknown-algorithm", badParams(func(p *paramsVector) { p.Algorithm = "fastcdc-gear64/v2" }),
			"a profile this format does not define; a client that chunks anyway computes ids " +
				"under a rule the library was not created with"},
		{"min-size-below-64", badParams(func(p *paramsVector) { p.MinSize = 63 }),
			"the floor exists because the gear hash's window is 64 bytes: below it the first " +
				"hashed byte still carries the initial zero state"},
		{"min-size-equal-to-target", badParams(func(p *paramsVector) { p.MinSize = p.TargetSize }),
			"min_size must be strictly below the target, or the strict loop has no bytes to run over"},
		{"target-above-max", badParams(func(p *paramsVector) { p.MaxSize = p.TargetSize - 1 }),
			"the lax loop would start after the forced cut, so it never runs"},
		{"max-size-above-1-GiB", badParams(func(p *paramsVector) { p.MaxSize = 2 << 30 }),
			"the ceiling a decoder sizes its buffer from; a stranger's number must not"},
		{"normalization-above-3", badParams(func(p *paramsVector) { p.Normalization = 4 }),
			"the level is 0..3; 4 is a mask of the top 24 bits, which no library is created with"},
		{"normalization-negative", badParams(func(p *paramsVector) { p.Normalization = -1 }),
			"the other end of the same bound, and the one a port using an unsigned type " +
				"silently reads as a very large level"},
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
		"every existing library's ids move, and silo-drive stops interoperating")
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

// The check a port has to pass, done the way a port does it: read the file,
// rebuild every described input, and cut it. TestVectors above asks whether
// this build still writes the same file; this asks whether the file alone is
// enough to reproduce, which is the question porter-mac is actually holding.
func TestTheCommittedCutsAreReproducibleFromTheFile(t *testing.T) {
	var doc vectorDoc
	loadVectors(t, vectorFile, &doc)
	if len(doc.Chunks) == 0 {
		t.Fatal("the file commits no cut vectors")
	}
	for _, v := range doc.Chunks {
		t.Run(v.Name, func(t *testing.T) {
			p := v.Params.params(t)
			if err := p.Validate(); err != nil {
				t.Fatalf("the vector's own parameters do not validate: %v", err)
			}
			got := chunkAll(t, p, v.Input.bytes())
			if len(got) != v.Count || len(got) != len(v.Chunks) {
				t.Fatalf("cut into %d chunks, the vector says %d (%d listed)",
					len(got), v.Count, len(v.Chunks))
			}
			off := 0
			for i, ch := range got {
				want := v.Chunks[i]
				if int(ch.Offset) != want.Offset || len(ch.Data) != want.Size ||
					ChunkID(ch.Data).String() != want.ID {
					t.Fatalf("chunk %d is (%d, %d, %s), the vector says (%d, %d, %s)",
						i, ch.Offset, len(ch.Data), ChunkID(ch.Data), want.Offset, want.Size, want.ID)
				}
				// The invariants a reader of the file should be able to assume,
				// checked against the file rather than against this build: the
				// chunks tile the input, and only the last may fall short of
				// the minimum.
				if want.Offset != off {
					t.Fatalf("chunk %d starts at %d, %d bytes from where the previous one ended", i, want.Offset, want.Offset-off)
				}
				off += want.Size
				if i < len(got)-1 && (want.Size < p.MinSize || want.Size > p.MaxSize) {
					t.Fatalf("chunk %d is %d bytes, outside [%d, %d]", i, want.Size, p.MinSize, p.MaxSize)
				}
			}
			if off != v.Input.Length {
				t.Fatalf("the chunks cover %d bytes of a %d-byte input", off, v.Input.Length)
			}
		})
	}
}

// The masks, checked as a derivation rather than as a table. A port that
// transcribed the default's two constants passes every cut vector that uses
// the default parameters and fails here on the first row that does not.
func TestTheCommittedMasksAreDerivedFromTheParameters(t *testing.T) {
	var doc vectorDoc
	loadVectors(t, vectorFile, &doc)
	if len(doc.Masks) == 0 {
		t.Fatal("the file commits no masks")
	}
	for _, v := range doc.Masks {
		t.Run(v.Name, func(t *testing.T) {
			p := Params{
				Algorithm: ChunkerAlgorithm, Seed: PlainSeed(),
				MinSize: v.MinSize, TargetSize: v.TargetSize,
				MaxSize: v.MaxSize, Normalization: v.Normalization,
			}
			if err := p.Validate(); err != nil {
				t.Fatalf("the mask row's own parameters do not validate: %v", err)
			}
			if got := targetBits(v.TargetSize); got != v.TargetBits {
				t.Fatalf("target_bits %d for a target of %d, the vector says %d", got, v.TargetSize, v.TargetBits)
			}
			// Against the chunker itself, not against a second copy of the
			// formula: what a boundary depends on is the field, not the rule
			// as restated in this test.
			c, err := NewChunker(p, bytes.NewReader(nil))
			if err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprintf("%016x", c.maskS); got != v.MaskS {
				t.Errorf("mask_s %s, the vector says %s", got, v.MaskS)
			}
			if got := fmt.Sprintf("%016x", c.maskL); got != v.MaskL {
				t.Errorf("mask_l %s, the vector says %s", got, v.MaskL)
			}
			if v.Normalization == 0 && v.MaskS != v.MaskL {
				t.Errorf("normalisation 0 must leave one mask, got %s and %s", v.MaskS, v.MaskL)
			}
		})
	}
}

// The shape refusals, the counterpart to params_refused: these fail the check
// that params_refused passes. A port that accepts them accepts libraries no
// other client can read.
func TestTheCommittedInvalidParametersAreRefused(t *testing.T) {
	var doc vectorDoc
	loadVectors(t, vectorFile, &doc)
	if len(doc.ParamsInvalid) == 0 {
		t.Fatal("the file commits no invalid parameters")
	}
	for _, v := range doc.ParamsInvalid {
		p := v.Params.params(t)
		if err := p.Validate(); !errors.Is(err, ErrParams) {
			t.Errorf("%s: accepted, and the vector says it must not (%s): %v", v.Name, v.Why, err)
		}
		if _, err := NewChunker(p, bytes.NewReader(nil)); !errors.Is(err, ErrParams) {
			t.Errorf("%s: a chunker was built from it", v.Name)
		}
	}
}

// Both size edges have to appear in the committed output of some case, or the
// two rules that only show up at a boundary are pinned by prose alone.
//
// The min_size edge is the sharp one. Hashing starts at data[min_size-1], and
// the gear hash forgets its starting point after 64 bytes, so an off-by-one
// there is invisible unless a chunk comes out within 64 bytes of the minimum —
// which at a 256 KiB min_size never happens in any input anyone will generate.
// If a regeneration ever drops the small-parameter case, this says so.
func TestTheCommittedCutsPinBothSizeEdges(t *testing.T) {
	var doc vectorDoc
	loadVectors(t, vectorFile, &doc)
	var atMin, atMax string
	for _, v := range doc.Chunks {
		for i, ch := range v.Chunks {
			last := i == len(v.Chunks)-1
			if ch.Size == v.Params.MinSize && !last {
				atMin = v.Name
			}
			if ch.Size == v.Params.MaxSize {
				atMax = v.Name
			}
		}
	}
	if atMin == "" {
		t.Error("no committed case cuts a chunk of exactly min_size, so nothing here " +
			"catches a port that begins hashing at data[min_size]")
	}
	if atMax == "" {
		t.Error("no committed case cuts a chunk of exactly max_size, so nothing here catches a port that never forces a cut")
	}
	t.Logf("min_size edge pinned by %q, max_size edge by %q", atMin, atMax)
}
