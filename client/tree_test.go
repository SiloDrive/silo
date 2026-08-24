package client

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// treeServer is a fake Silo that knows the three surfaces a tree upload uses.
// It remembers what it was asked, because these tests are about what the
// client sends: how many requests, in what order, and carrying what.
type treeServer struct {
	features []string
	held     map[string]bool // chunks the server already has

	batches   [][]BatchOp // one entry per batch request
	chunksPut []string
	asked     [][]string // one entry per blocks/missing request
	puts      []string   // whole-file PUTs, the fallback path
	mkdirs    []string
}

func newTreeServer(t *testing.T, features ...string) (*treeServer, *APIClient) {
	t.Helper()
	ts := &treeServer{features: features, held: map[string]bool{}}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/api/silo/v1/server-info":
			_ = json.NewEncoder(w).Encode(ServerInfo{Version: "test", Features: ts.features})

		case path == "/api/silo/v1/libraries":
			_ = json.NewEncoder(w).Encode([]Library{{ID: "r1", Name: "Library", Chunker: testChunker()}})

		case strings.HasSuffix(path, "/blocks/missing"):
			var body struct {
				Blocks []string `json:"blocks"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			ts.asked = append(ts.asked, body.Blocks)
			missing := []string{}
			for _, id := range body.Blocks {
				if !ts.held[id] {
					missing = append(missing, id)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string][]string{"missing": missing})

		case strings.Contains(path, "/blocks/"):
			id := path[strings.LastIndex(path, "/")+1:]
			_, _ = io.Copy(io.Discard, r.Body)
			ts.chunksPut = append(ts.chunksPut, id)
			ts.held[id] = true

		case strings.HasSuffix(path, "/batch"):
			var body struct {
				Ops []BatchOp `json:"ops"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			ts.batches = append(ts.batches, body.Ops)
			_ = json.NewEncoder(w).Encode(BatchResult{CommitID: "c1", Ops: len(body.Ops), Changed: true})

		case strings.Contains(path, "/entries/"):
			entry := path[strings.Index(path, "/entries/")+len("/entries/"):]
			if strings.EqualFold(r.URL.Query().Get("type"), "dir") {
				ts.mkdirs = append(ts.mkdirs, "/"+entry)
			} else {
				_, _ = io.Copy(io.Discard, r.Body)
				ts.puts = append(ts.puts, "/"+entry)
			}

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	return ts, NewClient(srv.URL)
}

// sampleTree writes a small tree and returns its root:
//
//	tree/a.txt          "alpha"
//	tree/dup.txt        "alpha"   — same content as a.txt
//	tree/empty/         no files
//	tree/sub/b.txt      "beta"
func sampleTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "tree")
	for _, dir := range []string{root, filepath.Join(root, "empty"), filepath.Join(root, "sub")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(root, "a.txt"):        "alpha",
		filepath.Join(root, "dup.txt"):      "alpha",
		filepath.Join(root, "sub", "b.txt"): "beta",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestWalkTreeMapsPathsAndOrdersParentsFirst(t *testing.T) {
	root := sampleTree(t)

	dirs, files, skipped, err := walkTree(root, "/dest/tree")
	if err != nil {
		t.Fatal(err)
	}

	wantDirs := []string{"/dest/tree", "/dest/tree/empty", "/dest/tree/sub"}
	if fmt.Sprint(dirs) != fmt.Sprint(wantDirs) {
		t.Errorf("dirs = %v, want %v", dirs, wantDirs)
	}

	var remotes []string
	for _, f := range files {
		remotes = append(remotes, f.remote)
	}
	wantFiles := []string{"/dest/tree/a.txt", "/dest/tree/dup.txt", "/dest/tree/sub/b.txt"}
	if fmt.Sprint(remotes) != fmt.Sprint(wantFiles) {
		t.Errorf("files = %v, want %v", remotes, wantFiles)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v, want none", skipped)
	}
}

// A symlink is neither a directory to walk into nor a regular file to read, so
// it is reported rather than followed.
func TestWalkTreeReportsWhatItWillNotFollow(t *testing.T) {
	root := sampleTree(t)
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(filepath.Join(root, "a.txt"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, files, skipped, err := walkTree(root, "/tree")
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0] != link {
		t.Errorf("skipped = %v, want [%s]", skipped, link)
	}
	for _, f := range files {
		if f.local == link {
			t.Error("the symlink was uploaded as a regular file")
		}
	}
}

func TestUploadDirSendsBlocksOnceAndCommitsInOneBatch(t *testing.T) {
	root := sampleTree(t)
	ts, c := newTreeServer(t, "batch", "blocks")

	up, err := c.UploadDir("r1", "/", root, nil)
	if err != nil {
		t.Fatal(err)
	}

	// "alpha" appears twice in the tree and is one block, asked about once and
	// sent once. Dedup is across the tree, not within a file.
	if len(ts.asked) != 1 {
		t.Fatalf("asked %d times about missing blocks, want 1", len(ts.asked))
	}
	if len(ts.asked[0]) != 2 {
		t.Errorf("offered %d distinct blocks, want 2 (alpha, beta)", len(ts.asked[0]))
	}
	if len(ts.chunksPut) != 2 {
		t.Errorf("uploaded %d blocks, want 2", len(ts.chunksPut))
	}

	// One commit for the whole tree, with every mkdir ahead of every create.
	if len(ts.batches) != 1 {
		t.Fatalf("sent %d batches, want 1", len(ts.batches))
	}
	var ops []string
	seenCreate := false
	for _, op := range ts.batches[0] {
		ops = append(ops, op.Op+" "+op.Path)
		if op.Op == "create" {
			seenCreate = true
		}
		if op.Op == "mkdir" && seenCreate {
			t.Error("a mkdir follows a create; a parent would not exist yet")
		}
	}
	want := "[mkdir /tree mkdir /tree/empty mkdir /tree/sub create /tree/a.txt create /tree/dup.txt create /tree/sub/b.txt]"
	if fmt.Sprint(ops) != want {
		t.Errorf("ops = %v,\nwant %s", ops, want)
	}

	if up.Files != 3 || up.Dirs != 3 || up.Commits != 1 || up.ChunksSent != 2 || up.ChunksHeld != 0 {
		t.Errorf("summary = %+v, want 3 files, 3 dirs, 1 commit, 2 blocks sent, 0 held", up)
	}
}

// The point of the block surface: a second run of the same tree transfers no
// content at all.
func TestUploadDirSecondRunSendsNothing(t *testing.T) {
	root := sampleTree(t)
	ts, c := newTreeServer(t, "batch", "blocks")

	if _, err := c.UploadDir("r1", "/", root, nil); err != nil {
		t.Fatal(err)
	}
	sentFirst := len(ts.chunksPut)

	up, err := c.UploadDir("r1", "/", root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts.chunksPut) != sentFirst {
		t.Errorf("the second run uploaded %d more blocks, want 0", len(ts.chunksPut)-sentFirst)
	}
	if up.ChunksSent != 0 || up.ChunksHeld != 2 {
		t.Errorf("second run: sent %d, held %d; want 0 sent and 2 held", up.ChunksSent, up.ChunksHeld)
	}
}

// A server without the surfaces still works, one request at a time.
func TestUploadDirFallsBackToOneRequestPerEntry(t *testing.T) {
	root := sampleTree(t)
	ts, c := newTreeServer(t) // no features advertised

	up, err := c.UploadDir("r1", "/", root, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(ts.batches) != 0 || len(ts.chunksPut) != 0 {
		t.Error("the fallback used the batch or block surface")
	}
	wantMkdirs := "[/tree /tree/empty /tree/sub]"
	if fmt.Sprint(ts.mkdirs) != wantMkdirs {
		t.Errorf("mkdirs = %v, want %s", ts.mkdirs, wantMkdirs)
	}
	wantPuts := "[/tree/a.txt /tree/dup.txt /tree/sub/b.txt]"
	if fmt.Sprint(ts.puts) != wantPuts {
		t.Errorf("puts = %v, want %s", ts.puts, wantPuts)
	}
	if up.Commits != 6 {
		t.Errorf("commits = %d, want 6 — this is the shape the batch path exists to replace", up.Commits)
	}
}

func TestUploadDirCallsBackWithEveryCommittedFile(t *testing.T) {
	root := sampleTree(t)
	_, c := newTreeServer(t, "batch", "blocks")

	var landed []string
	if _, err := c.UploadDir("r1", "/", root, func(remote string) {
		landed = append(landed, remote)
	}); err != nil {
		t.Fatal(err)
	}

	want := "[/tree/a.txt /tree/dup.txt /tree/sub/b.txt]"
	if fmt.Sprint(landed) != want {
		t.Errorf("reported %v, want %s", landed, want)
	}
}

func TestUploadDirRefusesAFile(t *testing.T) {
	root := sampleTree(t)
	_, c := newTreeServer(t, "batch", "blocks")

	_, err := c.UploadDir("r1", "/", filepath.Join(root, "a.txt"), nil)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("err = %v, want a complaint that it is not a directory", err)
	}
}

// A tree bigger than one batch is split, and the split still puts every mkdir
// ahead of the creates that depend on it.
func TestApplyInBatchesSplitsOnTheOperationLimit(t *testing.T) {
	ts, c := newTreeServer(t, "batch", "blocks")

	ops := make([]BatchOp, 0, maxOpsPerBatch+10)
	for i := 0; i < maxOpsPerBatch+10; i++ {
		ops = append(ops, BatchOp{Op: "create", Path: fmt.Sprintf("/f%d", i)})
	}

	result := &TreeUpload{}
	if err := c.applyInBatches("r1", ops, nil, result); err != nil {
		t.Fatal(err)
	}
	if len(ts.batches) != 2 {
		t.Fatalf("sent %d batches, want 2", len(ts.batches))
	}
	if len(ts.batches[0]) != maxOpsPerBatch || len(ts.batches[1]) != 10 {
		t.Errorf("split %d/%d, want %d/10", len(ts.batches[0]), len(ts.batches[1]), maxOpsPerBatch)
	}
	if result.Commits != 2 {
		t.Errorf("commits = %d, want 2", result.Commits)
	}
}

// The body limit bites before the op limit when files are large, because a
// create op carries an id per block.
func TestApplyInBatchesSplitsOnTheBodyLimit(t *testing.T) {
	ts, c := newTreeServer(t, "batch", "blocks")

	blocks := make([]string, 20000) // ~860 KB of ids in one op
	for i := range blocks {
		blocks[i] = strings.Repeat("a", 40)
	}
	var ops []BatchOp
	for i := 0; i < 8; i++ {
		ops = append(ops, BatchOp{Op: "create", Path: fmt.Sprintf("/big%d", i), Blocks: blocks})
	}

	if err := c.applyInBatches("r1", ops, nil, &TreeUpload{}); err != nil {
		t.Fatal(err)
	}
	if len(ts.batches) < 2 {
		t.Fatalf("sent %d batches, want more than one — the body limit was ignored", len(ts.batches))
	}
	for i, batch := range ts.batches {
		size := 0
		for _, op := range batch {
			size += opBytes(op)
		}
		if size > maxBatchBodySize && len(batch) > 1 {
			t.Errorf("batch %d is %d bytes over %d ops, past the %d limit", i, size, len(batch), maxBatchBodySize)
		}
	}
}
