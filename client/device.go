package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Signing in through the server's identity provider. The client never speaks
// to the IdP: it asks the server to start, shows the person the code and URL
// the server relays, and polls the server until the person has approved at the
// IdP. See docs/protocol.md, POST auth/device.

// DeviceSignInRequest is what the credential will be. ClientName is required:
// it is the label the account's credential list shows, and a row nobody can
// tell apart from the others turns revocation into a guess.
type DeviceSignInRequest struct {
	Kind       string `json:"kind,omitempty"` // "device" or "session"; the server's default is session
	ClientName string `json:"client_name"`
	ClientID   string `json:"client_id,omitempty"`
	Perm       string `json:"perm,omitempty"`
	Scope      string `json:"scope,omitempty"`
}

// DeviceSignIn is a sign-in in progress: what to show the person, and what to
// poll with.
type DeviceSignIn struct {
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	PollToken               string `json:"poll_token"`
	Interval                int    `json:"interval"`
	ExpiresIn               int    `json:"expires_in"`
}

// Enrolment is a credential the server issued, with the account it is for.
type Enrolment struct {
	Credential string `json:"credential"`
	ExpiresAt  int64  `json:"expires_at"`
	Email      string `json:"email"`
}

// StartDeviceSignIn asks the server to start a sign-in through its identity
// provider.
func (c *APIClient) StartDeviceSignIn(req DeviceSignInRequest) (*DeviceSignIn, error) {
	var out DeviceSignIn
	if err := c.postUnauthenticated("/api/silo/v1/auth/device", req, http.StatusOK, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AwaitDeviceSignIn polls until the person approves or the sign-in ends, and on
// success uses the credential for this client's later requests.
//
// It honours the interval, and a 429's Retry-After over it. The server enforces
// the interval, so polling faster gains nothing and is answered with a wait.
// A 403 or 410 ends the sign-in, and its body -- written for the person -- is
// the error's.
func (c *APIClient) AwaitDeviceSignIn(ctx context.Context, s *DeviceSignIn) (*Enrolment, error) {
	wait := time.Duration(s.Interval) * time.Second
	if wait <= 0 {
		wait = 5 * time.Second
	}
	for {
		var out Enrolment
		err := c.postUnauthenticated("/api/silo/v1/auth/device/poll",
			map[string]string{"poll_token": s.PollToken}, http.StatusOK, &out)
		if err == nil {
			c.UseCredential(out.Credential)
			return &out, nil
		}

		delay := wait
		var se *StatusError
		switch {
		case hasStatus(err, http.StatusAccepted):
		case hasStatus(err, http.StatusTooManyRequests):
			if se, _ = err.(*StatusError); se != nil && se.retryAfter > 0 {
				delay = se.retryAfter
			}
		default:
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
}

// postUnauthenticated posts JSON to one of the endpoints that take no
// credential, decoding the body on the status wanted and returning a
// StatusError on any other -- including other 2xx, which is how a poll's 202
// reaches AwaitDeviceSignIn.
func (c *APIClient) postUnauthenticated(path string, body any, want int, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := c.sendRequest("POST", path, b, "")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != want {
		msg, _ := io.ReadAll(resp.Body)
		se := &StatusError{Code: resp.StatusCode, Status: resp.Status, Body: strings.TrimSpace(string(msg))}
		if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && n > 0 {
			se.retryAfter = time.Duration(n) * time.Second
		}
		return se
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("failed to parse response: %v", err)
	}
	return nil
}

// EnrolWithPassword signs in with an address and password and asks for a
// named, durable credential rather than a session -- the same credential a
// device sign-in yields, for a server with no identity provider. On success
// the client uses it for its later requests, and holds no password.
func (c *APIClient) EnrolWithPassword(email, password string, req DeviceSignInRequest) (*Enrolment, error) {
	body := struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		DeviceSignInRequest
	}{email, password, req}
	var out Enrolment
	if err := c.postUnauthenticated("/api/silo/v1/auth/login", body, http.StatusCreated, &out); err != nil {
		return nil, err
	}
	c.UseCredential(out.Credential)
	return &out, nil
}
