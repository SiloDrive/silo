package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/authmgr"
	"github.com/dkam/silo/fileserver/credential"
	"github.com/dkam/silo/fileserver/libmgr"
	"github.com/dkam/silo/fileserver/middleware"
	"github.com/dkam/silo/fileserver/notif"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/setup"
	"github.com/dkam/silo/fileserver/share"
	"github.com/dkam/silo/store"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

var readDB *sql.DB // read handle

// Init points the package at the database, once, as a server starts.
//
// It also empties the rate-limit buckets, which is not obviously its job until
// you ask what Init means: a server instance is beginning against this handle.
// The limiters are package-level vars, so they outlive any one instance — in
// production that happens exactly once and clearing empty buckets is a no-op,
// while in tests a dozen servers share a process and whatever the last one
// spent is still spent. That was a real failure: a test that exhausts the setup
// bucket deliberately decided whether its neighbours passed, with a 429 that
// read like a product bug.
//
// Doing it here rather than through an exported reset keeps the seam one every
// harness already uses, instead of one each new harness has to remember.
func Init(read, _ *sql.DB) {
	readDB = read
	resetRateLimiters()
}

type siloServerInfo struct {
	Version  string   `json:"version"`
	Features []string `json:"features"`

	// SetupRequired says this server has no accounts and is holding a setup
	// token. It is state rather than a capability, which is why it is a field
	// here and not a name in features: it is true once, on a server nobody has
	// claimed, and false forever after -- and features promises never to
	// remove a name it has published.
	//
	// omitempty is load-bearing, not tidiness. Without it every server that has
	// been set up would start sending a key it did not send before, and
	// docs/bugs/fixed/adding-a-number-to-a-token-response-breaks-clients.md is
	// what that costs. With it, a claimed server's body is byte-identical to
	// the one it sent yesterday.
	SetupRequired bool `json:"setup_required,omitempty"`
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
// conditional: a client that sees the name can go straight to the socket
// instead of learning from a failed upgrade that this server was started
// without it.
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
		"chunks",             // chunks/missing, PUT chunks/{id}, PUT entries?type=chunks
		// The read half of the store, and the three names below exist because
		// of one failure worth not repeating. All of this was built and none
		// of it was named, so silo-drive asked for `GET chunks/{id}` and for
		// a way to read a manifest — both of which had been answering for a
		// release. A capability a client cannot discover is a capability that
		// does not exist to it, and the chunk surface's own name says only
		// what a client may write.
		"objects",          // GET/PUT objects/{id}, GET/HEAD chunks/{id}, PUT head
		"chunks-fetch",     // POST chunks/fetch — many chunks, one framed response
		"entries-manifest", // GET entries/{path}?type=manifest
		"chunks-upload",    // POST chunks — many chunks, one framed request
		"pagination",       // ?limit on changes and directory listings, Link: rel="next"
		"batch",            // POST libraries/{id}/batch — many operations, one commit
		"usage",            // GET account/usage, and size/file_count on the libraries listing
		// Credential self-service. A client cannot discover these by version
		// number and should not learn them from a 404: a 404 on logout reads
		// as a broken route rather than as an older server, and a client that
		// cannot tell the difference has no safe fallback but to leave the
		// credential live.
		"logout",          // POST auth/logout, POST auth/logout/everywhere
		"password-change", // POST auth/password
		// A live device credential minting its successor. This one has to be
		// discoverable rather than learned from a 404, because the fallback is
		// not "try again later" but an architecture: a client that cannot renew
		// must keep the password at rest to survive day 90, and it has to make
		// that decision at enrolment rather than on the morning it stops
		// working. See renew.go for why this is not a sliding expiry.
		"credential-renew", // POST auth/renew
		// The account side of end-to-end encryption. A client that cannot see
		// this name is talking to a server with nowhere to put an identity
		// key, and must say so rather than enrol into a library whose content
		// key would die with the device that made it.
		"account-keys", // GET/PUT account/keys, DELETE …/recovery/{n}, POST auth/kdf
		// Split-derivation login: this server will store a hash of an authKey
		// rather than of a password, and POST auth/password will take the
		// client KDF parameters alongside it and write both together.
		//
		// A client that cannot see this name must send the password, because
		// an older server would hash the authKey as if it were one and the
		// account would be reachable only by sending that same authKey
		// forever — a crossover nothing recorded and nothing can undo. Seeing
		// it does not mean any particular account has crossed over; that is
		// per-account, and POST auth/kdf is the question that answers it.
		"split-login", // authKey on POST auth/login, kdf_params on POST auth/password
		// Creating an end-to-end encrypted library, which takes a different
		// request from creating a plain one: the client brings the sealed
		// root, the sealed initial commit, the library id and the wrapped
		// content key. A client that cannot see this name must not offer the
		// option -- a POST without those fields silently makes a
		// server-readable library.
		"e2ee-libraries", // POST /libraries with "e2ee": true, GET libraries/{id}/key
		// Claiming an unclaimed server. The name says this build has the
		// endpoint; the setup_required field beside this list says whether
		// this server still needs it. A client that cannot see the name is
		// talking to a server that creates its own admin account at boot, and
		// should say so rather than read the 404 as a transient failure.
		"setup", // POST auth/setup, and setup_required on this response
	}
	if option.EnableNotification {
		f = append(f, "notifications") // WS /notification
		// The socket authorizes a subscribe from the Authorization header the
		// handshake carried. This is now the only lane -- the minted token and
		// its endpoint are gone -- but the name stays and stays worth
		// checking: a server old enough to lack it answers a tokenless frame
		// with jwt-expired, and a client that reads that as a re-mint loops
		// forever. Seeing this name is what says the tokenless frame is
		// understood.
		f = append(f, "notifications-credential") // subscribe with no jwt_token
		// One subscribe frame for everything the account can see, answered
		// with a bare ring; a narrowed credential gets its one library's
		// ring for the same frame. It needs a name because without it a
		// client cannot tell "this server has no account mode" from "this
		// account is quiet", and those two look identical from the outside.
		// One name for both rings: a client does not choose between them.
		f = append(f, "notifications-account") // subscribe with {"account": true}, account-update
	}
	return f
}

// ServerInfoHandler handles GET /api/silo/v1/server-info.
func ServerInfoHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	// One primary-key lookup on a route nothing polls. A process-level cached
	// bool would be cheaper and wrong: `silo user add` can create the first
	// account from another process, and a cache would go on advertising setup
	// on a server that had already been claimed.
	//
	// A failure here is not worth a 500 -- the version and the feature list are
	// what most callers came for -- so it is logged and answered as "no setup
	// needed", which is the answer that sends a client to the login screen
	// rather than to a setup screen that cannot work.
	required, err := setup.Required(ctx)
	if err != nil {
		log.Errorf("Failed to check whether setup is required: %v", err)
	}

	writeJSON(w, http.StatusOK, siloServerInfo{
		Version:       option.Version,
		Features:      features(),
		SetupRequired: required,
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

// loginRequest is both halves of the endpoint: a plain login, and enrolment.
//
// docs/auth.md makes the password an *enrolment* credential rather than a
// request credential -- presented once, exchanged, and forgotten -- so this is
// where a device gets the thing it will actually hold. There is no device
// grant because there is no third party: silo-drive collects the password in its
// own window, and the code-and-approval dance would buy nothing.
type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`

	// The enrolment half. Any of these present makes this an enrolment
	// request; see enrolling.
	Kind       string `json:"kind"`
	ClientName string `json:"client_name"`
	PublicKey  string `json:"public_key"`
	Perm       string `json:"perm"`
	Scope      string `json:"scope"`
}

// enrolling reports whether the caller asked for the enrolment response.
//
// The *request* decides, not a version or a header, because the response
// shapes differ and the old one cannot be widened: adding a number to a token
// body broke a client once already, on the day the field it asked for shipped
// (docs/bugs/fixed/adding-a-number-to-a-token-response-breaks-clients.md). A
// client that sends what it always sent gets what it always got, byte for
// byte, and a client that asks for a credential gets the documented shape.
func (req loginRequest) enrolling() bool {
	return req.Kind != "" || req.ClientName != "" || req.PublicKey != "" ||
		req.Perm != "" || req.Scope != ""
}

type loginResponse struct {
	Token string `json:"token"`
}

// enrolmentResponse is what docs/auth.md specifies. It carries no "token":
// two spellings of one secret in one body is one spelling too many, and a
// client that reads both would not know which to store.
type enrolmentResponse struct {
	Credential string `json:"credential"`
	ExpiresAt  int64  `json:"expires_at"`
	Email      string `json:"email"`
}

// The lifetimes docs/auth.md's table of kinds gives each. Both are absolute
// and do not slide, and either can be revoked before it is reached.
const (
	sessionLifetime = 24 * time.Hour
	deviceLifetime  = 90 * 24 * time.Hour
)

func LoginHandler(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.Email == "" || req.Password == "" {
		http.Error(w, "Email and password are required", http.StatusBadRequest)
		return
	}

	// The request is checked before the password is, so a malformed enrolment
	// is a 400 whether or not the password was right -- which means the shape
	// of the answer says nothing about the account. It also keeps the rate
	// limiter charging password attempts rather than typos.
	opts, ok := enrolmentOpts(w, req)
	if !ok {
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

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	opts.AccountID = acct.ID
	if opts.Label == "" {
		opts.Label = credentialLabel(r)
	}

	cred, token, err := credential.Issue(ctx, opts)
	if err != nil {
		log.Errorf("Failed to issue a %s credential: %v", opts.Kind, err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	if req.enrolling() {
		writeJSON(w, http.StatusCreated, enrolmentResponse{
			Credential: token, ExpiresAt: cred.ExpiresAt, Email: acct.Email,
		})
		return
	}

	// The response field is still "token" holding a string. What the string
	// is has changed completely; what a client has to do with it has not, and
	// keeping the shape identical is what let this land without every client
	// shipping on the same day. Its expiry is deliberately not reported here:
	// see docs/bugs/fixed/adding-a-number-to-a-token-response-breaks-clients.md
	// for what adding a number to a token body costs.
	writeJSON(w, http.StatusOK, loginResponse{Token: token})
}

// defaultSessionOpts is what a credential looks like when the client asked for
// nothing in particular: a session, for as long as sessions last, with a
// ceiling middleware.Perm narrows per request.
//
// One definition, because two handlers start from it -- login and setup -- and
// the one an operator is most likely to keep using is the one setup issues.
// Narrowing the default perm or changing the lifetime in a place setup did not
// read would leave that credential on the old policy.
func defaultSessionOpts() credential.IssueOpts {
	return credential.IssueOpts{Kind: credential.KindSession, Perm: "rw", Lifetime: sessionLifetime}
}

// enrolmentOpts turns the request into what credential.Issue takes, answering
// the client itself and returning false if the request cannot be honoured.
//
// perm and scope get no "may they ask for this?" branch, deliberately. They
// are a ceiling rather than a grant -- middleware.Perm intersects them with
// what the account may do on every request -- so asking for rw on an account
// that has r yields r, not a 403. A field that can only narrow needs no
// validation beyond being spellable.
func enrolmentOpts(w http.ResponseWriter, req loginRequest) (credential.IssueOpts, bool) {
	opts := defaultSessionOpts()
	if !req.enrolling() {
		return opts, true
	}

	switch req.Kind {
	case "", string(credential.KindSession):
		// The default. A client may ask for a named session rather than one
		// labelled from its User-Agent.
	case string(credential.KindDevice):
		opts.Kind = credential.KindDevice
		opts.Lifetime = deviceLifetime
	default:
		// access and s3 are real kinds and are not minted by presenting a
		// password: one belongs to a capability URL and lives in memory, the
		// other derives its secret from the master key. Naming them here is a
		// client that has misread the model, not one that lacks permission.
		http.Error(w, `kind must be "session" or "device"`, http.StatusBadRequest)
		return opts, false
	}

	// Proof of possession is designed and the RFC 9421 verifier is not built,
	// so a public-key row would resolve to ErrSignatureNotImplemented forever.
	// Refusing here matches what RequireCredential answers for Authorization:
	// Silo, and is better than handing back a credential that can never work.
	if req.PublicKey != "" {
		http.Error(w, "Signature authentication is not implemented; omit public_key",
			http.StatusNotImplemented)
		return opts, false
	}

	// A label is what turns revocation from a guess into a decision. A client
	// that asks for a durable credential and will not say what it is leaves an
	// operator four indistinguishable rows, so this one is required rather
	// than defaulted from the User-Agent.
	if req.ClientName == "" {
		http.Error(w, "client_name is required when asking for a credential", http.StatusBadRequest)
		return opts, false
	}
	opts.Label = req.ClientName

	if req.Perm != "" {
		// Checked because credential.Issue refuses an unrecognised perm, and
		// because minPerm reads one as no access at all: a typo would
		// otherwise mint a credential that authenticates and permits nothing.
		if req.Perm != "r" && req.Perm != "rw" {
			http.Error(w, `perm must be "r" or "rw"`, http.StatusBadRequest)
			return opts, false
		}
		opts.Perm = req.Perm
	}

	scope, err := credential.ParseScope(req.Scope)
	if err != nil {
		// The parser's message describes the string the client sent and
		// nothing about which libraries exist.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return opts, false
	}
	opts.Scope = scope

	return opts, true
}

// credentialLabel names the credential after the client that asked for it.
//
// A label is what turns revocation from a guess into a decision, so the worst
// answer here is an empty one -- an operator looking at four unnamed rows
// cannot tell which is the laptop they just lost. A User-Agent is a weak name
// and a great deal better than none.
func credentialLabel(r *http.Request) string {
	ua := strings.TrimSpace(r.UserAgent())
	if ua == "" {
		return "unnamed client"
	}
	if len(ua) > maxLabel {
		// Truncated rather than refused: the label is for a human reading a
		// list, and a client sending a paragraph should not fail to log in.
		return ua[:maxLabel]
	}
	return ua
}

const maxLabel = 96

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

// librarySelect is shared by the owned and shared queries so the two cannot drift
// into scanning different columns than they select. The join is LEFT because a
// library with no branch row is broken but should still be listable — a client
// that can see it can delete it.
func librarySelect(alias string) string {
	// f.e2ee, not i.is_encrypted. LibraryInfo.is_encrypted is the old column --
	// a password over a server-side key -- which nothing writes and every
	// creation path hard-codes to 0, so this flag read false for an
	// end-to-end encrypted library as surely as for a plain one. Library.e2ee
	// is the library's own answer to "can the server read this", and is the
	// column the schema comment says is authoritative.
	return "SELECT " + alias + ".library_id, i.name, i.update_time, f.e2ee, b.commit_id, " +
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
		var name, commitID, headRoot, measuredAt, chunker sql.NullString
		var isEncrypted sql.NullBool
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
		library.Encrypted = isEncrypted.Bool
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
		librarySelect("o")+
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

	// Shared with me, read through the grant model rather than through
	// SharedLibrary. A listing that answered from a table CheckPerm no longer
	// consults would show a library the caller cannot open, or hide one they
	// can -- which is the second-reader drift the unification exists to end.
	//
	// Group grants come along for free: principalsFor expands the account into
	// every principal it carries, so a library shared to a team the caller is
	// in now appears here. It did not before, and that was a gap rather than a
	// decision.
	principals := share.PrincipalsFor(id)
	sharedRows, err := readDB.QueryContext(ctx,
		librarySelect("g")+
			"FROM LibraryGrant g LEFT JOIN LibraryInfo i ON g.library_id = i.library_id "+
			"LEFT JOIN Branch b ON b.library_id = g.library_id AND b.name = 'master' "+
			usageJoin+"g.library_id "+
			formatJoin+"g.library_id "+
			"WHERE g.path = '/' AND g.principal IN ("+sharePlaceholders(principals)+")",
		principalArgs(principals)...)
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

	// E2EE asks for an end-to-end encrypted library, and the four fields below
	// come with it. They are here rather than on a second route because a
	// client is doing one thing -- creating a library -- and the difference is
	// what it has to bring.
	E2EE bool `json:"e2ee"`

	// LibraryID is the client's, and only for an E2EE library. store.WrapCK
	// binds the library id into the wrap as associated data, so the id has to
	// exist before the content key can be wrapped to anybody -- which means
	// either the client mints it, or creation takes two requests with a window
	// in between holding a library whose key nobody stored.
	LibraryID string `json:"library_id"`

	Root       []byte `json:"root"`
	Commit     []byte `json:"commit"`
	WrappedKey []byte `json:"wrapped_key"`
}

type createLibraryResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func CreateLibraryHandler(w http.ResponseWriter, r *http.Request) {
	// Creating a library is a write, and a read-only credential must not do
	// one. There is no library to ask share.CheckPerm about yet -- the account
	// is creating its own -- so this asks the credential's ceiling directly
	// rather than through middleware.Perm.
	if !middleware.CredentialCanWrite(r) {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return
	}
	acct := middleware.GetAccount(r)

	var req createLibraryRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.Name == "" {
		http.Error(w, "Name is required", http.StatusBadRequest)
		return
	}

	if req.E2EE {
		createEncryptedLibrary(w, r, acct, req)
		return
	}
	// A plain library's initial objects are the server's to mint, so nothing
	// else has to arrive with the request. Fields that only mean something for
	// an encrypted library are refused rather than ignored: a client that sent
	// a wrapped key and got a library the server can read has been told its
	// request succeeded, and the difference will not surface until somebody
	// reads the data.
	if req.LibraryID != "" || len(req.Root) > 0 || len(req.Commit) > 0 || len(req.WrappedKey) > 0 {
		http.Error(w, `library_id, root, commit and wrapped_key are only for "e2ee": true`,
			http.StatusBadRequest)
		return
	}

	libraryID, err := libmgr.CreateLibrary(req.Name, acct, libmgr.DefaultFormat(false))
	if err != nil {
		log.Errorf("Failed to create library: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	notif.NotifyAccountUpdate(acct.ID)
	writeJSON(w, http.StatusCreated, createLibraryResponse{ID: libraryID, Name: req.Name})
}

// createEncryptedLibrary is the E2EE half of CreateLibraryHandler.
//
// The account must already have published an identity key. That is checked
// here rather than inside libmgr because it is an account fact rather than a
// library one, and it is checked at all because a content key wrapped to
// nothing lives on the device that made it and dies with it -- which is data
// loss wearing a feature's clothes, and is exactly why this route could not
// exist until account/keys did.
func createEncryptedLibrary(w http.ResponseWriter, r *http.Request, acct *account.Account, req createLibraryRequest) {
	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	if _, err := account.PublicKey(ctx, acct.ID); err != nil {
		if errors.Is(err, account.ErrNoKeys) {
			http.Error(w,
				"This account has published no identity key, so an encrypted library's "+
					"content key would have nowhere recoverable to live. PUT account/keys first.",
				http.StatusConflict)
			return
		}
		log.Errorf("Failed to read the creator's public key: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	libraryID, err := libmgr.CreateEncryptedLibrary(req.Name, acct, libmgr.DefaultFormat(true),
		libmgr.EncryptedSeed{
			LibraryID:  req.LibraryID,
			Root:       req.Root,
			Commit:     req.Commit,
			WrappedKey: req.WrappedKey,
		})
	switch {
	case err == nil:
	case errors.Is(err, libmgr.ErrBadSeed):
		// Said in full: every one of these is a client bug that would
		// otherwise produce a library which loads and cannot be read.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case errors.Is(err, libmgr.ErrLibraryExists):
		http.Error(w, "A library with that id already exists", http.StatusConflict)
		return
	default:
		log.Errorf("Failed to create an encrypted library: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	notif.NotifyAccountUpdate(acct.ID)
	writeJSON(w, http.StatusCreated, createLibraryResponse{ID: libraryID, Name: req.Name})
}

// LibraryKeyHandler handles GET /api/silo/v1/libraries/{libraryid}/key.
//
// It serves the library's content key wrapped to the calling account, which is
// what a new device needs after it has opened its identity key: the wrap is
// useless without that key, and the server holds neither in a form it can use.
//
// Read permission, because that is what the wrap grants: whoever can read the
// library's ciphertext and holds this can read the library, and whoever cannot
// read it gains nothing from a blob they cannot open. The library id is in the
// route, so a scoped credential reaches its own library's key and no other.
func LibraryKeyHandler(w http.ResponseWriter, r *http.Request) {
	libraryID := mux.Vars(r)["libraryid"]
	if middleware.Perm(r, libraryID, "") == "" {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return
	}

	ctx, cancel := option.WithDBTimeout(r.Context())
	defer cancel()

	wrapped, err := libmgr.ContentKeyWrap(ctx, libraryID, middleware.GetAccountID(r))
	if errors.Is(err, libmgr.ErrNoContentKeyWrap) {
		// Not 403: the caller may read this library. There is simply no wrap
		// -- because it is a plain library, or because nobody has shared this
		// one with them yet.
		http.Error(w, "No content key wrap for this library", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Errorf("Failed to read a content key wrap: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		WrappedKey []byte `json:"wrapped_key"`
	}{WrappedKey: wrapped})
}

func DeleteLibraryHandler(w http.ResponseWriter, r *http.Request) {
	// Deleting is the most destructive write there is, and it was reachable
	// with a read-only credential: this handler asks only whether the account
	// owns the library. Ownership is still the rule; the ceiling narrows it.
	if !middleware.CredentialCanWrite(r) {
		http.Error(w, "Permission denied", http.StatusForbidden)
		return
	}
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
	notif.NotifyLibraryChanged(libraryID)

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
	Size  *int64 `json:"size,omitempty"`
	Mtime int64  `json:"mtime"`
	// There is deliberately no Modifier. There was one, tagged omitempty and
	// never assigned by any code path, so it could not reach a client — but it
	// reached the documentation, whose listing example showed it on every file
	// row, and a client written to that example waits for a field that never
	// arrives. Who last wrote a file is a real thing to want and is not
	// recorded anywhere the listing can reach; if it comes back it comes back
	// with a value.
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
// The sizes come from libmgr, which reads them through the sidecar and repairs
// what it does not have yet. How much repair a listing is worth, and that a
// repaired size is written down, are the sidecar's decisions and are made
// there; what belongs here is only which entries want a size.
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
	sizes := libmgr.FileSizes(library, ids)

	for i := range page {
		if page[i].Type != "file" {
			continue
		}
		if size, ok := sizes[page[i].ID]; ok {
			page[i].Size = &size
		}
	}
}

// sharePlaceholders and principalArgs turn a principal set into an IN clause.
// Two tiny helpers rather than a string-built list, because a principal is
// caller-influenced text the moment link principals exist, and an IN clause
// assembled by concatenation is the one place that stops being safe.
func sharePlaceholders(p []share.Principal) string {
	return strings.TrimSuffix(strings.Repeat("?,", len(p)), ",")
}

func principalArgs(p []share.Principal) []any {
	args := make([]any, 0, len(p))
	for _, one := range p {
		args = append(args, one)
	}
	return args
}
