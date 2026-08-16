package objstore

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

const (
	repoID = "b1f2ad61-9164-418a-a47f-ab805dbd5694"
	objID  = "0401fc662e3bc87a41f299a907c056aaf8322a27"
)

// Set from os.MkdirTemp in TestMain (t.TempDir needs a *testing.T, which
// TestMain has no access to) so this package's object store is its own and
// does not collide with the other packages' tests when they run in parallel.
// testFile lives under it too, rather than being written into the package dir.
var seafileConfPath string
var seafileDataDir string
var testFile string

func createFile() error {
	outputFile, err := os.OpenFile(testFile, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		return err
	}
	defer func() { _ = outputFile.Close() }()

	outputString := "hello world!\n"
	for i := 0; i < 10; i++ {
		_, _ = outputFile.WriteString(outputString)
	}

	return nil
}

func delFile() error {
	// testFile lives under seafileConfPath, so one RemoveAll covers both.
	err := os.RemoveAll(seafileConfPath)
	if err != nil {
		return err
	}

	return nil
}

func TestMain(m *testing.M) {
	var err error
	seafileConfPath, err = os.MkdirTemp("", "silo-objstore-test")
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

func testWrite(t *testing.T) {
	inputFile, err := os.Open(testFile)
	if err != nil {
		t.Errorf("Failed to open test file : %v\n", err)
	}
	defer func() { _ = inputFile.Close() }()

	bend := New(seafileConfPath, seafileDataDir, "commit")
	_ = bend.Write(repoID, objID, inputFile, true)
}

func testRead(t *testing.T) {
	outputFile, err := os.OpenFile(testFile, os.O_WRONLY, 0666)
	if err != nil {
		t.Errorf("Failed to open test file:%v\n", err)
	}
	defer func() { _ = outputFile.Close() }()

	bend := New(seafileConfPath, seafileDataDir, "commit")
	err = bend.Read(repoID, objID, outputFile)
	if err != nil {
		t.Errorf("Failed to read backend : %s\n", err)
	}
}

func testExists(t *testing.T) {
	bend := New(seafileConfPath, seafileDataDir, "commit")
	ret, _ := bend.Exists(repoID, objID)
	if !ret {
		t.Errorf("File is not exist\n")
	}

	filePath := path.Join(seafileDataDir, "storage", "commit", repoID, objID[:2], objID[2:])
	fileInfo, _ := os.Stat(filePath)
	if fileInfo.Size() != 130 {
		t.Errorf("File is exist, but the size of file is incorrect.\n")
	}
}

func TestObjStore(t *testing.T) {
	testWrite(t)
	testRead(t)
	testExists(t)
}

// The store fans objects out as objID[:2]/objID[2:], which panics outright on
// an ID shorter than two characters, so every entry point must reject a
// malformed ID rather than slicing it.
func TestObjStoreRejectsInvalidObjectID(t *testing.T) {
	bad := []string{
		"",
		"a",
		"ab",
		"../../../etc/passwd",
		"0401fc662e3bc87a41f299a907c056aaf8322a2",   // 39 chars
		"0401fc662e3bc87a41f299a907c056aaf8322a277", // 41 chars
		"0401FC662E3BC87A41F299A907C056AAF8322A27",  // uppercase hex
		"0401fc662e3bc87a41f299a907c056aaf8322g27",  // non-hex
	}

	bend := New(seafileConfPath, seafileDataDir, "commit")
	for _, id := range bad {
		// Each of these would panic rather than return if the guard were gone.
		if err := bend.Read(repoID, id, io.Discard); err == nil {
			t.Errorf("Read(%q) returned nil error, want rejection", id)
		}
		if err := bend.Write(repoID, id, strings.NewReader("data"), true); err == nil {
			t.Errorf("Write(%q) returned nil error, want rejection", id)
		}
		exists, err := bend.Exists(repoID, id)
		if err == nil || exists {
			t.Errorf("Exists(%q) = (%v, %v), want (false, error)", id, exists, err)
		}
		if _, err := bend.Stat(repoID, id); err == nil {
			t.Errorf("Stat(%q) returned nil error, want rejection", id)
		}
	}
}
