package blockmgr

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

const repoID = "b1f2ad61-9164-418a-a47f-ab805dbd5694"

// blockID is the SHA-1 of testFile's contents, because that is what a block id
// is. The test used to write this content under an unrelated fixed id, which
// the store now refuses.
var blockID string

// Set from os.MkdirTemp in TestMain (t.TempDir needs a *testing.T, which
// TestMain has no access to) so this package's object store is its own and
// does not collide with the other packages' tests when they run in parallel.
// testFile lives under it too, rather than being written into the package dir.
var seafileConfPath string
var seafileDataDir string
var testFile string
var testContent string

func delFile() error {
	// testFile lives under seafileConfPath, so one RemoveAll covers both.
	err := os.RemoveAll(seafileConfPath)
	if err != nil {
		return err
	}

	return nil
}

func createFile() error {
	testContent = strings.Repeat("hello world!\n", 10)
	checkSum := sha1.Sum([]byte(testContent))
	blockID = hex.EncodeToString(checkSum[:])

	return os.WriteFile(testFile, []byte(testContent), 0666)
}

func TestMain(m *testing.M) {
	var err error
	seafileConfPath, err = os.MkdirTemp("", "silo-blockmgr-test")
	if err != nil {
		fmt.Printf("Failed to create test dir : %v\n", err)
		os.Exit(1)
	}
	seafileDataDir = filepath.Join(seafileConfPath, "seafile-data")
	testFile = filepath.Join(seafileConfPath, "output.data")

	err = createFile()
	if err != nil {
		fmt.Printf("Failed to create test file : %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	err = delFile()
	if err != nil {
		fmt.Printf("Failed to remove test file : %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func testBlockRead(t *testing.T) {
	var buf bytes.Buffer
	err := Read(repoID, blockID, &buf)
	if err != nil {
		t.Errorf("Failed to read block: %v\n", err)
	}
	if buf.String() != testContent {
		t.Errorf("Block content does not round trip\n")
	}
}

func testBlockWrite(t *testing.T) {
	inputFile, err := os.Open(testFile)
	if err != nil {
		t.Fatalf("Failed to open test file : %v\n", err)
	}
	defer func() { _ = inputFile.Close() }()

	err = Write(repoID, blockID, inputFile)
	if err != nil {
		t.Fatalf("Failed to write block: %v\n", err)
	}
}

func testBlockExists(t *testing.T) {
	ret := Exists(repoID, blockID)
	if !ret {
		t.Fatalf("Block is not exist\n")
	}

	filePath := path.Join(seafileDataDir, "storage", "blocks", repoID, blockID[:2], blockID[2:])
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("Failed to stat block: %v\n", err)
	}
	if fileInfo.Size() != int64(len(testContent)) {
		t.Errorf("Block is exist, but the size of file is incorrect.\n")
	}
}

func TestBlock(t *testing.T) {
	Init(seafileConfPath, seafileDataDir)
	testBlockWrite(t)
	testBlockRead(t)
	testBlockExists(t)
}

// A block id is the SHA-1 of the bytes stored. Writing other content under it
// is permanent damage: every later writer of that id skips it as already
// present, and reads serve the wrong bytes to everyone sharing the store.
func TestWriteRejectsContentThatDoesNotMatchItsID(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "seafile-data")
	Init(seafileConfPath, dataDir)
	t.Cleanup(func() { Init(seafileConfPath, seafileDataDir) })

	const claimedID = "0401fc662e3bc87a41f299a907c056aaf8322a27"
	if err := Write(repoID, claimedID, strings.NewReader("not the content of that id")); err == nil {
		t.Error("Write accepted content that does not hash to its id")
	}

	// Nothing may be left behind under the id it lied about.
	if Exists(repoID, claimedID) {
		t.Error("a rejected block was published anyway")
	}
	if _, err := os.Stat(path.Join(dataDir, "storage", "blocks", repoID,
		claimedID[:2], claimedID[2:])); err == nil {
		t.Error("a rejected block was left on disk")
	}
}

// The check must not be able to destroy a block that is already correct: it
// runs before the rename that publishes, so a bad upload of an id that already
// exists leaves the good copy alone.
func TestWriteRejectionLeavesAnExistingGoodBlockIntact(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "seafile-data")
	Init(seafileConfPath, dataDir)
	t.Cleanup(func() { Init(seafileConfPath, seafileDataDir) })

	good := "the real contents"
	checkSum := sha1.Sum([]byte(good))
	id := hex.EncodeToString(checkSum[:])

	if err := Write(repoID, id, strings.NewReader(good)); err != nil {
		t.Fatalf("Write returned %v", err)
	}
	if err := Write(repoID, id, strings.NewReader("impostor")); err == nil {
		t.Error("Write accepted impostor content for an existing id")
	}

	var buf bytes.Buffer
	if err := Read(repoID, id, &buf); err != nil {
		t.Fatalf("Read returned %v", err)
	}
	if buf.String() != good {
		t.Errorf("the stored block is now %q, want %q", buf.String(), good)
	}
}

// WriteBytes names a block from its own content, so the upload paths hash once
// instead of once to name the block and again to verify it. What has to hold
// is that the weaker-looking API is not actually weaker: the id it returns
// always describes the bytes it stored.
func TestWriteBytesNamesTheBlockFromItsContent(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "seafile-data")
	Init(seafileConfPath, dataDir)
	t.Cleanup(func() { Init(seafileConfPath, seafileDataDir) })

	content := "some block contents"
	checkSum := sha1.Sum([]byte(content))
	want := hex.EncodeToString(checkSum[:])

	id, err := WriteBytes(repoID, []byte(content), "")
	if err != nil {
		t.Fatalf("WriteBytes returned %v", err)
	}
	if id != want {
		t.Errorf("WriteBytes stored the block as %s, want %s", id, want)
	}

	var buf bytes.Buffer
	if err := Read(repoID, id, &buf); err != nil {
		t.Fatalf("Read returned %v", err)
	}
	if buf.String() != content {
		t.Errorf("stored block is %q, want %q", buf.String(), content)
	}
}

// A caller that was promised an id — the raw-block upload is handed one by the
// client — must have the block refused, and refused before anything is
// written, when the content turns out to be something else.
func TestWriteBytesRejectsAMismatchedPromisedID(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "seafile-data")
	Init(seafileConfPath, dataDir)
	t.Cleanup(func() { Init(seafileConfPath, seafileDataDir) })

	claimed := "0401fc662e3bc87a41f299a907c056aaf8322a27"
	content := []byte("not what the client claimed")

	if _, err := WriteBytes(repoID, content, claimed); err == nil {
		t.Fatal("WriteBytes accepted content that is not the id it was promised")
	}

	if Exists(repoID, claimed) {
		t.Error("the rejected block was stored under the claimed id")
	}
	checkSum := sha1.Sum(content)
	if actual := hex.EncodeToString(checkSum[:]); Exists(repoID, actual) {
		t.Error("the rejected block was stored under its own id instead")
	}
}
