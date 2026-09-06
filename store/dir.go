package store

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"slices"
)

// DirVersion is the version byte written into directory objects. Version
// counters are per object type; this one moves independently of the manifest's.
const DirVersion = 1

// DirEntry is one child of a directory.
type DirEntry struct {
	ChildID ID
	Type    NodeType
	// Name is the raw name bytes as they appear in the object: the plaintext
	// name in a plain library, the AES-SIV ciphertext in an E2EE one. This
	// codec treats it as opaque — encrypting names is the caller's job, and
	// keeping it out of here is what lets one encoder serve both library
	// types.
	//
	// Never base64. The URL encoding for entries/{path} is base64url
	// unpadded, but a port that base64s the name into the object produces
	// different bytes, a different key and a different id for an identical
	// tree.
	Name []byte
	// Mtime is unix seconds, UTC. Public in a plain library, sealed under
	// E2EE — per-file activity timing is a leak the threat model does not
	// concede.
	Mtime int64
	// Mode is permission bits only.
	Mode uint32
}

// Directory is a directory object: an ordered list of children, plus (in an
// E2EE library) the salt its child names are encrypted under.
type Directory struct {
	// Salt is generated once at directory creation and carried forward on
	// every rewrite — the writer reads the current object first, which it
	// must anyway, and copies the salt. A fresh salt per write would change
	// the directory's id on every write, hence every ancestor's, hence the
	// root: changes?since= would report the whole tree modified on every
	// commit.
	//
	// Zero in a plain library.
	Salt [DirSaltSize]byte
	// Entries are stored in strictly increasing bytewise order by Name.
	// Encode sorts a copy into that order, so a caller may build the list in
	// any order — but a duplicate name is refused rather than resolved,
	// because leaving duplicate handling to each implementation puts an
	// unstated choice inside an object the client trusts after one tag check.
	Entries []DirEntry
}

// Validate reports whether the directory can be encoded at all, for the rules
// that hold whatever the library type is. Encode applies one more that does
// not: plain names are held to ValidName, which an E2EE name — SIV ciphertext,
// and any byte may appear in it — cannot be.
func (d *Directory) Validate() error {
	for i, e := range d.Entries {
		if !e.Type.valid() {
			return fmt.Errorf("%w: entry %d has type %d", ErrEncoding, i, e.Type)
		}
		if len(e.Name) == 0 {
			return fmt.Errorf("%w: entry %d has no name", ErrEncoding, i)
		}
		if len(e.Name) > MaxNameBytes {
			return fmt.Errorf("%w: entry %d name is %d bytes, above %d",
				ErrEncoding, i, len(e.Name), MaxNameBytes)
		}
		if e.Mode > MaxMode {
			return fmt.Errorf("%w: entry %d mode %#o above %#o — file type belongs in Type, not Mode",
				ErrEncoding, i, e.Mode, MaxMode)
		}
	}
	return nil
}

// EntryIndex finds an entry by its stored name bytes, or -1.
//
// The one scan every caller needs: a directory's entries are a list, and what
// identifies one is the name exactly as the object holds it — ciphertext in an
// encrypted library, and opaque to this code either way.
func (d *Directory) EntryIndex(name []byte) int {
	for i := range d.Entries {
		if bytes.Equal(d.Entries[i].Name, name) {
			return i
		}
	}
	return -1
}

// SetEntry inserts e, or replaces the entry sharing its name.
func (d *Directory) SetEntry(e DirEntry) {
	if i := d.EntryIndex(e.Name); i >= 0 {
		d.Entries[i] = e
		return
	}
	d.Entries = append(d.Entries, e)
}

// Clone copies a directory deeply enough to be rewritten.
//
// A reader that caches decoded directories hands out shared pointers, so an
// entry slice appended to in place would be appended to under every reader
// that had already resolved through it. RewriteSpine mutates the directories
// it is given; a caller holding cached ones passes clones.
func (d *Directory) Clone() *Directory {
	out := &Directory{Salt: d.Salt, Entries: make([]DirEntry, len(d.Entries))}
	copy(out.Entries, d.Entries)
	return out
}

// canonical returns the entries in the order the object stores them, with the
// two writer-side normalisations applied: timestamps clamped into range and
// symlink modes forced to the pinned value.
func (d *Directory) canonical() ([]DirEntry, error) {
	entries := slices.Clone(d.Entries)
	slices.SortFunc(entries, func(a, b DirEntry) int { return bytes.Compare(a.Name, b.Name) })
	for i := range entries {
		entries[i].Mtime = clampTimestamp(entries[i].Mtime)
		if entries[i].Type == NodeSymlink {
			entries[i].Mode = SymlinkMode
		}
		if i > 0 && bytes.Equal(entries[i].Name, entries[i-1].Name) {
			return nil, fmt.Errorf("%w: two entries share the name %x", ErrEncoding, entries[i].Name)
		}
	}
	return entries, nil
}

// Encode encodes the directory for a plain library.
func (d *Directory) Encode() ([]byte, error) { return d.encode(nil) }

// EncodeSealed encodes the directory for an E2EE library, sealing the
// per-entry mtimes and modes under its content key.
//
// What stops a graft — an entry lifted whole out of another directory — is
// this whole-object seal: inserting or altering an entry means recomputing a
// tag, which needs the content key. The per-directory name keys are not the
// graft defence and would not survive being mistaken for it.
func (d *Directory) EncodeSealed(ck []byte) ([]byte, error) {
	if len(ck) == 0 {
		return nil, fmt.Errorf("store: sealing a directory needs a content key")
	}
	return d.encode(ck)
}

func (d *Directory) encode(ck []byte) ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	entries, err := d.canonical()
	if err != nil {
		return nil, err
	}
	e2ee := ck != nil

	var flags byte
	if e2ee {
		flags |= flagE2EE
	}
	out := []byte{DirVersion, flags}
	out = appendUvarint(out, uint64(len(entries)))
	if e2ee {
		out = append(out, d.Salt[:]...)
	}

	var sealed []byte
	for i, e := range entries {
		// Plain-library names are the names themselves, so they are held to
		// the rule that keeps a directory entry from being a path. E2EE names
		// are SIV ciphertext, which may legitimately contain any byte; the
		// same rule is applied there by NameCipher.Decrypt, once the name
		// exists.
		if !e2ee {
			if err := ValidName(e.Name); err != nil {
				return nil, fmt.Errorf("%w: entry %d: %w", ErrEncoding, i, err)
			}
		}
		out = append(out, e.ChildID[:]...)
		out = append(out, byte(e.Type))
		out = appendUvarint(out, uint64(len(e.Name)))
		out = append(out, e.Name...)
		if e2ee {
			sealed = appendUvarint(sealed, uint64(e.Mtime))
			sealed = appendUvarint(sealed, uint64(e.Mode))
		} else {
			out = appendUvarint(out, uint64(e.Mtime))
			out = appendUvarint(out, uint64(e.Mode))
		}
	}

	if e2ee {
		sealHash := sha256.Sum256(sealed)
		out = append(out, sealHash[:]...)
		frame, err := sealSection(ck, domainDir, out, sealed)
		if err != nil {
			return nil, err
		}
		out = append(out, frame...)
	}

	if len(out) > MaxDirBytes {
		return nil, fmt.Errorf("%w: encoded directory is %d bytes, above the %d ceiling",
			ErrEncoding, len(out), MaxDirBytes)
	}
	return out, nil
}

// DecodeDirectory reads a plain library's directory object.
func DecodeDirectory(b []byte) (*Directory, error) { return decodeDirectory(b, nil) }

// DecodeSealedDirectory reads an E2EE library's directory object and opens its
// sealed section.
func DecodeSealedDirectory(b, ck []byte) (*Directory, error) {
	if len(ck) == 0 {
		return nil, fmt.Errorf("store: opening a directory needs a content key")
	}
	return decodeDirectory(b, ck)
}

// PublicDirectory is a directory object as a server can see it: the edges of
// the tree, with no key in hand.
//
// Under E2EE, Name is SIV ciphertext and **Mtime and Mode are zero** — they
// live in the sealed section, and reading them here is reading the absence of
// data rather than a directory whose entries were all created at the epoch.
// In a plain library every field is the real one.
type PublicDirectory struct {
	E2EE bool
	Salt [DirSaltSize]byte
	// Entries in the object's own order: strictly increasing bytewise by
	// stored name, which under E2EE is an order over ciphertext and therefore
	// says nothing about the names.
	Entries []DirEntry
}

// DecodeDirectoryPublic reads the public section of a directory of either
// library type, without a content key.
//
// It is the directory half of what DecodeManifestPublic does for files, and it
// exists for the same reason: the tracing collector has to walk from a commit
// to every chunk it reaches, and half of that walk is directories. Child ids
// and node types are public **by design** — a mark phase that could not
// classify an edge could not follow it — so the walk needs no key, and a
// server that cannot read its own libraries can still reclaim them.
//
// **A client must not use this.** The public section of a sealed directory is
// covered by the AEAD tag, but nothing here checks it, because checking it
// needs the key. A holder of CK calls DecodeSealedDirectory and gets the edges
// authenticated; a server calls this and gets them unverified, which is the
// correct trade for the one party the threat model already calls actively
// malicious for integrity.
func DecodeDirectoryPublic(b []byte) (*PublicDirectory, error) {
	d, e2ee, _, err := parseDirectoryPublic(b)
	if err != nil {
		return nil, err
	}
	return &PublicDirectory{E2EE: e2ee, Salt: d.Salt, Entries: d.Entries}, nil
}

func decodeDirectory(b, ck []byte) (*Directory, error) {
	d, declared, p, err := parseDirectoryPublic(b)
	if err != nil {
		return nil, err
	}
	e2ee := ck != nil
	if declared != e2ee {
		return nil, fmt.Errorf("%w: directory declares E2EE=%t, library is E2EE=%t",
			ErrEncoding, declared, e2ee)
	}

	if !e2ee {
		if p != len(b) {
			return nil, fmt.Errorf("%w: %d bytes past the end of a plain directory",
				ErrEncoding, len(b)-p)
		}
		return d, nil
	}

	if len(b)-p < IDSize {
		return nil, fmt.Errorf("%w: directory ends before its seal hash", ErrEncoding)
	}
	var sealHash ID
	copy(sealHash[:], b[p:p+IDSize])
	p += IDSize

	plain, err := openSection(ck, domainDir, b[:p], b[p:])
	if err != nil {
		return nil, err
	}
	if sha256.Sum256(plain) != sealHash {
		return nil, fmt.Errorf("%w: directory seal hash does not match its sealed section", ErrEncoding)
	}

	// The pair count must equal entry_count and trailing bytes reject — from
	// inside the tag, where no server can forge a mismatch. A buggy writer
	// can, though, and two ports must refuse it identically rather than one
	// indexing past the end and one silently truncating.
	q := 0
	for i := range d.Entries {
		e := &d.Entries[i]
		var err error
		if e.Mtime, e.Mode, q, err = readTimeAndMode(plain, q); err != nil {
			return nil, fmt.Errorf("%w: sealed entry %d: %v", ErrEncoding, i, err)
		}
		if err := checkMode(e.Type, e.Mode); err != nil {
			return nil, fmt.Errorf("%w: sealed entry %d: %v", ErrEncoding, i, err)
		}
	}
	if q != len(plain) {
		return nil, fmt.Errorf("%w: %d bytes past the end of the sealed section",
			ErrEncoding, len(plain)-q)
	}
	return d, nil
}

// parseDirectoryPublic reads the part of a directory object that needs no key,
// and returns it, the library type the object declares, and how far it got.
//
// Both decoders start here, for the reason the manifest's shared parser gives:
// two hand-written parsers of one pinned layout are two things a port has to
// implement and keep in step. What follows the public section is the caller's
// rule — a plain directory ends here, an E2EE one has a seal hash and a sealed
// section — so the offset is returned rather than a remainder slice.
//
// The declared flag decides the layout, not the caller's expectation: an
// object says what it is, and whether that is what was wanted is a question
// for the caller one frame up.
func parseDirectoryPublic(b []byte) (*Directory, bool, int, error) {
	if len(b) > MaxDirBytes {
		return nil, false, 0, fmt.Errorf("%w: directory is %d bytes, above the %d ceiling",
			ErrEncoding, len(b), MaxDirBytes)
	}
	if len(b) < 3 {
		return nil, false, 0, fmt.Errorf("%w: directory is %d bytes, too short for a header", ErrEncoding, len(b))
	}
	if b[0] != DirVersion {
		return nil, false, 0, fmt.Errorf("%w: directory version %d, this build writes %d",
			ErrEncoding, b[0], DirVersion)
	}
	flags := b[1]
	// Bit 1 is the manifest's inline flag and is reserved here; a directory
	// carrying it is refused rather than tolerated.
	if flags&^byte(flagE2EE) != 0 {
		return nil, false, 0, fmt.Errorf("%w: directory reserved flag bits are set (%#02x)", ErrEncoding, flags)
	}
	e2ee := flags&flagE2EE != 0

	p := 2
	count, n, err := readUvarint(b[p:])
	if err != nil {
		return nil, false, 0, fmt.Errorf("%w: directory entry count", ErrEncoding)
	}
	p += n
	// The smallest entry is 32 bytes of id, a type, a length, one name byte
	// and — in a plain library — two more varints.
	if count > uint64(len(b)/(IDSize+3)) {
		return nil, false, 0, fmt.Errorf("%w: directory claims %d entries in %d bytes", ErrEncoding, count, len(b))
	}

	d := &Directory{}
	if e2ee {
		if len(b)-p < DirSaltSize {
			return nil, false, 0, fmt.Errorf("%w: directory ends before its salt", ErrEncoding)
		}
		copy(d.Salt[:], b[p:p+DirSaltSize])
		p += DirSaltSize
	}

	d.Entries = make([]DirEntry, count)
	var prev []byte
	for i := range d.Entries {
		e := &d.Entries[i]
		if len(b)-p < IDSize+1 {
			return nil, false, 0, fmt.Errorf("%w: directory ends inside entry %d", ErrEncoding, i)
		}
		copy(e.ChildID[:], b[p:p+IDSize])
		p += IDSize
		e.Type = NodeType(b[p])
		p++
		if !e.Type.valid() {
			return nil, false, 0, fmt.Errorf("%w: entry %d has type %d", ErrEncoding, i, e.Type)
		}
		nameLen, n, err := readUvarint(b[p:])
		if err != nil {
			return nil, false, 0, fmt.Errorf("%w: entry %d name length", ErrEncoding, i)
		}
		p += n
		if nameLen == 0 || nameLen > MaxNameBytes || uint64(len(b)-p) < nameLen {
			return nil, false, 0, fmt.Errorf("%w: entry %d name length %d", ErrEncoding, i, nameLen)
		}
		e.Name = append([]byte(nil), b[p:p+int(nameLen)]...)
		p += int(nameLen)

		// Strictly increasing, not merely sorted: a duplicate name is
		// unrepresentable, refused by the same single pass that validates
		// canonical order, at no extra cost.
		if prev != nil && bytes.Compare(prev, e.Name) >= 0 {
			return nil, false, 0, fmt.Errorf("%w: entry %d is not after the entry before it", ErrEncoding, i)
		}
		prev = e.Name

		if !e2ee {
			if err := ValidName(e.Name); err != nil {
				return nil, false, 0, fmt.Errorf("%w: entry %d: %w", ErrEncoding, i, err)
			}
			if e.Mtime, e.Mode, p, err = readTimeAndMode(b, p); err != nil {
				return nil, false, 0, fmt.Errorf("%w: entry %d: %v", ErrEncoding, i, err)
			}
			if err := checkMode(e.Type, e.Mode); err != nil {
				return nil, false, 0, fmt.Errorf("%w: entry %d: %v", ErrEncoding, i, err)
			}
		}
	}
	return d, e2ee, p, nil
}

func readTimeAndMode(b []byte, p int) (mtime int64, mode uint32, next int, err error) {
	mt, n, err := readUvarint(b[p:])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("mtime")
	}
	p += n
	if mt > MaxTimestamp {
		return 0, 0, 0, fmt.Errorf("mtime %d above the %d ceiling", mt, uint64(MaxTimestamp))
	}
	md, n, err := readUvarint(b[p:])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("mode")
	}
	p += n
	if md > MaxMode {
		return 0, 0, 0, fmt.Errorf("mode %#o above %#o", md, MaxMode)
	}
	return int64(mt), uint32(md), p, nil
}

func checkMode(t NodeType, mode uint32) error {
	if t == NodeSymlink && mode != SymlinkMode {
		return fmt.Errorf("symlink mode %#o, want %#o", mode, uint32(SymlinkMode))
	}
	return nil
}
