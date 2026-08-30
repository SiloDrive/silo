// Packs as the write path: appending into an open pack, and the three rules
// that close one.
//
// A pack seals at target size, or when its oldest frame has been sitting there
// too long, or at shutdown. The age rule is the one that is easy to leave out
// and the one that matters most: size alone leaves the single-copy window
// unbounded *in time*, because an open pack cannot be uploaded to a durable
// tier, so on a quiet server the day's last chunks sit on one disk until
// unrelated future traffic happens to fill the pack — which may be never. With
// an age bound the window becomes seal age plus upload-and-verify time, which
// is a number an operator can reason about. What it costs is occasional
// undersized packs, and compaction merges those.
package objstore

import (
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// packMaxAge is how long a pack may hold its oldest frame before it is sealed
// regardless of size. storage.md sets it at about five minutes.
//
// Read once, when a packStore is created, and kept on the store rather than
// consulted from the sweeper. The sweeper runs on its own goroutine, so a
// package variable it read on every tick would be shared mutable state with
// whatever set it — which is a data race in exactly the tests that shorten it.
var packMaxAge = 5 * time.Minute

// packSweep is how often the sealer looks. It only has to be fine enough that
// "sealed within about packMaxAge" is true, so it is a fraction of it rather
// than a second timer to reason about. Copied at creation, like packMaxAge.
var packSweep = 30 * time.Second

// The registry of open packs, one per store directory, for the whole process.
//
// Process-global rather than per ObjectStore, and that is forced rather than
// convenient. objstore.New is called once per loaded library — libmgr opens a
// Store for each one — so an open pack held on an ObjectStore would mean two
// instances appending to the same library through two file handles at two
// offsets, each believing it owned the end of the file. loadPackSet refuses
// that state when it finds it on disk; this is what stops it being created.
//
// Keyed by the store directory, which is what actually has one open pack:
// TypeDir(dataDir, objType), so chunks and objects pack separately and two
// data directories in one process (tests do this) never share.
var (
	packStoresMu sync.Mutex
	packStores   = map[string]*packStore{}
)

func packStoreFor(objDir string) *packStore {
	packStoresMu.Lock()
	defer packStoresMu.Unlock()
	if ps, ok := packStores[objDir]; ok {
		return ps
	}
	ps := newPackStore(objDir)
	packStores[objDir] = ps
	return ps
}

// forgetPackStore drops a store directory from the registry, so the next
// ObjectStore over it rediscovers its packs from disk. Simulating a restart is
// the only thing that wants this, and it is what a restart actually does.
func forgetPackStore(objDir string) {
	packStoresMu.Lock()
	defer packStoresMu.Unlock()
	delete(packStores, objDir)
}

// Close seals every open pack in this process and stops the sealers.
//
// Package-level because the registry is: an ObjectStore is opened per library
// and discarded, so a Close on one of those would either do nothing or close
// the store out from under every other holder. A server calls this once, on
// the way down.
//
// Sealing at shutdown is the third rule, and it is what makes a clean stop
// leave nothing half-open: the alternative is a pack that waits for the next
// start to be recovered, which works but leaves the last frames of the run
// outside any sealed pack for as long as the process is down.
func Close() error {
	packStoresMu.Lock()
	stores := make([]*packStore, 0, len(packStores))
	for _, ps := range packStores {
		stores = append(stores, ps)
	}
	packStoresMu.Unlock()

	var firstErr error
	for _, ps := range stores {
		if err := ps.close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// startSealer brings up the age sweeper, once, the first time anything writes
// a pack.
//
// Lazily, so that a process which never writes a pack — every install until
// the write path moves, and every test that only reads — has no goroutine and
// no ticker.
func (ps *packStore) startSealer() {
	ps.sealerOnce.Do(func() {
		ps.stop = make(chan struct{})
		ps.wg.Add(1)
		go func() {
			defer ps.wg.Done()
			t := time.NewTicker(ps.sweep)
			defer t.Stop()
			for {
				select {
				case <-ps.stop:
					return
				case <-t.C:
					ps.sealAged()
				}
			}
		}()
	})
}

// sealAged seals every open pack whose oldest frame is older than packMaxAge.
func (ps *packStore) sealAged() {
	for libraryID, set := range ps.snapshot() {
		if err := set.sealIfAged(ps.objDir, libraryID, ps.maxAge); err != nil {
			// Logged rather than returned: this is a background sweep with
			// nobody to answer to, and one library that cannot seal must not
			// stop the others from sealing. The frames are still in the pack
			// and still readable; what is lost is the seal, and the next sweep
			// tries again.
			log.Errorf("Failed to seal the aged pack of %s: %v", libraryID, err)
		}
	}
}

// snapshot copies the library map so the sweep does not hold the registry lock
// while it does fsyncs.
func (ps *packStore) snapshot() map[string]*packSet {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	out := make(map[string]*packSet, len(ps.libs))
	for k, v := range ps.libs {
		out[k] = v
	}
	return out
}

func (ps *packStore) close() error {
	ps.sealerMu.Lock()
	if ps.stop != nil && !ps.stopped {
		close(ps.stop)
		ps.stopped = true
	}
	ps.sealerMu.Unlock()
	ps.wg.Wait()

	var firstErr error
	for libraryID, set := range ps.snapshot() {
		if err := set.sealOpen(ps.objDir, libraryID); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("sealing the open pack of %s: %w", libraryID, err)
		}
	}
	return firstErr
}

// append writes one frame into a library's open pack.
func (ps *packStore) append(libraryID, objID string, frame []byte, sync bool) error {
	set, err := ps.set(libraryID)
	if err != nil {
		return err
	}
	ps.startSealer()
	return set.appendFrame(ps.objDir, libraryID, objID, frame, sync)
}

// appendFrame writes one frame into the library's open pack, opening one if
// there is none and sealing it if the append filled it.
//
// writeMu is what serialises writers, and it is a second lock rather than the
// set's because of what each protects. The set's RWMutex guards *which* packs
// exist, and readers take it for a moment to look; this one guards the append
// itself, and is held across the fsync. Sharing one lock would mean every read
// of the library waiting on some writer's fsync, which is a latency cost paid
// by the wrong request.
//
// Serialising writers at all is the one genuinely new coordination packs
// introduce — writes to distinct loose paths needed none. The batch surface is
// what keeps it cheap: POST chunks carries up to 256 frames, so this is taken
// once per request rather than once per chunk.
func (s *packSet) appendFrame(objDir, libraryID, objID string, frame []byte, sync bool) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	p, err := s.writablePack(objDir, libraryID)
	if err != nil {
		return err
	}
	if _, err := p.append(frame, objID, sync); err != nil {
		return err
	}
	if p.full() {
		return s.rotate(objDir, libraryID)
	}
	return nil
}

// writablePack returns the pack to append to, opening or rotating as needed.
// The caller holds writeMu.
func (s *packSet) writablePack(objDir, libraryID string) (*openPack, error) {
	s.mu.RLock()
	p := s.open
	s.mu.RUnlock()

	// A pack recovered from a previous run is not appended to. Its frames have
	// been sitting outside any sealed pack for at least as long as this process
	// was down, which is exactly the window the age rule exists to bound — so
	// it is sealed now rather than kept open until it happens to fill.
	if p != nil && p.recovered && len(p.snapshot()) > 0 {
		if err := s.rotate(objDir, libraryID); err != nil {
			return nil, err
		}
		p = nil
	} else if p != nil && p.recovered {
		// Empty, so there is nothing stale in it. Keep it and stop treating it
		// as inherited.
		p.recovered = false
	}

	if p != nil {
		return p, nil
	}

	fresh, err := createPack(objDir, libraryID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.open = fresh
	s.mu.Unlock()
	return fresh, nil
}

// rotate seals the open pack and leaves the library with none, so the next
// append opens a fresh one. The caller holds writeMu.
func (s *packSet) rotate(objDir, libraryID string) error {
	s.mu.Lock()
	p := s.open
	s.open = nil
	s.mu.Unlock()
	if p == nil {
		return nil
	}

	if err := p.seal(); err != nil {
		return err
	}
	sealed, err := openSealedPack(objDir, libraryID, p.id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.sealed = append(s.sealed, sealed)
	s.mu.Unlock()
	return nil
}

// sealOpen seals whatever is open, for shutdown.
//
// An empty pack is removed rather than sealed. Sealing it would leave a file
// holding a footer, a filter and no frames, which every later lookup would ask
// and every listing would walk — a permanent cost for a pack that never held
// anything.
func (s *packSet) sealOpen(objDir, libraryID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.RLock()
	p := s.open
	s.mu.RUnlock()
	if p == nil {
		return nil
	}
	if len(p.snapshot()) == 0 {
		return s.discardOpen(objDir, libraryID)
	}
	return s.rotate(objDir, libraryID)
}

// sealIfAged seals the open pack when its oldest frame has waited long enough.
func (s *packSet) sealIfAged(objDir, libraryID string, maxAge time.Duration) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.RLock()
	p := s.open
	s.mu.RUnlock()
	if p == nil || !p.olderThan(maxAge) {
		return nil
	}
	return s.rotate(objDir, libraryID)
}

// discardOpen throws away an open pack that never held a frame. The caller
// holds writeMu.
func (s *packSet) discardOpen(objDir, libraryID string) error {
	s.mu.Lock()
	p := s.open
	s.open = nil
	s.mu.Unlock()
	if p == nil {
		return nil
	}
	return p.discard(objDir, libraryID)
}
