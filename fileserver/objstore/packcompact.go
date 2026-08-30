// Compaction: rewriting a sealed pack without the frames nothing reaches.
//
// A pack is immutable once sealed, which is what lets a tier replicate it as a
// file copy and what makes "reclaim by rewrite, never by hole reuse" a rule
// rather than a preference. So reclaiming a dead frame means writing a new pack
// holding only the live ones and taking the old one away.
//
// The frames are copied, not re-sealed. A frame is self-describing and bound to
// its id, so moving one between packs changes nothing about it — which means
// compaction needs no storage key and can run on a store whose key is gone.
//
// docs/plans/compaction.md owns the decisions; the three that shape this file
// are the temporary name (so a rewrite cannot be mistaken for a second writer),
// the add-then-remove ordering (so a lookup never falls into a window where
// neither pack holds a live frame), and the redundancy rule (so an interrupted
// rewrite is decidable from the indexes alone).
package objstore

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Compaction is what one rewrite did.
type Compaction struct {
	// PackID is the pack that was rewritten or removed. NewPackID is what
	// replaced it, or empty when nothing did — a pack with no live frames left
	// is deleted rather than rewritten into an empty one.
	PackID    string
	NewPackID string

	Kept    int64
	Dropped int64
	// Reclaimed is the bytes the old pack occupied that the new one does not.
	// Measured over the whole file rather than over frames, because the footer
	// and the filter shrink with the record count and an operator watching a
	// disk cares about the file.
	Reclaimed int64
}

// CompactPack rewrites one sealed pack, keeping only what live reports as
// reachable.
//
// live is re-evaluated here rather than taken from a stored PackStats row,
// which is the whole point of that row being a scheduling input: an object
// nothing reached when the mark ran may be reachable by the time a rewrite
// starts, and dropping it would delete data a client believes it has stored.
// The caller supplies a fresh answer and this trusts it for the length of one
// pack.
//
// No storage key is needed and none is asked for. Frames are copied.
func (s *ObjectStore) CompactPack(libraryID, packID string, live func(objID string) bool) (Compaction, error) {
	if err := s.present(); err != nil {
		return Compaction{}, err
	}
	set, err := s.packs.set(libraryID)
	if err != nil {
		return Compaction{}, err
	}
	return set.compact(packID, live)
}

// compact is CompactPack with the library's packs already in hand.
//
// It takes compactMu rather than writeMu, and that distinction matters: a
// rewrite copies up to a whole pack and takes as long as that costs, so holding
// the lock the write path takes would stall every upload for the duration.
// Compaction touches only sealed packs, which the write path never writes to,
// so the two need no mutual exclusion at all — only compactions need excluding
// from each other.
func (s *packSet) compact(packID string, live func(objID string) bool) (Compaction, error) {
	s.compactMu.Lock()
	defer s.compactMu.Unlock()

	old := s.sealedPack(packID)
	if old == nil {
		return Compaction{}, fmt.Errorf("%s is not a sealed pack of %s", packID, s.libraryID)
	}

	var keep []indexEntry
	var dropped int64
	for _, e := range old.entries() {
		if live(e.ID) {
			keep = append(keep, e)
			continue
		}
		dropped++
	}
	result := Compaction{PackID: packID, Kept: int64(len(keep)), Dropped: dropped}
	if dropped == 0 {
		// Nothing to reclaim. Rewriting would copy the whole pack to produce
		// an identical one under a new name.
		return result, nil
	}

	before, err := fileSize(old.path)
	if err != nil {
		return result, err
	}

	// A pack nothing reaches at all needs no rewrite. Dropping it whole is
	// both cheaper and the case most worth getting right: a library whose
	// history has just been expired produces exactly this.
	if len(keep) == 0 {
		if err := s.dropSealed(old); err != nil {
			return result, err
		}
		result.Reclaimed = before
		return result, nil
	}

	fresh, err := createTmpPack(s.objDir, s.libraryID)
	if err != nil {
		return result, err
	}
	if err := copyFrames(old, fresh, keep); err != nil {
		_ = fresh.discard()
		return result, err
	}
	// Sealed under the temporary name: the footer is written and fsynced and
	// the sidecar removed, so what is on disk is a complete pack that nothing
	// loads yet.
	if err := fresh.seal(); err != nil {
		return result, fmt.Errorf("sealing the rewrite of %s: %v", packID, err)
	}

	// Publication. Until this rename the old pack is the authority and a crash
	// leaves only debris; after it, both packs hold every live frame, which is
	// the window pruneRedundant closes.
	finalPath, _ := packPaths(s.objDir, s.libraryID, fresh.id)
	if err := os.Rename(fresh.path, finalPath); err != nil {
		return result, fmt.Errorf("publishing the rewrite of %s: %v", packID, err)
	}
	if err := syncDir(packDir(s.objDir, s.libraryID)); err != nil {
		return result, err
	}

	sealed, err := openSealedPack(s.objDir, s.libraryID, fresh.id)
	if err != nil {
		return result, fmt.Errorf("reading back the rewrite of %s: %v", packID, err)
	}
	after, err := fileSize(sealed.path)
	if err != nil {
		return result, err
	}

	// Add and remove under one lock hold, which is stronger than the
	// add-then-remove the plan asks for: no reader sees a moment with only one
	// of them, in either direction.
	s.mu.Lock()
	s.sealed = append(s.sealed, sealed)
	s.sealed = withoutPack(s.sealed, packID)
	s.mu.Unlock()

	if err := removePackFiles(old.path); err != nil {
		return result, err
	}
	result.NewPackID = sealed.id
	result.Reclaimed = before - after
	return result, nil
}

// copyFrames moves the live frames of one pack into another, **in the order
// they physically sit in the old pack**.
//
// That is not the order the index gives them. A footer is sorted by id, so
// entries() hands back id order, and ids are SHA-256 — uniformly random, and
// therefore uncorrelated with anything. Copying in that order would scatter a
// file's chunks across the new pack, and because id order is stable, every
// later rewrite would preserve the scattering. One compaction would destroy
// locality permanently rather than degrade it.
//
// The physical order is recoverable and costs a sort: the frames were appended
// in arrival order and every index record carries its offset, so sorting by
// offset reconstructs exactly the layout the pack was written with. Compaction
// then preserves whatever locality the writer achieved instead of discarding
// it — and, because dead frames are dropped, the survivors end up closer
// together than they were.
//
// It also makes the copy itself a forward scan of the old pack rather than a
// random walk over it.
//
// What this does not do is *improve* locality. Chunks of one file that were
// split across two packs when the first one sealed stay split, because nothing
// here knows which file a chunk belongs to. storage.md wants file-order
// locality; that needs information from above this seam.
func copyFrames(from *sealedPack, to *openPack, keep []indexEntry) error {
	inPackOrder := make([]indexEntry, len(keep))
	copy(inPackOrder, keep)
	sort.Slice(inPackOrder, func(i, j int) bool { return inPackOrder[i].Offset < inPackOrder[j].Offset })

	for _, e := range inPackOrder {
		frame, err := from.readFrameAt(e)
		if err != nil {
			return fmt.Errorf("reading %s out of %s: %v", e.ID, from.id, err)
		}
		// sync is false per frame: the pack is fsynced once when it is sealed,
		// and nothing may read it before then because nothing can find it.
		if _, err := to.append(frame, e.ID, false); err != nil {
			return fmt.Errorf("writing %s into %s: %v", e.ID, to.id, err)
		}
	}
	return nil
}

// dropSealed removes a pack from the set and from the disk.
func (s *packSet) dropSealed(p *sealedPack) error {
	s.mu.Lock()
	s.sealed = withoutPack(s.sealed, p.id)
	s.mu.Unlock()
	return removePackFiles(p.path)
}

// sealedPack finds one of the library's sealed packs by id.
func (s *packSet) sealedPack(packID string) *sealedPack {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.sealed {
		if p.id == packID {
			return p
		}
	}
	return nil
}

func withoutPack(packs []*sealedPack, packID string) []*sealedPack {
	out := packs[:0]
	for _, p := range packs {
		if p.id != packID {
			out = append(out, p)
		}
	}
	return out
}

// removePackFiles deletes a pack and any sidecar beside it, idempotently.
func removePackFiles(packPath string) error {
	for _, p := range []string{packPath, sidecarPathFor(packPath)} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return syncDir(filepath.Dir(packPath))
}

// sidecarPathFor is the index that would sit beside a pack. A sealed one has
// none; this exists so that removing a pack cannot leave one behind whatever
// state it was in.
func sidecarPathFor(packPath string) string {
	return packPath[:len(packPath)-len(".pack")] + ".idx"
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// PruneRedundantPacks deletes packs whose every object is also in another pack,
// and removes the debris of an interrupted rewrite.
//
// This is the crash rule, and it is stated as a property of the store rather
// than as "finish what compaction started" — which is what keeps it from being
// a second mechanism with failure modes of its own. **A pack every one of whose
// ids lives in another pack holds nothing that would be lost by deleting it**,
// and that is decidable from the indexes alone: no frame is read, no key is
// needed, and it is true whether the duplication came from a rewrite that was
// interrupted after publishing, from two writers racing past dedup, or from
// anything else.
//
// It runs from the compaction scheduler rather than at load. The check costs a
// pass over every index in the library, which is affordable beside a rewrite
// and is not something to put in front of the first read of a library.
//
// The larger pack is kept when two hold exactly the same ids. Either would do;
// preferring the larger keeps the tie-break deterministic rather than dependent
// on directory order, which is what stops two runs disagreeing about which of a
// pair to remove and deleting both.
func (s *ObjectStore) PruneRedundantPacks(libraryID string) (int, error) {
	if err := s.present(); err != nil {
		return 0, err
	}
	set, err := s.packs.set(libraryID)
	if err != nil {
		return 0, err
	}
	return set.pruneRedundant()
}

func (s *packSet) pruneRedundant() (int, error) {
	s.compactMu.Lock()
	defer s.compactMu.Unlock()

	if err := s.clearTmpDebris(); err != nil {
		return 0, err
	}

	_, sealed := s.packs()
	if len(sealed) < 2 {
		return 0, nil
	}

	// Ids held by each pack, and how many packs hold each id. A pack is
	// redundant when every one of its ids is held by at least one other.
	holders := map[string]int{}
	ids := make([]map[string]struct{}, len(sealed))
	for i, p := range sealed {
		ids[i] = map[string]struct{}{}
		for _, e := range p.entries() {
			ids[i][e.ID] = struct{}{}
		}
		for id := range ids[i] {
			holders[id]++
		}
	}

	// Largest first, so that of two packs holding the same ids the smaller is
	// the one considered for removal.
	order := make([]int, len(sealed))
	for i := range order {
		order[i] = i
	}
	sortByFileSizeDesc(sealed, order)

	removed := 0
	for _, i := range order {
		p := sealed[i]
		if len(ids[i]) == 0 {
			// An empty sealed pack holds nothing anybody can lose.
			if err := s.dropSealed(p); err != nil {
				return removed, err
			}
			removed++
			continue
		}
		redundant := true
		for id := range ids[i] {
			if holders[id] < 2 {
				redundant = false
				break
			}
		}
		if !redundant {
			continue
		}
		// Give up this pack's claim on its ids before deleting it, so that two
		// packs holding exactly the same set cannot both be judged redundant.
		for id := range ids[i] {
			holders[id]--
		}
		if err := s.dropSealed(p); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// clearTmpDebris removes what an interrupted rewrite left behind.
//
// Roll back rather than forward, which is available because the old pack was
// never touched: a rewrite that did not reach its rename contributed nothing,
// and the cheapest correct thing to do with it is to throw it away and let the
// scheduler decide again.
func (s *packSet) clearTmpDebris() error {
	dir := packDir(s.objDir, s.libraryID)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != packTmpSuffix {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return syncDir(dir)
}

// sortByFileSizeDesc orders indices so the largest pack comes first. A pack
// whose size cannot be read sorts last: it is the one least safe to keep, and
// least safe to delete on the strength of a number that could not be read.
func sortByFileSizeDesc(packs []*sealedPack, order []int) {
	size := make([]int64, len(packs))
	for i, p := range packs {
		if n, err := fileSize(p.path); err == nil {
			size[i] = n
		} else {
			size[i] = -1
		}
	}
	sort.SliceStable(order, func(a, b int) bool { return size[order[a]] > size[order[b]] })
}
