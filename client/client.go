package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

type APIClient struct {
	BaseURL string

	// mu guards token/email/password. Bubble Tea runs tea.Cmd callbacks in
	// separate goroutines, so concurrent requests can race on token refresh.
	mu       sync.Mutex
	token    string
	email    string
	password string
	// serverInfo is what /server-info said, fetched at most once. Nil means
	// not yet asked; a failed ask caches the zero value, because a server that
	// cannot answer has no capabilities worth waiting for.
	serverInfo *ServerInfo
	// chunkers is what the libraries listing said about each library's
	// chunking, keyed by library id. A library's parameters are frozen when it
	// is created, so this never needs invalidating within a process — and
	// without it every large file uploaded on its own re-fetches the whole
	// listing to read one library's row.
	chunkers map[string]chunkerCache
}

func (c *APIClient) getToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

type Library struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	UpdateTime int64  `json:"update_time"`
	Encrypted  bool   `json:"encrypted"`
	// HeadCommitID is where the library is now — the anchor to pass to Changes
	// on the first call, before there is a previous anchor to hand back.
	HeadCommitID string `json:"head_commit_id"`
	// Chunker is how this library's bytes are cut, and it is the library's
	// property rather than the server's: two libraries on one server can be
	// chunked differently, and a client that assumed otherwise would compute
	// ids that dedup against nothing. Nil means the server did not say, which
	// is a reason to upload whole files rather than to fall back on a guess.
	Chunker *ChunkerParams `json:"chunker,omitempty"`
}

// ChunkerParams is a library's chunker as the server reports it.
//
// No seed: a plain library chunks under the published constant, which every
// implementation derives for itself, and an E2EE library's seed comes from a
// content key no server holds.
type ChunkerParams struct {
	Algorithm     string `json:"algorithm"`
	MinSize       int    `json:"min_size"`
	TargetSize    int    `json:"target_size"`
	MaxSize       int    `json:"max_size"`
	Normalization int    `json:"normalization"`
}

type DirEntry struct {
	Name string `json:"name"`
	Type string `json:"type"` // "file" or "dir"
	ID   string `json:"id"`
	// Size is a pointer because absent and zero are different answers, and on
	// this endpoint the difference is the common case. A dirent
	// carries no size — it lives in the file's manifest — so the server omits
	// it rather than reading N manifests to answer one listing. Rendering that
	// as 0 B states a fact the server never gave: an empty file and a file of
	// unknown size are not the same thing to anyone reading the output.
	Size     *int64 `json:"size,omitempty"`
	Mtime    int64  `json:"mtime"`
	Modifier string `json:"modifier,omitempty"`
}

func NewClient(baseURL string) *APIClient {
	return &APIClient{BaseURL: baseURL}
}

// doRequest performs an authenticated HTTP request, transparently re-logging
// in and retrying once if the server returns 401.
func (c *APIClient) doRequest(method, path string, body, result interface{}) error {
	_, err := c.doRequestHeaders(method, path, body, result)
	return err
}

// doRequestHeaders is doRequest with the response headers handed back, for the
// callers that need them — pagination reads its next page out of Link.
func (c *APIClient) doRequestHeaders(method, path string, body, result interface{}) (http.Header, error) {
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to encode request: %v", err)
		}
	}

	tokenUsed := c.getToken()
	resp, err := c.sendRequest(method, path, bodyBytes, tokenUsed)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized && c.hasCreds() {
		_ = resp.Body.Close()
		if err := c.reloginIfStale(tokenUsed); err != nil {
			return nil, fmt.Errorf("re-login failed: %v", err)
		}
		resp, err = c.sendRequest(method, path, bodyBytes, c.getToken())
		if err != nil {
			return nil, err
		}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		return resp.Header, &StatusError{Code: resp.StatusCode, Status: resp.Status, Body: string(msg)}
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return resp.Header, fmt.Errorf("failed to parse response: %v", err)
		}
	}
	return resp.Header, nil
}

// StatusError is a response the server refused with. It carries the code as
// well as the text because some refusals are answers rather than failures: a
// 404 from libraries/{id}/key means this account holds no wrap for that
// library, which is what a plain library and an unshared one both look like,
// and a caller has to be able to tell that from a server that fell over.
//
// Its message is the status line and the body, which is what this client has
// always returned, so anything that only displays the error is unaffected.
type StatusError struct {
	Code   int
	Status string
	Body   string
}

func (e *StatusError) Error() string { return fmt.Sprintf("%s: %s", e.Status, e.Body) }

// hasStatus reports whether err is the server answering with this status.
//
// errors.As rather than a comparison, because the transport wraps some statuses
// in a package sentinel on the way out -- a 404 is both ErrNotFound and a 404,
// and a caller asking either question has to get the same answer.
func hasStatus(err error, code int) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == code
}

// isNotFound reports whether err is the server saying 404.
func isNotFound(err error) bool { return hasStatus(err, http.StatusNotFound) }

// doStream performs an authenticated request whose body is streamed rather than
// buffered, and — like doRequest — re-logs in and retries once on a 401.
//
// newBody is a factory rather than a reader because the retry has to send the
// body again, and a reader that has already been consumed cannot. It returns
// the length too, so the request can declare Content-Length instead of falling
// back to chunked encoding; the server checks the declared length against its
// upload limit before reading anything.
//
// The response body is left open for the caller to stream from.
func (c *APIClient) doStream(method, path, contentType string, newBody func() (io.ReadCloser, int64, error)) (*http.Response, error) {
	return c.doStreamHeaders(method, path, contentType, nil, newBody)
}

// doStreamHeaders is doStream with extra request headers -- If-Match on a head
// move is the one that needs them, and it needs them on the retry too, which
// is why they go in here rather than being set on a response that has already
// been sent.
func (c *APIClient) doStreamHeaders(method, path, contentType string, header http.Header,
	newBody func() (io.ReadCloser, int64, error),
) (*http.Response, error) {
	send := func(token string) (*http.Response, error) {
		var (
			body   io.ReadCloser
			length int64
		)
		if newBody != nil {
			b, n, err := newBody()
			if err != nil {
				return nil, err
			}
			body, length = b, n
		}

		req, err := http.NewRequest(method, c.BaseURL+path, body)
		if err != nil {
			if body != nil {
				_ = body.Close()
			}
			return nil, fmt.Errorf("failed to create request: %v", err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		for k, vs := range header {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		if newBody != nil {
			req.ContentLength = length
		}
		return http.DefaultClient.Do(req)
	}

	tokenUsed := c.getToken()
	resp, err := send(tokenUsed)
	if err != nil {
		return nil, fmt.Errorf("request failed: %v", err)
	}
	if resp.StatusCode == http.StatusUnauthorized && c.hasCreds() {
		_ = resp.Body.Close()
		if err := c.reloginIfStale(tokenUsed); err != nil {
			return nil, fmt.Errorf("re-login failed: %v", err)
		}
		resp, err = send(c.getToken())
		if err != nil {
			return nil, fmt.Errorf("request failed: %v", err)
		}
	}
	return resp, nil
}

func (c *APIClient) sendRequest(method, path string, bodyBytes []byte, token string) (*http.Response, error) {
	var bodyReader io.Reader
	if bodyBytes != nil {
		bodyReader = bytes.NewReader(bodyBytes)
	}

	req, err := http.NewRequest(method, c.BaseURL+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if bodyBytes != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connection failed: %v", err)
	}
	return resp, nil
}

func (c *APIClient) hasCreds() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.email != "" && c.password != ""
}

// reloginIfStale refreshes the token only if it hasn't already been refreshed
// since the caller observed `tokenUsed`. This collapses concurrent 401s from a
// single expiry into a single login request.
func (c *APIClient) reloginIfStale(tokenUsed string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != tokenUsed {
		return nil
	}
	return c.reloginLocked()
}

// reloginLocked performs a login and updates c.token. Caller must hold c.mu.
func (c *APIClient) reloginLocked() error {
	return c.postForTokenLocked("/api/silo/v1/auth/login",
		map[string]string{"email": c.email, "password": c.password})
}

// postForTokenLocked posts to one of the unauthenticated credential endpoints
// and keeps the token it answers with. Caller must hold c.mu.
//
// Login and setup share it because their responses are deliberately the same
// shape -- one string under "token" -- so that a client needs one piece of
// parsing rather than two that could drift.
func (c *APIClient) postForTokenLocked(path string, body map[string]string) error {
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := c.sendRequest("POST", path, bodyBytes, "")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	c.token = result.Token
	return nil
}

func (c *APIClient) Login(email, password string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.email = email
	c.password = password
	return c.reloginLocked()
}

// Setup creates this server's first account and signs in as it.
//
// The credentials are kept exactly as Login keeps them, because the session it
// returns expires in twenty-four hours like any other and the automatic
// re-login on a 401 needs something to present. Unlike Login they are stored
// only once the request has succeeded: Login is re-authenticating an account
// that exists, whereas a refused setup names an account that was never created,
// and holding those would turn every later 401 into a re-login that cannot
// succeed.
func (c *APIClient) Setup(email, password, setupToken string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	err := c.postForTokenLocked("/api/silo/v1/auth/setup", map[string]string{
		"email": email, "password": password, "setup_token": setupToken,
	})
	if err != nil {
		return err
	}
	c.email = email
	c.password = password
	return nil
}

// ChangePassword sets a new password on the signed-in account and reports how
// many session credentials the server signed out.
//
// The current password is required even though the request is authenticated,
// and the server is the one insisting: a credential handed to a device must not
// be able to promote itself into the account. See api.ChangePasswordHandler.
//
// The two things after the request are not tidying. The server revokes every
// session credential, and the TUI holds one, so by the time this returns the
// token that made the call is dead and the cached password it would replay on
// the resulting 401 is the one that no longer works. Swapping the cache and
// signing in again is what keeps a successful change from presenting as being
// signed out with a complaint about a password the caller just proved they
// knew.
//
// The order matters on the failure paths. The cache is swapped before the
// re-login rather than after, so that a re-login which fails for its own
// reasons -- the server restarting in the gap, a network that dropped -- still
// leaves the client holding the password that is now true; the next request
// retries against it and succeeds. Swapping afterwards would strand the client
// on a password the server has forgotten.
func (c *APIClient) ChangePassword(current, next string) (int, error) {
	var result struct {
		Revoked int `json:"revoked"`
		// Set when the password changed but the sessions it should have
		// signed out are still live. A success with a caveat, not a failure —
		// the server reports it as 200 for exactly that reason.
		SessionsStillLive bool `json:"sessions_still_live"`
	}
	if err := c.doRequest("POST", "/api/silo/v1/auth/password", map[string]string{
		"current_password": current,
		"new_password":     next,
	}, &result); err != nil {
		return 0, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.password = next
	// Nothing to sign back in as. A client driven by a token it was handed
	// rather than by a login has no address to present, and it never had the
	// automatic re-login either; the caller gets the count and the fact that
	// its token is now spent.
	if c.email == "" {
		return result.Revoked, nil
	}
	return result.Revoked, c.reloginLocked()
}

func (c *APIClient) ListLibraries() ([]Library, error) {
	var libraries []Library
	err := c.doRequest("GET", "/api/silo/v1/libraries", nil, &libraries)
	return libraries, err
}

// Library returns one library's record from the listing.
//
// A scan of GET libraries rather than a request for the one, because there is
// no route for the one: a library's head, its chunker and whether it is
// encrypted are all only served by the collection. Everything that needs any
// of those goes through here, so when a single-library route lands there is
// one call site to change.
func (c *APIClient) Library(libraryID string) (Library, error) {
	libraries, err := c.ListLibraries()
	if err != nil {
		return Library{}, err
	}
	for _, lib := range libraries {
		if lib.ID == libraryID {
			return lib, nil
		}
	}
	return Library{}, fmt.Errorf("%w: library %s", ErrNotFound, libraryID)
}

func (c *APIClient) CreateLibrary(name string) (*Library, error) {
	var library Library
	err := c.doRequest("POST", "/api/silo/v1/libraries", map[string]string{"name": name}, &library)
	if err != nil {
		return nil, err
	}
	return &library, nil
}

// The file operations below all address one endpoint — entries — with the HTTP
// method as the verb, rather than the older dir/mkdir/rename/move/file/download
// spelling where each operation had its own URL and the path moved between the
// query string and the body depending which one you called.
//
// The method signatures here are deliberately unchanged, so the TUI and the CLI
// carried over without edits. That is the argument for having done it now:
// while every caller ships in this binary, the surface can be fixed in place.

// entriesURL builds the address of one entry. The path is part of the URL
// rather than a query parameter, so it is escaped per segment: the separators
// have to survive as separators for the route to match, and everything else has
// to be escaped or a file named "a?b" would truncate the request.
func entriesURL(libraryID, p string) string {
	return "/api/silo/v1/libraries/" + url.PathEscape(libraryID) + "/entries/" + escapePathSegments(p)
}

func escapePathSegments(p string) string {
	segments := strings.Split(strings.Trim(p, "/"), "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	// The root trims to nothing, leaving "entries/" — which is the root.
	return strings.Join(segments, "/")
}

func (c *APIClient) ListDir(libraryID, path string) ([]DirEntry, error) {
	var entries []DirEntry
	err := c.doRequest("GET", entriesURL(libraryID, path), nil, &entries)
	return entries, err
}

func (c *APIClient) DeleteLibrary(libraryID string) error {
	return c.doRequest("DELETE", "/api/silo/v1/libraries/"+libraryID, nil, nil)
}

// Mkdir asks for a directory explicitly. A PUT with no ?type stores the request
// body as a file, so the parameter is load-bearing rather than decorative: drop
// it and this creates an empty file where a directory was meant.
func (c *APIClient) Mkdir(libraryID, path string) error {
	return c.doRequest("PUT", entriesURL(libraryID, path)+"?type=dir", nil, nil)
}

// RenameFile renames by moving: a rename is a move whose destination shares a
// parent with its source, and the server has no separate operation for it.
func (c *APIClient) RenameFile(libraryID, remotePath, newName string) error {
	return c.MoveFile(libraryID, remotePath, path.Join(path.Dir(remotePath), newName))
}

func (c *APIClient) MoveFile(libraryID, src, dst string) error {
	return c.doRequest("POST", entriesURL(libraryID, src),
		map[string]string{"op": "move", "to": dst}, nil)
}

// CopyFile copies a file or a whole directory server-side. The destination
// dirent points at the object the source already names, so this transfers no
// content whatever the size — which is the difference between calling it and
// emulating it with a download followed by an upload.
func (c *APIClient) CopyFile(libraryID, src, dst string) error {
	return c.doRequest("POST", entriesURL(libraryID, src),
		map[string]string{"op": "copy", "to": dst}, nil)
}

// RenameLibrary renames a library. PATCH, not PUT: the body names what changes and
// leaves the rest alone, so this keeps meaning the same thing when the server
// grows a second mutable field.
func (c *APIClient) RenameLibrary(libraryID, name string) error {
	return c.doRequest("PATCH", "/api/silo/v1/libraries/"+libraryID,
		map[string]string{"name": name}, nil)
}

func (c *APIClient) DeleteFile(libraryID, path string) error {
	return c.doRequest("DELETE", entriesURL(libraryID, path), nil, nil)
}

// HeadCommitID on Library is the anchor for the first Changes call; see libraryInfo
// in fileserver/api.
//
// Change is one path that differs between two commits, as reported by Changes.
type Change struct {
	Op      string `json:"op"` // create | delete | modify | move
	Path    string `json:"path"`
	OldPath string `json:"old_path,omitempty"`
	ID      string `json:"id,omitempty"`
	Size    int64  `json:"size"`
	IsDir   bool   `json:"is_dir"`
}

// ChangesResponse carries the changes and the commit they bring you up to.
type ChangesResponse struct {
	Anchor  string   `json:"anchor"`
	Changes []Change `json:"changes"`
}

// Changes reports everything that differs between a commit and the current
// head. Pass the previous call's Anchor as since; the first call has no anchor,
// so enumerate with ListDir and use the library's head commit.
func (c *APIClient) Changes(libraryID, since string) (*ChangesResponse, error) {
	var resp ChangesResponse
	err := c.doRequest("GET", fmt.Sprintf("/api/silo/v1/libraries/%s/changes?since=%s",
		url.PathEscape(libraryID), url.QueryEscape(since)), nil, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// DownloadFile writes a file from the library to localPath.
//
// The bytes arrive on this response. There used to be a redirect here, to a
// /files/{token}/{name} URL carrying a one-time credential, and following it
// took a second unauthenticated request — see docs/capability-urls.md for why
// that shape existed and why this lane does not need it.
func (c *APIClient) DownloadFile(libraryID, libraryPath, localPath string) error {
	resp, err := c.doStream("GET", entriesURL(libraryID, libraryPath), "", nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("download failed: %s: %s", resp.Status, string(msg))
	}

	out, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("failed to create local file: %v", err)
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("failed to write file: %v", err)
	}
	return nil
}

// UploadFile writes a local file into a library directory.
//
// One request: the body is the file. This used to be three — mint an access
// token, build a multipart form, POST it to /upload-api/{token} — which is the
// shape a browser needs, because an HTML form cannot set an Authorization
// header either. See docs/capability-urls.md.
//
// The file is streamed from disk rather than buffered, so uploading something
// large does not mean holding it in memory. It is opened per attempt, so a
// retry after a token refresh sends the file from the beginning rather than
// from wherever the first attempt stopped.
// UploadFile sends a local file to a library.
//
// Which way it goes is not a flag. A big file goes up as chunks, which skips
// content the server already holds and resumes from what landed rather than
// from zero; a small one goes as a single PUT, which is one request against
// three and needs no hashing at all. Three things have to hold for the chunk
// path — the server offers it, the library's chunker is known, and the file is
// large enough to pay for the extra round trips — and any of them failing
// takes the whole-file path, which always works.
func (c *APIClient) UploadFile(libraryID, parentDir, localPath string) error {
	if st, err := os.Stat(localPath); err == nil && st.Size() > chunkLaneThreshold {
		if c.capabilities().Has("chunks") {
			if p, ok := c.chunkerFor(libraryID); ok {
				return c.uploadChunks(libraryID, parentDir, localPath, p)
			}
		}
	}
	return c.uploadWhole(libraryID, parentDir, localPath)
}

func (c *APIClient) uploadWhole(libraryID, parentDir, localPath string) error {
	remote := path.Join("/", parentDir, filepath.Base(localPath))
	resp, err := c.doStream("PUT", entriesURL(libraryID, remote), "application/octet-stream",
		func() (io.ReadCloser, int64, error) {
			file, err := os.Open(localPath)
			if err != nil {
				return nil, 0, fmt.Errorf("failed to open file: %v", err)
			}
			info, err := file.Stat()
			if err != nil {
				_ = file.Close()
				return nil, 0, fmt.Errorf("failed to stat file: %v", err)
			}
			return file, info.Size(), nil
		})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload failed: %s: %s", resp.Status, string(msg))
	}
	return nil
}

// ServerInfo holds the response from /api/silo/v1/server-info.
type ServerInfo struct {
	Version  string   `json:"version"`
	Features []string `json:"features"`

	// SetupRequired says this server has no accounts yet and is waiting for
	// someone holding its setup token to create the first one. State rather
	// than a capability, so it is a field here and "setup" is the feature name
	// beside it: the name says the server can be claimed at all, this says it
	// still needs to be.
	SetupRequired bool `json:"setup_required"`
}

// Has reports whether the server advertises a capability. Prefer it to
// comparing Version: the version says which build answered, the feature list
// says what that build will accept, and only the second question is the one a
// caller actually has. An older server simply returns no list, so an unknown
// name is absent rather than an error.
func (s ServerInfo) Has(feature string) bool {
	return slices.Contains(s.Features, feature)
}

// GetServerInfo fetches version information from the server.
func (c *APIClient) GetServerInfo() (ServerInfo, error) {
	resp, err := http.Get(c.BaseURL + "/api/silo/v1/server-info")
	if err != nil {
		return ServerInfo{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		return ServerInfo{}, fmt.Errorf("%s: %s", resp.Status, string(msg))
	}

	var info ServerInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return ServerInfo{}, err
	}
	return info, nil
}

// BatchOp is one operation in a batch. Op is "mkdir", "delete", "move",
// "copy" or "create"; To is for move and copy, Chunks for create.
type BatchOp struct {
	Op     string   `json:"op"`
	Path   string   `json:"path"`
	To     string   `json:"to,omitempty"`
	Chunks []string `json:"chunks,omitempty"`
}

// BatchResult is what the server did with a batch.
type BatchResult struct {
	CommitID string `json:"commit_id"`
	Ops      int    `json:"ops"`
	// Changed is false when the operations left the tree as it was — every
	// mkdir already existed, say. The library is untouched and no commit was
	// minted, which is worth distinguishing from having done the work.
	Changed bool `json:"changed"`
}

// Batch applies many operations as one commit, in order, all or nothing.
//
// Operations see the effects of the ones before them, so a mkdir followed by
// creates inside it is a single request. If any fails, nothing is written and
// the error names the index that stopped it.
//
// Pair it with the chunk surface for a bulk upload: send the chunks first,
// which skips everything the server already holds, then create every file in
// one commit rather than one commit per file.
func (c *APIClient) Batch(libraryID string, ops []BatchOp) (*BatchResult, error) {
	var result BatchResult
	err := c.doRequest("POST", "/api/silo/v1/libraries/"+libraryID+"/batch",
		map[string][]BatchOp{"ops": ops}, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}
