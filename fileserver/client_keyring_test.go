package silod

import (
	"bytes"
	"errors"
	"net/http"
	"testing"

	"github.com/SiloDrive/silo/client"
	"github.com/SiloDrive/silo/store"
)

// The bootstrap, over HTTP, by the client rather than by the test.
//
// Every other test in this package builds its key material by hand and reads
// the account id out of the database. This one goes the way a device goes:
// log in, fetch what was published, derive under the parameters the blob
// carries, unwrap the identity, then unwrap one library's content key with it.
// Nothing here touches the database, which is the point — it is the first
// evidence that the server's half of E2EE is usable by something that only
// has HTTP.
func TestAClientOpensItsAccountAndALibraryKeyOverHTTP(t *testing.T) {
	base, token, km := enrolled(t)
	seed := mintSeed(t, km.Public)
	if code, body := call(t, "POST", base+"/api/silo/v1/libraries", token, seed.body(t, "Sealed")); code != http.StatusCreated {
		t.Fatalf("creating the encrypted library: status %d, body %s", code, body)
	}

	c := client.NewClient(base)
	acct, err := c.OpenAccount("wire@example.com", wirePassword)
	if err != nil {
		t.Fatalf("OpenAccount: %v", err)
	}
	if want := wireAccountID(t); acct.ID != want {
		t.Errorf("account id = %q, want %q", acct.ID, want)
	}
	if pub := acct.Identity.Public(); !bytes.Equal(pub[:], km.Public) {
		t.Error("the identity that came back is not the one that was published")
	}

	kr, err := acct.OpenLibrary(seed.LibraryID)
	if err != nil {
		t.Fatalf("OpenLibrary: %v", err)
	}

	// The keyring holds the right content key, shown by what it can open
	// rather than by comparing the key itself: the keyring does not hand it
	// out, and this is the property that actually matters.
	sealed, err := seed.Keyring.SealChunk([]byte("a chunk sealed under the key that was wrapped"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := kr.OpenChunk(sealed.PlaintextHash, sealed.Frame)
	if err != nil {
		t.Fatalf("the keyring cannot open a chunk sealed under the library's key: %v", err)
	}
	if string(plain) != "a chunk sealed under the key that was wrapped" {
		t.Error("the chunk did not come back")
	}

	// And the chunker it implies is the library's, not the published default:
	// an encrypted library's boundaries are unguessable without CK, which is
	// only true if the seed is derived from it.
	if got, want := kr.Params(), seed.Keyring.Params(); got != want {
		t.Errorf("chunker params = %+v, want the library's own %+v", got, want)
	}
	if kr.Params() == store.DefaultParams(store.PlainSeed()) {
		t.Error("an encrypted library is chunking under the published seed")
	}
}

// A library this account holds no wrap for is not a failure to report as one.
// A plain library and one nobody has shared are the same answer, and the
// server does not distinguish them either.
func TestOpeningALibraryWithNoWrapIsErrNoKeyring(t *testing.T) {
	base, token, _ := enrolled(t)
	plainID := makeLibrary(t, base, token)

	c := client.NewClient(base)
	acct, err := c.OpenAccount("wire@example.com", wirePassword)
	if err != nil {
		t.Fatalf("OpenAccount: %v", err)
	}
	if _, err := acct.OpenLibrary(plainID); !errors.Is(err, store.ErrNoKeyring) {
		t.Errorf("OpenLibrary on a plain library = %v, want ErrNoKeyring", err)
	}
}

// The wrong password does not open the identity, and the failure is the
// unwrap rather than the login: the server accepts nothing it can check here,
// which is the whole shape of the thing.
func TestTheWrongPasswordDoesNotOpenTheIdentity(t *testing.T) {
	base, _, _ := enrolled(t)
	c := client.NewClient(base)
	if _, err := c.OpenAccount("wire@example.com", "not the password"); err == nil {
		t.Fatal("OpenAccount succeeded with the wrong password")
	}
}
