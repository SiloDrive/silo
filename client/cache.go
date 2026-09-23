package client

import (
	"container/list"

	"github.com/SiloDrive/silo/store"
)

// The two caches EncryptedLibrary keeps are both keyed by object id, and an
// id names one set of bytes forever -- which is what makes a hit always
// correct, and why Refresh deliberately keeps them. It is also why eviction is
// always safe: the only cost of a miss is a refetch.
//
// Bounded in approximate retained bytes rather than in entries, because an
// entry count says nothing about what an entry holds. One directory can carry
// a single name or a hundred thousand, and a manifest under the inline
// threshold carries the file itself. A count-based bound would let a hundred
// large objects sail past a limit that a hundred thousand small ones respect.
//
// The numbers are a budget, not a measurement. They are the shape the ticket
// asked for -- what a sync's working set actually looks like is worth
// measuring before either is tuned.
const (
	defaultDirCacheBytes      = 16 << 20
	defaultManifestCacheBytes = 16 << 20

	// perDirEntryOverhead is what a DirEntry costs beyond its name: an id, a
	// type, an mtime and a mode, plus the slice header's share. Approximate on
	// purpose -- this is a budget, and a budget that needed unsafe.Sizeof to
	// be right would be measuring the wrong thing.
	perDirEntryOverhead = 64
	// perDirectoryOverhead is the NameCipher kept beside each decoded
	// directory: two AES-256 key schedules and a CMAC subkey.
	perDirectoryOverhead = 512
	// perChunkRefBytes is an id, a plaintext size and a plaintext hash.
	perChunkRefBytes = 80
)

func directoryWeight(d *directory) int {
	n := perDirectoryOverhead
	if d == nil || d.dir == nil {
		return n
	}
	for _, e := range d.dir.Entries {
		n += len(e.Name) + perDirEntryOverhead
	}
	return n
}

func manifestWeight(m *store.Manifest) int {
	if m == nil {
		return 0
	}
	// Inline counts as the bytes it is. A manifest under the inline threshold
	// carries the file's content, so a cache of them is a cache of files.
	return len(m.Chunks)*perChunkRefBytes + len(m.Inline)
}

// lru is a cache bounded by the summed weight of what it holds, evicting
// least-recently-used first.
//
// Recency is updated on read as well as write, which is the point: a sync loop
// walking one part of a tree should keep that part and shed the rest, and a
// cache that only ordered by insertion would evict the directory it is about
// to ask for again.
type lru[V any] struct {
	max   int
	total int
	order *list.List // front is most recently used
	items map[store.ID]*list.Element
}

type lruEntry[V any] struct {
	key    store.ID
	value  V
	weight int
}

func newLRU[V any](max int) *lru[V] {
	return &lru[V]{max: max, order: list.New(), items: map[store.ID]*list.Element{}}
}

func (c *lru[V]) get(key store.ID) (V, bool) {
	el, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*lruEntry[V]).value, true
}

// put stores a value, replacing whatever was under the key, and evicts until
// the cache is inside its budget.
//
// A single value larger than the whole budget is stored and then immediately
// evicted by the loop, which is deliberate: the alternative is a cache that
// silently refuses to hold the one directory a caller is working in, and a
// miss that no amount of use can turn into a hit.
func (c *lru[V]) put(key store.ID, value V, weight int) {
	if el, ok := c.items[key]; ok {
		e := el.Value.(*lruEntry[V])
		c.total += weight - e.weight
		e.value, e.weight = value, weight
		c.order.MoveToFront(el)
	} else {
		c.total += weight
		c.items[key] = c.order.PushFront(&lruEntry[V]{key: key, value: value, weight: weight})
	}
	for c.total > c.max && c.order.Len() > 1 {
		c.evictOldest()
	}
}

func (c *lru[V]) evictOldest() {
	el := c.order.Back()
	if el == nil {
		return
	}
	e := el.Value.(*lruEntry[V])
	c.order.Remove(el)
	delete(c.items, e.key)
	c.total -= e.weight
}

// weight is what the cache currently holds, for tests and for anything that
// wants to report on it.
func (c *lru[V]) weight() int { return c.total }

// len is how many values are cached.
func (c *lru[V]) len() int { return c.order.Len() }
