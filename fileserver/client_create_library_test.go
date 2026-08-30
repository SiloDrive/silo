package silod

import (
	"net/http"
	"testing"

	"github.com/dkam/silo/client"
	"github.com/dkam/silo/store"

	"github.com/google/uuid"
)

// Creating an encrypted library from the client side, end to end.
//
// Everything the server needs is minted here and the server can read none of
// it: the library id, the content key, the sealed empty root, the sealed
// initial commit, and the wrap that is the only copy of the key anyone will
// ever recover. Until this exists, the only thing in the tree that could
// build that request was a test helper -- which meant the seed format was
// pinned by the server's validation and by nothing on the writing side.
//
// The keyring comes back with the library because the caller has it at that
// moment and will never be able to derive it again from anything the server
// holds. A create that returned only an id would be a create that threw the
// key away.
func TestAClientCreatesAnEncryptedLibraryAndReadsItBack(t *testing.T) {
	base, token, c, acct := enrolledAccount(t)

	lib, kr, err := acct.CreateEncryptedLibrary("Sealed by the client")
	if err != nil {
		t.Fatalf("CreateEncryptedLibrary: %v", err)
	}
	if id, err := uuid.Parse(lib.ID); err != nil || id.String() != lib.ID {
		t.Errorf("library id %q is not a canonical lower-case hyphenated UUID", lib.ID)
	}
	if lib.Name != "Sealed by the client" {
		t.Errorf("name = %q, want the one asked for", lib.Name)
	}
	if !lib.Encrypted {
		t.Error("the library the client made does not report itself encrypted")
	}
	if lib.HeadCommitID == "" {
		t.Error("the create did not say what commit the library starts at")
	}

	// The server agrees, which is the part the local struct cannot assert:
	// the seed was accepted, stored, and made into a loadable library.
	code, body := call(t, "GET", base+"/api/silo/v1/libraries", token, "")
	if code != http.StatusOK {
		t.Fatalf("listing: status %d, body %s", code, body)
	}
	listed, err := c.ListLibraries()
	if err != nil {
		t.Fatalf("ListLibraries: %v", err)
	}
	var found *client.Library
	for i := range listed {
		if listed[i].ID == lib.ID {
			found = &listed[i]
		}
	}
	if found == nil {
		t.Fatalf("the library the client created is not in the listing: %s", body)
	}
	if !found.Encrypted {
		t.Error("the server does not report the library as encrypted")
	}
	if found.HeadCommitID != lib.HeadCommitID {
		t.Errorf("head = %s, want the initial commit the client sealed, %s",
			found.HeadCommitID, lib.HeadCommitID)
	}

	// The wrap went to this account's own identity: a second device, holding
	// only the password, gets the same content key back. Shown by what the
	// two keyrings can do for each other rather than by comparing keys.
	again, err := acct.OpenLibrary(lib.ID)
	if err != nil {
		t.Fatalf("OpenLibrary on the library we just created: %v", err)
	}
	sealed, err := kr.SealChunk([]byte("sealed by the creator, opened by a new device"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := again.OpenChunk(sealed.PlaintextHash, sealed.Frame)
	if err != nil {
		t.Fatalf("the fetched keyring cannot open what the created one sealed: %v", err)
	}
	if string(plain) != "sealed by the creator, opened by a new device" {
		t.Error("the chunk did not come back")
	}

	// And it chunks under its own seed. A library created with the published
	// constant would dedup against every other library on the server, which
	// is the leak the derived seed exists to close.
	if kr.Params() == store.DefaultParams(store.PlainSeed()) {
		t.Error("the created library chunks under the published seed")
	}
	if kr.Params() != again.Params() {
		t.Error("the created keyring and the fetched one disagree about the chunker")
	}
}

// An account that has published no identity key cannot create an encrypted
// library, and the client must not mint a content key it is about to lose.
func TestCreatingAnEncryptedLibraryWithNoIdentityFails(t *testing.T) {
	base, _ := wire(t)
	c := client.NewClient(base)
	if _, err := c.OpenAccount("wire@example.com", wirePassword); err == nil {
		t.Fatal("OpenAccount succeeded for an account with no published keys")
	}
}
