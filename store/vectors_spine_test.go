package store

import (
	"bytes"
	"encoding/hex"
	"testing"
)

const spineVectorFile = "testdata/vectors/spine.json"

// The spine rewrite: the three rules protocol.md § A write rewrites the spine
// states, with bytes under them.
//
// Every other vector in this directory pins one object. This one pins a
// *rewrite* — what a chain of directories becomes when one of them changes —
// which is the only part of the format whose statement was previously a
// paragraph of prose and a method welded to a network transport. A port could
// pass every directory vector in objects.json and still stamp the wrong
// mtimes on every write.
type spineVectorDoc struct {
	Format     string        `json:"format"`
	Note       string        `json:"note"`
	ContentKey string        `json:"content_key_utf8"`
	Cases      []spineVector `json:"spine"`
}

type spineVector struct {
	Name string `json:"name"`
	Why  string `json:"why"`
	// Now is the mutation's timestamp, in unix seconds. It is not any file's
	// mtime; that rides in the file's own entry, below.
	Now int64 `json:"now"`
	// Changed is the index in Chain of the shallowest directory whose entry
	// list the caller altered. A value past the leaf means nothing changed.
	Changed int                 `json:"changed"`
	Chain   []spineLevelVector  `json:"chain"`
	Sealed  []spineSealedVector `json:"sealed"`
	Root    string              `json:"root"`
}

// spineLevelVector is one directory in the chain, described so a port can
// build it: the salt as bytes, the child names in the clear, and the entries
// as they were read.
type spineLevelVector struct {
	Salt string `json:"salt_hex"`
	// Child is the plaintext name of the next level down inside this one.
	// Empty at the leaf.
	Child   string             `json:"child,omitempty"`
	Entries []spineEntryVector `json:"entries"`
}

// spineEntryVector describes one entry by its plaintext name and commits the
// ciphertext that name encrypts to under this level's salt. A port that
// derives a different NameHex is building a different directory and every id
// below will differ for that reason rather than for a spine reason.
type spineEntryVector struct {
	Name    string `json:"name"`
	NameHex string `json:"name_hex"`
	Type    string `json:"type"`
	ChildID string `json:"child_id"`
	Mtime   int64  `json:"mtime"`
	Mode    uint32 `json:"mode"`
}

// spineSealedVector is what one level became.
//
// ChildMtime is the rule this file exists for, stated where a port can read it
// without decoding anything: the mtime the entry naming the next level down
// ended up with. Above the changed directory it is the mtime that entry
// already had; at or below it, it is Now.
type spineSealedVector struct {
	Level      int    `json:"level"`
	EncodedLen int    `json:"encoded_len"`
	Encoded    string `json:"encoded_hex"`
	ObjectID   string `json:"object_id"`
	ChildMtime *int64 `json:"child_entry_mtime,omitempty"`
	// Rewritten is false when the level sealed to the id it was read at, which
	// is the caller's signal not to store it again.
	Rewritten bool `json:"rewritten"`
}

var spineNodeTypes = map[NodeType]string{
	NodeFile:    "file",
	NodeDir:     "dir",
	NodeSymlink: "symlink",
}

func spineNodeType(t *testing.T, s string) NodeType {
	t.Helper()
	for k, v := range spineNodeTypes {
		if v == s {
			return k
		}
	}
	t.Fatalf("unknown node type %q", s)
	return 0
}

// salt fills a directory salt with one repeated byte, so the vector's salts
// are legible in the file and trivially reproducible in a port.
func salt(b byte) [DirSaltSize]byte {
	var s [DirSaltSize]byte
	for i := range s {
		s[i] = b
	}
	return s
}

// spineCase is one case's input, before it is rewritten.
type spineCase struct {
	name    string
	why     string
	now     int64
	changed int
	levels  []spineLevel
}

type spineLevel struct {
	salt    [DirSaltSize]byte
	child   string
	entries []DirEntry // Name is the plaintext here; build encrypts it
}

// build turns a described chain into the *Directory values RewriteSpine takes,
// encrypting each entry name under its own level's salt and then settling the
// chain: each level's entry for the level below is given that level's real
// object id, deepest first.
//
// Settling is what makes the chain a tree that could exist. Left unsettled,
// every parent names a child id nothing hashes to, so every level comes back
// rewritten even when the mutation changed nothing — and the one case that
// pins a no-op costing no writes could not be stated at all.
func (c spineCase) build(t *testing.T, k *Keyring) []SpineDir {
	t.Helper()
	chain := make([]SpineDir, len(c.levels))
	for i, l := range c.levels {
		d := &Directory{Salt: l.salt}
		nc, err := k.NameCipher(l.salt)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range l.entries {
			enc, err := nc.Encrypt(string(e.Name))
			if err != nil {
				t.Fatal(err)
			}
			e.Name = enc
			d.Entries = append(d.Entries, e)
		}
		chain[i] = SpineDir{Dir: d, Name: l.child}
	}
	settleSpine(t, k, chain)
	return chain
}

// settleSpine points each level's entry for the next level down at that
// level's real id, deepest first. A level whose parent has no entry for it yet
// is a directory being created, and there is nothing to point.
func settleSpine(t *testing.T, k *Keyring, chain []SpineDir) {
	t.Helper()
	for i := len(chain) - 1; i > 0; i-- {
		b, err := k.SealDirectory(chain[i].Dir)
		if err != nil {
			t.Fatal(err)
		}
		parent := chain[i-1]
		nc, err := k.NameCipher(parent.Dir.Salt)
		if err != nil {
			t.Fatal(err)
		}
		name, err := nc.Encrypt(parent.Name)
		if err != nil {
			t.Fatal(err)
		}
		if at := parent.Dir.EntryIndex(name); at >= 0 {
			parent.Dir.Entries[at].ChildID = ObjectID(b)
		}
	}
}

func spineCases() []spineCase {
	// A file entry the leaf already holds, so the leaf is never empty and the
	// rewrite has something to preserve.
	held := DirEntry{ChildID: id(0xa1), Type: NodeFile, Name: []byte("held.txt"), Mtime: 1500000000, Mode: 0o644}
	// The file the mutation just wrote. Its own mtime is deliberately not now:
	// rule three is that the mutation's timestamp and the file's are separate
	// numbers, and a writer that conflates them cannot be caught by a case
	// where they are equal.
	written := DirEntry{ChildID: id(0xb2), Type: NodeFile, Name: []byte("written.txt"), Mtime: 1234567890, Mode: 0o644}

	const now = 1700000000

	return []spineCase{
		{
			name: "leaf-only",
			why: "a chain of one: a write into the library root. There is no parent " +
				"to stamp, so changed decides nothing and the only output is the " +
				"leaf sealed as it arrived. RewriteSpine never rewrites the leaf — " +
				"the caller's own mutation already happened there — which is why " +
				"the deepest level of every case below is unrewritten too",
			now: now, changed: 0,
			levels: []spineLevel{
				{salt: salt(0x11), entries: []DirEntry{held, written}},
			},
		},
		{
			name: "two-levels",
			why: "a write into one subdirectory. The root's entry for docs takes both " +
				"the new id and the new mtime, because docs is the directory that " +
				"changed. photos, which did not, is untouched — a rewrite is not a " +
				"rebuild, and every sibling keeps the id and mtime it had",
			now: now, changed: 1,
			levels: []spineLevel{
				{salt: salt(0x11), child: "docs", entries: []DirEntry{
					{ChildID: id(0xc3), Type: NodeDir, Name: []byte("docs"), Mtime: 1600000000, Mode: 0o755},
					{ChildID: id(0xc4), Type: NodeDir, Name: []byte("photos"), Mtime: 1600000001, Mode: 0o755},
				}},
				{salt: salt(0x22), entries: []DirEntry{held, written}},
			},
		},
		{
			name: "three-levels-only-the-changed-mtime-moves",
			why: "the rule that costs a port a real fileserver to test: a write into " +
				"root/docs/notes stamps notes' entry inside docs with now, and leaves " +
				"docs' entry inside root at the mtime it already had. A writer that " +
				"stamps every level reports the whole path modified on every write, " +
				"and no single-object vector can catch it",
			now: now, changed: 2,
			levels: []spineLevel{
				{salt: salt(0x11), child: "docs", entries: []DirEntry{
					{ChildID: id(0xc3), Type: NodeDir, Name: []byte("docs"), Mtime: 1600000000, Mode: 0o755},
				}},
				{salt: salt(0x22), child: "notes", entries: []DirEntry{
					{ChildID: id(0xc5), Type: NodeDir, Name: []byte("notes"), Mtime: 1610000000, Mode: 0o755},
				}},
				{salt: salt(0x33), entries: []DirEntry{held, written}},
			},
		},
		{
			name: "nothing-changed",
			why: "changed is past the leaf, which is what a caller passes when the " +
				"mutation turned out to be a no-op — an MkdirAll of a directory that " +
				"exists, or a write of the bytes that were already there. No entry " +
				"takes a new mtime, so every level seals to the bytes it was read at " +
				"and the caller stores nothing. Same chain as the case above, so the " +
				"two ids can be compared directly",
			now: now, changed: 3,
			levels: []spineLevel{
				{salt: salt(0x11), child: "docs", entries: []DirEntry{
					{ChildID: id(0xc3), Type: NodeDir, Name: []byte("docs"), Mtime: 1600000000, Mode: 0o755},
				}},
				{salt: salt(0x22), child: "notes", entries: []DirEntry{
					{ChildID: id(0xc5), Type: NodeDir, Name: []byte("notes"), Mtime: 1610000000, Mode: 0o755},
				}},
				{salt: salt(0x33), entries: []DirEntry{held, written}},
			},
		},
		{
			name: "a-new-directory-at-the-leaf",
			why: "MkdirAll reaching a level that does not exist yet. The new leaf " +
				"arrives with a fresh salt and no entries, and its name is not yet in " +
				"the parent, so the parent gains an entry rather than replacing one. " +
				"changed is the parent, because the parent's entry list is what grew",
			now: now, changed: 0,
			levels: []spineLevel{
				{salt: salt(0x11), child: "fresh", entries: []DirEntry{held}},
				{salt: salt(0x44)},
			},
		},
		{
			name: "every-level-has-its-own-salt",
			why: "the same plaintext name at two depths. AES-SIV is deterministic, so " +
				"one salt for the whole tree would make both entries the same bytes; " +
				"the salt is per directory, so they are not. A port that derives the " +
				"name key from the content key alone passes every case above and " +
				"fails this one",
			now: now, changed: 2,
			levels: []spineLevel{
				{salt: salt(0x55), child: "same", entries: []DirEntry{
					{ChildID: id(0xd6), Type: NodeDir, Name: []byte("same"), Mtime: 1620000000, Mode: 0o755},
				}},
				{salt: salt(0x66), child: "same", entries: []DirEntry{
					{ChildID: id(0xd7), Type: NodeDir, Name: []byte("same"), Mtime: 1630000000, Mode: 0o755},
				}},
				{salt: salt(0x77), entries: []DirEntry{written}},
			},
		},
	}
}

func buildSpineVectors(t *testing.T) spineVectorDoc {
	t.Helper()
	k, err := NewKeyring(vectorCK)
	if err != nil {
		t.Fatal(err)
	}

	doc := spineVectorDoc{
		Format: "silo/store/vectors/v1",
		Note: "Generated by go test ./store -run TestSpineVectors -update. One case is " +
			"one spine rewrite: chain is the directories from the root down, read as " +
			"they were before the mutation, with entry names in the clear and the " +
			"ciphertext each encrypts to beside it. changed is the index of the " +
			"shallowest directory whose entry list the caller altered, and now is the " +
			"mutation's timestamp — never a file's. sealed is what each level became, " +
			"with child_entry_mtime stating the mtime rule's outcome per level so a " +
			"port can check it without decoding.",
		ContentKey: string(vectorCK),
	}

	for _, c := range spineCases() {
		chain := c.build(t, k)
		// The ids each level was read at, so the vector can say which levels a
		// caller would actually store.
		orig := make([]ID, len(chain))
		for i, sd := range chain {
			b, err := k.SealDirectory(sd.Dir)
			if err != nil {
				t.Fatalf("%s: sealing level %d as read: %v", c.name, i, err)
			}
			orig[i] = ObjectID(b)
		}

		v := spineVector{Name: c.name, Why: c.why, Now: c.now, Changed: c.changed}
		for i, l := range c.levels {
			lv := spineLevelVector{Salt: hex.EncodeToString(l.salt[:]), Child: l.child}
			for j, e := range l.entries {
				lv.Entries = append(lv.Entries, spineEntryVector{
					Name:    string(e.Name),
					NameHex: hex.EncodeToString(chain[i].Dir.Entries[j].Name),
					Type:    spineNodeTypes[e.Type],
					ChildID: chain[i].Dir.Entries[j].ChildID.String(),
					Mtime:   e.Mtime,
					Mode:    e.Mode,
				})
			}
			v.Chain = append(v.Chain, lv)
		}

		sealed, root, err := k.RewriteSpine(chain, c.changed, c.now)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		for i, b := range sealed {
			sv := spineSealedVector{
				Level:      i,
				EncodedLen: len(b),
				Encoded:    hex.EncodeToString(b),
				ObjectID:   ObjectID(b).String(),
				Rewritten:  ObjectID(b) != orig[i],
			}
			if i < len(chain)-1 {
				m := spineChildMtime(t, k, b, chain[i].Dir.Salt, c.levels[i].child)
				sv.ChildMtime = &m
			}
			v.Sealed = append(v.Sealed, sv)
		}
		v.Root = root.String()
		doc.Cases = append(doc.Cases, v)
	}
	return doc
}

// spineChildMtime opens a sealed directory and returns the mtime of the entry
// naming child. It is the number the mtime rule decides.
func spineChildMtime(t *testing.T, k *Keyring, encoded []byte, dirSalt [DirSaltSize]byte, child string) int64 {
	t.Helper()
	d, err := k.OpenDirectory(encoded)
	if err != nil {
		t.Fatal(err)
	}
	nc, err := k.NameCipher(dirSalt)
	if err != nil {
		t.Fatal(err)
	}
	want, err := nc.Encrypt(child)
	if err != nil {
		t.Fatal(err)
	}
	i := d.EntryIndex(want)
	if i < 0 {
		t.Fatalf("the rewritten directory has no entry for %q", child)
	}
	return d.Entries[i].Mtime
}

func TestSpineVectors(t *testing.T) {
	checkVectorFile(t, spineVectorFile, buildSpineVectors(t),
		"every directory id above a write moves, and a client's changes?since= "+
			"reports a different set of paths modified")
}

// The check a port has to pass: rebuild every chain from its description, run
// the rewrite, and confirm the bytes, the ids, the root and the mtimes.
func TestTheCommittedSpineRewritesAreReproducibleFromTheFile(t *testing.T) {
	var doc spineVectorDoc
	loadVectors(t, spineVectorFile, &doc)
	k, err := NewKeyring([]byte(doc.ContentKey))
	if err != nil {
		t.Fatal(err)
	}

	for _, v := range doc.Cases {
		t.Run(v.Name, func(t *testing.T) {
			if len(v.Sealed) != len(v.Chain) {
				t.Fatalf("%d levels in, %d out", len(v.Chain), len(v.Sealed))
			}

			chain := make([]SpineDir, len(v.Chain))
			for i, lv := range v.Chain {
				var s [DirSaltSize]byte
				copy(s[:], mustHex(t, lv.Salt))
				d := &Directory{Salt: s}
				nc, err := k.NameCipher(s)
				if err != nil {
					t.Fatal(err)
				}
				for _, ev := range lv.Entries {
					enc, err := nc.Encrypt(ev.Name)
					if err != nil {
						t.Fatal(err)
					}
					// The committed ciphertext, checked before it is used: a
					// port whose name cipher differs should fail here rather
					// than in a directory id fifty lines later.
					if hex.EncodeToString(enc) != ev.NameHex {
						t.Fatalf("%q encrypts to %s, vector says %s",
							ev.Name, hex.EncodeToString(enc), ev.NameHex)
					}
					childID, err := ParseID(ev.ChildID)
					if err != nil {
						t.Fatal(err)
					}
					d.Entries = append(d.Entries, DirEntry{
						ChildID: childID,
						Type:    spineNodeType(t, ev.Type),
						Name:    enc,
						Mtime:   ev.Mtime,
						Mode:    ev.Mode,
					})
				}
				chain[i] = SpineDir{Dir: d, Name: lv.Child}
			}

			// What a caller would have stored before the mutation, so the
			// vector's rewritten flag can be checked rather than trusted.
			orig := make([]ID, len(chain))
			for i, sd := range chain {
				b, err := k.SealDirectory(sd.Dir)
				if err != nil {
					t.Fatal(err)
				}
				orig[i] = ObjectID(b)
			}

			sealed, root, err := k.RewriteSpine(chain, v.Changed, v.Now)
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if root.String() != v.Root {
				t.Fatalf("root %s, vector says %s", root, v.Root)
			}
			for i, b := range sealed {
				sv := v.Sealed[i]
				if len(b) != sv.EncodedLen || !bytes.Equal(b, mustHex(t, sv.Encoded)) {
					t.Fatalf("level %d sealed to different bytes than the vector", i)
				}
				if ObjectID(b).String() != sv.ObjectID {
					t.Fatalf("level %d id %s, vector says %s", i, ObjectID(b), sv.ObjectID)
				}
				if got := ObjectID(b) != orig[i]; got != sv.Rewritten {
					t.Fatalf("level %d rewritten=%v, vector says %v", i, got, sv.Rewritten)
				}
				if sv.ChildMtime != nil {
					got := spineChildMtime(t, k, b, chain[i].Dir.Salt, v.Chain[i].Child)
					if got != *sv.ChildMtime {
						t.Fatalf("level %d stamped its child %d, vector says %d",
							i, got, *sv.ChildMtime)
					}
				}
			}
		})
	}
}

// The mtime rule, asserted against the file rather than restated: above the
// changed directory nothing takes now, and at or below it everything does.
//
// This is the assertion TestOnlyTheChangedDirectoryGetsANewMtime needs a real
// fileserver, an account, a login and a library to make.
func TestTheCommittedSpinesStampOnlyFromTheChangedLevel(t *testing.T) {
	var doc spineVectorDoc
	loadVectors(t, spineVectorFile, &doc)

	stampedAbove, keptAbove, stampedBelow := 0, 0, 0
	for _, v := range doc.Cases {
		for i, sv := range v.Sealed {
			if sv.ChildMtime == nil {
				continue // the leaf names no child
			}
			// Level i's entry names level i+1.
			switch {
			case i+1 >= v.Changed:
				if *sv.ChildMtime != v.Now {
					t.Errorf("%s: level %d is at or below the change and kept %d",
						v.Name, i, *sv.ChildMtime)
				}
				stampedBelow++
			default:
				if *sv.ChildMtime == v.Now {
					t.Errorf("%s: level %d is above the change and took now",
						v.Name, i)
				}
				stampedAbove++
				keptAbove++
			}
		}
	}
	// Both halves have to be exercised, or the loop above passes on a file
	// that only ever contains one of them.
	if stampedBelow == 0 || keptAbove == 0 {
		t.Fatalf("the vectors cover %d stamped and %d kept; both must be non-zero",
			stampedBelow, keptAbove)
	}
	_ = stampedAbove
}

// The salt is carried forward: a rewritten directory holds the salt it was
// read with, never a fresh one. A writer that mints a salt per write changes
// every directory's id on every write, hence every ancestor's, hence the root
// — and changes?since= reports the whole tree modified on every commit.
func TestTheCommittedSpinesCarryEverySaltForward(t *testing.T) {
	var doc spineVectorDoc
	loadVectors(t, spineVectorFile, &doc)
	k, err := NewKeyring([]byte(doc.ContentKey))
	if err != nil {
		t.Fatal(err)
	}

	for _, v := range doc.Cases {
		for i, sv := range v.Sealed {
			d, err := k.OpenDirectory(mustHex(t, sv.Encoded))
			if err != nil {
				t.Fatalf("%s level %d: %v", v.Name, i, err)
			}
			if got := hex.EncodeToString(d.Salt[:]); got != v.Chain[i].Salt {
				t.Errorf("%s level %d sealed under salt %s, was read under %s",
					v.Name, i, got, v.Chain[i].Salt)
			}
		}
	}
}
