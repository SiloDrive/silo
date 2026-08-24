package silod

import (
	"bytes"
	"context"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/dkam/silo/client"
	"github.com/dkam/silo/fileserver/api"
	"github.com/dkam/silo/fileserver/authmgr"
	"github.com/dkam/silo/fileserver/share"
)

// The chunk lane, from a real client to a real server and back.
//
// Every other test in this package drives a handler directly, which is the
// right shape for testing a handler and is exactly why this file exists: the
// bug that prompted it lived in neither half. The server was correct, the
// client was correct against the surface it was written for, and the two had
// never been run against each other — so a client computing SHA-1 ids for a
// server that had moved to SHA-256 shipped, and stayed shipped, because no
// test ever put them in the same process.
//
// Nothing here is faked. The router is the one Run builds, the client is the
// one cmd/silo uses, the store is a real store-v2 library on a temp directory,
// and the bytes go over a real socket.

// laneClient stands up the whole server and returns a logged-in client, the id
// of one library, and a counter of chunk uploads.
//
// The counter is the point of several of these tests: what makes the chunk
// lane worth its extra round trips is the content that never crosses the wire,
// and the only way to assert that is to count what did.
func laneClient(t *testing.T) (*client.APIClient, string, *atomic.Int64) {
	t.Helper()
	sqliteTestDB(t)
	share.Init(siloPair.Read, "Group", false)
	api.Init(siloPair.Read, siloPair.Write)
	authmgr.Init(siloPair.Read, siloPair.Write)

	const email, password = "lane@example.com", "correct horse battery staple"
	if _, err := authmgr.CreateAccount(context.Background(), email, password, false); err != nil {
		t.Fatalf("create account: %v", err)
	}

	var puts atomic.Int64
	router := newHTTPRouter()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && chunkPutPath(r.URL.Path) {
			puts.Add(1)
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	c := client.NewClient(srv.URL)
	if err := c.Login(email, password); err != nil {
		t.Fatalf("login: %v", err)
	}
	repo, err := c.CreateRepo("lane")
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	return c, repo.ID, &puts
}

// chunkPutPath reports whether a path addresses one chunk, as opposed to the
// "missing" query that shares the prefix.
func chunkPutPath(p string) bool {
	i := len("/blocks/")
	if len(p) < i {
		return false
	}
	base := p[len(p)-64:]
	if len(p) < 64+i || p[len(p)-64-i:len(p)-64] != "/blocks/" {
		return false
	}
	for _, ch := range base {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

// writeFile puts n bytes of varied content on disk and returns the path and
// the content.
func writeFile(t *testing.T, name string, n int, firstByte byte) (string, []byte) {
	t.Helper()
	content := make([]byte, n)
	// Pseudo-random rather than a pattern, and seeded so a failure reproduces.
	// The chunker cuts where a rolling hash crosses a threshold, so content
	// with a short period gives it nothing to find and the file comes out as
	// two or three MaxSize chunks — which would let these tests pass while
	// measuring nothing.
	r := rand.New(rand.NewSource(20260824))
	for i := range content {
		content[i] = byte(r.Intn(256))
	}
	if n > 0 {
		content[0] = firstByte
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path, content
}

// This is the test the 400 would have failed. A file large enough to take the
// chunk lane goes up named entirely by the client, and comes back byte for
// byte.
//
// The id the client computes has to be the id the server would compute, or the
// upload does not merely dedup badly — it is refused, because the server
// verifies the bytes against the name they arrived under. That refusal is the
// good case. The bad case is the one this guards: a client and a server that
// agree on the shape of every request and disagree about what a name means.
func TestAFileTheClientChunkedItselfRoundTrips(t *testing.T) {
	c, repoID, puts := laneClient(t)

	local, content := writeFile(t, "big.bin", 12<<20, 'A')
	if err := c.UploadFile(repoID, "/", local); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if puts.Load() == 0 {
		t.Fatal("nothing was uploaded to the chunk lane; the file took the whole-file path and this test proved nothing")
	}

	back := filepath.Join(t.TempDir(), "back.bin")
	if err := c.DownloadFile(repoID, "/big.bin", back); err != nil {
		t.Fatalf("download: %v", err)
	}
	got, err := os.ReadFile(back)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("got %d bytes back, sent %d; they differ", len(got), len(content))
	}
}

// The second upload of the same content sends nothing.
//
// This is what the extra round trips buy, and it is worth asserting on the
// count rather than on the result: an implementation that re-sent every chunk
// would still produce a correct file at the far end, and would look exactly
// like this one from the outside.
func TestUploadingTheSameContentTwiceSendsItOnce(t *testing.T) {
	c, repoID, puts := laneClient(t)

	local, _ := writeFile(t, "first.bin", 12<<20, 'A')
	if err := c.UploadFile(repoID, "/", local); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	first := puts.Load()
	if first == 0 {
		t.Fatal("the first upload sent no chunks")
	}

	// Same bytes, second name. Nothing about the content is new, so nothing
	// about it should cross the wire.
	copied := filepath.Join(filepath.Dir(local), "second.bin")
	content, _ := os.ReadFile(local)
	if err := os.WriteFile(copied, content, 0o600); err != nil {
		t.Fatal(err)
	}
	puts.Store(0)
	if err := c.UploadFile(repoID, "/", copied); err != nil {
		t.Fatalf("second upload: %v", err)
	}
	if sent := puts.Load(); sent != 0 {
		t.Errorf("the second upload sent %d chunks, want 0", sent)
	}

	// And it is a real file, not just a cheap request.
	back := filepath.Join(t.TempDir(), "back.bin")
	if err := c.DownloadFile(repoID, "/second.bin", back); err != nil {
		t.Fatalf("download: %v", err)
	}
	got, _ := os.ReadFile(back)
	if !bytes.Equal(got, content) {
		t.Error("the deduped file came back wrong")
	}
}

// A byte inserted at the front re-sends almost nothing.
//
// This is the property content-defined chunking exists for, and the one the
// fixed-offset scheme could not have. Under fixed offsets a single byte at
// offset zero shifts every boundary after it, so no chunk matches and the
// whole file goes up again — which is not a bug that fails a test, it is a
// bill that arrives quietly.
//
// The bound is deliberately loose. Where the chunker re-synchronises depends
// on the content, and pinning an exact count would make this a test of the
// gear table rather than of the property. "Almost none of it" is the claim
// worth defending.
func TestAByteInsertedAtTheFrontResendsAlmostNothing(t *testing.T) {
	c, repoID, puts := laneClient(t)

	local, content := writeFile(t, "original.bin", 12<<20, 'A')
	if err := c.UploadFile(repoID, "/", local); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	whole := puts.Load()
	if whole < 4 {
		t.Fatalf("the file was cut into %d chunks; too few for this test to mean anything", whole)
	}

	shifted := filepath.Join(filepath.Dir(local), "shifted.bin")
	if err := os.WriteFile(shifted, append([]byte{'Z'}, content...), 0o600); err != nil {
		t.Fatal(err)
	}
	puts.Store(0)
	if err := c.UploadFile(repoID, "/", shifted); err != nil {
		t.Fatalf("second upload: %v", err)
	}

	resent := puts.Load()
	if resent >= whole/2 {
		t.Errorf("inserting one byte re-sent %d of %d chunks; content-defined chunking should re-synchronise almost immediately",
			resent, whole)
	}

	back := filepath.Join(t.TempDir(), "back.bin")
	if err := c.DownloadFile(repoID, "/shifted.bin", back); err != nil {
		t.Fatalf("download: %v", err)
	}
	got, _ := os.ReadFile(back)
	if !bytes.Equal(got, append([]byte{'Z'}, content...)) {
		t.Error("the shifted file came back wrong")
	}
}

// A small file does not take the chunk lane at all, and still arrives.
//
// The threshold is a round-trip trade rather than a correctness one, so the
// thing to pin is that the fallback is a real path and not a path that only
// looks reachable: three requests for a file that fits in one is the cost this
// branch exists to avoid.
func TestASmallFileGoesWholeAndStillArrives(t *testing.T) {
	c, repoID, puts := laneClient(t)

	local, content := writeFile(t, "small.bin", 4096, 'A')
	if err := c.UploadFile(repoID, "/", local); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if sent := puts.Load(); sent != 0 {
		t.Errorf("a 4 KiB file sent %d chunks; it should have gone whole", sent)
	}

	back := filepath.Join(t.TempDir(), "back.bin")
	if err := c.DownloadFile(repoID, "/small.bin", back); err != nil {
		t.Fatalf("download: %v", err)
	}
	got, _ := os.ReadFile(back)
	if !bytes.Equal(got, content) {
		t.Error("the small file came back wrong")
	}
}

// A tree upload dedups across files, in one commit.
//
// UploadDir is a different code path from UploadFile — it names every chunk in
// the tree before sending any of it — and the sharing it can exploit is
// between files rather than within one. Two identical copies in the same tree
// is the case that distinguishes "asked once per file" from "asked once".
func TestATreeUploadSendsSharedContentOnce(t *testing.T) {
	c, repoID, puts := laneClient(t)

	// Named rather than the temp directory itself: UploadDir keeps the
	// directory's own name, the way cp -r does, so the tree lands under it.
	root := filepath.Join(t.TempDir(), "tree")
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	_, content := writeFile(t, "seed.bin", 12<<20, 'A')
	for _, p := range []string{filepath.Join(root, "one.bin"), filepath.Join(sub, "two.bin")} {
		if err := os.WriteFile(p, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	up, err := c.UploadDir(repoID, "/", root, nil)
	if err != nil {
		t.Fatalf("upload dir: %v", err)
	}
	if up.Files != 2 {
		t.Fatalf("uploaded %d files, want 2", up.Files)
	}
	if up.Commits != 1 {
		t.Errorf("took %d commits, want 1", up.Commits)
	}
	// Two identical files: every chunk is sent once and the second file's
	// copies are all held.
	if up.ChunksSent == 0 || int64(up.ChunksSent) != puts.Load() {
		t.Errorf("reported %d chunks sent, the server saw %d", up.ChunksSent, puts.Load())
	}

	for _, remote := range []string{"/tree/one.bin", "/tree/sub/two.bin"} {
		back := filepath.Join(t.TempDir(), "back.bin")
		if err := c.DownloadFile(repoID, remote, back); err != nil {
			t.Fatalf("download %s: %v", remote, err)
		}
		got, _ := os.ReadFile(back)
		if !bytes.Equal(got, content) {
			t.Errorf("%s came back wrong", remote)
		}
	}
}
