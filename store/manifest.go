package store

import (
	"crypto/sha256"
	"fmt"
)

// ManifestVersion is the version byte this package writes. Version counters
// are per object type — manifests, directories and commits revise
// independently, as their separate domain strings already imply — so a port
// sharing one counter across all three would bump directory ids when the
// manifest format changed.
const ManifestVersion = 1

// The flag bits, pinned. They sit inside the authenticated range, so two
// ports that assign the same fact to different bits mint different keys and
// different ids for identical trees: the failure the byte layouts exist to
// prevent, arriving through the one byte a layout diagram does not describe.
const (
	flagE2EE     = 0x01 // all objects: gates every E2EE-only field
	flagInline   = 0x02 // manifests only: the file's bytes replace its chunk list
	flagReserved = 0xfc // must be zero; parsers reject if set
)

// MaxManifestBytes bounds an encoded manifest, checkable from Content-Length
// before a byte is parsed or allocated — which is what actually bounds
// allocation, where a cap on the chunk count would not.
//
// It is a *format* ceiling — what a file may be — not a memory promise.
// Manifests do not segment and their size is linear in the file's
// (~67 bytes per chunk, so ~67 MB per TB at the 1 MiB target), so a very
// large file means a large immutable object rewritten in full on every edit.
// The answer for those is a streaming parse, not a smaller constant.
const MaxManifestBytes = 1 << 30

// MaxFileSize bounds the file a manifest may claim to describe: 256 TiB. Not
// a manifest can reach it — the ceiling above binds first, and long before —
// but file_size is parsed before anything is sized from it, so it needs a
// bound of its own rather than inheriting one by accident.
const MaxFileSize = 1 << 48

// ChunkRef is one entry of a manifest's chunk list.
type ChunkRef struct {
	ID ID
	// Size is the chunk's PLAINTEXT length. Pinned as plaintext because
	// mapping a read offset to a chunk needs it; the wire size is derivable
	// (+16 for the content tag under E2EE, +0 plain). Storage framing is
	// server-internal and never enters client arithmetic.
	Size int64
	// PlaintextHash is H_p, carried in an E2EE manifest's sealed section and
	// zero in a plain one. It exists because ids alone cannot decrypt: a
	// reader holds SHA-256(ciphertext) and needs a key derived from
	// SHA-256(plaintext), and there is no path between them.
	PlaintextHash ID
}

// Manifest is a file: an ordered chunk list, or the bytes themselves when the
// file is small enough to inline.
//
// Whether a manifest inlines is a function of the file's size alone, never a
// writer's choice — see Validate. Two clients that disagreed about a 30 KB
// file would mint two manifest ids for identical content, and dedup and
// changes?since= would both see a modification that did not happen.
type Manifest struct {
	FileSize int64
	Chunks   []ChunkRef // empty when inline
	Inline   []byte     // nil unless inline; the file's bytes
}

// Inlined reports whether a file of this size carries its bytes in its
// manifest. Measurement found a third of all files under the threshold holding
// 0.01% of all bytes: inlining removes a third of the store's chunk objects,
// and spares a 4 KB file the three round trips of manifest, dirent and chunk.
func Inlined(fileSize int64) bool { return fileSize < InlineThreshold }

// Validate reports whether the manifest describes a file at all.
func (m *Manifest) Validate() error {
	if m.FileSize < 0 {
		return fmt.Errorf("%w: negative file size %d", ErrEncoding, m.FileSize)
	}
	if Inlined(m.FileSize) {
		if len(m.Chunks) != 0 {
			return fmt.Errorf("%w: a %d-byte file inlines, but the manifest lists %d chunks",
				ErrEncoding, m.FileSize, len(m.Chunks))
		}
		if int64(len(m.Inline)) != m.FileSize {
			return fmt.Errorf("%w: inline data is %d bytes, file size says %d",
				ErrEncoding, len(m.Inline), m.FileSize)
		}
		return nil
	}
	if m.Inline != nil {
		return fmt.Errorf("%w: a %d-byte file is chunked, but the manifest carries inline data",
			ErrEncoding, m.FileSize)
	}
	if len(m.Chunks) == 0 {
		return fmt.Errorf("%w: a %d-byte file has no chunks", ErrEncoding, m.FileSize)
	}
	var total int64
	for i, c := range m.Chunks {
		if c.Size <= 0 {
			return fmt.Errorf("%w: chunk %d is %d bytes", ErrEncoding, i, c.Size)
		}
		if c.Size > MaxFileSize {
			return fmt.Errorf("%w: chunk %d is %d bytes, above the %d ceiling",
				ErrEncoding, i, c.Size, int64(MaxFileSize))
		}
		// Checked before adding, not after: total and c.Size are each already
		// bounded by MaxFileSize (2^48), but a manifest can list far more
		// chunks than fit in that ceiling divided by one, and int64 wraps
		// silently long before the loop runs out of chunks to add.
		if total > MaxFileSize-c.Size {
			return fmt.Errorf("%w: chunk %d overflows the file size total", ErrEncoding, i)
		}
		total += c.Size
	}
	if total != m.FileSize {
		return fmt.Errorf("%w: chunks total %d bytes, file size says %d",
			ErrEncoding, total, m.FileSize)
	}
	return nil
}

// Encode encodes the manifest for a plain library.
func (m *Manifest) Encode() ([]byte, error) { return m.encode(nil) }

// EncodeSealed encodes the manifest for an E2EE library under its content
// key, sealing the per-chunk plaintext hashes — or, for an inline file, the
// file's own bytes.
func (m *Manifest) EncodeSealed(ck []byte) ([]byte, error) {
	if err := checkCK(ck, "sealing a manifest"); err != nil {
		return nil, err
	}
	return m.encode(ck)
}

func (m *Manifest) encode(ck []byte) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	e2ee := ck != nil
	inline := Inlined(m.FileSize)

	var flags byte
	if e2ee {
		flags |= flagE2EE
	}
	if inline {
		flags |= flagInline
	}

	out := []byte{ManifestVersion, flags}
	out = appendUvarint(out, uint64(m.FileSize))

	// The sealed plaintext is built first because its hash is a public
	// field: seal_hash sits inside the authenticated range, which is what
	// makes the sealing key commit to the plaintext as well as the AD.
	var sealed []byte
	if inline {
		if e2ee {
			sealed = m.Inline
		} else {
			out = append(out, m.Inline...)
		}
	} else {
		out = appendUvarint(out, uint64(len(m.Chunks)))
		for _, c := range m.Chunks {
			out = append(out, c.ID[:]...)
			out = appendUvarint(out, uint64(c.Size))
		}
		if e2ee {
			sealed = make([]byte, 0, len(m.Chunks)*IDSize)
			for _, c := range m.Chunks {
				sealed = append(sealed, c.PlaintextHash[:]...)
			}
		}
	}

	if e2ee {
		sealHash := sha256.Sum256(sealed)
		out = append(out, sealHash[:]...)
		frame, err := sealSection(ck, domainManifest, out, sealed)
		if err != nil {
			return nil, err
		}
		out = append(out, frame...)
	}

	if len(out) > MaxManifestBytes {
		return nil, fmt.Errorf("%w: encoded manifest is %d bytes, above the %d ceiling",
			ErrEncoding, len(out), MaxManifestBytes)
	}
	return out, nil
}

// DecodeManifest reads a plain library's manifest.
func DecodeManifest(b []byte) (*Manifest, error) { return decodeManifest(b, nil) }

// DecodeSealedManifest reads an E2EE library's manifest and opens its sealed
// section.
func DecodeSealedManifest(b, ck []byte) (*Manifest, error) {
	if err := checkCK(ck, "opening a manifest"); err != nil {
		return nil, err
	}
	return decodeManifest(b, ck)
}

func decodeManifest(b, ck []byte) (*Manifest, error) {
	pub, p, err := parsePublicSection(b)
	if err != nil {
		return nil, err
	}

	// The expected library type is an input, not something read out of the
	// object. Locating the tag requires parsing, parsing requires flags, and
	// under E2EE the tag is what protects flags — so a server flipping bit 0
	// would get a parse under the wrong layout before any verification ran.
	// The catalog already says which kind of library this is. The public
	// section parses identically either way, so this is checked the moment it
	// can be and still before anything looks for the tag.
	e2ee := ck != nil
	if pub.E2EE != e2ee {
		return nil, fmt.Errorf("%w: manifest declares E2EE=%t, library is E2EE=%t",
			ErrEncoding, pub.E2EE, e2ee)
	}

	m := &Manifest{FileSize: pub.FileSize}

	// sealedLen is fixed by the public section in both shapes, so the sealed
	// section carries no length of its own and trailing bytes have nowhere
	// to hide.
	var sealedLen int
	if pub.Inlined {
		sealedLen = int(pub.FileSize)
		if !e2ee {
			if len(b[p:]) != int(pub.FileSize) {
				return nil, fmt.Errorf("%w: inline manifest carries %d bytes, file size says %d",
					ErrEncoding, len(b[p:]), pub.FileSize)
			}
			m.Inline = append([]byte(nil), b[p:]...)
			return m, nil
		}
	} else {
		// The public chunk list is this list, minus the plaintext hashes that
		// only the sealed section carries and only a key holder can read.
		m.Chunks = make([]ChunkRef, len(pub.Chunks))
		for i, c := range pub.Chunks {
			m.Chunks[i].ID, m.Chunks[i].Size = c.ID, c.Size
		}
		if !e2ee {
			if p != len(b) {
				return nil, fmt.Errorf("%w: %d bytes past the end of a plain manifest",
					ErrEncoding, len(b)-p)
			}
			return m, nil
		}
		sealedLen = len(m.Chunks) * IDSize
	}

	if len(b)-p < IDSize {
		return nil, fmt.Errorf("%w: manifest ends before its seal hash", ErrEncoding)
	}
	var sealHash ID
	copy(sealHash[:], b[p:p+IDSize])
	p += IDSize

	ad := b[:p]
	if len(b)-p != sealedLen+TagSize {
		return nil, fmt.Errorf("%w: manifest sealed section is %d bytes, want %d",
			ErrEncoding, len(b)-p, sealedLen+TagSize)
	}
	plain, err := openSection(ck, domainManifest, ad, b[p:])
	if err != nil {
		return nil, err
	}
	// Redundant against the AEAD, which authenticates seal_hash as part of
	// the AD — but it catches a broken *writer*, which the tag never can.
	if sha256.Sum256(plain) != sealHash {
		return nil, fmt.Errorf("%w: manifest seal hash does not match its sealed section", ErrEncoding)
	}

	if pub.Inlined {
		m.Inline = plain
		return m, nil
	}
	for i := range m.Chunks {
		copy(m.Chunks[i].PlaintextHash[:], plain[i*IDSize:])
	}
	return m, nil
}

// PublicChunk is one entry of a manifest's public chunk list.
type PublicChunk struct {
	ID   ID
	Size int64
}

// PublicManifest is everything a manifest says without its content key: the
// library type, the file size, and — for a chunked file — the ordered chunk
// ids and their stored sizes.
//
// It carries no inline bytes and no plaintext hashes. In a plain library those
// are readable anyway, through DecodeManifest; in an E2EE library they are the
// sealed section, and there is no key here to open it.
type PublicManifest struct {
	E2EE     bool
	Inlined  bool
	FileSize int64
	Chunks   []PublicChunk
}

// DecodeManifestPublic reads the public section of a manifest of either
// library type, without a content key.
//
// This is what makes garbage collection possible on a server that cannot read
// its own libraries. Chunk ids and sizes are public **by design** — the
// argument is in the plan's Manifests section — precisely so that tracing
// which chunks are still referenced does not require the key. A server that
// could not enumerate them could never reclaim anything in an E2EE library.
//
// **A client must not use this.** The public section of an E2EE manifest is
// covered by the AEAD tag, but nothing here checks it, because checking it
// needs the key. A holder of CK calls DecodeSealedManifest and gets the chunk
// list authenticated; a server calls this and gets it unverified, which is the
// correct trade for the one party the threat model already calls actively
// malicious for integrity. The values are safe for the server's own
// bookkeeping and are never a statement to a client about what a file is.
func DecodeManifestPublic(b []byte) (*PublicManifest, error) {
	m, _, err := parsePublicSection(b)
	return m, err
}

// parsePublicSection reads the part of a manifest that needs no key, and
// returns how far it got.
//
// Every manifest starts with this, in both library types and whether or not a
// content key is in hand — so it is parsed once, here, and the two decoders
// differ only in what they do afterwards. DecodeManifestPublic returns it;
// decodeManifest checks the declared library type against the caller's,
// then continues from the returned offset into the seal hash and the sealed
// section. Two hand-written parsers of one pinned layout is two things a port
// has to implement and keep in step, and they had already begun to disagree.
//
// The offset is returned rather than a remainder slice because what may follow
// is the caller's rule, not this function's: a plain manifest ends here, an
// E2EE one has a seal hash and a sealed section, and an inline plain one has
// its bytes.
func parsePublicSection(b []byte) (*PublicManifest, int, error) {
	if len(b) > MaxManifestBytes {
		return nil, 0, fmt.Errorf("%w: manifest is %d bytes, above the %d ceiling",
			ErrEncoding, len(b), MaxManifestBytes)
	}
	if len(b) < 3 {
		return nil, 0, fmt.Errorf("%w: manifest is %d bytes, too short for a header", ErrEncoding, len(b))
	}
	if b[0] != ManifestVersion {
		return nil, 0, fmt.Errorf("%w: manifest version %d, this build writes %d",
			ErrEncoding, b[0], ManifestVersion)
	}
	flags := b[1]
	if flags&flagReserved != 0 {
		return nil, 0, fmt.Errorf("%w: manifest reserved flag bits are set (%#02x)", ErrEncoding, flags)
	}
	m := &PublicManifest{
		E2EE:    flags&flagE2EE != 0,
		Inlined: flags&flagInline != 0,
	}

	p := 2
	fileSize, n, err := readUvarint(b[p:])
	if err != nil {
		return nil, 0, fmt.Errorf("%w: manifest file size", ErrEncoding)
	}
	p += n
	if fileSize > MaxFileSize {
		return nil, 0, fmt.Errorf("%w: manifest file size %d above the %d ceiling",
			ErrEncoding, fileSize, uint64(MaxFileSize))
	}
	m.FileSize = int64(fileSize)

	// The flag and the size have to agree, and this is the one place a server
	// can check it: a manifest claiming to be inline at 4 GiB, or chunked at
	// 12 bytes, is malformed whichever bit was flipped to make it so.
	if m.Inlined != Inlined(m.FileSize) {
		return nil, 0, fmt.Errorf("%w: manifest declares inline=%t for a %d-byte file",
			ErrEncoding, m.Inlined, m.FileSize)
	}
	if m.Inlined {
		return m, p, nil
	}

	count, n, err := readUvarint(b[p:])
	if err != nil {
		return nil, 0, fmt.Errorf("%w: manifest chunk count", ErrEncoding)
	}
	p += n
	// A chunk entry is at least IDSize+1 bytes, so a count larger than the
	// object could hold is refused before anything is allocated for it.
	// Bounding allocation is the whole job of this check, which is why the
	// bound comes from the bytes in hand and not from MaxManifestBytes — that
	// is a format ceiling, and sizing a slice from it would allocate against a
	// number the object never had to justify.
	if count == 0 || count > uint64(len(b)/(IDSize+1)) {
		return nil, 0, fmt.Errorf("%w: manifest claims %d chunks in %d bytes",
			ErrEncoding, count, len(b))
	}

	m.Chunks = make([]PublicChunk, count)
	var total int64
	for i := range m.Chunks {
		if len(b)-p < IDSize {
			return nil, 0, fmt.Errorf("%w: manifest ends inside chunk %d", ErrEncoding, i)
		}
		copy(m.Chunks[i].ID[:], b[p:p+IDSize])
		p += IDSize
		size, n, err := readUvarint(b[p:])
		if err != nil {
			return nil, 0, fmt.Errorf("%w: manifest chunk %d size", ErrEncoding, i)
		}
		p += n
		// The upper bound keeps a single size from overflowing on its own, but
		// a manifest can list far more chunks than MaxFileSize/MaxFileSize
		// leaves room for, so the running total is checked before every add
		// too — int64 wraps silently, and a wrapped total can land on exactly
		// the declared FileSize by construction.
		if size == 0 || size > uint64(MaxFileSize) {
			return nil, 0, fmt.Errorf("%w: manifest chunk %d is %d bytes", ErrEncoding, i, size)
		}
		if total > MaxFileSize-int64(size) {
			return nil, 0, fmt.Errorf("%w: manifest chunk %d overflows the file size total", ErrEncoding, i)
		}
		m.Chunks[i].Size = int64(size)
		total += int64(size)
	}
	if total != m.FileSize {
		return nil, 0, fmt.Errorf("%w: manifest chunks total %d bytes, file size says %d",
			ErrEncoding, total, m.FileSize)
	}
	return m, p, nil
}
