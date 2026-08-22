package store

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

func id(b byte) ID {
	var v ID
	for i := range v {
		v[i] = b
	}
	return v
}

func testDir() *Directory {
	return &Directory{
		Salt: [DirSaltSize]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		Entries: []DirEntry{
			{ChildID: id(0xaa), Type: NodeFile, Name: []byte("notes.txt"), Mtime: 1700000000, Mode: 0o644},
			{ChildID: id(0xbb), Type: NodeDir, Name: []byte("archive"), Mtime: 1600000000, Mode: 0o755},
			{ChildID: id(0xcc), Type: NodeSymlink, Name: []byte("latest"), Mtime: 1650000000, Mode: 0o777},
		},
	}
}

func TestDirectoryRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		name   string
		encode func(*Directory) ([]byte, error)
		decode func([]byte) (*Directory, error)
	}{
		{"plain", (*Directory).Encode, DecodeDirectory},
		{"sealed",
			func(d *Directory) ([]byte, error) { return d.EncodeSealed(testCK) },
			func(b []byte) (*Directory, error) { return DecodeSealedDirectory(b, testCK) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDir()
			if tc.name == "plain" {
				d.Salt = [DirSaltSize]byte{}
			}
			encoded, err := tc.encode(d)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got, err := tc.decode(encoded)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			want, err := d.canonical()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Entries, want) {
				t.Fatalf("entries changed:\n got %+v\nwant %+v", got.Entries, want)
			}
			if got.Salt != d.Salt {
				t.Fatalf("salt changed")
			}
		})
	}
}

// The encoder guarantees canonical order, so a caller may build the list in
// whatever order its filesystem walk produced.
func TestEntryOrderIsTheEncodersJob(t *testing.T) {
	forwards := testDir()
	backwards := testDir()
	backwards.Entries[0], backwards.Entries[2] = backwards.Entries[2], backwards.Entries[0]

	a, err := forwards.EncodeSealed(testCK)
	if err != nil {
		t.Fatal(err)
	}
	b, err := backwards.EncodeSealed(testCK)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("the same directory encoded two ways")
	}
}

func TestDuplicateNamesAreRefused(t *testing.T) {
	d := testDir()
	d.Entries[1].Name = d.Entries[0].Name
	if _, err := d.Encode(); !errors.Is(err, ErrEncoding) {
		t.Fatalf("got %v, want ErrEncoding", err)
	}
}

// sealHashOffset is where a sealed directory's seal_hash begins — the end of
// its public section, and so the end of the authenticated range.
func sealHashOffset(t *testing.T, d *Directory) int {
	t.Helper()
	entries, err := d.canonical()
	if err != nil {
		t.Fatal(err)
	}
	var sealed []byte
	for _, e := range entries {
		sealed = appendUvarint(sealed, uint64(e.Mtime))
		sealed = appendUvarint(sealed, uint64(e.Mode))
	}
	encoded, err := d.EncodeSealed(testCK)
	if err != nil {
		t.Fatal(err)
	}
	return len(encoded) - (IDSize + len(sealed) + TagSize)
}

// Under E2EE the mtimes are sealed — per-file activity timing is a leak the
// threat model does not concede. Two directories differing only in mtime must
// therefore share every public byte up to the seal hash.
func TestSealedDirectoryHidesItsTimestamps(t *testing.T) {
	early := testDir()
	late := testDir()
	for i := range late.Entries {
		late.Entries[i].Mtime += 86400
	}
	a, err := early.EncodeSealed(testCK)
	if err != nil {
		t.Fatal(err)
	}
	b, err := late.EncodeSealed(testCK)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("changing every mtime changed nothing")
	}
	at := sealHashOffset(t, early)
	if at != sealHashOffset(t, late) {
		t.Fatal("the two objects have different public section lengths")
	}
	if !bytes.Equal(a[:at], b[:at]) {
		t.Fatal("the public sections differ, so mtimes are not confined to the seal")
	}
}

// The graft the whole-object seal exists to stop: an entry lifted out of one
// directory and dropped into another, where every object still verifies
// internally.
func TestAnEntryCannotBeGraftedFromAnotherDirectory(t *testing.T) {
	mine := testDir()
	theirs := testDir()
	theirs.Entries[0].ChildID = id(0x11)

	a, err := mine.EncodeSealed(testCK)
	if err != nil {
		t.Fatal(err)
	}
	b, err := theirs.EncodeSealed(testCK)
	if err != nil {
		t.Fatal(err)
	}
	// Public sections are the same length, so splice theirs in front of my
	// sealed section — the attack a portable sealed blob would allow.
	at := sealHashOffset(t, mine)
	if at != sealHashOffset(t, theirs) {
		t.Fatal("the two objects have different public section lengths")
	}
	forged := append(bytes.Clone(b[:at+IDSize]), a[at+IDSize:]...)
	_ = a
	if _, err := DecodeSealedDirectory(forged, testCK); err == nil {
		t.Fatal("a directory carrying another's seal was accepted")
	}
}

// Swapping a child id beside a name is the other half of the same attack, and
// it fails structurally: the id is in the authenticated range, so the key
// changes with it.
func TestSwappingAChildIDBreaksTheSeal(t *testing.T) {
	encoded, err := testDir().EncodeSealed(testCK)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Clone(encoded)
	tampered[20] ^= 0xff
	if _, err := DecodeSealedDirectory(tampered, testCK); err == nil {
		t.Fatal("an edited child id was accepted")
	}
}

func TestSymlinkModeIsPinned(t *testing.T) {
	d := testDir()
	d.Entries[2].Mode = 0o755 // writers emit the pinned value regardless
	encoded, err := d.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeDirectory(encoded)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range got.Entries {
		if e.Type == NodeSymlink && e.Mode != SymlinkMode {
			t.Fatalf("symlink mode %#o, want %#o", e.Mode, uint32(SymlinkMode))
		}
	}
	// And a reader refuses one that arrives with anything else.
	i := bytes.Index(encoded, []byte("latest"))
	if i < 0 {
		t.Fatal("cannot find the symlink entry")
	}
	tampered := bytes.Clone(encoded)
	tampered[i+len("latest")+1] = 0o755 & 0x7f
	if _, err := DecodeDirectory(tampered); !errors.Is(err, ErrEncoding) {
		t.Fatalf("got %v, want a rejected symlink mode", err)
	}
}

// File type lives in Type, never in Mode — otherwise two encodings of one
// fact reappear one field over.
func TestModeCannotCarryFileType(t *testing.T) {
	d := testDir()
	d.Entries[0].Mode = 0o100644 // S_IFREG | 0644, the classic mistake
	if _, err := d.Encode(); !errors.Is(err, ErrEncoding) {
		t.Fatalf("got %v, want ErrEncoding", err)
	}
}

func TestUnknownEntryTypesAreRefused(t *testing.T) {
	d := testDir()
	d.Entries[0].Type = 7
	if _, err := d.Encode(); !errors.Is(err, ErrEncoding) {
		t.Fatalf("encode: got %v, want ErrEncoding", err)
	}
	encoded, err := testDir().Encode()
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Clone(encoded)
	// version, flags, entry_count, then the first entry's child id.
	tampered[3+IDSize] = 7
	if _, err := DecodeDirectory(tampered); !errors.Is(err, ErrEncoding) {
		t.Fatalf("decode: got %v, want ErrEncoding", err)
	}
}

// Pre-epoch mtimes exist on real disks; the writer clamps rather than failing,
// and there is exactly one clamping story for the whole format.
func TestTimestampsClampOnWriteAndRejectOnRead(t *testing.T) {
	d := testDir()
	d.Entries[0].Mtime = -5
	d.Entries[1].Mtime = MaxTimestamp + 1000
	encoded, err := d.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeDirectory(encoded)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range got.Entries {
		if e.Mtime < 0 || e.Mtime > MaxTimestamp {
			t.Fatalf("entry %q kept an mtime of %d", e.Name, e.Mtime)
		}
	}

	out := &Directory{Entries: []DirEntry{{ChildID: id(1), Type: NodeFile, Name: []byte("f"), Mode: 0o644}}}
	encoded, err = out.Encode()
	if err != nil {
		t.Fatal(err)
	}
	// Replace the zero mtime varint with one past the ceiling.
	i := bytes.Index(encoded, []byte("f")) + 1
	tampered := append(bytes.Clone(encoded[:i]), appendUvarint(nil, MaxTimestamp+1)...)
	tampered = append(tampered, encoded[i+1:]...)
	if _, err := DecodeDirectory(tampered); !errors.Is(err, ErrEncoding) {
		t.Fatalf("got %v, want an out-of-range mtime rejected", err)
	}
}

func TestAnEmptyDirectoryRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		name   string
		encode func(*Directory) ([]byte, error)
		decode func([]byte) (*Directory, error)
	}{
		{"plain", (*Directory).Encode, DecodeDirectory},
		{"sealed",
			func(d *Directory) ([]byte, error) { return d.EncodeSealed(testCK) },
			func(b []byte) (*Directory, error) { return DecodeSealedDirectory(b, testCK) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := tc.encode(&Directory{})
			if err != nil {
				t.Fatal(err)
			}
			got, err := tc.decode(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Entries) != 0 {
				t.Fatalf("got %d entries from an empty directory", len(got.Entries))
			}
		})
	}
}

// Two directories that differ only in salt must differ as objects — the salt
// is what stops the same filename encrypting to the same ciphertext
// library-wide.
func TestTheSaltIsPartOfTheObject(t *testing.T) {
	a := testDir()
	b := testDir()
	b.Salt[0] ^= 1
	ea, err := a.EncodeSealed(testCK)
	if err != nil {
		t.Fatal(err)
	}
	eb, err := b.EncodeSealed(testCK)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ea, eb) {
		t.Fatal("the salt did not reach the object")
	}
}

func TestMalformedDirectoriesAreRefused(t *testing.T) {
	good, err := testDir().Encode()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"empty":             {},
		"unknown version":   append([]byte{9}, good[1:]...),
		"reserved flag set": append([]byte{good[0], flagInline}, good[2:]...),
		"truncated":         good[:len(good)-4],
		"trailing bytes":    append(bytes.Clone(good), 0),
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeDirectory(b); !errors.Is(err, ErrEncoding) {
				t.Fatalf("got %v, want ErrEncoding", err)
			}
		})
	}
	if _, err := DecodeSealedDirectory(good, testCK); !errors.Is(err, ErrEncoding) {
		t.Errorf("a plain directory opened as E2EE: %v", err)
	}
}

// Order is checked on read as well as guaranteed on write: a server that
// reorders entries in an object the client would otherwise trust is refused,
// and so is the duplicate the same single pass catches.
func TestOutOfOrderEntriesAreRefusedOnRead(t *testing.T) {
	d := &Directory{Entries: []DirEntry{
		{ChildID: id(1), Type: NodeFile, Name: []byte("bb"), Mode: 0o644},
		{ChildID: id(2), Type: NodeFile, Name: []byte("aa"), Mode: 0o644},
	}}
	encoded, err := d.Encode()
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(encoded, []byte("aa"))
	j := bytes.Index(encoded, []byte("bb"))
	tampered := bytes.Clone(encoded)
	copy(tampered[i:i+2], []byte("bb"))
	copy(tampered[j:j+2], []byte("aa"))
	if _, err := DecodeDirectory(tampered); !errors.Is(err, ErrEncoding) {
		t.Fatalf("got %v, want out-of-order entries rejected", err)
	}
}
