package serversecret

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/dkam/silo/fileserver/dbutil"
)

func testDB(t *testing.T) {
	t.Helper()
	pair, err := dbutil.OpenSQLite(filepath.Join(t.TempDir(), "silo.db"))
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	if err := dbutil.Prepare(pair.Write); err != nil {
		t.Fatalf("creating the test tables: %v", err)
	}
	Init(pair.Read, pair.Write)
	t.Cleanup(func() {
		_ = pair.Read.Close()
		_ = pair.Write.Close()
	})
}

// The value has to be the same on the second call, and the second call is the
// one that reads it back out of the database rather than minting it. That is
// the property the caller depends on: a secret that changed when the process
// restarted would turn the pre-login endpoint's indistinguishable answer into
// an answer that differs across a restart, which is the enumeration oracle it
// exists to close.
func TestNamedMintsOnceAndThenReadsItBack(t *testing.T) {
	testDB(t)
	ctx := context.Background()

	first, err := Named(ctx, "test/one")
	if err != nil {
		t.Fatalf("Named: %v", err)
	}
	if len(first) != Size {
		t.Fatalf("secret is %d bytes, want %d", len(first), Size)
	}

	second, err := Named(ctx, "test/one")
	if err != nil {
		t.Fatalf("Named again: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("the second call minted a new secret instead of reading the stored one")
	}
}

func TestSecretsAreSeparatedByName(t *testing.T) {
	testDB(t)
	ctx := context.Background()

	a, err := Named(ctx, "test/one")
	if err != nil {
		t.Fatalf("Named: %v", err)
	}
	b, err := Named(ctx, "test/two")
	if err != nil {
		t.Fatalf("Named: %v", err)
	}
	if bytes.Equal(a, b) {
		t.Error("two names share one secret; the name is not part of the key")
	}
}

func TestAnUnnamedSecretIsRefused(t *testing.T) {
	testDB(t)
	if _, err := Named(context.Background(), ""); err == nil {
		t.Error("minted a secret under no name")
	}
}
