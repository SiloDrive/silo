package silod

import "testing"

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
