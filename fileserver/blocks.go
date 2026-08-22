package silod

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/dkam/silo/fileserver/blockmgr"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/objstore"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/utils"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

// The block surface: the Silo lane's answer to "don't send what the server
// already has, and don't start over when a transfer dies".
//
//	POST /api/silo/v1/repos/{repo}/blocks/missing              {"blocks":[…]} -> {"missing":[…]}
//	PUT  /api/silo/v1/repos/{repo}/blocks/{sha1}               the block's bytes
//	PUT  /api/silo/v1/repos/{repo}/entries/{path}?type=blocks  {"blocks":[…]}
//
// Whole-file PUT still exists and is still the right call for one small file:
// it is one request, and it needs no hashing on the client. What it cannot do
// is resume, skip content the server already holds, or upload in parallel, and
// all three matter to a sync client the moment a file is large or a link is
// unreliable.
//
// This works because a client can compute block ids without asking. Chunking
// is at fixed option.FixedBlockSize offsets and a block's id is the SHA-1 of
// its bytes, so any client with stdlib SHA-1 and a loop arrives at exactly the
// names the server would. That is what makes "which of these do you have?" a
// question a client can pose before transferring anything.
//
// It is check-blocks re-spelled in this lane's idiom rather than something new
// — the value is not the mechanism, it is that a Silo-native client no longer
// has to mint a second credential and cross into the frozen Seafile lane to
// perform the single most common operation a sync client performs.
//
// The limit inherited from fixed-offset chunking is real: inserting a byte
// near the front of a file shifts every boundary after it and nothing dedups.
// See docs/protocol-gaps.md.

// maxBlockListBody bounds a block-id list. At roughly 43 bytes per quoted id
// and comma this is some 380,000 blocks, which at the default block size is
// several terabytes in one file — far past any real request, which is what a
// limit is for.
const maxBlockListBody = 16 << 20

// blocksMissingHandler answers which of the offered blocks the store does not
// already hold, in the order they were offered.
//
// Write permission, not read, even though this only reads. Its whole purpose
// is to precede an upload, and a store's block ids are content: answering
// "yes, I have that one" to anyone with read access to any library turns this
// into an oracle for whether a given file exists somewhere on the server.
func blocksMissingHandler(w http.ResponseWriter, r *http.Request) {
	repo := entryRepo(w, mux.Vars(r)["repoid"], middleware.GetAccountID(r), true)
	if repo == nil {
		return
	}

	var body struct {
		Blocks []string `json:"blocks"`
	}
	if appErr := decodeLimitedJSON(w, r, maxBlockListBody, &body); appErr != nil {
		if appErr.Code == http.StatusBadRequest {
			appErr.Message = `Expected a JSON body such as {"blocks":["<sha1>",…]}`
		}
		http.Error(w, appErr.Message, appErr.Code)
		return
	}

	for _, id := range body.Blocks {
		if !utils.IsObjectIDValid(id) {
			http.Error(w, "Not a block id: "+id, http.StatusBadRequest)
			return
		}
	}

	missing, _, err := blockInventory(repo.StoreID, body.Blocks)
	if err != nil {
		log.WithContext(r.Context()).WithError(err).Errorf("failed to inventory blocks in repo %s", repo.ID)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	writeEntryJSON(w, http.StatusOK, map[string]any{"missing": missing})
}

// blockInventory reports which of the offered ids the store does not hold, in
// the order they were offered, and the total size of a file made of them.
//
// An id is inspected once however often it repeats, and reported missing once,
// because a file that repeats a block — a run of zeroes, a duplicated section
// — would otherwise have the client upload the same bytes twice. Its size
// still counts every time it appears: the repeat is a real part of the file's
// length even though it is one object on disk.
//
// Presence is asked of Exists rather than inferred from a failed Stat. They
// are not the same question: a stat that fails because the disk is unhappy
// would come back as "not present", the client would upload the block again,
// and the second write would fail the same way with the cause now two steps
// removed from where it happened.
func blockInventory(storeID string, ids []string) (missing []string, size int64, err error) {
	// Never nil: an empty list has to encode as [] and not null, for the same
	// reason GET /repos does. See docs/bugs/fixed/.
	missing = []string{}
	seen := make(map[string]int64, len(ids))
	for _, id := range ids {
		known, ok := seen[id]
		if !ok {
			if !blockmgr.Exists(storeID, id) {
				seen[id] = -1
				missing = append(missing, id)
				continue
			}
			known, err = blockmgr.Stat(storeID, id)
			if err != nil {
				return nil, 0, fmt.Errorf("failed to stat block %s/%s: %w", storeID, id, err)
			}
			seen[id] = known
		}
		if known < 0 {
			continue
		}
		size += known
	}
	return missing, size, nil
}

// putBlockHandler stores one block under the id it names.
//
// The id is not taken on trust: blockmgr.Write hashes the bytes on the way to
// disk and refuses to publish content that does not match, because a
// wrong-bytes-under-a-right-id write is permanent — every later writer of that
// id skips it as already present, and every reader sharing the store gets the
// wrong content. So this is also a complete integrity check of the transfer,
// and the client gets told rather than finding out later.
func putBlockHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	repo := entryRepo(w, vars["repoid"], middleware.GetAccountID(r), true)
	if repo == nil {
		return
	}
	blockID := vars["id"]

	// Answered before the body is read, which is what makes Expect:
	// 100-continue worth sending here: a client that offers the header is told
	// to stop before it transfers anything. Without it the bytes arrive and
	// are discarded, which is no worse than the upload it was going to do.
	//
	// Blocks are immutable — the id is the content — so an id already present
	// is the same bytes, and re-storing them could only cost a write.
	if blockmgr.Exists(repo.StoreID, blockID) {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// A block larger than the chunk size is not something any client of this
	// store produces, and the cap keeps one request from being a whole file.
	// Smaller is allowed: a short final block is normal, and a client free to
	// chunk differently is merely one that dedups against nothing.
	if r.ContentLength > int64(option.FixedBlockSize) {
		http.Error(w, "Block is larger than the block size", http.StatusRequestEntityTooLarge)
		return
	}
	body := http.MaxBytesReader(w, r.Body, int64(option.FixedBlockSize))

	if err := blockmgr.Write(repo.StoreID, blockID, body); err != nil {
		if errors.Is(err, objstore.ErrContentMismatch) {
			http.Error(w, "Block content does not hash to "+blockID, http.StatusBadRequest)
			return
		}
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "Block is larger than the block size", http.StatusRequestEntityTooLarge)
			return
		}
		if isNetworkErr(err) {
			// The client hung up mid-block. Nothing was stored: the write is a
			// temp file and a rename, so a partial block never gets a name.
			return
		}
		log.WithContext(r.Context()).WithError(err).Errorf("failed to write block %s in repo %s", blockID, repo.ID)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Only when the length was declared: a chunked request reports -1, and
	// that converted to unsigned is 16 exabytes of recorded traffic.
	if r.ContentLength > 0 {
		sendStatisticMsg(repo.ID, middleware.GetUserEmail(r), "web-file-upload", uint64(r.ContentLength))
	}

	w.WriteHeader(http.StatusCreated)
}
