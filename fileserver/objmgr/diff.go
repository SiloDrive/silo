package objmgr

import (
	"encoding/base64"
	"fmt"
	"path"

	"github.com/dkam/silo/store"
)

// Change is one difference between two trees.
//
// Path is rendered the way the library's names are: plaintext in a plain
// library, and base64url of the SIV ciphertext in an E2EE one — which is
// exactly what `entries/{path}` routes on, so a client can take a path from
// here and use it there without transforming it. That is the reason a server
// with no key can still answer changes?since= at all.
type Change struct {
	Op      string // create | delete | modify | move
	Path    string
	OldPath string // for moves
	ID      store.ID
	Size    int64
	IsDir   bool
}

// Diff reports what changed between two roots.
//
// It is a Merkle diff, and that is the whole reason this is affordable: a
// subtree whose id is unchanged is unchanged, whatever it holds, so an
// identical child is skipped without being read. A one-file change in a
// million-file library reads the directories along one path and nothing else.
//
// It works without a content key, in both library types, because it reads
// public directory sections — child ids, types and stored names. Under E2EE
// the names are ciphertext and the server learns nothing from them; what it
// learns is the shape of the change, which it must, because it is the thing
// serving the answer.
//
// Sizes are read from the manifests of files that actually changed. That is
// bounded by the size of the diff rather than the size of the tree, which is
// what makes it a different question from the listing sidecar — a listing pays
// per entry shown, and this pays per entry that moved.
func (s *Store) Diff(oldRoot, newRoot store.ID) ([]Change, error) {
	var out []Change
	if err := s.diffDir(oldRoot, newRoot, "", &out); err != nil {
		return nil, err
	}
	return detectMoves(out), nil
}

// renderName turns a stored name into the segment a client would send back.
func (s *Store) renderName(name []byte) string {
	if s.e2ee {
		return base64.RawURLEncoding.EncodeToString(name)
	}
	return string(name)
}

func (s *Store) diffDir(oldID, newID store.ID, prefix string, out *[]Change) error {
	if oldID == newID {
		return nil
	}

	oldEntries, err := s.publicEntries(oldID)
	if err != nil {
		return err
	}
	newEntries, err := s.publicEntries(newID)
	if err != nil {
		return err
	}

	// Both lists are in strictly increasing bytewise order by stored name, so
	// this is a merge rather than a lookup per entry: the format's ordering
	// rule is what turns the comparison from O(n log n) into O(n).
	i, j := 0, 0
	for i < len(oldEntries) || j < len(newEntries) {
		switch {
		case j == len(newEntries):
			if err := s.emitSubtree(oldEntries[i], prefix, "delete", out); err != nil {
				return err
			}
			i++
		case i == len(oldEntries):
			if err := s.emitSubtree(newEntries[j], prefix, "create", out); err != nil {
				return err
			}
			j++
		default:
			o, n := oldEntries[i], newEntries[j]
			switch cmp := compareNames(o.Name, n.Name); {
			case cmp < 0:
				if err := s.emitSubtree(o, prefix, "delete", out); err != nil {
					return err
				}
				i++
			case cmp > 0:
				if err := s.emitSubtree(n, prefix, "create", out); err != nil {
					return err
				}
				j++
			default:
				if err := s.diffEntry(o, n, prefix, out); err != nil {
					return err
				}
				i++
				j++
			}
		}
	}
	return nil
}

// diffEntry compares two entries that share a name.
func (s *Store) diffEntry(o, n store.DirEntry, prefix string, out *[]Change) error {
	if o.ChildID == n.ChildID && o.Type == n.Type {
		return nil
	}
	p := path.Join(prefix, s.renderName(n.Name))

	// A name that was a file and is now a directory, or the reverse. Reported
	// as two operations because it is two: a client that treated it as a
	// modification would try to write a directory's bytes over a file.
	if o.Type != n.Type {
		if err := s.emitSubtree(o, prefix, "delete", out); err != nil {
			return err
		}
		return s.emitSubtree(n, prefix, "create", out)
	}

	if n.Type == store.NodeDir {
		return s.diffDir(o.ChildID, n.ChildID, p, out)
	}

	size, err := s.fileSize(n.ChildID)
	if err != nil {
		return err
	}
	*out = append(*out, Change{Op: "modify", Path: "/" + p, ID: n.ChildID, Size: size})
	return nil
}

// emitSubtree reports an entry, and every path beneath it when it is a
// directory.
//
// A directory that arrives with content is reported as itself AND as the paths
// inside it. A client applying the answer needs every path it must create or
// remove, and telling it only about the directory would leave it to enumerate
// the subtree itself — which is the round trip this endpoint exists to avoid.
func (s *Store) emitSubtree(e store.DirEntry, prefix, op string, out *[]Change) error {
	p := path.Join(prefix, s.renderName(e.Name))
	if e.Type == store.NodeDir {
		*out = append(*out, Change{Op: op, Path: "/" + p, ID: e.ChildID, IsDir: true})
		return s.walkPublic(e.ChildID, p, op, out)
	}
	size, err := s.fileSize(e.ChildID)
	if err != nil {
		return err
	}
	*out = append(*out, Change{Op: op, Path: "/" + p, ID: e.ChildID, Size: size})
	return nil
}

func (s *Store) walkPublic(dirID store.ID, prefix, op string, out *[]Change) error {
	entries, err := s.publicEntries(dirID)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := s.emitSubtree(e, prefix, op, out); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) publicEntries(id store.ID) ([]store.DirEntry, error) {
	d, err := s.GetDirectoryPublic(id)
	if err != nil {
		return nil, fmt.Errorf("directory %s: %w", id, err)
	}
	return d.Entries, nil
}

// fileSize reads a manifest's declared size. Public, so it works in both
// library types without a key.
func (s *Store) fileSize(id store.ID) (int64, error) {
	m, err := s.GetManifestPublic(id)
	if err != nil {
		return 0, fmt.Errorf("manifest %s: %w", id, err)
	}
	return m.FileSize, nil
}

func compareNames(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return int(a[i]) - int(b[i])
		}
	}
	return len(a) - len(b)
}

// detectMoves pairs a delete and a create of the same object into one move.
//
// Content addressing makes this exact where Seafile's diff had to guess: two
// entries with the same id ARE the same bytes, so a delete of an id and a
// create of that id in the same diff is a rename, not a coincidence worth
// heuristics. It matters because the alternative tells a client to delete and
// re-download a file that only moved.
//
// Only unambiguous pairs are folded — exactly one delete and one create of
// that id. A file copied and the original removed produces two creates for one
// delete, and calling one of them the move and the other a create would be a
// guess about which. Left as delete-plus-creates, the client's result is the
// same; it just transfers what it already has.
func detectMoves(changes []Change) []Change {
	deletes := make(map[store.ID][]int)
	creates := make(map[store.ID][]int)
	for i, c := range changes {
		switch c.Op {
		case "delete":
			deletes[c.ID] = append(deletes[c.ID], i)
		case "create":
			creates[c.ID] = append(creates[c.ID], i)
		}
	}

	dropped := make(map[int]bool)
	for id, ds := range deletes {
		cs := creates[id]
		if len(ds) != 1 || len(cs) != 1 {
			continue
		}
		d, c := ds[0], cs[0]
		if changes[d].IsDir != changes[c].IsDir {
			continue
		}
		changes[c].Op = "move"
		changes[c].OldPath = changes[d].Path
		dropped[d] = true
	}
	if len(dropped) == 0 {
		return changes
	}

	out := make([]Change, 0, len(changes)-len(dropped))
	for i, c := range changes {
		if !dropped[i] {
			out = append(out, c)
		}
	}
	return out
}
