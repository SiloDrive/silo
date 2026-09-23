package client

import (
	"testing"

	"github.com/SiloDrive/silo/store"
)

func dirOfSize(entries int) *directory {
	d := &store.Directory{Entries: make([]store.DirEntry, entries)}
	for i := range d.Entries {
		d.Entries[i].Name = []byte("an-entry-name")
	}
	return &directory{dir: d}
}

func idOf(n int) store.ID {
	var id store.ID
	id[0], id[1] = byte(n), byte(n>>8)
	return id
}

// The bug: both caches only ever grew. An id names one set of bytes forever,
// so a hit is always right and Refresh keeps them on purpose -- but nothing
// ever gave the memory back, and a writer is worse than a reader because every
// rewrite mints a new directory id, so a sync loop accumulates one entry per
// *version* of every directory it walks.
func TestTheDirectoryCacheStaysInsideItsBudget(t *testing.T) {
	l := &EncryptedLibrary{dirs: newLRU[*directory](64 << 10)}

	for i := range 500 {
		d := dirOfSize(20)
		l.cacheDirectory(idOf(i), d)
	}

	if w := l.dirs.weight(); w > 64<<10 {
		t.Errorf("the directory cache holds %d bytes, over its %d budget", w, 64<<10)
	}
	if l.dirs.len() >= 500 {
		t.Errorf("the cache kept all %d directories: nothing was evicted", l.dirs.len())
	}
}

func TestTheManifestCacheStaysInsideItsBudget(t *testing.T) {
	l := &EncryptedLibrary{manifests: newLRU[*store.Manifest](64 << 10)}

	for i := range 500 {
		// An inline manifest carries the file's own bytes, so a cache of them
		// is a cache of files -- which is why weight counts them.
		l.cacheManifest(idOf(i), &store.Manifest{Inline: make([]byte, 4096)})
	}

	if w := l.manifests.weight(); w > 64<<10 {
		t.Errorf("the manifest cache holds %d bytes, over its %d budget", w, 64<<10)
	}
}

// Recency, not insertion order: a sync loop working in one part of a tree
// should keep that part and shed the rest, and a cache ordered only by when a
// thing arrived would evict the directory it is about to ask for again.
func TestTheCacheEvictsWhatIsNotBeingUsed(t *testing.T) {
	c := newLRU[*directory](3 * (perDirectoryOverhead + 100))

	keep, drop := idOf(1), idOf(2)
	c.put(keep, dirOfSize(1), perDirectoryOverhead+100)
	c.put(drop, dirOfSize(1), perDirectoryOverhead+100)
	if _, ok := c.get(keep); !ok {
		t.Fatal("the cache lost a value it had room for")
	}

	// Two more arrive; the budget holds three, so one of the originals goes.
	c.put(idOf(3), dirOfSize(1), perDirectoryOverhead+100)
	c.put(idOf(4), dirOfSize(1), perDirectoryOverhead+100)

	if _, ok := c.get(keep); !ok {
		t.Error("the cache evicted the entry that was read most recently")
	}
	if _, ok := c.get(drop); ok {
		t.Error("the cache kept the least recently used entry")
	}
}

// Re-putting a key must not double-count it, or a directory rewritten in place
// inflates the total until the cache evicts everything it holds.
func TestReplacingAKeyReplacesItsWeight(t *testing.T) {
	c := newLRU[*directory](1 << 20)
	id := idOf(1)

	c.put(id, dirOfSize(1), 500)
	c.put(id, dirOfSize(1), 700)

	if c.len() != 1 {
		t.Errorf("the cache holds %d values for one key", c.len())
	}
	if c.weight() != 700 {
		t.Errorf("the cache totals %d, want 700 -- the old weight was not replaced", c.weight())
	}
}
