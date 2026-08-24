package client

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/dkam/silo/store"
)

// Uploading a whole directory.
//
// UploadFile is the wrong shape for a tree twice over: it re-sends content the
// server already holds, and it mints one commit per file, so a folder of five
// hundred photos arrives as five hundred commits for what the user did once.
// The two surfaces that fix those are the chunk surface and the batch surface,
// and they compose — name every chunk in the tree, send only the ones nobody
// has, then create every directory and file in one ordered, all-or-nothing
// request:
//
//	POST repos/{id}/blocks/missing   which of these do you not have?
//	PUT  repos/{id}/blocks/{id}      only the ones it asked for
//	POST repos/{id}/batch            mkdir …, create …  -> one commit
//
// Dedup is across the tree, not just within a file: a block claimed by one
// file is not offered again by the next, so a directory holding the same
// export twice transfers it once.
//
// A server that advertises neither surface still works. uploadTreeSerially is
// the old shape — a request per directory, a request per file — kept because
// the client ships separately from the server it is pointed at, and because an
// encrypted library cannot be written block by block at all.

// Batches are bounded by the server at 1000 operations and a 4 MiB body
// (fileserver/batch.go). A create op carries 40 hex characters per block, so a
// tree of large files reaches the body limit long before the op limit — both
// are counted here, with room left for the server's own accounting.
const (
	maxOpsPerBatch   = 500
	maxBatchBodySize = 3 << 20
)

// maxIDsPerQuery bounds one "which of these are missing?" request. The server
// reads up to 16 MiB of block-id list, which is far more than this; the limit
// here is about keeping one request's worth of ids a predictable size.
const maxIDsPerQuery = 5000

// TreeUpload is the account of what UploadDir did. Blocks held is the part
// worth reporting: it is the content the server already had, which is the
// difference between this and a loop over UploadFile.
type TreeUpload struct {
	Dirs       int   `json:"dirs"`
	Files      int   `json:"files"`
	Bytes      int64 `json:"bytes"` // chunk content actually sent
	ChunksSent int   `json:"chunks_sent"`
	ChunksHeld int   `json:"chunks_held"`
	Commits    int   `json:"commits"`
	// Skipped are local paths that were neither a directory nor a regular
	// file. What is on the other end of a symlink is the caller's decision,
	// not a walk's, so they are reported rather than followed.
	Skipped []string `json:"skipped,omitempty"`
}

// treeFile is one local file, where it lands, and what it is made of.
type treeFile struct {
	local  string
	remote string
	chunks []string
}

// UploadDir uploads localDir, and everything under it, into parentDir. The
// directory keeps its own name: UploadDir(repo, "/", "./photos") fills
// /photos, the same way UploadFile keeps a file's name and `cp -r` keeps a
// directory's.
//
// onFile, if given, is called with each remote path once it is committed. It
// arrives in bursts on the batched path, because that is when files actually
// land — one call per file would be a progress bar for a commit that has not
// happened yet.
func (c *APIClient) UploadDir(repoID, parentDir, localDir string, onFile func(remotePath string)) (*TreeUpload, error) {
	info, err := os.Stat(localDir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", localDir)
	}

	// Resolved before taking the base name, so "." and "photos/" name the
	// directory they mean rather than "." and "photos" by accident.
	abs, err := filepath.Abs(localDir)
	if err != nil {
		return nil, err
	}
	base := path.Join("/", parentDir, filepath.Base(abs))

	dirs, files, skipped, err := walkTree(localDir, base)
	if err != nil {
		return nil, err
	}

	// Dirs and Files are counted as they commit rather than as they are
	// walked. A summary printed after a failure is read to find out what
	// landed, which is not the same question as what was offered.
	result := &TreeUpload{Skipped: skipped}

	server := c.capabilities()
	if server.Has("batch") && server.Has("blocks") {
		if p, ok := c.chunkerFor(repoID); ok {
			return result, c.uploadTreeBatched(repoID, dirs, files, p, onFile, result)
		}
	}
	return result, c.uploadTreeSerially(repoID, dirs, files, onFile, result)
}

// walkTree lists the directories and regular files under localDir, in an order
// where a parent always precedes its children, and maps each to where it lands
// under remoteBase. The order is what makes the batch legal: parents are never
// created implicitly, so every mkdir has to reach the server after the mkdir
// that made room for it.
func walkTree(localDir, remoteBase string) (dirs []string, files []treeFile, skipped []string, err error) {
	err = filepath.WalkDir(localDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(localDir, p)
		if err != nil {
			return err
		}
		remote := remoteBase
		if rel != "." {
			remote = path.Join(remoteBase, filepath.ToSlash(rel))
		}

		switch {
		case d.IsDir():
			dirs = append(dirs, remote)
		case d.Type().IsRegular():
			files = append(files, treeFile{local: p, remote: remote})
		default:
			skipped = append(skipped, p)
		}
		return nil
	})
	return dirs, files, skipped, err
}

// uploadTreeBatched sends the content nobody has, then writes the whole tree
// in as few commits as the server's limits allow.
func (c *APIClient) uploadTreeBatched(repoID string, dirs []string, files []treeFile, p store.Params, onFile func(string), result *TreeUpload) error {
	// Name every chunk in the tree first. Nothing is sent yet: the point of
	// hashing everything up front is to be able to ask one question about all
	// of it, and to ask about a chunk once however many files contain it.
	sources := make(map[string]chunkSource)
	var ids []string // distinct, in the order first seen
	for i := range files {
		chunks, found, err := chunkFile(files[i].local, p)
		if err != nil {
			return fmt.Errorf("failed to read %s: %v", files[i].local, err)
		}
		files[i].chunks = chunks
		for _, id := range chunks {
			if _, seen := sources[id]; seen {
				continue
			}
			sources[id] = found[id]
			ids = append(ids, id)
		}
	}

	missing, err := c.missingInBatches(repoID, ids)
	if err != nil {
		return err
	}
	result.ChunksHeld = len(ids) - len(missing)

	for _, id := range missing {
		src, ok := sources[id]
		if !ok {
			// The server answered with an id that was never offered.
			return fmt.Errorf("server asked for chunk %.8s, which is not part of this upload", id)
		}
		sent, err := c.putChunkFrom(repoID, id, src)
		if err != nil {
			return err
		}
		result.ChunksSent++
		result.Bytes += sent
	}

	ops := make([]BatchOp, 0, len(dirs)+len(files))
	for _, dir := range dirs {
		if dir == "/" {
			continue // the library root is always already there
		}
		ops = append(ops, BatchOp{Op: "mkdir", Path: dir})
	}
	for _, f := range files {
		ops = append(ops, BatchOp{Op: "create", Path: f.remote, Blocks: f.chunks})
	}
	return c.applyInBatches(repoID, ops, onFile, result)
}

// missingInBatches asks about a long chunk list in several requests, and keeps
// the server's answers in the order asked. Named for the request splitting
// rather than for the chunks, which are what is being asked about — the two
// senses of the word meet on this one line and only one of them is the
// format's.
func (c *APIClient) missingInBatches(repoID string, ids []string) ([]string, error) {
	var missing []string
	for start := 0; start < len(ids); start += maxIDsPerQuery {
		end := min(start+maxIDsPerQuery, len(ids))
		got, err := c.MissingChunks(repoID, ids[start:end])
		if err != nil {
			return nil, err
		}
		missing = append(missing, got...)
	}
	return missing, nil
}

// applyInBatches sends the operations in as few requests as the limits allow,
// in order, so every mkdir lands before the creates that need it.
//
// Each request is atomic on its own; a tree too big for one is not atomic as a
// whole, which is the honest trade for a limit that exists to keep a request
// bounded. A failure therefore leaves the batches that already committed, and
// re-running the upload is what fixes it: mkdir on an existing directory
// succeeds and create replaces, so a second run converges rather than doubling
// anything up.
func (c *APIClient) applyInBatches(repoID string, ops []BatchOp, onFile func(string), result *TreeUpload) error {
	for len(ops) > 0 {
		n, size := 0, 0
		for n < len(ops) && n < maxOpsPerBatch {
			cost := opBytes(ops[n])
			// n > 0 so a single oversized op is still attempted rather than
			// looping forever refusing to fit it.
			if n > 0 && size+cost > maxBatchBodySize {
				break
			}
			size += cost
			n++
		}

		res, err := c.Batch(repoID, ops[:n])
		if err != nil {
			return err
		}
		if res.Changed {
			result.Commits++
		}
		for _, op := range ops[:n] {
			switch op.Op {
			case "mkdir":
				result.Dirs++
			case "create":
				result.Files++
				if onFile != nil {
					onFile(op.Path)
				}
			}
		}
		ops = ops[n:]
	}
	return nil
}

// opBytes is roughly what one operation costs in the request body: a block id
// is 40 hex characters plus its quotes and comma, and everything else is small
// and fixed. It only has to be close enough to keep a batch under a limit the
// server measures exactly.
func opBytes(op BatchOp) int {
	return 64 + len(op.Path) + len(op.To) + 43*len(op.Blocks)
}

// uploadTreeSerially is the shape that works against any server: a request per
// directory, a request per file, and a commit for each. It is what a server
// without the batch or block surfaces gets, and what an encrypted library
// gets, since that cannot be assembled from blocks server-side at all.
func (c *APIClient) uploadTreeSerially(repoID string, dirs []string, files []treeFile, onFile func(string), result *TreeUpload) error {
	for _, dir := range dirs {
		if dir == "/" {
			continue
		}
		// Mkdir on a directory that is already there answers 201, so a re-run
		// over a tree that partly landed needs no existence check.
		if err := c.Mkdir(repoID, dir); err != nil {
			return fmt.Errorf("mkdir %s: %v", dir, err)
		}
		result.Dirs++
		result.Commits++
	}
	for _, f := range files {
		if err := c.UploadFile(repoID, path.Dir(f.remote), f.local); err != nil {
			return fmt.Errorf("upload %s: %v", f.local, err)
		}
		result.Files++
		result.Commits++
		if onFile != nil {
			onFile(f.remote)
		}
	}
	return nil
}
