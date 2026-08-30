package silod

import (
	"bytes"
	"crypto/rand"
	"sync"
	"testing"

	"github.com/dkam/silo/client"
	"github.com/dkam/silo/store"
)

// Writing an encrypted library.
//
// The server cannot do any of this: it cannot chunk a file it cannot read,
// cannot name an entry, and cannot rewrite a directory. So a write is a spine
// rewrite the client computes and the server merely stores, and the three
// rules the server cannot enforce are the tests below.

// writable stands up a library and returns a reader-writer over it with a
// clock the test controls, so that "the mutation happened now" is a fact a
// test can assert rather than a second it has to race.
func writable(t *testing.T) (*client.EncryptedLibrary, *client.Account) {
	t.Helper()
	base, _, _ := enrolled(t)
	c := client.NewClient(base)
	acct, err := c.OpenAccount("wire@example.com", wirePassword)
	if err != nil {
		t.Fatalf("OpenAccount: %v", err)
	}
	lib, kr, err := acct.CreateEncryptedLibrary("Written by the client")
	if err != nil {
		t.Fatalf("CreateEncryptedLibrary: %v", err)
	}
	return client.NewEncryptedLibrary(c, lib.ID, kr), acct
}

func TestAClientWritesAndReadsBackAnEncryptedLibrary(t *testing.T) {
	lib, _ := writable(t)
	lib.Now = func() int64 { return 2000 }

	notes := []byte("the quick brown fox jumps over the lazy dog")
	if err := lib.WriteFile("notes.txt", notes, 1000); err != nil {
		t.Fatalf("WriteFile(notes.txt): %v", err)
	}

	big := make([]byte, 3<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	if err := lib.MkdirAll("photos"); err != nil {
		t.Fatalf("MkdirAll(photos): %v", err)
	}
	if err := lib.WriteFile("photos/big.bin", big, 1001); err != nil {
		t.Fatalf("WriteFile(photos/big.bin): %v", err)
	}

	got, err := lib.ReadFile("notes.txt")
	if err != nil {
		t.Fatalf("ReadFile(notes.txt): %v", err)
	}
	if !bytes.Equal(got, notes) {
		t.Error("notes.txt did not come back")
	}
	got, err = lib.ReadFile("photos/big.bin")
	if err != nil {
		t.Fatalf("ReadFile(photos/big.bin): %v", err)
	}
	if !bytes.Equal(got, big) {
		t.Errorf("big.bin came back as %d bytes, want %d", len(got), len(big))
	}

	// Rule three: the mutation's timestamp is not the file's mtime. The file
	// keeps the one it was written with, however long ago that was.
	st, err := lib.Stat("photos/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if st.Mtime != 1001 {
		t.Errorf("big.bin mtime = %d, want the file's own 1001, not the mutation's 2000", st.Mtime)
	}

	// Rewriting a file replaces it rather than adding a second entry.
	if err := lib.WriteFile("notes.txt", []byte("rewritten"), 1002); err != nil {
		t.Fatalf("rewriting notes.txt: %v", err)
	}
	entries, err := lib.List("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("the root holds %d entries after a rewrite, want 2: %+v", len(entries), entries)
	}
	if got, err := lib.ReadFile("notes.txt"); err != nil || string(got) != "rewritten" {
		t.Errorf("notes.txt = %q, %v; want the rewritten bytes", got, err)
	}
}

// Rule one: a rewritten directory keeps the salt of the object it replaces.
// A fresh one changes its id, hence every ancestor's, hence the root — and
// changes?since= then reports the whole library modified on every commit.
func TestARewrittenDirectoryCarriesItsSaltForward(t *testing.T) {
	lib, _ := writable(t)
	if err := lib.MkdirAll("photos"); err != nil {
		t.Fatal(err)
	}
	if err := lib.WriteFile("photos/one.txt", []byte("one"), 1000); err != nil {
		t.Fatal(err)
	}
	first := saltOf(t, lib, "photos")

	if err := lib.WriteFile("photos/two.txt", []byte("two"), 1001); err != nil {
		t.Fatal(err)
	}
	if second := saltOf(t, lib, "photos"); second != first {
		t.Error("the rewritten directory minted a new salt")
	}
}

func saltOf(t *testing.T, lib *client.EncryptedLibrary, p string) [store.DirSaltSize]byte {
	t.Helper()
	d, err := lib.Directory(p)
	if err != nil {
		t.Fatalf("reading %s: %v", p, err)
	}
	return d.Salt
}

// Rule two: only the directory whose entry list changed gets a new mtime, and
// it lives one level up, in its parent's entry. Stamping the whole spine makes
// every commit look like it touched everything between the change and the
// root.
func TestOnlyTheChangedDirectoryGetsANewMtime(t *testing.T) {
	lib, _ := writable(t)
	lib.Now = func() int64 { return 2000 }
	if err := lib.MkdirAll("a/b"); err != nil {
		t.Fatal(err)
	}
	if err := lib.WriteFile("a/b/one.txt", []byte("one"), 1000); err != nil {
		t.Fatal(err)
	}

	before := entryMtime(t, lib, "/", "a")

	// A second write, later, into a/b. Only b's entry list changes.
	lib.Now = func() int64 { return 3000 }
	if err := lib.WriteFile("a/b/two.txt", []byte("two"), 1001); err != nil {
		t.Fatal(err)
	}

	if got := entryMtime(t, lib, "/", "a"); got != before {
		t.Errorf("the root's entry for a is now %d, was %d: the spine was stamped", got, before)
	}
	if got := entryMtime(t, lib, "a", "b"); got != 3000 {
		t.Errorf("a's entry for b is %d, want the mutation's 3000", got)
	}
}

func entryMtime(t *testing.T, lib *client.EncryptedLibrary, dir, name string) int64 {
	t.Helper()
	entries, err := lib.List(dir)
	if err != nil {
		t.Fatalf("List(%s): %v", dir, err)
	}
	for _, e := range entries {
		if e.Name == name {
			return e.Mtime
		}
	}
	t.Fatalf("%s holds no entry named %s", dir, name)
	return 0
}

// PUT head is a compare-and-swap, so a writer that loses gets 412 and has to
// re-read, rebuild on the new root and retry. A client that treated it as a
// failure would drop a write whenever two devices were awake at once.
func TestAWriterThatLosesTheHeadRebuildsAndRetries(t *testing.T) {
	lib, acct := writable(t)
	if err := lib.WriteFile("first.txt", []byte("first"), 1000); err != nil {
		t.Fatal(err)
	}

	// A second handle on the same library, which reads — and so caches a head
	// that the first handle is about to move out from under it.
	other, err := acct.OpenEncryptedLibrary(lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.List("/"); err != nil {
		t.Fatal(err)
	}
	if err := lib.WriteFile("second.txt", []byte("second"), 1001); err != nil {
		t.Fatal(err)
	}

	// other is now building on a head that is no longer the head.
	if err := other.WriteFile("third.txt", []byte("third"), 1002); err != nil {
		t.Fatalf("the losing writer did not recover: %v", err)
	}

	lib.Refresh()
	entries, err := lib.List("/")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	if !names["first.txt"] || !names["second.txt"] || !names["third.txt"] {
		t.Errorf("the retry lost a write: %v", names)
	}
}

// Concurrent writers, which is the case the loop exists for rather than the
// staged one above: every write must land, whichever order they resolve in.
func TestConcurrentWritersAllLand(t *testing.T) {
	lib, acct := writable(t)

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w, err := acct.OpenEncryptedLibrary(lib.ID)
			if err != nil {
				errs[i] = err
				return
			}
			errs[i] = w.WriteFile(string(rune('a'+i))+".txt", []byte{byte('a' + i)}, 1000)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("writer %d: %v", i, err)
		}
	}

	lib.Refresh()
	entries, err := lib.List("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Errorf("the root holds %d entries, want all 4 writes: %+v", len(entries), entries)
	}
}
