package silod

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SiloDrive/silo/client"
	"github.com/SiloDrive/silo/internal/cli"
	"github.com/SiloDrive/silo/internal/oidctest"
)

// The client's half, against the real server and the fake IdP: what silo login
// and the drive clients will call.
func TestTheClientSignsInThroughTheIdentityProvider(t *testing.T) {
	base, _ := wire(t)
	idp := oidctest.New(t)
	withOIDC(t, idp, "create")

	c := client.NewClient(base)
	s, err := c.StartDeviceSignIn(client.DeviceSignInRequest{Kind: "device", ClientName: "silo CLI (test)"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	idp.Approve(t, s.UserCode, oidctest.Identity{Subject: "sub", Email: "cli@example.com", EmailVerified: oidctest.Verified})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	got, err := c.AwaitDeviceSignIn(ctx, s)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if got.Email != "cli@example.com" || got.Credential == "" {
		t.Fatalf("enrolment = %+v", got)
	}
	// The client now uses the credential for everything after, and owns no
	// session of its own to discard.
	if _, err := c.ListLibraries(); err != nil {
		t.Fatalf("listing with the new credential: %v", err)
	}
	if c.OwnsSession() {
		t.Error("a credential from a device sign-in reads as this process's throwaway session")
	}
}

func TestTheClientReportsARefusalInThePersonsWords(t *testing.T) {
	base, _ := wire(t)
	idp := oidctest.New(t)
	withOIDC(t, idp, "link")

	c := client.NewClient(base)
	s, err := c.StartDeviceSignIn(client.DeviceSignInRequest{ClientName: "silo CLI (test)"})
	if err != nil {
		t.Fatal(err)
	}
	idp.Approve(t, s.UserCode, oidctest.Identity{Subject: "sub", Email: "stranger@example.com", EmailVerified: oidctest.Verified})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = c.AwaitDeviceSignIn(ctx, s)
	if err == nil || !strings.Contains(err.Error(), "ask an administrator for an invite") {
		t.Fatalf("await = %v; want the binding's refusal", err)
	}
}

// `silo login` against a server with an identity provider: the CLI shows the
// code, the person approves, and the next command runs on the stored
// credential with no address or password anywhere.
func TestSiloLoginThroughTheIdentityProvider(t *testing.T) {
	base, _ := wire(t)
	idp := oidctest.New(t)
	withOIDC(t, idp, "create")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	done := make(chan error, 1)
	go func() { done <- cli.Run(base, "", "", []string{"login"}) }()

	// The person reads the code off the terminal and types it at the IdP.
	deadline := time.Now().Add(10 * time.Second)
	for len(idp.Pending()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("silo login never started a sign-in")
		}
		time.Sleep(20 * time.Millisecond)
	}
	idp.Approve(t, idp.Pending()[0], oidctest.Identity{Subject: "sub", Email: "cli@example.com", EmailVerified: oidctest.Verified})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("silo login: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("silo login did not finish after approval")
	}

	if err := cli.Run(base, "", "", []string{"libraries"}); err != nil {
		t.Fatalf("silo libraries on the stored credential: %v", err)
	}
}
