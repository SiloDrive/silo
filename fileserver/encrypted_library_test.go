package silod

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/dkam/silo/client"
	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/credential"
	"github.com/dkam/silo/store"

	"github.com/google/uuid"
)

// Creating an end-to-end encrypted library.
//
// The server cannot mint one. An E2EE library's initial root directory and its
// initial commit are both sealed under a content key the server never holds,
// so the two objects arrive with the request rather than being built by the
// handler -- which is why this is a different shape from creating a plain
// library rather than a flag on the same one.
//
// The library id arrives with the request too, and that is the part worth
// naming: store.WrapCK binds the library id into the wrap as associated data,
// so the id has to exist before the content key can be wrapped to anybody. The
// alternative is creating the library and publishing its key in two requests,
// which leaves a window holding a library whose key nobody stored.

// sealedSeed is what a client sends, built by the client's own code.
//
// It delegates to client.NewEncryptedSeed rather than reimplementing it, so
// the refusals below are tested against seeds something actually writes: a
// hand-built prototype here would let the two drift, and the server's
// validation would then be pinning a format no client produces.
type sealedSeed struct {
	client.EncryptedSeed
	// Keyring is the content key, and the only handle on it there will be.
	Keyring  *store.Keyring
	RootID   store.ID
	CommitID store.ID
}

func mintSeed(t *testing.T, pub []byte) sealedSeed {
	t.Helper()

	var recipient [store.X25519KeySize]byte
	copy(recipient[:], pub)
	seed, kr, err := client.NewEncryptedSeed(uuid.New().String(), recipient)
	if err != nil {
		t.Fatalf("minting a seed: %v", err)
	}
	return sealedSeed{
		EncryptedSeed: seed,
		Keyring:       kr,
		RootID:        store.ObjectID(seed.Root),
		CommitID:      store.ObjectID(seed.Commit),
	}
}

func (s sealedSeed) body(t *testing.T, name string) string {
	t.Helper()
	b, err := json.Marshal(struct {
		Name string `json:"name"`
		E2EE bool   `json:"e2ee"`
		client.EncryptedSeed
	}{name, true, s.EncryptedSeed})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	return string(b)
}

// enrolled stands the server up, publishes key material for the account, and
// returns the material along with the base URL and a token.
func enrolled(t *testing.T) (base, token string, km keyMaterial) {
	t.Helper()
	base, token = wire(t)
	km = mintKeyMaterial(t, wireAccountID(t), "correct horse battery staple")
	if code, body := call(t, "PUT", base+keysPath, token, km.body(t)); code != http.StatusOK {
		t.Fatalf("publishing key material: status %d, body %s", code, body)
	}
	return base, token, km
}

func TestCreatingAnEncryptedLibrary(t *testing.T) {
	base, token, km := enrolled(t)
	seed := mintSeed(t, km.Public)

	code, body := call(t, "POST", base+"/api/silo/v1/libraries", token, seed.body(t, "Sealed"))
	if code != http.StatusCreated {
		t.Fatalf("creating an encrypted library: status %d, body %s", code, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	if created.ID != seed.LibraryID {
		t.Errorf("id = %s, want the one sent, %s", created.ID, seed.LibraryID)
	}

	// It loads: the head is the commit that arrived, and the library reports
	// itself encrypted rather than merely existing.
	code, body = call(t, "GET", base+"/api/silo/v1/libraries", token, "")
	if code != http.StatusOK {
		t.Fatalf("listing: status %d, body %s", code, body)
	}
	var listing []struct {
		ID           string `json:"id"`
		Encrypted    bool   `json:"encrypted"`
		HeadCommitID string `json:"head_commit_id"`
	}
	if err := json.Unmarshal([]byte(body), &listing); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	var found bool
	for _, l := range listing {
		if l.ID != seed.LibraryID {
			continue
		}
		found = true
		if !l.Encrypted {
			t.Error("the library does not report itself encrypted")
		}
		if l.HeadCommitID != seed.CommitID.String() {
			t.Errorf("head = %s, want the commit that was sent, %s", l.HeadCommitID, seed.CommitID)
		}
	}
	if !found {
		t.Fatal("the library is not in the listing")
	}

	// The two objects are in the store, byte for byte.
	for _, o := range []struct {
		what string
		id   store.ID
		want []byte
	}{{"root", seed.RootID, seed.Root}, {"commit", seed.CommitID, seed.Commit}} {
		code, got := call(t, "GET",
			base+"/api/silo/v1/libraries/"+seed.LibraryID+"/objects/"+o.id.String(), token, "")
		if code != http.StatusOK {
			t.Fatalf("reading the %s object: status %d, body %s", o.what, code, got)
		}
		if got != string(o.want) {
			t.Errorf("the %s object came back changed", o.what)
		}
	}
}

// The whole point of storing the wrap: a new device logs in, opens its
// identity key, and unwraps the content key of every library it holds.
func TestTheContentKeyWrapComesBackAndOpens(t *testing.T) {
	base, token, km := enrolled(t)
	seed := mintSeed(t, km.Public)
	if code, body := call(t, "POST", base+"/api/silo/v1/libraries", token, seed.body(t, "Sealed")); code != http.StatusCreated {
		t.Fatalf("creating: status %d, body %s", code, body)
	}

	code, body := call(t, "GET", base+"/api/silo/v1/libraries/"+seed.LibraryID+"/key", token, "")
	if code != http.StatusOK {
		t.Fatalf("reading the content key wrap: status %d, body %s", code, body)
	}
	var out struct {
		WrappedKey []byte `json:"wrapped_key"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}

	// Opened the long way round, through the password, because that is the
	// path a new device actually walks.
	keys, err := account.GetKeys(testCtx(t), acctFor(t, "wire@example.com").ID)
	if err != nil {
		t.Fatalf("GetKeys: %v", err)
	}
	id, _, err := store.OpenIdentityWithPassword(
		"correct horse battery staple", wireAccountID(t), keys.WrappedKey)
	if err != nil {
		t.Fatalf("opening the identity key: %v", err)
	}
	ck, err := store.UnwrapCK(id, seed.LibraryID, out.WrappedKey)
	if err != nil {
		t.Fatalf("unwrapping the content key: %v", err)
	}
	// Key equality, shown by what the key does: the keyring the seed was
	// minted with seals a chunk, and one built from what came back opens it.
	kr, err := store.NewKeyring(ck)
	if err != nil {
		t.Fatalf("the unwrapped key is not a content key: %v", err)
	}
	sealed, err := seed.Keyring.SealChunk([]byte("sealed under the key that went in"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kr.OpenChunk(sealed.PlaintextHash, sealed.Frame); err != nil {
		t.Errorf("the content key that came back is not the one that went in: %v", err)
	}
}

// A plain library has no wrap to serve, and saying so is not the same as
// saying the library does not exist.
func TestAPlainLibraryHasNoContentKeyWrap(t *testing.T) {
	base, token := wire(t)
	id := makeLibrary(t, base, token)
	if code, body := call(t, "GET", base+"/api/silo/v1/libraries/"+id+"/key", token, ""); code != http.StatusNotFound {
		t.Errorf("status %d, want 404; body %s", code, body)
	}
}

// A credential cut to one library may read that library's content key wrap:
// its subject is the library, not the account.
func TestAScopedCredentialCanReadItsOwnLibrarysKey(t *testing.T) {
	base, token, km := enrolled(t)
	seed := mintSeed(t, km.Public)
	if code, body := call(t, "POST", base+"/api/silo/v1/libraries", token, seed.body(t, "Sealed")); code != http.StatusCreated {
		t.Fatalf("creating: status %d, body %s", code, body)
	}

	scoped := narrowed(t, "r", credential.Scope{LibraryID: seed.LibraryID})
	if code, body := call(t, "GET", base+"/api/silo/v1/libraries/"+seed.LibraryID+"/key", scoped, ""); code != http.StatusOK {
		t.Errorf("a credential scoped to this library: status %d, body %s", code, body)
	}

	other := makeLibrary(t, base, token)
	elsewhere := narrowed(t, "r", credential.Scope{LibraryID: other})
	if code, _ := call(t, "GET", base+"/api/silo/v1/libraries/"+seed.LibraryID+"/key", elsewhere, ""); code != http.StatusForbidden {
		t.Errorf("a credential scoped elsewhere: status %d, want 403", code)
	}
}

// Every refusal below leaves nothing behind. Each one is a request that would
// otherwise produce a library that loads and cannot be read, which is worse
// than one that was never created.
func TestAnEncryptedLibraryIsRefusedWhenTheSeedIsWrong(t *testing.T) {
	base, token, km := enrolled(t)

	t.Run("the commit does not name the root that was sent", func(t *testing.T) {
		seed := mintSeed(t, km.Public)
		other := mintSeed(t, km.Public)
		seed.Root = other.Root
		if code, body := call(t, "POST", base+"/api/silo/v1/libraries", token, seed.body(t, "Sealed")); code != http.StatusBadRequest {
			t.Errorf("status %d, want 400; body %s", code, body)
		}
	})

	t.Run("the root is missing", func(t *testing.T) {
		seed := mintSeed(t, km.Public)
		seed.Root = nil
		if code, body := call(t, "POST", base+"/api/silo/v1/libraries", token, seed.body(t, "Sealed")); code != http.StatusBadRequest {
			t.Errorf("status %d, want 400; body %s", code, body)
		}
	})

	t.Run("the commit is missing", func(t *testing.T) {
		seed := mintSeed(t, km.Public)
		seed.Commit = nil
		if code, body := call(t, "POST", base+"/api/silo/v1/libraries", token, seed.body(t, "Sealed")); code != http.StatusBadRequest {
			t.Errorf("status %d, want 400; body %s", code, body)
		}
	})

	t.Run("the content key wrap is missing", func(t *testing.T) {
		seed := mintSeed(t, km.Public)
		seed.WrappedKey = nil
		if code, body := call(t, "POST", base+"/api/silo/v1/libraries", token, seed.body(t, "Sealed")); code != http.StatusBadRequest {
			t.Errorf("status %d, want 400; body %s", code, body)
		}
	})

	t.Run("the library id is not a canonical uuid", func(t *testing.T) {
		seed := mintSeed(t, km.Public)
		seed.LibraryID = "Not-A-UUID"
		if code, body := call(t, "POST", base+"/api/silo/v1/libraries", token, seed.body(t, "Sealed")); code != http.StatusBadRequest {
			t.Errorf("status %d, want 400; body %s", code, body)
		}
	})

	t.Run("the commit is not sealed", func(t *testing.T) {
		seed := mintSeed(t, km.Public)
		plain := &store.Commit{Root: seed.RootID, CreatedAt: 1756339200}
		b, err := plain.Encode()
		if err != nil {
			t.Fatalf("encoding a plain commit: %v", err)
		}
		seed.Commit = b
		if code, body := call(t, "POST", base+"/api/silo/v1/libraries", token, seed.body(t, "Sealed")); code != http.StatusBadRequest {
			t.Errorf("a plain commit was accepted as an encrypted library's head: status %d, body %s", code, body)
		}
	})

	t.Run("the id is already taken", func(t *testing.T) {
		seed := mintSeed(t, km.Public)
		if code, body := call(t, "POST", base+"/api/silo/v1/libraries", token, seed.body(t, "Sealed")); code != http.StatusCreated {
			t.Fatalf("creating: status %d, body %s", code, body)
		}
		again := mintSeed(t, km.Public)
		again.LibraryID = seed.LibraryID
		if code, body := call(t, "POST", base+"/api/silo/v1/libraries", token, again.body(t, "Sealed")); code != http.StatusConflict {
			t.Errorf("status %d, want 409; body %s", code, body)
		}
	})
}

// The content key is wrapped to an identity key. An account that has published
// none has nowhere for the wrap to be recoverable from, so the library it
// creates would be readable exactly until the device that made it was lost.
func TestAnEncryptedLibraryNeedsAPublishedIdentityKey(t *testing.T) {
	base, token := wire(t)

	// Key material bound to this account, but never published.
	km := mintKeyMaterial(t, wireAccountID(t), "correct horse battery staple")
	seed := mintSeed(t, km.Public)

	code, body := call(t, "POST", base+"/api/silo/v1/libraries", token, seed.body(t, "Sealed"))
	if code != http.StatusConflict {
		t.Errorf("status %d, want 409; body %s", code, body)
	}
}

func TestAReadOnlyCredentialCannotCreateAnEncryptedLibrary(t *testing.T) {
	base, _, km := enrolled(t)
	seed := mintSeed(t, km.Public)
	ro := narrowed(t, "r", credential.Scope{})
	if code, _ := call(t, "POST", base+"/api/silo/v1/libraries", ro, seed.body(t, "Sealed")); code != http.StatusForbidden {
		t.Errorf("status %d, want 403", code)
	}
}

// Deleting a library must take its content key wrap with it. The wrap is
// per (library, account), so a row left behind is a blob keyed to an id that
// can be handed to the next library created -- and LibraryKeyWrap is the
// newest of a dozen per-library tables, which is exactly the kind of row a
// delete list forgets.
func TestDeletingAnEncryptedLibraryTakesItsContentKeyWrap(t *testing.T) {
	base, token, km := enrolled(t)
	seed := mintSeed(t, km.Public)
	if code, body := call(t, "POST", base+"/api/silo/v1/libraries", token, seed.body(t, "Sealed")); code != http.StatusCreated {
		t.Fatalf("creating: status %d, body %s", code, body)
	}

	if code, body := call(t, "DELETE", base+"/api/silo/v1/libraries/"+seed.LibraryID, token, ""); code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("deleting: status %d, body %s", code, body)
	}

	var n int
	if err := siloPair.Read.QueryRow(
		"SELECT count(*) FROM LibraryKeyWrap WHERE library_id = ?", seed.LibraryID).Scan(&n); err != nil {
		t.Fatalf("counting wraps: %v", err)
	}
	if n != 0 {
		t.Errorf("%d content key wrap(s) survived the library", n)
	}
}

func TestServerInfoAdvertisesEncryptedLibraries(t *testing.T) {
	base, token := wire(t)
	code, body := call(t, "GET", base+"/api/silo/v1/server-info", token, "")
	if code != http.StatusOK {
		t.Fatalf("server-info: status %d, body %s", code, body)
	}
	var out struct {
		Features []string `json:"features"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	for _, f := range out.Features {
		if f == "e2ee-libraries" {
			return
		}
	}
	t.Errorf("server-info does not advertise e2ee-libraries: %v", out.Features)
}

// Reading an E2EE library by path is refused, and the refusal has to say so.
//
// The server holds no content key, so it cannot match a plaintext path segment
// against the sealed names in a directory object. That much is intended. What
// is not intended is answering "Not found": the file may well exist, and a
// client told it does not is being lied to about the library's contents. The
// comment on resolve says exactly this -- the refusal sits where it does
// "instead of surfacing as not found, which would be a lie about whether the
// file exists" -- while getEntry flattened every resolve error to 404.
//
// The write path already gets this right: it answers 403 and names the
// id-addressed surface. A read should be the same shape.
func TestReadingAnEncryptedLibraryByPathIsNotALie(t *testing.T) {
	base, token, km := enrolled(t)
	seed := mintSeed(t, km.Public)

	if code, body := call(t, "POST", base+"/api/silo/v1/libraries", token, seed.body(t, "Sealed")); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("creating the encrypted library: status %d, body %s", code, body)
	}

	entry := base + "/api/silo/v1/libraries/" + seed.LibraryID + "/entries/notes.txt"
	code, body := call(t, "GET", entry, token, "")

	if code == http.StatusNotFound {
		t.Fatalf("a path read on an E2EE library answered 404 %q; "+
			"the server cannot know whether the file exists, so this is a lie about its contents", strings.TrimSpace(body))
	}
	if code != http.StatusForbidden {
		t.Fatalf("status %d, want 403; body %s", code, body)
	}
	if !strings.Contains(strings.ToLower(body), "end-to-end encrypted") {
		t.Errorf("the refusal does not say why: %q", strings.TrimSpace(body))
	}
	if !strings.Contains(strings.ToLower(body), "id") {
		t.Errorf("the refusal does not name the surface that works: %q", strings.TrimSpace(body))
	}
}
