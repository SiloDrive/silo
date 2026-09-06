package store

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"strconv"
	"testing"
)

const objectVectorFile = "testdata/vectors/objects.json"

// vectorCK is the published stand-in for a library content key. A real one is
// 32 random bytes; this one is in the spec so every derived value below is
// reproducible.
//
// It is 32 bytes because a content key is, and TestTheVectorContentKeyIsOne
// holds it there. The first stand-in was 33 — a readable sentence nobody
// counted — and nothing on this path counts either: SealChunk, NameKey and the
// EncodeSealed pair check only that a key is non-empty, ChunkerSeed checks
// nothing, and HKDF absorbs any width. It derived perfectly stable ids from a
// key the format says cannot exist, which a port typing its key as 32 bytes
// could not load at all.
var vectorCK = []byte("silo test content key, 32 bytes!")

type objectVectorDoc struct {
	Format         string                     `json:"format"`
	Note           string                     `json:"note"`
	ContentKey     string                     `json:"content_key_utf8"`
	Chunks         []chunkSealVector          `json:"chunk_seal"`
	Manifests      []manifestVector           `json:"manifest"`
	Directories    []objectVector             `json:"directory"`
	Commits        []objectVector             `json:"commit"`
	Streams        []chunkStreamVector        `json:"chunk_stream"`
	StreamsRefused []chunkStreamRefusedVector `json:"chunk_stream_refused"`
}

// The chunk stream: the framing POST chunks/fetch answers in, pinned here
// because it is the one wire format in this package that had no vectors and
// the one a port meets on the download half of a sync.
//
// A frame is id (32) ‖ status (1) ‖ length (4, big-endian) ‖ bytes. The two
// things a port gets wrong silently are both committed below: the length is
// big-endian where every other count in this format is a varint, and the bytes
// are the chunk **as stored**, so in an E2EE library they are the sealed frame
// and the id is the sealed id — a decoder hashing what it thinks is plaintext
// rejects every frame it is sent.
type chunkStreamVector struct {
	Name   string             `json:"name"`
	Why    string             `json:"why"`
	Frames []chunkFrameVector `json:"frames"`
	Len    int                `json:"stream_len"`
	SHA256 string             `json:"stream_sha256"`
	Stream string             `json:"stream_hex,omitempty"`
}

// chunkFrameVector describes one frame by its content rather than storing it:
// the payload is the described input, sealed under the content key when the
// library is. id is what the frame carries and what a decoder must hash to.
type chunkFrameVector struct {
	Present bool        `json:"present"`
	Sealed  bool        `json:"sealed"`
	Input   inputVector `json:"input"`
	ID      string      `json:"id"`
	Length  int         `json:"length"`
}

// chunkStreamRefusedVector is a body a decoder must refuse. This is the half a
// port passes every stream above without: every valid stream decodes whether
// or not an implementation checks the length against what remains, the status
// byte against the two it knows, or the bytes against the id they arrived
// under — and that last check is what makes a chunk from a cache of uncertain
// provenance safe to use at all.
type chunkStreamRefusedVector struct {
	Name   string `json:"name"`
	Stream string `json:"stream_hex"`
	Why    string `json:"why"`
}

// objectVector pins a directory or commit whose fields are literals rather
// than generated: the description is in the spec, and the bytes are here.
type objectVector struct {
	Name       string `json:"name"`
	Sealed     bool   `json:"sealed"`
	EncodedLen int    `json:"encoded_len"`
	Encoded    string `json:"encoded_hex"`
	ObjectID   string `json:"object_id"`
}

type chunkSealVector struct {
	Name          string      `json:"name"`
	Input         inputVector `json:"input"`
	PlaintextHash string      `json:"plaintext_hash"`
	FrameLen      int         `json:"frame_len"`
	FrameSHA256   string      `json:"frame_sha256"`
	ID            string      `json:"id"`
}

type manifestVector struct {
	Name       string      `json:"name"`
	Sealed     bool        `json:"sealed"`
	Input      inputVector `json:"input"`
	ChunkCount int         `json:"chunk_count"`
	EncodedLen int         `json:"encoded_len"`
	Encoded    string      `json:"encoded_hex,omitempty"`
	ObjectID   string      `json:"object_id"`
}

// vectorManifest builds the manifest a client would write for the described
// input, in the named library type — chunking under that type's seed, and
// encrypting first when sealed.
func vectorManifest(t *testing.T, in inputVector, sealed bool) *Manifest {
	t.Helper()
	data := in.bytes()
	m := &Manifest{FileSize: int64(len(data))}
	if Inlined(m.FileSize) {
		m.Inline = data
		return m
	}
	seed := PlainSeed()
	if sealed {
		seed = ChunkerSeed(vectorCK)
	}
	for _, ch := range chunkAll(t, DefaultParams(seed), data) {
		ref := ChunkRef{ID: ChunkID(ch.Data), Size: int64(len(ch.Data))}
		if sealed {
			sc, err := SealChunk(vectorCK, ch.Data)
			if err != nil {
				t.Fatal(err)
			}
			ref.ID, ref.PlaintextHash = sc.ID, sc.PlaintextHash
		}
		m.Chunks = append(m.Chunks, ref)
	}
	return m
}

// vectorStream builds the body a server writes for these frames, filling in
// each frame's id and stored length. The generator and the from-the-file check
// both go through it, so the committed hex and the check cannot drift apart.
func vectorStream(t *testing.T, frames []chunkFrameVector) ([]byte, []chunkFrameVector) {
	t.Helper()
	var buf bytes.Buffer
	out := make([]chunkFrameVector, len(frames))
	for i, f := range frames {
		stored := f.Input.bytes()
		id := ChunkID(stored)
		if f.Sealed {
			sc, err := SealChunk(vectorCK, stored)
			if err != nil {
				t.Fatal(err)
			}
			stored, id = sc.Frame, sc.ID
		}
		f.ID, f.Length = id.String(), 0
		if f.Present {
			f.Length = len(stored)
			if err := WriteChunkFrame(&buf, id, stored); err != nil {
				t.Fatal(err)
			}
		} else if err := WriteAbsentChunkFrame(&buf, id); err != nil {
			t.Fatal(err)
		}
		out[i] = f
	}
	// The terminator, which is what makes the body self-delimiting. Without it
	// a server that dies half way through emits whole frames and stops, and
	// the short body decodes cleanly under a status committed before the first
	// byte moved.
	if err := WriteChunkStreamEnd(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), out
}

// rawFrame assembles a frame header by hand, for the bodies below that no
// writer in this package will produce.
func rawFrame(id ID, status byte, length uint32, payload []byte) []byte {
	b := make([]byte, ChunkFrameHeaderSize, ChunkFrameHeaderSize+len(payload))
	copy(b[:IDSize], id[:])
	b[IDSize] = status
	binary.BigEndian.PutUint32(b[IDSize+1:], length)
	return append(b, payload...)
}

func buildObjectVectors(t *testing.T) objectVectorDoc {
	t.Helper()
	doc := objectVectorDoc{
		Format: "silo/store/vectors/v1",
		Note: "Generated by go test ./store -run TestObjectVectors -update. Inputs are " +
			"described as in chunker.json. content_key_utf8 is the library content key as " +
			"literal ASCII. encoded_hex is present where the object is small enough to " +
			"compare byte for byte; where it is not, object_id serves. chunk_stream is " +
			"the framing POST chunks/fetch answers in: id (32) || status (1, 0 present " +
			"and 1 absent) || length (4, big-endian) || the chunk as stored.",
		ContentKey: string(vectorCK),
	}

	for _, in := range []inputVector{
		{"pseudorandom", "silo/vector/empty", 0},
		{"pseudorandom", "silo/vector/onebyte", 1},
		{"pseudorandom", "silo/vector/small", 1000},
		{"zeros", "", 1 << 20},
		{"pseudorandom", "silo/vector/random", 1 << 20},
	} {
		plain := in.bytes()
		sc, err := SealChunk(vectorCK, plain)
		if err != nil {
			t.Fatal(err)
		}
		doc.Chunks = append(doc.Chunks, chunkSealVector{
			Name:          in.Generator + "-" + strconv.Itoa(in.Length),
			Input:         in,
			PlaintextHash: sc.PlaintextHash.String(),
			FrameLen:      len(sc.Frame),
			FrameSHA256:   ObjectID(sc.Frame).String(),
			ID:            sc.ID.String(),
		})
	}

	for _, c := range []struct {
		name   string
		in     inputVector
		sealed bool
	}{
		{"empty-plain", inputVector{"pseudorandom", "silo/vector/empty", 0}, false},
		{"empty-sealed", inputVector{"pseudorandom", "silo/vector/empty", 0}, true},
		{"inline-plain", inputVector{"pseudorandom", "silo/vector/small", 1000}, false},
		{"inline-sealed", inputVector{"pseudorandom", "silo/vector/small", 1000}, true},
		{"threshold-minus-one-plain", inputVector{"pseudorandom", "silo/vector/edge", InlineThreshold - 1}, false},
		{"threshold-plain", inputVector{"pseudorandom", "silo/vector/edge", InlineThreshold}, false},
		{"threshold-sealed", inputVector{"pseudorandom", "silo/vector/edge", InlineThreshold}, true},
		{"chunked-16MiB-plain", inputVector{"pseudorandom", "silo/vector/random", 16 << 20}, false},
		{"chunked-16MiB-sealed", inputVector{"pseudorandom", "silo/vector/random", 16 << 20}, true},
	} {
		m := vectorManifest(t, c.in, c.sealed)
		var encoded []byte
		var err error
		if c.sealed {
			encoded, err = m.EncodeSealed(vectorCK)
		} else {
			encoded, err = m.Encode()
		}
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		v := manifestVector{
			Name:       c.name,
			Sealed:     c.sealed,
			Input:      c.in,
			ChunkCount: len(m.Chunks),
			EncodedLen: len(encoded),
			ObjectID:   ObjectID(encoded).String(),
		}
		if len(encoded) <= 4096 {
			v.Encoded = hex.EncodeToString(encoded)
		}
		doc.Manifests = append(doc.Manifests, v)
	}
	for _, sealed := range []bool{false, true} {
		suffix := "-plain"
		if sealed {
			suffix = "-sealed"
		}
		doc.Directories = append(doc.Directories,
			encodedVector(t, "empty"+suffix, sealed, encodeDir(&Directory{
				Salt: [DirSaltSize]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
			})),
			encodedVector(t, "three-entries"+suffix, sealed, encodeDir(vectorDir())),
		)
		doc.Commits = append(doc.Commits,
			encodedVector(t, "root-only"+suffix, sealed, encodeCommit(&Commit{Root: id(7)})),
			encodedVector(t, "two-parents"+suffix, sealed, encodeCommit(vectorCommit())),
		)
	}

	onebyte := inputVector{"pseudorandom", "silo/vector/onebyte", 1}
	small := inputVector{"pseudorandom", "silo/vector/small", 1000}
	absentee := inputVector{"pseudorandom", "silo/vector/one", 300000}
	for _, c := range []struct {
		name   string
		why    string
		frames []chunkFrameVector
	}{
		{"empty", "no chunk frames at all, so the body is the terminator alone. " +
			"A stream of no chunks is still 37 bytes: zero bytes would be a " +
			"truncated body, and the two must not look alike", nil},
		{"one-present", "the ordinary frame, with a payload long enough that a " +
			"little-endian length reader reads a wildly different number",
			[]chunkFrameVector{{Present: true, Input: small}}},
		{"one-absent", "the store does not hold this id. The frame is a header and " +
			"nothing else, and the id is the one this input hashes to — described " +
			"rather than stored so a port derives it",
			[]chunkFrameVector{{Input: absentee}}},
		{"present-absent-present", "the mixed case a real fetch returns, and the one " +
			"that catches a decoder advancing by a fixed stride: the absent frame in " +
			"the middle has no payload to skip",
			[]chunkFrameVector{
				{Present: true, Input: onebyte},
				{Input: absentee},
				{Present: true, Input: small},
			}},
		{"sealed", "an E2EE library's chunk on the wire. The bytes are the sealed " +
			"frame, the id is the sealed id, and the plaintext hash appears nowhere " +
			"in the stream — a decoder that hashes plaintext rejects every frame",
			[]chunkFrameVector{{Present: true, Sealed: true, Input: small}}},
	} {
		body, frames := vectorStream(t, c.frames)
		v := chunkStreamVector{
			Name: c.name, Why: c.why, Frames: frames,
			Len: len(body), SHA256: ObjectID(body).String(),
		}
		if len(body) <= 4096 {
			v.Stream = hex.EncodeToString(body)
		}
		doc.Streams = append(doc.Streams, v)
	}

	payload := []byte("silo")
	wrongID := ChunkID(payload)
	wrongID[IDSize-1] ^= 1
	doc.StreamsRefused = []chunkStreamRefusedVector{
		{"ends-mid-header", hex.EncodeToString(rawFrame(id(0x11), 1, 0, nil)[:ChunkFrameHeaderSize-1]),
			"one byte short of a header. The header is fixed-width so a reader can " +
				"take it without a loop, which is also why a short one is unambiguous"},
		{"ends-mid-payload", hex.EncodeToString(rawFrame(ChunkID(payload), 0, 1000, payload)),
			"declares 1000 bytes and carries four. A decoder that trusts the length " +
				"hands its caller a truncated chunk, or reads past its buffer"},
		{"length-over-the-limit", hex.EncodeToString(rawFrame(id(0x22), 0, 1<<27, nil)),
			"128 MiB, over the 64 MiB frame bound. The bound exists so an allocation " +
				"is sized from the format and not from the number a stranger sent"},
		{"absent-with-a-length", hex.EncodeToString(rawFrame(id(0x33), 1, 5, nil)),
			"absence is a status byte, not a length of zero, and the two must not " +
				"disagree — a reader inferring absence from the length reads this as present"},
		{"unknown-status", hex.EncodeToString(rawFrame(id(0x44), 3, 0, nil)),
			"a status this version does not define — 0, 1 and 2 are present, absent " +
				"and the terminator. A later version may define more, and a decoder " +
				"that treats anything non-zero as absent would silently lose those bytes"},
		{"ends-without-a-terminator", hex.EncodeToString(rawFrame(id(0x55), 1, 0, nil)),
			"one complete, well-formed frame and then the body stops. This is the " +
				"shape a server cut off mid-response produces, and it is the reason " +
				"the terminator exists: nothing in a run of whole frames says whether " +
				"the count was the intended one, so a decoder that stops at the end " +
				"of the buffer reports a short answer as a complete one"},
		{"id-does-not-match-the-bytes", hex.EncodeToString(rawFrame(wrongID, 0, uint32(len(payload)), payload)),
			"the valid frame for the four bytes \"silo\" with one bit flipped in its id. " +
				"Verifying on arrival is what makes a chunk from a cache of uncertain " +
				"provenance, a peer, or a mirror legal to use at all"},
	}
	return doc
}

// vectorDir and vectorCommit are the fixed structures the directory and
// commit vectors encode. Child ids are byte-repeated so a port can type them
// out; names, modes and timestamps are literals.
func vectorDir() *Directory {
	return &Directory{
		Salt: [DirSaltSize]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
		Entries: []DirEntry{
			{ChildID: id(0xaa), Type: NodeFile, Name: []byte("report.pdf"), Mtime: 1700000000, Mode: 0o644},
			{ChildID: id(0xbb), Type: NodeDir, Name: []byte("photos"), Mtime: 1600000000, Mode: 0o755},
			{ChildID: id(0xcc), Type: NodeSymlink, Name: []byte("current"), Mtime: 1650000000, Mode: 0o777},
		},
	}
}

func vectorCommit() *Commit {
	return &Commit{
		Root:      id(0x42),
		Parents:   []ID{id(0x01), id(0x02)},
		CreatedAt: 1700000000,
		Author:    "vector@silo.invalid",
		Message:   "a commit for the vectors",
	}
}

func encodedVector(t *testing.T, name string, sealed bool, encode func(bool) ([]byte, error)) objectVector {
	t.Helper()
	b, err := encode(sealed)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return objectVector{
		Name:       name,
		Sealed:     sealed,
		EncodedLen: len(b),
		Encoded:    hex.EncodeToString(b),
		ObjectID:   ObjectID(b).String(),
	}
}

func encodeDir(d *Directory) func(bool) ([]byte, error) {
	return func(sealed bool) ([]byte, error) {
		if sealed {
			return d.EncodeSealed(vectorCK)
		}
		plain := *d
		plain.Salt = [DirSaltSize]byte{}
		return plain.Encode()
	}
}

func encodeCommit(c *Commit) func(bool) ([]byte, error) {
	return func(sealed bool) ([]byte, error) {
		if sealed {
			return c.EncodeSealed(vectorCK)
		}
		return c.Encode()
	}
}

// The published content key has to be one the format would accept.
//
// Every id in objects.json, names.json and spine.json derives from it, so a
// stand-in of the wrong width makes all three unusable by a port that types a
// content key the way the spec describes it — and the two boundaries that do
// check a key's width, NewKeyring and WrapCK, are not on the path that
// generates any of them.
func TestTheVectorContentKeyIsOne(t *testing.T) {
	if len(vectorCK) != CKSize {
		t.Fatalf("the published content key is %d bytes, a content key is %d",
			len(vectorCK), CKSize)
	}
	if _, err := NewKeyring(vectorCK); err != nil {
		t.Fatalf("the published content key is not one: %v", err)
	}
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WrapCK(id.Public(), "3f2a1c58-9b0d-4e77-8a61-5c2d0e4f9ab3", vectorCK); err != nil {
		t.Fatalf("the published content key cannot be wrapped: %v", err)
	}
}

func TestObjectVectors(t *testing.T) {
	checkVectorFile(t, objectVectorFile, buildObjectVectors(t),
		"every manifest id in every library moves, and silo-drive stops interoperating")
}

// The check a port has to pass: rebuild every object from its description and
// confirm the bytes, then decode the committed hex back and confirm it opens.
func TestObjectVectorsAreReproducibleFromTheFile(t *testing.T) {
	var doc objectVectorDoc
	loadVectors(t, objectVectorFile, &doc)
	ck := []byte(doc.ContentKey)

	for _, v := range doc.Chunks {
		t.Run("chunk/"+v.Name, func(t *testing.T) {
			sc, err := SealChunk(ck, v.Input.bytes())
			if err != nil {
				t.Fatal(err)
			}
			if sc.ID.String() != v.ID || sc.PlaintextHash.String() != v.PlaintextHash ||
				len(sc.Frame) != v.FrameLen || ObjectID(sc.Frame).String() != v.FrameSHA256 {
				t.Fatalf("frame does not match the vector")
			}
		})
	}

	for _, v := range doc.Manifests {
		t.Run("manifest/"+v.Name, func(t *testing.T) {
			m := vectorManifest(t, v.Input, v.Sealed)
			var encoded []byte
			var err error
			if v.Sealed {
				encoded, err = m.EncodeSealed(ck)
			} else {
				encoded, err = m.Encode()
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(m.Chunks) != v.ChunkCount || len(encoded) != v.EncodedLen {
				t.Fatalf("got %d chunks in %d bytes, vector says %d in %d",
					len(m.Chunks), len(encoded), v.ChunkCount, v.EncodedLen)
			}
			if ObjectID(encoded).String() != v.ObjectID {
				t.Fatalf("object id %s, vector says %s", ObjectID(encoded), v.ObjectID)
			}
			if v.Encoded != "" && hex.EncodeToString(encoded) != v.Encoded {
				t.Fatal("encoded bytes differ from the vector")
			}
			if v.Sealed {
				if _, err := DecodeSealedManifest(encoded, ck); err != nil {
					t.Fatalf("the vector's own manifest does not open: %v", err)
				}
			} else if _, err := DecodeManifest(encoded); err != nil {
				t.Fatalf("the vector's own manifest does not decode: %v", err)
			}
		})
	}

	// Directory and commit vectors are checked from the bytes alone: decode
	// what the file holds, re-encode it, and require the same object back.
	// That exercises the decoder against bytes this build did not just write.
	for _, v := range doc.Directories {
		t.Run("directory/"+v.Name, func(t *testing.T) {
			raw, err := hex.DecodeString(v.Encoded)
			if err != nil || len(raw) != v.EncodedLen || ObjectID(raw).String() != v.ObjectID {
				t.Fatal("the vector's own bytes do not match its id")
			}
			var d *Directory
			if v.Sealed {
				d, err = DecodeSealedDirectory(raw, ck)
			} else {
				d, err = DecodeDirectory(raw)
			}
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			again, err := encodeDir(d)(v.Sealed)
			if err != nil {
				t.Fatalf("re-encode: %v", err)
			}
			if !bytes.Equal(again, raw) {
				t.Fatal("re-encoding the decoded directory produced different bytes")
			}
		})
	}
	for _, v := range doc.Commits {
		t.Run("commit/"+v.Name, func(t *testing.T) {
			raw, err := hex.DecodeString(v.Encoded)
			if err != nil || len(raw) != v.EncodedLen || ObjectID(raw).String() != v.ObjectID {
				t.Fatal("the vector's own bytes do not match its id")
			}
			var c *Commit
			if v.Sealed {
				c, err = DecodeSealedCommit(raw, ck)
			} else {
				c, err = DecodeCommit(raw)
			}
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			again, err := encodeCommit(c)(v.Sealed)
			if err != nil {
				t.Fatalf("re-encode: %v", err)
			}
			if !bytes.Equal(again, raw) {
				t.Fatal("re-encoding the decoded commit produced different bytes")
			}
		})
	}
}

// Every committed manifest, read the way a server reads it: no content key,
// and the chunk list has to come back correct in both library types. If this
// diverges from the keyed decode, garbage collection and the client disagree
// about which chunks a file references — and the collector is the one holding
// the delete.
func TestEveryManifestVectorIsReadableWithoutTheKey(t *testing.T) {
	var doc objectVectorDoc
	loadVectors(t, objectVectorFile, &doc)

	checked := 0
	for _, v := range doc.Manifests {
		if v.Encoded == "" {
			continue // the large cases carry only a length and an id
		}
		encoded := mustHex(t, v.Encoded)

		pub, err := DecodeManifestPublic(encoded)
		if err != nil {
			t.Errorf("%s: %v", v.Name, err)
			continue
		}
		if pub.E2EE != v.Sealed {
			t.Errorf("%s: public reader says E2EE=%v, vector says %v", v.Name, pub.E2EE, v.Sealed)
		}
		if len(pub.Chunks) != v.ChunkCount {
			t.Errorf("%s: public reader found %d chunks, vector says %d",
				v.Name, len(pub.Chunks), v.ChunkCount)
		}

		// And it agrees with the keyed decode, which is the assertion that
		// matters: same ids, same sizes, same order.
		var keyed *Manifest
		if v.Sealed {
			keyed, err = DecodeSealedManifest(encoded, vectorCK)
		} else {
			keyed, err = DecodeManifest(encoded)
		}
		if err != nil {
			t.Errorf("%s: keyed decode: %v", v.Name, err)
			continue
		}
		if len(keyed.Chunks) != len(pub.Chunks) {
			t.Errorf("%s: keyed decode found %d chunks, public reader %d",
				v.Name, len(keyed.Chunks), len(pub.Chunks))
			continue
		}
		for i := range keyed.Chunks {
			if keyed.Chunks[i].ID != pub.Chunks[i].ID || keyed.Chunks[i].Size != pub.Chunks[i].Size {
				t.Errorf("%s: chunk %d differs between the two readers", v.Name, i)
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no manifest vector carried encoded bytes to check")
	}
}

// The chunk streams, rebuilt from their descriptions and then decoded. The
// second half is the one that matters for a port: the committed bytes are what
// a server sends, and a decoder has to get the same frames back out of them.
func TestTheCommittedChunkStreamsAreReproducibleFromTheFile(t *testing.T) {
	var doc objectVectorDoc
	loadVectors(t, objectVectorFile, &doc)
	if len(doc.Streams) == 0 {
		t.Fatal("the file commits no chunk streams")
	}
	for _, v := range doc.Streams {
		t.Run("stream/"+v.Name, func(t *testing.T) {
			body, frames := vectorStream(t, v.Frames)
			if len(body) != v.Len || ObjectID(body).String() != v.SHA256 {
				t.Fatalf("rebuilt %d bytes hashing to %s, the vector says %d and %s",
					len(body), ObjectID(body), v.Len, v.SHA256)
			}
			if v.Stream != "" && hex.EncodeToString(body) != v.Stream {
				t.Fatal("rebuilt bytes differ from the committed stream")
			}
			for i, f := range frames {
				if f.ID != v.Frames[i].ID || f.Length != v.Frames[i].Length {
					t.Fatalf("frame %d is (%s, %d), the vector says (%s, %d)",
						i, f.ID, f.Length, v.Frames[i].ID, v.Frames[i].Length)
				}
			}

			// And the decode, from the committed bytes rather than the ones
			// just built, so this exercises the reader against a body this
			// build did not write.
			raw := body
			if v.Stream != "" {
				raw = mustHex(t, v.Stream)
			}
			got, err := DecodeChunkFrames(raw)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(got) != len(v.Frames) {
				t.Fatalf("decoded %d frames, the vector describes %d", len(got), len(v.Frames))
			}
			for i, f := range got {
				want := v.Frames[i]
				if f.ID.String() != want.ID || f.Present != want.Present || len(f.Bytes) != want.Length {
					t.Errorf("frame %d decoded as (%s, present=%v, %d bytes), the vector says (%s, present=%v, %d)",
						i, f.ID, f.Present, len(f.Bytes), want.ID, want.Present, want.Length)
				}
			}
		})
	}
}

// Every body the file says must be refused, refused. Nothing in the streams
// above notices a decoder that skips the length check, the status check or the
// hash check: they all decode correctly either way.
func TestTheCommittedChunkStreamsRefusedAreRefused(t *testing.T) {
	var doc objectVectorDoc
	loadVectors(t, objectVectorFile, &doc)
	if len(doc.StreamsRefused) == 0 {
		t.Fatal("the file commits no refused streams")
	}
	for _, v := range doc.StreamsRefused {
		if frames, err := DecodeChunkFrames(mustHex(t, v.Stream)); err == nil {
			t.Errorf("%s: decoded into %d frames, and the vector says it must not (%s)",
				v.Name, len(frames), v.Why)
		}
	}
}
