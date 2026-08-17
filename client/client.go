package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
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

// Mkdir asks for a directory explicitly. A PUT with no ?type is a file upload,
// which the server does not accept yet, so the parameter is not optional here
// even though a trailing slash would also do.
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

func (c *APIClient) DownloadFile(repoID, repoPath, localPath string) error {
	req, err := http.NewRequest("GET", c.BaseURL+entriesURL(repoID, repoPath), nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.getToken())

	// Don't follow redirects — we need to follow with auth-less request to /files/
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusFound {
		// Follow the redirect to /files/{token}/filename
		loc := resp.Header.Get("Location")
		if loc == "" {
			return fmt.Errorf("redirect with no location")
		}
		// Build absolute URL if relative
		if loc[0] == '/' {
			loc = c.BaseURL + loc
		}
		fileResp, err := http.Get(loc)
		if err != nil {
			return fmt.Errorf("download failed: %v", err)
		}
		defer func() { _ = fileResp.Body.Close() }()

		if fileResp.StatusCode >= 400 {
			msg, _ := io.ReadAll(fileResp.Body)
			return fmt.Errorf("download failed: %s", string(msg))
		}

		out, err := os.Create(localPath)
		if err != nil {
			return fmt.Errorf("failed to create local file: %v", err)
		}
		defer func() { _ = out.Close() }()

		if _, err := io.Copy(out, fileResp.Body); err != nil {
			return fmt.Errorf("failed to write file: %v", err)
		}
		return nil
	}

	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("download failed: %s: %s", resp.Status, string(msg))
	}

	return fmt.Errorf("unexpected response: %d", resp.StatusCode)
}

// UploadFile uploads a local file to a repo directory.
// It creates an access token, then POSTs a multipart form to /upload-api/.
func (c *APIClient) UploadFile(repoID, parentDir, localPath string) error {
	// Step 1: Create access token with op=upload
	objID, err := json.Marshal(map[string]string{"parent_dir": parentDir})
	if err != nil {
		return fmt.Errorf("failed to marshal upload obj_id: %v", err)
	}
	var tokenResp struct {
		Token string `json:"token"`
	}
	if err := c.doRequest("POST", "/api/silo/v1/access-tokens", map[string]interface{}{
		"repo_id": repoID,
		"obj_id":  string(objID),
		"op":      "upload",
	}, &tokenResp); err != nil {
		return fmt.Errorf("failed to get upload token: %v", err)
	}

	// Step 2: Build multipart form
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("failed to open file: %v", err)
	}
	defer func() { _ = file.Close() }()

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	if err := writer.WriteField("parent_dir", parentDir); err != nil {
		return fmt.Errorf("failed to write parent_dir field: %v", err)
	}
	if err := writer.WriteField("ret-json", "1"); err != nil {
		return fmt.Errorf("failed to write ret-json field: %v", err)
	}

	part, err := writer.CreateFormFile("file", filepath.Base(localPath))
	if err != nil {
		return fmt.Errorf("failed to create form file: %v", err)
	}
	if _, err := io.Copy(part, file); err != nil {
		return fmt.Errorf("failed to copy file content: %v", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("failed to close multipart writer: %v", err)
	}

	// Step 3: POST to /upload-api/{token}
	uploadURL := fmt.Sprintf("%s/upload-api/%s", c.BaseURL, tokenResp.Token)
	req, err := http.NewRequest("POST", uploadURL, &buf)
	if err != nil {
		return fmt.Errorf("failed to create upload request: %v", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

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
