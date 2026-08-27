package libmgr

import (
	"context"
	"fmt"
	"strings"

	"github.com/dkam/silo/fileserver/dbutil"
	"github.com/dkam/silo/fileserver/objmgr"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/store"
	log "github.com/sirupsen/logrus"
)

// The listing sidecar: manifest id to file size.
//
// A directory object names its children and their types and says
// nothing about how big any of them are, because the size is a property of the
// file rather than of the directory that mentions it. That is the right shape
// for the format — a rename rewrites one directory and touches no sizes — and
// it leaves the listing endpoint holding N ids and no numbers.
//
// This is the number, written down once. It is filled on the read path rather
// than at the write, for the same reason accounting is: there is more than one
// way for a manifest to reach a store — the whole-file PUT, the chunk lane, a
// batch, and a client uploading objects directly under E2EE — and a table that
// depends on every one of those remembering to call it is a table with holes
// in it that nobody notices, because a missing size looks exactly like a size
// that has not been asked for yet.
//
// Reading it back is the only thing that has to be right, and reading it back
// repairs it.

// maxSizeRepairs bounds how many manifests one listing will open to fill gaps.
//
// The steady state is zero: a directory listed once has every size recorded,
// and the ids never change because they are content hashes. This is for the
// first listing, and it is capped so that the first listing of a very large
// directory is a slow response rather than an unbounded one. Entries past the
// cap come back without a size and are filled by the next request, so the gap
// closes on its own.
const maxSizeRepairs = 1024

// FileSizes returns the size of each of these manifests, reading through to
// the manifests themselves for any the sidecar does not have yet and writing
// what it finds back on the way past.
//
// Read-through and repair are one call because they are one policy: how many
// manifests a gap is worth opening, and that a repaired size is recorded, are
// decisions about the sidecar and not about whoever is asking. A second
// consumer of file sizes that had to re-implement the loop would either skip
// the recording — leaving the table permanently empty for the ids only it
// asks about — or pick its own cap, and neither is visible from here.
//
// The manifest read is public, not opened: file_size is in the part a server
// can read without a content key, which is what lets an end-to-end encrypted
// library show sizes at all.
//
// Nothing here fails the caller. A store that cannot be opened, a manifest
// that cannot be read, a database that will not take the write — each costs
// one id its size, and an id with no size is simply absent from the result.
// The caller cannot tell a file of zero bytes from one nobody has measured
// unless the two answers stay distinct all the way out to the wire.
func FileSizes(library *Library, ids []string) map[string]int64 {
	sizes, err := recordedSizes(ids)
	if err != nil {
		log.Warnf("could not read recorded file sizes: %v", err)
		sizes = map[string]int64{}
	}
	if len(sizes) == len(ids) {
		return sizes
	}

	var st *objmgr.Store
	found := map[string]int64{}
	for _, id := range ids {
		if _, ok := sizes[id]; ok {
			continue
		}
		if len(found) >= maxSizeRepairs {
			break
		}
		if st == nil {
			if st, err = library.Store(); err != nil {
				log.Warnf("could not open store %s to size a listing: %v", library.StoreID, err)
				break
			}
		}
		parsed, err := store.ParseID(id)
		if err != nil {
			continue
		}
		m, err := st.GetManifestPublic(parsed)
		if err != nil {
			// A missing manifest is a broken library, not a broken listing.
			// The entry loses its size and the name still lists, which is what
			// lets someone see the damage and delete it.
			continue
		}
		found[id] = m.FileSize
		sizes[id] = m.FileSize
	}
	recordFileSizes(found)
	return sizes
}

// recordedSizes returns the recorded size for each id that has one.
func recordedSizes(ids []string) (map[string]int64, error) {
	sizes := make(map[string]int64, len(ids))
	if len(ids) == 0 {
		return sizes, nil
	}
	// Chunked because SQLite's parameter limit is finite and a listing's page
	// is not bounded by anything this function controls.
	const perQuery = 500
	for start := 0; start < len(ids); start += perQuery {
		end := min(start+perQuery, len(ids))
		batch := ids[start:end]

		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		query := "SELECT object_id, file_size FROM ObjectSize WHERE object_id IN (?" +
			strings.Repeat(",?", len(batch)-1) + ")"

		ctx, cancel := option.WithDBTimeout(context.Background())
		rows, err := readDB.QueryContext(ctx, query, args...)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("reading recorded file sizes: %w", err)
		}
		for rows.Next() {
			var id string
			var size int64
			if err := rows.Scan(&id, &size); err != nil {
				_ = rows.Close()
				cancel()
				return nil, fmt.Errorf("reading recorded file sizes: %w", err)
			}
			sizes[id] = size
		}
		err = rows.Err()
		_ = rows.Close()
		cancel()
		if err != nil {
			return nil, fmt.Errorf("reading recorded file sizes: %w", err)
		}
	}
	return sizes, nil
}

// recordFileSizes writes sizes that were not recorded before.
//
// Failures are logged and swallowed. This is a cache of a number the manifest
// still holds, so a write that does not land costs the next listing one
// manifest read and nothing else — refusing to serve a listing because an
// optimisation could not be saved would be the wrong trade by a wide margin.
//
// INSERT OR IGNORE rather than upsert, because a row cannot be wrong: the key
// is a content hash, so an existing row was written from the same bytes this
// one came from. A concurrent writer racing here is two processes agreeing.
func recordFileSizes(sizes map[string]int64) {
	if len(sizes) == 0 {
		return
	}
	ctx, cancel := option.WithDBTimeout(context.Background())
	defer cancel()

	tx, err := writeDB.BeginTx(ctx, nil)
	if err != nil {
		log.Warnf("could not record file sizes: %v", err)
		return
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, dbutil.InsertOrIgnore("ObjectSize", "object_id, file_size"))
	if err != nil {
		log.Warnf("could not record file sizes: %v", err)
		return
	}
	defer func() { _ = stmt.Close() }()

	for id, size := range sizes {
		if _, err := stmt.ExecContext(ctx, id, size); err != nil {
			log.Warnf("could not record the size of %s: %v", id, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		log.Warnf("could not record file sizes: %v", err)
	}
}
