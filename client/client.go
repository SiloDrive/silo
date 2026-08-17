package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
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
}

func (c *APIClient) getToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

type Repo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	UpdateTime int64  `json:"update_time"`
	Encrypted  bool   `json:"encrypted"`
	// HeadCommitID is where the library is now — the anchor to pass to Changes
	// on the first call, before there is a previous anchor to hand back.
	HeadCommitID string `json:"head_commit_id"`
}

type DirEntry struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // "file" or "dir"
	ID       string `json:"id"`
	Size     int64  `json:"size,omitempty"`
	Mtime    int64  `json:"mtime"`
	Modifier string `json:"modifier,omitempty"`
}

func NewClient(baseURL string) *APIClient {
	return &APIClient{BaseURL: baseURL}
}

// doRequest performs an authenticated HTTP request, transparently re-logging
// in and retrying once if the server returns 401.
func (c *APIClient) doRequest(method, path string, body, result interface{}) error {
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("failed to encode request: %v", err)
		}
	}

	tokenUsed := c.getToken()
	resp, err := c.sendRequest(method, path, bodyBytes, tokenUsed)
	if err != nil {
		return err
	}

	if resp.StatusCode == http.StatusUnauthorized && c.hasCreds() {
		_ = resp.Body.Close()
		if err := c.reloginIfStale(tokenUsed); err != nil {
			return fmt.Errorf("re-login failed: %v", err)
		}
		resp, err = c.sendRequest(method, path, bodyBytes, c.getToken())
		if err != nil {
			return err
		}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, string(msg))
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("failed to parse response: %v", err)
		}
	}
	return nil
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
	bodyBytes, err := json.Marshal(map[string]string{"email": c.email, "password": c.password})
	if err != nil {
		return err
	}
	resp, err := c.sendRequest("POST", "/api/silo/v1/auth/login", bodyBytes, "")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, string(msg))
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

func (c *APIClient) ListRepos() ([]Repo, error) {
	var repos []Repo
	err := c.doRequest("GET", "/api/silo/v1/repos", nil, &repos)
	return repos, err
}

func (c *APIClient) CreateRepo(name string) (*Repo, error) {
	var repo Repo
	err := c.doRequest("POST", "/api/silo/v1/repos", map[string]string{"name": name}, &repo)
	if err != nil {
		return nil, err
	}
	return &repo, nil
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
func entriesURL(repoID, p string) string {
	return "/api/silo/v1/repos/" + url.PathEscape(repoID) + "/entries/" + escapePathSegments(p)
}

func escapePathSegments(p string) string {
	segments := strings.Split(strings.Trim(p, "/"), "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	// The root trims to nothing, leaving "entries/" — which is the root.
	return strings.Join(segments, "/")
}

func (c *APIClient) ListDir(repoID, path string) ([]DirEntry, error) {
	var entries []DirEntry
	err := c.doRequest("GET", entriesURL(repoID, path), nil, &entries)
	return entries, err
}

func (c *APIClient) DeleteRepo(repoID string) error {
	return c.doRequest("DELETE", "/api/silo/v1/repos/"+repoID, nil, nil)
}

// Mkdir asks for a directory explicitly. A PUT with no ?type stores the request
// body as a file, so the parameter is load-bearing rather than decorative: drop
// it and this creates an empty file where a directory was meant.
func (c *APIClient) Mkdir(repoID, path string) error {
	return c.doRequest("PUT", entriesURL(repoID, path)+"?type=dir", nil, nil)
}

// RenameFile renames by moving: a rename is a move whose destination shares a
// parent with its source, and the server has no separate operation for it.
func (c *APIClient) RenameFile(repoID, remotePath, newName string) error {
	return c.MoveFile(repoID, remotePath, path.Join(path.Dir(remotePath), newName))
}

func (c *APIClient) MoveFile(repoID, src, dst string) error {
	return c.doRequest("POST", entriesURL(repoID, src),
		map[string]string{"op": "move", "to": dst}, nil)
}

func (c *APIClient) DeleteFile(repoID, path string) error {
	return c.doRequest("DELETE", entriesURL(repoID, path), nil, nil)
}

// HeadCommitID on Repo is the anchor for the first Changes call; see repoInfo
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
// so enumerate with ListDir and use the repo's head commit.
func (c *APIClient) Changes(repoID, since string) (*ChangesResponse, error) {
	var resp ChangesResponse
	err := c.doRequest("GET", fmt.Sprintf("/api/silo/v1/repos/%s/changes?since=%s",
		url.PathEscape(repoID), url.QueryEscape(since)), nil, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// DownloadFile writes a file from the repo to localPath.
//
// The bytes arrive on this response. There used to be a redirect here, to a
// /files/{token}/{name} URL carrying a one-time credential, and following it
// took a second unauthenticated request — see docs/capability-urls.md for why
// that shape existed and why this lane does not need it.
func (c *APIClient) DownloadFile(repoID, repoPath, localPath string) error {
	req, err := http.NewRequest("GET", c.BaseURL+entriesURL(repoID, repoPath), nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.getToken())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %v", err)
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

// UploadFile writes a local file into a repo directory.
//
// One request: the body is the file. This used to be three — mint an access
// token, build a multipart form, POST it to /upload-api/{token} — which is the
// shape a browser needs, because an HTML form cannot set an Authorization
// header either. See docs/capability-urls.md.
//
// The file is streamed from disk rather than buffered, so uploading something
// large does not mean holding it in memory. ContentLength is set explicitly
// because a *os.File body would otherwise be sent chunked, and the server
// checks the declared length against its upload limit before reading anything.
func (c *APIClient) UploadFile(repoID, parentDir, localPath string) error {
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("failed to open file: %v", err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat file: %v", err)
	}

	remote := path.Join("/", parentDir, filepath.Base(localPath))
	req, err := http.NewRequest("PUT", c.BaseURL+entriesURL(repoID, remote), file)
	if err != nil {
		return fmt.Errorf("failed to create upload request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.getToken())
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = info.Size()

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("upload failed: %v", err)
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
	Version string `json:"version"`
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
