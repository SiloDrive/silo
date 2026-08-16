package fsmgr

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

const (
	repoID   = "b1f2ad61-9164-418a-a47f-ab805dbd5694"
	blkID    = "0401fc662e3bc87a41f299a907c056aaf8322a26"
	subDirID = "0401fc662e3bc87a41f299a907c056aaf8322a27"
)

// Set from os.MkdirTemp in TestMain (t.TempDir needs a *testing.T, which
// TestMain has no access to) so this package's object store is its own and
// does not collide with the other packages' tests when they run in parallel.
var seafileConfPath string
var seafileDataDir string

var dirID string
var fileID string

func createFile() error {
	var blkIDs []string
	for i := 0; i < 2; i++ {
		blkshal := blkID
		blkIDs = append(blkIDs, blkshal)
	}

	seafile, err := NewSeafile(1, 100, blkIDs)
	if err != nil {
		return err
	}

	err = SaveSeafile(repoID, seafile)
	if err != nil {
		return err
	}
	fileID = seafile.FileID

	var entries []*SeafDirent
	for i := 0; i < 2; i++ {
		dirent := SeafDirent{ID: subDirID, Name: "/", Mode: 0x4000}
		entries = append(entries, &dirent)
	}
	seafdir, err := NewSeafdir(1, entries)
	if err != nil {
		err := fmt.Errorf("failed to new seafdir: %v", err)
		return err
	}
	err = SaveSeafdir(repoID, seafdir)
	if err != nil {
		return err
	}

	dirID = seafdir.DirID

	return nil
}

func delFile() error {
	err := os.RemoveAll(seafileConfPath)
	if err != nil {
		return err
	}

	return nil
}

func TestMain(m *testing.M) {
	var err error
	seafileConfPath, err = os.MkdirTemp("", "silo-fsmgr-test")
	if err != nil {
		fmt.Printf("Failed to create test dir : %v.\n", err)
		os.Exit(1)
	}
	seafileDataDir = filepath.Join(seafileConfPath, "seafile-data")

	Init(seafileConfPath, seafileDataDir, 2<<30)
	err = createFile()
	if err != nil {
		fmt.Printf("Failed to create test file : %v.\n", err)
		os.Exit(1)
	}
	code := m.Run()
	err = delFile()
	if err != nil {
		fmt.Printf("Failed to remove test file : %v\n", err)
	}
	os.Exit(code)
}

func TestGetSeafile(t *testing.T) {
	exists, err := Exists(repoID, fileID)
	if !exists {
		t.Errorf("seafile is not exists : %v.\n", err)
	}
	seafile, err := GetSeafile(repoID, fileID)
	if err != nil || seafile == nil {
		t.Errorf("Failed to get seafile : %v.\n", err)
		t.FailNow()
	}

	for _, v := range seafile.BlkIDs {
		if v != blkID {
			t.Errorf("Wrong file content.\n")
		}
	}
}

func TestGetSeafdir(t *testing.T) {
	exists, err := Exists(repoID, dirID)
	if !exists {
		t.Errorf("seafile is not exists : %v.\n", err)
	}
	seafdir, err := GetSeafdir(repoID, dirID)
	if err != nil || seafdir == nil {
		t.Errorf("Failed to get seafdir : %v.\n", err)
		t.FailNow()
	}

	for _, v := range seafdir.Entries {
		if v.ID != subDirID {
			t.Errorf("Wrong file content.\n")
		}
	}

}

func TestGetSeafdirByPath(t *testing.T) {
	seafdir, err := GetSeafdirByPath(repoID, dirID, "/")
	if err != nil || seafdir == nil {
		t.Errorf("Failed to get seafdir : %v.\n", err)
		t.FailNow()
	}

	for _, v := range seafdir.Entries {
		if v.ID != subDirID {
			t.Errorf("Wrong file content.\n")
		}
	}

}

// A few kilobytes of zlib expand to gigabytes, and the fs object they claim to
// be is only decompressed later — recvFSCB stores the compressed bytes without
// looking at them. Decompressing into an unbounded buffer let one request from
// any client with write access to one library OOM-kill the fileserver.
func TestUncompressRejectsDecompressionBomb(t *testing.T) {
	var compressed bytes.Buffer
	w := zlib.NewWriter(&compressed)
	// Zeroes compress to almost nothing, so the bomb is a few KB on the wire.
	zeros := make([]byte, 1<<20)
	for written := 0; written <= MaxObjectSize; written += len(zeros) {
		if _, err := w.Write(zeros); err != nil {
			t.Fatalf("failed to build test payload: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("failed to build test payload: %v", err)
	}
	if compressed.Len() > MaxObjectSize {
		t.Fatalf("test payload is %d bytes compressed, which does not test the limit", compressed.Len())
	}

	if _, err := uncompress(compressed.Bytes(), nil); err == nil {
		t.Error("uncompress accepted a payload larger than MaxObjectSize, want an error")
	}

	// The same path with a reused reader, which is how the hot loop calls it.
	reader, err := zlib.NewReader(bytes.NewReader(compressed.Bytes()))
	if err != nil {
		t.Fatalf("failed to create reader: %v", err)
	}
	defer func() { _ = reader.Close() }()
	if _, err := uncompress(compressed.Bytes(), reader); err == nil {
		t.Error("uncompress with a reused reader accepted an oversized payload, want an error")
	}
}

// The limit must not reject objects of a legitimate size, including one right
// at the boundary.
func TestUncompressAcceptsObjectAtTheLimit(t *testing.T) {
	payload := make([]byte, MaxObjectSize)
	for i := range payload {
		payload[i] = byte(i)
	}

	compressed, err := compress(payload)
	if err != nil {
		t.Fatalf("failed to compress: %v", err)
	}
	got, err := uncompress(compressed, nil)
	if err != nil {
		t.Fatalf("uncompress returned %v for an object exactly at the limit", err)
	}
	if len(got) != len(payload) {
		t.Errorf("uncompress returned %d bytes, want %d", len(got), len(payload))
	}
}
