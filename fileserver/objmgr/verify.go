package objmgr

import (
	"errors"
	"fmt"

	"github.com/dkam/silo/fileserver/objstore"
	"github.com/dkam/silo/store"
)

// MissingObject is what VerifyDelta returns when a tree names something the
// store does not hold. It is a type rather than a sentinel so the caller can
// say which object, and it is a client error: the client named it, and the
// client is the one that can upload it.
type MissingObject struct {
	Kind string // "directory", "manifest" or "chunk"
	ID   store.ID
}

func (e *MissingObject) Error() string {
	return fmt.Sprintf("%s %s is not in this library", e.Kind, e.ID)
}

// VerifyDelta checks that everything newRoot reaches and oldRoot does not is
// in the store: every changed directory, every manifest under it, and every
// chunk those manifests name.
//
// This is the head move's check that a commit is publishable, and it is the
// only place it can be made. A client uploads objects one at a time and each
// upload is verified on its own, but nothing until the head move says the set
// is complete -- and after the head move a reader on another device is the
// one that finds the hole. The walk is the delta walk MeasureDelta makes, so a
// one-file change in a million-file library reads one path and one manifest;
// it costs one index lookup per chunk the change names, which is bounded by
// what the client just uploaded.
//
// It needs no key. Directory entries, manifest chunk lists and chunk ids are
// public in both library types, which is the same fact the collector's mark
// rests on.
// A DAG bomb costs what its objects cost rather than what its paths do: see
// [walk]. Verifying is idempotent, so a subtree already checked is skipped,
// and the walk ends up bounded by the number of distinct objects the library
// holds — which is the bound that belongs on it.
func (s *Store) VerifyDelta(oldRoot, newRoot store.ID) error {
	return s.verifyDeltaWalk(newVerifyWalk(), oldRoot, newRoot)
}

func (s *Store) verifyDeltaWalk(w *walk, oldRoot, newRoot store.ID) error {
	if !w.enterPair(oldRoot, newRoot) {
		return nil
	}
	return s.mergeDirs(w, oldRoot, newRoot,
		func(store.DirEntry) error { return nil },
		func(n store.DirEntry) error { return s.verifyEntry(w, n) },
		func(o, n store.DirEntry) error {
			if o.ChildID == n.ChildID && o.Type == n.Type {
				return nil
			}
			if o.Type == store.NodeDir && n.Type == store.NodeDir {
				return s.verifyDelta(w, o.ChildID, n.ChildID)
			}
			return s.verifyEntry(w, n)
		},
	)
}

// verifyDelta is VerifyDelta below the root, with the missing-directory case
// attributed: mergeDirs reads both directories itself, and a new one that is
// not there has to be reported as the new one.
func (s *Store) verifyDelta(w *walk, oldID, newID store.ID) error {
	err := s.verifyDeltaWalk(w, oldID, newID)
	if errors.Is(err, objstore.ErrNotFound) {
		if ok, hErr := s.HasObject(newID); hErr == nil && !ok {
			return &MissingObject{Kind: "directory", ID: newID}
		}
	}
	return err
}

// verifyEntry checks one entry and everything beneath it.
func (s *Store) verifyEntry(w *walk, e store.DirEntry) error {
	// A subtree that is complete is complete however many names reach it, so
	// the second visit has nothing to learn and the walk skips it. That is
	// what turns a DAG bomb from 2^n paths into n objects.
	if !w.enterNode(e.ChildID) {
		return nil
	}
	if e.Type == store.NodeDir {
		d, err := s.GetDirectoryPublic(e.ChildID)
		if err != nil {
			if errors.Is(err, objstore.ErrNotFound) {
				return &MissingObject{Kind: "directory", ID: e.ChildID}
			}
			return fmt.Errorf("directory %s: %w", e.ChildID, err)
		}
		for _, child := range d.Entries {
			if err := s.verifyEntry(w, child); err != nil {
				return err
			}
		}
		return nil
	}
	// NodeFile and NodeSymlink both name a manifest, as in markTree.
	m, err := s.GetManifestPublic(e.ChildID)
	if err != nil {
		if errors.Is(err, objstore.ErrNotFound) {
			return &MissingObject{Kind: "manifest", ID: e.ChildID}
		}
		return fmt.Errorf("manifest %s: %w", e.ChildID, err)
	}
	for _, c := range m.Chunks {
		ok, err := s.HasChunk(c.ID)
		if err != nil {
			return fmt.Errorf("chunk %s: %w", c.ID, err)
		}
		if !ok {
			return &MissingObject{Kind: "chunk", ID: c.ID}
		}
	}
	return nil
}
