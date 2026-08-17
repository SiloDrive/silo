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
	"path/filepath"
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

func (c *APIClient) ListDir(repoID, path string) ([]DirEntry, error) {
	if path == "" {
		path = "/"
	}
	escapedPath := url.QueryEscape(path)
	var entries []DirEntry
	err := c.doRequest("GET", fmt.Sprintf("/api/silo/v1/repos/%s/dir/?path=%s", repoID, escapedPath), nil, &entries)
	return entries, err
}

func (c *APIClient) DeleteRepo(repoID string) error {
	return c.doRequest("DELETE", "/api/silo/v1/repos/"+repoID, nil, nil)
}

func (c *APIClient) Mkdir(repoID, path string) error {
	return c.doRequest("POST", fmt.Sprintf("/api/silo/v1/repos/%s/mkdir?path=%s", repoID, url.QueryEscape(path)), nil, nil)
}

func (c *APIClient) RenameFile(repoID, path, newName string) error {
	return c.doRequest("POST", fmt.Sprintf("/api/silo/v1/repos/%s/rename?path=%s&newname=%s",
		repoID, url.QueryEscape(path), url.QueryEscape(newName)), nil, nil)
}

func (c *APIClient) MoveFile(repoID, src, dst string) error {
	return c.doRequest("POST", fmt.Sprintf("/api/silo/v1/repos/%s/move?src=%s&dst=%s",
		repoID, url.QueryEscape(src), url.QueryEscape(dst)), nil, nil)
}

func (c *APIClient) DeleteFile(repoID, path string) error {
	return c.doRequest("DELETE", fmt.Sprintf("/api/silo/v1/repos/%s/file?path=%s", repoID, url.QueryEscape(path)), nil, nil)
}

func (c *APIClient) DownloadFile(repoID, repoPath, localPath string) error {
	escapedPath := url.QueryEscape(repoPath)
	downloadURL := fmt.Sprintf("%s/api/silo/v1/repos/%s/download?path=%s", c.BaseURL, repoID, escapedPath)

	req, err := http.NewRequest("GET", downloadURL, nil)
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
