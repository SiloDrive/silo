package silod

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/dkam/silo/fileserver/repomgr"
)

// tmpFileTestRepo is the library the staging tests upload into.
const tmpFileTestRepo = "5d3c7a91-2e64-4f80-b7a5-91c0de4f6a83"

// A chunked upload can reach the deferred clearTmpFile with fileNames still
// empty: every early return in the final-chunk path — more than one file part,
// an unreadable Content-Disposition, a temp file that will not open, a database
// that will not answer — happens before the name is appended. clearTmpFile
// indexed fileNames[0] regardless, so those returns panicked out of the handler
// instead of answering 500, and a client could trigger it by sending two file
// parts in the last chunk of a resumable upload.
func TestClearTmpFileWithNoFileNames(t *testing.T) {
	// rstart >= 0 and rend == fsize-1 is the final chunk of a chunked upload,
	// which is the only shape that reaches past the guard at all. No repomgr
	// call follows, so this needs no database: returning is the whole result.
	fsm := &recvData{
		repoID: "e1e0b30d-0e7e-4b3f-8b46-0a4b5e2f0a11",
		rstart: 0,
		rend:   1023,
		fsize:  1024,
	}

	clearTmpFile(fsm, "/somewhere")
}

// uploadRequest builds the multipart request writeBlockDataToTmpFile expects:
// the file part it reads from, and the Content-Disposition header it takes the
// name from.
func uploadRequest(t *testing.T, filename, content string) (*http.Request, map[string][]*multipart.FileHeader) {
	t.Helper()

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("failed to build the multipart body: %v", err)
	}
	if _, err := part.Write([]byte(content)); err != nil {
		t.Fatalf("failed to write the file part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("failed to close the multipart body: %v", err)
	}

	r := httptest.NewRequest(http.MethodPost, "/upload", body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("Content-Disposition", `attachment; filename="`+url.QueryEscape(filename)+`"`)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		t.Fatalf("failed to parse the multipart form: %v", err)
	}

	return r, r.MultipartForm.File
}

// uploadTmpDir points the package at a throwaway data dir and returns the
// directory resumable uploads are staged in.
func uploadTmpDir(t *testing.T) string {
	t.Helper()

	sqliteTestDB(t)

	shared := filepath.Join(absDataDir, "httptemp", "cluster-shared")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatalf("failed to create the staging dir: %v", err)
	}
	return shared
}

// A resumable upload asks the database where its half-written temp file is.
// When that lookup fails -- the database is down, the table is gone -- the
// answer is not "there is no temp file": treating it as one starts a fresh
// staging file and silently drops every chunk already uploaded, then hands the
// caller a truncated file reported as a success.
func TestResumeStopsWhenTheTmpFileLookupFails(t *testing.T) {
	shared := uploadTmpDir(t)
	dbExec(t, "DROP TABLE WebUploadTempFiles")

	r, formFiles := uploadRequest(t, "report.bin", "second chunk")
	fsm := &recvData{repoID: tmpFileTestRepo, rstart: 8, rend: 19, fsize: 40}

	if err := writeBlockDataToTmpFile(r, fsm, formFiles, tmpFileTestRepo, "/uploads"); err == nil {
		t.Fatal("a failed tmp file lookup was reported as success")
	}

	staged, err := os.ReadDir(shared)
	if err != nil {
		t.Fatalf("failed to read the staging dir: %v", err)
	}
	if len(staged) != 0 {
		t.Fatalf("started %d fresh staging file(s) after a failed lookup; the earlier chunks would be lost", len(staged))
	}
}

// The database can name a temp file the disk no longer has -- a cleaner ran,
// the volume was restored, the file was never created. Creating it here would
// leave the earlier chunks as a hole seeked past and produce a file that is the
// right length and the wrong content.
func TestResumeStopsWhenTheTmpFileIsGone(t *testing.T) {
	uploadTmpDir(t)

	filePath := "/uploads/report.bin"
	missing := filepath.Join(t.TempDir(), "vanished.tmp")
	if err := repomgr.AddUploadTmpFile(tmpFileTestRepo, filePath, missing); err != nil {
		t.Fatalf("failed to record the tmp file: %v", err)
	}

	r, formFiles := uploadRequest(t, "report.bin", "second chunk")
	fsm := &recvData{repoID: tmpFileTestRepo, rstart: 8, rend: 19, fsize: 40}

	if err := writeBlockDataToTmpFile(r, fsm, formFiles, tmpFileTestRepo, "/uploads"); err == nil {
		t.Fatal("resuming onto a missing tmp file was reported as success")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("resuming re-created the missing tmp file; the earlier chunks are a hole in it")
	}
}
