package fsmgr

import (
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
