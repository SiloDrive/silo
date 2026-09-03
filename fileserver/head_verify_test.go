package silod

import (
	"net/http"
	"testing"

	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/store"
)

// A head move publishes everything its commit reaches, and the server is the
// only party that can check the whole of it is there. It used to check the
// commit and its root directory and nothing beneath: a manifest naming a
// chunk nobody uploaded published fine, and the hole surfaced on another
// device at read time, with nothing in the server's logs to say when it was
// made.
func TestAHeadMoveOverAMissingChunkIsRefused(t *testing.T) {
	libraryID, acct := testLibrary(t)
	vars := map[string]string{"libraryid": libraryID}
	library, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		t.Fatal(err)
	}

	absent := store.ChunkID([]byte("a chunk that was never uploaded"))
	m := &store.Manifest{FileSize: 100000, Chunks: []store.ChunkRef{{ID: absent, Size: 100000}}}
	manifestBytes, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	manifestID := store.ObjectID(manifestBytes)
	dir := &store.Directory{Entries: []store.DirEntry{{
		ChildID: manifestID, Type: store.NodeFile, Name: []byte("hole.bin"), Mode: 0o644,
	}}}
	dirBytes, err := dir.Encode()
	if err != nil {
		t.Fatal(err)
	}
	rootID := store.ObjectID(dirBytes)
	parent, err := store.ParseID(library.HeadCommitID)
	if err != nil {
		t.Fatal(err)
	}
	commitBytes, commitID := buildCommit(t, rootID, []store.ID{parent}, acct.Email)

	for _, o := range []struct {
		id store.ID
		b  []byte
	}{{manifestID, manifestBytes}, {rootID, dirBytes}, {commitID, commitBytes}} {
		w := idReq(t, putObjectHandler, acct, http.MethodPut, "/objects/"+o.id.String(),
			merge(vars, "id", o.id.String()), o.b, nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("PUT %s = %d (%s), want 201", o.id, w.Code, w.Body.String())
		}
	}

	w := idReq(t, putHeadHandler, acct, http.MethodPut, "/head", vars,
		[]byte(commitID.String()), map[string]string{"If-Match": `"` + library.HeadCommitID + `"`})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("head move over a missing chunk = %d (%s), want 400", w.Code, w.Body.String())
	}
	after, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	if after.HeadCommitID != library.HeadCommitID {
		t.Fatalf("the head moved to %s over a missing chunk", after.HeadCommitID)
	}
}

// The same for a directory: the delta walk happened to read every changed
// directory on the way to measuring the change, which made a missing one a
// 500 by accident. This pins it as a refusal by contract, so the accounting
// walk can be optimised without quietly losing the check.
func TestAHeadMoveOverAMissingSubdirectoryIsRefused(t *testing.T) {
	libraryID, acct := testLibrary(t)
	vars := map[string]string{"libraryid": libraryID}
	library, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		t.Fatal(err)
	}

	absent := store.ObjectID([]byte("a directory that was never uploaded"))
	dir := &store.Directory{Entries: []store.DirEntry{{
		ChildID: absent, Type: store.NodeDir, Name: []byte("missing"), Mode: defaultDirMode,
	}}}
	dirBytes, err := dir.Encode()
	if err != nil {
		t.Fatal(err)
	}
	rootID := store.ObjectID(dirBytes)
	parent, err := store.ParseID(library.HeadCommitID)
	if err != nil {
		t.Fatal(err)
	}
	commitBytes, commitID := buildCommit(t, rootID, []store.ID{parent}, acct.Email)
	for _, o := range []struct {
		id store.ID
		b  []byte
	}{{rootID, dirBytes}, {commitID, commitBytes}} {
		w := idReq(t, putObjectHandler, acct, http.MethodPut, "/objects/"+o.id.String(),
			merge(vars, "id", o.id.String()), o.b, nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("PUT %s = %d (%s), want 201", o.id, w.Code, w.Body.String())
		}
	}

	w := idReq(t, putHeadHandler, acct, http.MethodPut, "/head", vars,
		[]byte(commitID.String()), map[string]string{"If-Match": `"` + library.HeadCommitID + `"`})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("head move over a missing directory = %d (%s), want 400", w.Code, w.Body.String())
	}
	after, err := libmgr.GetWithReason(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	if after.HeadCommitID != library.HeadCommitID {
		t.Fatalf("the head moved to %s over a missing directory", after.HeadCommitID)
	}
}
