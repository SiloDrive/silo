package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SiloDrive/silo/client"
	"github.com/SiloDrive/silo/internal/xdg"
	"golang.org/x/term"
)

// `silo login` and `silo logout`, and the credential they keep.
//
// Every other subcommand used to sign in with SILO_EMAIL and SILO_PASSWORD and
// mint a twenty-four-hour session each time it ran -- which is the only way it
// could work on a server that signs people in through an identity provider,
// where there is no password to send, and which left a row in the account's
// credential list for every `silo ls`. `silo login` signs this host in once,
// as a device credential, and the other subcommands present that.
//
// The environment still wins when it is set, so nothing that scripts the CLI
// with a password changes underneath it.

// storedLogin is one server's sign-in on this host.
type storedLogin struct {
	Credential string `json:"credential"`
	Email      string `json:"email"`
	ExpiresAt  int64  `json:"expires_at,omitempty"`
}

// loginStore is the whole file. ClientID is this host's identity, minted once
// and sent with every sign-in, so that the account's credential list can tell
// a re-login on the same machine from a new machine.
type loginStore struct {
	ClientID string                 `json:"client_id,omitempty"`
	Servers  map[string]storedLogin `json:"servers"`
}

// storePath is $XDG_CONFIG_HOME/silo/credentials.json.
func storePath() (string, error) {
	dir, err := xdg.ConfigHome("silo")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "credentials.json"), nil
}

// serverKey is the URL a sign-in is filed under. A trailing slash is the same
// server, and "signed in" must not depend on how SILO_URL was typed.
func serverKey(serverURL string) string { return strings.TrimRight(serverURL, "/") }

func readStore() (*loginStore, error) {
	path, err := storePath()
	if err != nil {
		return nil, err
	}
	s := &loginStore{Servers: map[string]storedLogin{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("%s is not readable: %v", path, err)
	}
	if s.Servers == nil {
		s.Servers = map[string]storedLogin{}
	}
	return s, nil
}

// writeStore replaces the file atomically, readable by this user alone: it
// holds credentials that open the account.
func writeStore(s *loginStore) error {
	path, err := storePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".credentials-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// storedFor is this host's sign-in for a server, if there is one.
func storedFor(serverURL string) (storedLogin, bool, error) {
	s, err := readStore()
	if err != nil {
		return storedLogin{}, false, err
	}
	l, ok := s.Servers[serverKey(serverURL)]
	return l, ok && l.Credential != "", nil
}

// hostClientName is the label this host's credential carries in the account's
// list: what a person scanning it for "which one is my laptop" needs to see.
func hostClientName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "silo CLI"
	}
	return "silo CLI on " + host
}

// stdin and stdout are the terminal login talks to, as variables so a test can
// talk to it instead.
var (
	stdin  io.Reader = os.Stdin
	stdout io.Writer = os.Stdout
)

// loginTimeout bounds the wait for somebody to approve. The server's code
// expires well before it, so it only ever fires on a server that stopped
// answering.
var loginTimeout = 20 * time.Minute

func runLogin(serverURL, email, password string, args []string) error {
	fs := newFlagSet("login")
	usePassword := fs.Bool("password", false,
		"sign in with an address and password even if the server has an identity provider")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: silo login [-password]")
	}

	store, err := readStore()
	if err != nil {
		return err
	}
	if store.ClientID == "" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		store.ClientID = hex.EncodeToString(b)
	}
	req := client.DeviceSignInRequest{Kind: "device", ClientName: hostClientName(), ClientID: store.ClientID}

	c := client.NewClient(serverURL)
	info, err := c.GetServerInfo()
	if err != nil {
		return fmt.Errorf("asking %s what it supports: %w", serverURL, err)
	}

	var got *client.Enrolment
	if info.Has("oidc") && !*usePassword {
		got, err = loginThroughIdP(c, serverURL, req)
	} else {
		got, err = loginWithPassword(c, email, password, req)
	}
	if err != nil {
		return err
	}

	// Replacing a sign-in this host already had: the old credential is
	// discarded rather than left to run out, or every re-login would leave a
	// ninety-day row behind in the list.
	if old, ok := store.Servers[serverKey(serverURL)]; ok && old.Credential != "" && old.Credential != got.Credential {
		prev := client.NewClient(serverURL)
		prev.UseCredential(old.Credential)
		_ = prev.Logout()
	}

	store.Servers[serverKey(serverURL)] = storedLogin{
		Credential: got.Credential, Email: got.Email, ExpiresAt: got.ExpiresAt,
	}
	if err := writeStore(store); err != nil {
		// The credential is live and nothing holds it. Discard it rather than
		// leave a row nobody can present.
		_ = c.Logout()
		return fmt.Errorf("storing the credential: %w", err)
	}
	path, _ := storePath()
	_, _ = fmt.Fprintf(stdout, "Signed in to %s as %s.\n", serverURL, got.Email)
	if got.ExpiresAt != 0 {
		_, _ = fmt.Fprintf(stdout, "This host's credential lasts until %s; it is kept in %s.\n",
			time.Unix(got.ExpiresAt, 0).Format("2 January 2006"), path)
	}
	return nil
}

func loginThroughIdP(c *client.APIClient, serverURL string, req client.DeviceSignInRequest) (*client.Enrolment, error) {
	s, err := c.StartDeviceSignIn(req)
	if err != nil {
		return nil, fmt.Errorf("starting the sign-in: %w", err)
	}
	_, _ = fmt.Fprintf(stdout, "To sign in to %s, visit\n\n    %s\n\nand enter the code\n\n    %s\n\n",
		serverURL, s.VerificationURI, s.UserCode)
	if s.VerificationURIComplete != "" {
		_, _ = fmt.Fprintf(stdout, "or open %s\n\n", s.VerificationURIComplete)
	}
	_, _ = fmt.Fprintln(stdout, "Waiting for you to approve it...")

	ctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
	defer cancel()
	got, err := c.AwaitDeviceSignIn(ctx, s)
	if err != nil {
		return nil, fmt.Errorf("signing in: %w", err)
	}
	return got, nil
}

// loginWithPassword asks for whichever of the address and password the
// environment did not supply.
func loginWithPassword(c *client.APIClient, email, password string, req client.DeviceSignInRequest) (*client.Enrolment, error) {
	if email == "" {
		_, _ = fmt.Fprint(stdout, "Email: ")
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && line == "" {
			return nil, fmt.Errorf("reading the address: %w", err)
		}
		email = strings.TrimSpace(line)
	}
	if password == "" {
		f, ok := stdin.(*os.File)
		if !ok || !term.IsTerminal(int(f.Fd())) {
			return nil, fmt.Errorf("no terminal to ask for a password on; set SILO_PASSWORD")
		}
		_, _ = fmt.Fprint(stdout, "Password: ")
		b, err := term.ReadPassword(int(f.Fd()))
		_, _ = fmt.Fprintln(stdout)
		if err != nil {
			return nil, fmt.Errorf("reading the password: %w", err)
		}
		password = string(b)
	}
	return c.EnrolWithPassword(email, password, req)
}

func runLogout(serverURL string, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: silo logout")
	}
	store, err := readStore()
	if err != nil {
		return err
	}
	key := serverKey(serverURL)
	l, ok := store.Servers[key]
	if !ok || l.Credential == "" {
		_, _ = fmt.Fprintf(stdout, "Not signed in to %s.\n", serverURL)
		return nil
	}

	c := client.NewClient(serverURL)
	c.UseCredential(l.Credential)
	logoutErr := c.Logout()

	// Forgotten whether or not the server agreed. A credential the server
	// refused is already dead, and one it could not be asked about is better
	// listed as live on the server -- where the person can see it and revoke
	// it -- than kept here, presented, and silently failing.
	delete(store.Servers, key)
	if err := writeStore(store); err != nil {
		return fmt.Errorf("forgetting the credential: %w", err)
	}
	if logoutErr != nil {
		_, _ = fmt.Fprintf(stdout, "Signed out here, but %s could not be told (%v); "+
			"revoke it from another host with `silo credential revoke` if it is still listed.\n", serverURL, logoutErr)
		return nil
	}
	_, _ = fmt.Fprintf(stdout, "Signed out of %s.\n", serverURL)
	return nil
}
