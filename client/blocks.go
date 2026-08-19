package client

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
)

// The client half of the block surface. See fileserver/blocks.go for the
// server's side and docs/protocol.md for the endpoints.
//
// The whole arrangement rests on one property: a block's id is the SHA-1 of
// its bytes and chunking is at fixed offsets, so this file computes exactly
// the names the server would without asking it anything. That is what lets a
// client say "here is what this file is made of — which parts do you not
// already have?" before transferring a byte.

// DefaultBlockSize is the chunk size to assume when the server does not say.
// Only a server too old to advertise block_size can leave it unset, and such a
// server has no block surface to upload to either — so this is a floor for
// arithmetic, not a guess anyone relies on.
const DefaultBlockSize = 8 << 20

func blockSizeOf(info ServerInfo) uint64 {
	if info.BlockSize == 0 {
		return DefaultBlockSize
	}
	return info.BlockSize
}

// capabilities returns what the server said about itself, fetched once and
// remembered. A server that cannot be asked reads as one with no features,
// which routes every caller to the path that has always worked.
func (c *APIClient) capabilities() ServerInfo {
	c.mu.Lock()
	cached := c.serverInfo
	c.mu.Unlock()
	if cached != nil {
		return *cached
	}

	info, err := c.GetServerInfo()
	if err != nil {
		info = ServerInfo{}
	}
	c.mu.Lock()
	c.serverInfo = &info
	c.mu.Unlock()
	return info
}

// MissingBlocks asks which of these blocks the library does not already hold.
// The answer comes back in the order asked, each id once.
func (c *APIClient) MissingBlocks(repoID string, blocks []string) ([]string, error) {
	var result struct {
		Missing []string `json:"missing"`
	}
	err := c.doRequest("POST", "/api/silo/v1/repos/"+repoID+"/blocks/missing",
		map[string][]string{"blocks": blocks}, &result)
	if err != nil {
		return nil, err
	}
	return result.Missing, nil
}

// PutBlock uploads one block. The server hashes what arrives and refuses it if
// it does not match blockID, so a successful call is also proof the bytes
// crossed intact.
func (c *APIClient) PutBlock(repoID, blockID string, newBody func() (io.ReadCloser, int64, error)) error {
	resp, err := c.doStream("PUT", "/api/silo/v1/repos/"+repoID+"/blocks/"+blockID,
		"application/octet-stream", newBody)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("block %.8s: %s: %s", blockID, resp.Status, string(msg))
	}
	return nil
}

// CommitBlocks creates or replaces a file from blocks already uploaded. It
// transfers no content: the body is the ordered list of ids.
func (c *APIClient) CommitBlocks(repoID, remotePath string, blocks []string) error {
	return c.doRequest("PUT", entriesURL(repoID, remotePath)+"?type=blocks",
		map[string][]string{"blocks": blocks}, nil)
}

// chunkFile computes the block ids of a local file, in order, along with the
// offset each one starts at. Nothing is held in memory but one block at a
// time, so this is bounded however large the file is.
//
// A repeated block appears once in offsets and as often as it occurs in ids:
// the offsets are for uploading, where one copy is enough, and the ids are the
// file, where every occurrence counts.
func chunkFile(localPath string, blockSize int64) (ids []string, offsets map[string]int64, err error) {
	file, err := os.Open(localPath)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = file.Close() }()

	offsets = make(map[string]int64)
	buf := make([]byte, blockSize)
	var off int64
	for {
		n, err := io.ReadFull(file, buf)
		if n > 0 {
			sum := sha1.Sum(buf[:n])
			id := hex.EncodeToString(sum[:])
			ids = append(ids, id)
			if _, seen := offsets[id]; !seen {
				offsets[id] = off
			}
			off += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
	}
	return ids, offsets, nil
}

// uploadBlocks sends a file as blocks: name every part, ask which parts are
// new, send only those, then name the whole thing in one call.
//
// Nothing exists at the destination path until that last call, so an upload
// interrupted at any point leaves the library exactly as it was. Run it again
// and the blocks that landed are already there — the second attempt asks the
// same question and gets a shorter answer.
func (c *APIClient) uploadBlocks(repoID, parentDir, localPath string, blockSize int64) error {
	ids, offsets, err := chunkFile(localPath, blockSize)
	if err != nil {
		return fmt.Errorf("failed to read %s: %v", localPath, err)
	}

	missing, err := c.MissingBlocks(repoID, ids)
	if err != nil {
		return err
	}

	for _, id := range missing {
		off, ok := offsets[id]
		if !ok {
			// The server answered with an id that was never offered.
			return fmt.Errorf("server asked for block %.8s, which is not part of this file", id)
		}
		if _, err := c.putBlockFrom(repoID, id, localPath, off, blockSize); err != nil {
			return err
		}
	}

	return c.CommitBlocks(repoID, path.Join("/", parentDir, filepath.Base(localPath)), ids)
}

// putBlockFrom uploads the block that starts at off in a local file, and
// reports how many bytes it sent. The last block of a file is short, so the
// length comes from the file rather than from blockSize.
func (c *APIClient) putBlockFrom(repoID, id, localPath string, off, blockSize int64) (int64, error) {
	var sent int64
	err := c.PutBlock(repoID, id, func() (io.ReadCloser, int64, error) {
		// A factory, not a reader: doStream re-sends the body after a 401, and
		// a reader already drained cannot be sent twice.
		file, err := os.Open(localPath)
		if err != nil {
			return nil, 0, err
		}
		if _, err := file.Seek(off, io.SeekStart); err != nil {
			_ = file.Close()
			return nil, 0, err
		}
		n := blockSize
		if info, err := file.Stat(); err == nil && info.Size()-off < n {
			n = info.Size() - off
		}
		sent = n
		return readCloser{io.LimitReader(file, n), file}, n, nil
	})
	return sent, err
}

// readCloser pairs a limited reader with the file underneath it, so the whole
// block can be sent without handing the request the rest of the file.
type readCloser struct {
	io.Reader
	io.Closer
}
