package api

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/dkam/silo/fileserver/authmgr"
	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/objmgr"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/share"
	"github.com/dkam/silo/fileserver/tokenstore"
	"github.com/dkam/silo/store"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

var readDB *sql.DB // read handle

func Init(read, _ *sql.DB) {
	readDB = read
}

type siloServerInfo struct {
	Version  string   `json:"version"`
	Features []string `json:"features"`
}

// There is deliberately no block_size here.
//
// There used to be, and it named the offset a fixed-size chunker cut at — one
// number, server-wide, that every library shared. Content-defined chunking
// killed both halves of that: boundaries fall where the content puts them, not
// at multiples of anything, and the parameters belong to the library rather
// than to the server that happens to be serving it. A client asks the libraries
// listing, which answers per library. See libraryInfo.Chunker.

// features names the capabilities a client may branch on, so a new client can
// ask this server what it does instead of comparing version strings against a
// changelog. Version numbers answer "which build is this"; they answer "can I
// call this" only for someone holding the release notes, and a client talking
// to a server it did not ship with is exactly the case that has neither.
//
// A name is added in the release its capability ships, and then never removed
// and never reused. Removing one breaks the clients that checked for it, and
// reusing one for something else is worse than having no name at all, because
// the check still passes.
//
// Runtime configuration belongs here too, which is why notifications is
// conditional: a client that sees the name can go straight to notify-token
// instead of learning from a 404 that this server was built without it.
func features() []string {
	f := []string{
		// The vocabulary, so a mismatch is legible. Every other rename in this
		// API fails loudly — a client built for one spelling gets 404 from a
		// server speaking the other. The listing is the exception: 404 on the
		// listing reaches a person as an account with nothing in it, which
		// looks like working software rather than like two builds that
		// disagree. A client that checks for this name can say which of the
		// two is out of date instead of showing an empty tree.
		"libraries",          // /libraries/…, and library_id in every payload
		"entries",            // one addressable noun, HTTP methods as its verbs
		"entries-copy",       // POST {"op":"copy","to":…}
		"conditional-writes", // If-Match / If-None-Match on every mutating method
		"ranged-reads",       // Range on GET entries, unencrypted libraries
		"changes",            // GET libraries/{id}/changes?since=
		"library-rename",     // PATCH libraries/{id}
		"blocks",             // blocks/missing, PUT blocks/{id}, PUT entries?type=blocks
		"pagination",         // ?limit on changes and directory listings, Link: rel="next"
		"batch",              // POST libraries/{id}/batch — many operations, one commit
		"usage",              // GET account/usage, and size/file_count on the libraries listing
	}
	if option.EnableNotification {
		f = append(f, "notifications") // WS /notification, POST libraries/{id}/notify-token
	}
	return f
}

// ServerInfoHandler handles GET /api/silo/v1/server-info.
func ServerInfoHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, siloServerInfo{
		Version:  option.Version,
		Features: features(),
	})
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Errorf("Failed to encode JSON response: %v", err)
	}
}

// decodeJSON reads and decodes a JSON request body (max 1MB).
func decodeJSON(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return false
	}
	return true
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginResponse struct {
	Token string `json:"token"`
}

func LoginHandler(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.Email == "" || req.Password == "" {
		http.Error(w, "Email and password are required", http.StatusBadRequest)
		return
	}

	if !allowLoginAttempt(w, r, req.Email) {
		return
	}

	acct, err := authmgr.ValidatePassword(req.Email, req.Password)
	if err != nil {
		loginFailed(r, req.Email)
		log.Infof("Login failed for %s: %v", req.Email, err)
		http.Error(w, "Invalid email or password", http.StatusUnauthorized)
		return
	}
	loginSucceeded(req.Email)

	token, err := authmgr.GenerateSessionToken(acct.ID)
	if err != nil {
		log.Errorf("Failed to generate session token: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, loginResponse{Token: token})
}

type accessTokenRequest struct {
	LibraryID string `json:"library_id"`
	ObjID     string `json:"obj_id"`
	Op        string `json:"op"`
	OneTime   bool   `json:"one_time"`
}

type accessTokenResponse struct {
	Token string `json:"token"`
}

// tokenOps maps each access-token operation to the library permission needed to
// mint a token for it. The handlers that consume these tokens (/files/,
// /blks/, /zip/, /upload-api/, ...) authorize from the token alone and never
// re-check the caller's permission, so this map is the only gate on them.
//
// Ops absent from the map are rejected rather than passed through: an
// unrecognized op must not be able to produce a bearer credential.
var tokenOps = map[string]string{
	// Read: any permission on the library is enough.
	"view":                "r",
	"download":            "r",
	"download-link":       "r",
	"downloadblks":        "r",
	"download-dir":        "r",
	"download-dir-link":   "r",
	"download-multi":      "r",
	"download-multi-link": "r",

	// Write: "rw" required. A read-only share must not yield an upload token.
	"upload":      "rw",
	"upload-link": "rw",
	"update":      "rw",
	"update-link": "rw",
}

func CreateAccessTokenHandler(w http.ResponseWriter, r *http.Request) {
	acct := middleware.GetAccount(r)

	var req accessTokenRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.LibraryID == "" || req.Op == "" {
		http.Error(w, "library_id and op are required", http.StatusBadRequest)
		return
	}

	needed, ok := tokenOps[req.Op]
	if !ok {
		http.Error(w, "Unsupported op", http.StatusBadRequest)
		return
	}

	// CheckPerm returns "" for a library the user can't see and for one that
	// doesn't exist, so a 403 here also avoids confirming which library IDs are
	// real.
	perm := share.CheckPerm(req.LibraryID, acct.ID)
	if perm == "" || (needed == "rw" && perm != "rw") {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return
	}

	// The token carries the address, not the id. It has already been
	// permission-checked here, and what the upload path does with the string
	// is write it into a commit as the author — display data, and baked into
	// a content hash that could never be rewritten anyway.
	token := tokenstore.CreateToken(req.LibraryID, req.ObjID, req.Op, acct.Email, req.OneTime)
	writeJSON(w, http.StatusOK, accessTokenResponse{Token: token})
}

type libraryInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	UpdateTime int64  `json:"update_time"`
	Encrypted  bool   `json:"encrypted"`
	// HeadCommitID is where the library is now. It is here because it is the
	// anchor the changes endpoint needs, and listing libraries is the first
	// thing a sync client does: without it a client's opening move is to
	// enumerate a library with no way to name the state it just enumerated,
	// so its first delta call has nothing to pass as `since`.
	HeadCommitID string `json:"head_commit_id,omitempty"`
	// Size and FileCount are what the library holds, as logical size at head:
	// the sum of the sizes of the files the head commit reaches. Not bytes on
	// disk — dedup and deferred compaction move that number under the user
	// without anything changing, and a figure that shifts because the server
	// ran a background job is not one a client can explain.
	//
	// They are here rather than under an account total because they are
	// library facts, and because sharing settles it: a library shared with you
	// appears in your listing but is charged to its owner's quota, so
	// per-library sizes in an account report would either leak libraries you
	// do not own into a total they must not sum to, or omit them and leave the
	// widget with rows it cannot size. On the listing each row carries its own
	// number and nothing has to add up.
	//
	// Pointers because absent and zero are different answers. Zero is an empty
	// library; absent is a library whose size this server could not work out,
	// and a client that rendered that as 0 B would be stating a fact it was
	// never told.
	Size      *int64 `json:"size,omitempty"`
	FileCount *int64 `json:"file_count,omitempty"`
	// Chunker is how this library's bytes are cut. It is on the listing
	// because a client cannot name a file's chunks without it, and naming them
	// is what the whole chunk surface rests on: "which of these do you have?"
	// is only askable by a client that arrives at the same ids the server
	// would. These are per-library data frozen at creation, never a constant
	// compiled into a client — a client that chunks differently computes
	// different ids for the same bytes and dedups against nothing.
	//
	// Absent, like Size, means the server could not say. A client that cannot
	// read the parameters must not guess them; it uploads whole files instead,
	// which is slower and always correct.
	Chunker *chunkerInfo `json:"chunker,omitempty"`
}

// chunkerInfo is the chunker as a client needs it.
//
// There is no seed here, and its absence is the design rather than an
// omission. A plain library chunks under the published constant every
// implementation derives for itself, and an E2EE library's seed is HKDF over
// the content key — which the server does not hold and must never be handed.
// Putting a seed on this wire would be the server claiming to know something
// that, for exactly the libraries that matter, it does not.
type chunkerInfo struct {
	Algorithm     string `json:"algorithm"`
	MinSize       int    `json:"min_size"`
	TargetSize    int    `json:"target_size"`
	MaxSize       int    `json:"max_size"`
	Normalization int    `json:"normalization"`
}

// librarieselect is shared by the owned and shared queries so the two cannot drift
// into scanning different columns than they select. The join is LEFT because a
// library with no branch row is broken but should still be listable — a client
// that can see it can delete it.
func librarieselect(alias string) string {
	return "SELECT " + alias + ".library_id, i.name, i.update_time, i.is_encrypted, b.commit_id, " +
		"b.root_id, u.size, u.file_count, u.root_id, " +
		"f.chunker, f.chunk_min, f.chunk_target, f.chunk_max, f.chunk_norm "
}

// formatJoin brings in the library's chunker. LEFT for the same reason as the
// others: a row missing here is a broken library, and a client that can see it
// must still be able to list it and delete it.
const formatJoin = "LEFT JOIN Library f ON f.library_id = "

// usageJoin brings in the recorded totals. LEFT, like the branch join, because
// a library nobody has asked the size of yet has no row and must still list.
const usageJoin = "LEFT JOIN LibraryUsage u ON u.library_id = "

func scanLibraries(rows *sql.Rows) []libraryInfo {
	// Allocated rather than declared, so an empty result set marshals as [] and
	// not null. /changes already promises "always an array, never null", and a
	// client has no way to learn that the two list endpoints on the same lane
	// disagree except by emptying an account and looking. Go hides it — a nil
	// slice ranges zero times — but an account with no libraries is the state
	// every new account is in, so null is the first response a fresh client
	// sees, and in TypeScript, Python or Swift it is not iterable.
	libraries := make([]libraryInfo, 0)
	for rows.Next() {
		var library libraryInfo
		var name, isEncrypted, commitID, headRoot, measuredAt, chunker sql.NullString
		var updateTime, size, fileCount sql.NullInt64
		var chunkMin, chunkTarget, chunkMax, chunkNorm sql.NullInt64
		if err := rows.Scan(&library.ID, &name, &updateTime, &isEncrypted, &commitID,
			&headRoot, &size, &fileCount, &measuredAt,
			&chunker, &chunkMin, &chunkTarget, &chunkMax, &chunkNorm); err != nil {
			log.Warnf("Failed to scan library row: %v", err)
			continue
		}
		library.Name = name.String
		library.UpdateTime = updateTime.Int64
		library.Encrypted = isEncrypted.String == "1"
		library.HeadCommitID = commitID.String
		if chunker.Valid {
			library.Chunker = &chunkerInfo{
				Algorithm:     chunker.String,
				MinSize:       int(chunkMin.Int64),
				TargetSize:    int(chunkTarget.Int64),
				MaxSize:       int(chunkMax.Int64),
				Normalization: int(chunkNorm.Int64),
			}
		}
		// A row measured at the root the library is on needs nothing computed,
		// which is the case a listing is almost always in. The rest are
		// brought forward one at a time below, so a poll pays only for the
		// libraries that have been written to since the last one.
		if measuredAt.Valid && headRoot.Valid && measuredAt.String == headRoot.String {
			library.Size, library.FileCount = &size.Int64, &fileCount.Int64
		}
		libraries = append(libraries, library)
	}
	return libraries
}

// withUsage fills in the sizes the query could not answer from the catalog
// alone, by asking each library to bring its total forward.
//
// A library that will not answer is left without the fields rather than
// failing the listing. Being unable to size one library is not a reason to
// tell a client it has none.
func withUsage(libraries []libraryInfo) []libraryInfo {
	for i := range libraries {
		if libraries[i].Size != nil {
			continue
		}
		library := libmgr.Get(libraries[i].ID)
		if library == nil {
			continue
		}
		u, err := libmgr.Usage(library)
		if err != nil {
			log.Warnf("Failed to size library %s for a listing: %v", libraries[i].ID, err)
			continue
		}
		size, count := u.Size, u.FileCount
		libraries[i].Size, libraries[i].FileCount = &size, &count
	}
	return libraries
}

func ListLibrariesHandler(w http.ResponseWriter, r *http.Request) {
	id := middleware.GetAccountID(r)
	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	rows, err := readDB.QueryContext(ctx,
		librarieselect("o")+
			"FROM LibraryOwner o LEFT JOIN LibraryInfo i ON o.library_id = i.library_id "+
			"LEFT JOIN Branch b ON b.library_id = o.library_id AND b.name = 'master' "+
			usageJoin+"o.library_id "+
			formatJoin+"o.library_id "+
			"WHERE o.account_id = ?", id)
	if err != nil {
		log.Errorf("Failed to query libraries: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = rows.Close() }()

	libraries := scanLibraries(rows)
	seen := make(map[string]bool, len(libraries))
	for _, r := range libraries {
		seen[r.ID] = true
	}

	sharedRows, err := readDB.QueryContext(ctx,
		librarieselect("s")+
			"FROM SharedLibrary s LEFT JOIN LibraryInfo i ON s.library_id = i.library_id "+
			"LEFT JOIN Branch b ON b.library_id = s.library_id AND b.name = 'master' "+
			usageJoin+"s.library_id "+
			formatJoin+"s.library_id "+
			"WHERE s.to_account_id = ?", id)
	if err != nil {
		log.Errorf("Failed to query shared libraries: %v", err)
	} else {
		defer func() { _ = sharedRows.Close() }()
		for _, r := range scanLibraries(sharedRows) {
			if !seen[r.ID] {
				seen[r.ID] = true
				libraries = append(libraries, r)
			}
		}
	}

	writeJSON(w, http.StatusOK, withUsage(libraries))
}

// usageKind labels every figure this server reports as a size.
//
// It is here because the first support question any of this generates is a
// mismatch against du, and the answer is not that one of them is wrong: they
// measure different things, and a client that can name which one it is showing
// can say so instead of arguing. A later disk figure gets its own kind rather
// than quietly replacing this one.
const usageKind = "logical-at-head"

type accountUsageResponse struct {
	Usage int64 `json:"usage"`
	// Quota is absent when there is no ceiling, never a sentinel.
	// option.InfiniteQuota is -2, and a widget rendering "-2 bytes" is the
	// predictable end of putting it on the wire.
	Quota *int64 `json:"quota,omitempty"`
	Kind  string `json:"kind"`
}

// AccountUsageHandler handles GET /api/silo/v1/account/usage.
//
// The account's quota and its logical usage, and nothing else. Per-library
// figures are on the libraries listing, for the sharing reason recorded on
// libraryInfo.
//
// Usage here is what the account owns, not what it can see. A library shared
// with you is charged to whoever owns it, so it appears in your listing with
// its own size and contributes nothing to this number.
func AccountUsageHandler(w http.ResponseWriter, r *http.Request) {
	id := middleware.GetAccountID(r)

	usage, err := libmgr.AccountUsage(id)
	if err != nil {
		log.Errorf("Failed to total account usage: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	quota, err := libmgr.AccountQuota(id)
	if err != nil {
		log.Errorf("Failed to read account quota: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	resp := accountUsageResponse{Usage: usage.Size, Kind: usageKind}
	if quota > 0 {
		resp.Quota = &quota
	}
	writeJSON(w, http.StatusOK, resp)
}

type createLibraryRequest struct {
	Name string `json:"name"`
}

type createLibraryResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func CreateLibraryHandler(w http.ResponseWriter, r *http.Request) {
	acct := middleware.GetAccount(r)

	var req createLibraryRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.Name == "" {
		http.Error(w, "Name is required", http.StatusBadRequest)
		return
	}

	// Server-readable for now: an E2EE library's initial commit is sealed
	// under a key the server never holds, so creating one is a client
	// operation. See libmgr.CreateLibrary.
	libraryID, err := libmgr.CreateLibrary(req.Name, acct, libmgr.DefaultFormat(false))
	if err != nil {
		log.Errorf("Failed to create library: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, createLibraryResponse{ID: libraryID, Name: req.Name})
}

func DeleteLibraryHandler(w http.ResponseWriter, r *http.Request) {
	id := middleware.GetAccountID(r)
	vars := mux.Vars(r)
	libraryID := vars["libraryid"]

	owner, err := libmgr.GetLibraryOwner(libraryID)
	if err != nil {
		log.Errorf("Failed to get library owner: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	if owner.IsZero() {
		http.Error(w, "Library not found", http.StatusNotFound)
		return
	}
	if owner != id {
		http.Error(w, "Only the library owner can delete it", http.StatusForbidden)
		return
	}

	if err := libmgr.DeleteLibrary(libraryID); err != nil {
		log.Errorf("Failed to delete library: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

type dirEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
	ID   string `json:"id"`
	// Size is a pointer for the reason libraryInfo.Size is: absent and zero are
	// different answers, and rendering "not measured" as 0 B states a fact the
	// server never gave. A directory never carries one — a directory object
	// has no file_size, and the sum of what is under it is a different
	// question with a different endpoint.
	Size     *int64 `json:"size,omitempty"`
	Mtime    int64  `json:"mtime"`
	Modifier string `json:"modifier,omitempty"`
}

// ListDirByID writes the listing of a directory the caller has already resolved
// and authorized. It is the only way to list a directory: getEntry resolves the
// path to answer conditional requests, so it arrives holding the id.
//
// Taking the id rather than the path is the point. A path-taking entry point
// has to re-check the permission, re-load the repository and walk from the root
// a second time, and none of that is cached — CheckPerm is two or more queries,
// libmgr.Get is a query plus a commit read, and every directory object on the
// way down is a fresh read and inflate. The old GET /libraries/{id}/dir/?path=
// handler did exactly that second walk and was deleted with the rest of the
// pre-entries surface; do not reintroduce a path-taking variant.
func ListDirByID(w http.ResponseWriter, r *http.Request, library *libmgr.Library, dirID string) {
	limit, ok := parseLimit(w, r)
	if !ok {
		return
	}

	// A cursor pins the directory object the first page was served from, so a
	// listing stays consistent while the directory is written to. The pinned
	// object is still readable: directory objects are immutable and nothing
	// reclaims them inside a live library.
	offset := 0
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, ok := decodeCursor(raw)
		if !ok || c.Dir == "" {
			http.Error(w, "cursor is not one this server issued; list again from the start", http.StatusBadRequest)
			return
		}
		// The directory may have changed under the client mid-listing. It
		// keeps reading the version it started on rather than half of each.
		offset, dirID = c.Offset, c.Dir
	}

	// A window is not the representation the id names, so it carries no
	// validator: an ETag here would validate a request for a different page of
	// the same listing. The caller set one before it knew this was paged.
	if limit > 0 || offset > 0 {
		w.Header().Del("ETag")
		w.Header().Del("Last-Modified")
	}

	entries, err := listEntries(library, dirID)
	if err != nil {
		log.Errorf("Failed to get directory object %s in store %s: %v", dirID, library.StoreID, err)
		http.Error(w, "Directory not found", http.StatusNotFound)
		return
	}

	from, to, more := window(len(entries), offset, limit)
	if more {
		setNextLink(w, r, encodeCursor(pageCursor{Dir: dirID, Offset: to}))
	}
	// Sized after windowing, not before, so a page costs a page. This is the
	// whole reason the fill lives here rather than in listEntries: a client
	// asking for twenty entries out of a hundred thousand should pay for
	// twenty, and listEntries reads the directory object entire because that
	// is what a directory object is.
	page := entries[from:to]
	withFileSizes(library, page)

	// The body stays an array whether or not it is paged. Pagination lives in
	// a header precisely so that adding it did not change the shape of a
	// response every existing client already parses.
	writeJSON(w, http.StatusOK, page)
}

// listEntries reads one directory.
//
// It reports no size: a dirent does not carry one — the size lives in the
// file's manifest — so sizes are a second lookup, and withFileSizes does it
// for the page that is actually served.
func listEntries(library *libmgr.Library, dirID string) ([]dirEntry, error) {
	st, err := library.Store()
	if err != nil {
		return nil, err
	}
	id, err := store.ParseID(dirID)
	if err != nil {
		return nil, err
	}
	nodes, err := st.List(id)
	if err != nil {
		return nil, err
	}
	entries := make([]dirEntry, 0, len(nodes))
	for _, n := range nodes {
		entryType := "file"
		if n.IsDir() {
			entryType = "dir"
		}
		entries = append(entries, dirEntry{
			Name:  n.Name,
			Type:  entryType,
			ID:    n.ID.String(),
			Mtime: n.Mtime,
		})
	}
	return entries, nil
}

// withFileSizes fills in the size of every file on a page.
//
// The sizes come from the sidecar, which is a recorded copy of one number out
// of each manifest. What is not recorded yet is read from the manifests
// themselves and written down on the way past, so a directory pays for this
// once however often it is listed — the ids are content hashes and never
// change, so a row once written is right forever.
//
// The read is public, not opened: file_size is in the part of a manifest the
// server can read without a content key, which is what lets an end-to-end
// encrypted library show sizes at all. The plan's argument for making it
// public is the same one that makes garbage collection possible.
//
// Nothing here can fail the listing. A store that cannot be opened, a manifest
// that cannot be read, a database that will not take the write — each costs
// one entry its size, which the wire already has a way to say.
func withFileSizes(library *libmgr.Library, page []dirEntry) {
	ids := make([]string, 0, len(page))
	for _, e := range page {
		if e.Type == "file" {
			ids = append(ids, e.ID)
		}
	}
	if len(ids) == 0 {
		return
	}

	sizes, err := libmgr.FileSizes(ids)
	if err != nil {
		log.Warnf("could not read recorded file sizes: %v", err)
		sizes = map[string]int64{}
	}

	// Whatever the sidecar did not have, read from the manifest and remember.
	var st *objmgr.Store
	found := map[string]int64{}
	for _, id := range ids {
		if _, ok := sizes[id]; ok {
			continue
		}
		if len(found) >= libmgr.MaxSizeRepairs {
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
	libmgr.RecordFileSizes(found)

	for i := range page {
		if page[i].Type != "file" {
			continue
		}
		if size, ok := sizes[page[i].ID]; ok {
			page[i].Size = &size
		}
	}
}
