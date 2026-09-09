package silod

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/dkam/silo/client"
	"github.com/dkam/silo/fileserver/share"
	"github.com/google/uuid"
)

// Deleting a library takes its rows and leaves its objects, because reclaiming
// them is the collector's job and the collector runs when an operator says so.
// Between those two moments the id is free — CreateEncryptedLibrary asks only
// whether a Library row exists — and the store the id opens is the dead
// library's, contents and all.
//
// So anyone who knows a deleted library's UUID can create a library on top of
// its data. It is worse than a read: gc.go's unsafeToReclaim then refuses to
// reclaim, because the id is live again, and the objects are adopted
// permanently.
//
// A UUID is not a secret. It is in every URL the library ever appeared in, in
// the client's local state, and in the logs.
func TestADeletedLibrarysIDCannotBeClaimedBack(t *testing.T) {
	base, token, _, acct := enrolledAccount(t)

	seed, _, err := client.NewEncryptedSeed(uuid.New().String(), acct.Identity.Public())
	if err != nil {
		t.Fatalf("building a seed: %v", err)
	}
	if code, body := createSeededLibrary(t, base, token, "The original", seed); code != http.StatusCreated {
		t.Fatalf("creating the library = %d (%s), want 201", code, body)
	}

	if code, body := call(t, "DELETE", base+"/api/silo/v1/libraries/"+seed.LibraryID, token, ""); code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("deleting = %d (%s)", code, body)
	}

	// The id names a store that still holds the old library's objects. Nothing
	// may be built on it until the collector has been through.
	again, _, err := client.NewEncryptedSeed(seed.LibraryID, acct.Identity.Public())
	if err != nil {
		t.Fatalf("building the second seed: %v", err)
	}
	code, body := createSeededLibrary(t, base, token, "The claim", again)
	if code == http.StatusCreated {
		t.Errorf("a deleted library's id was claimed back: %d (%s)", code, body)
	}
}

// share.RemoveLibrary exists, says in its own comment that it is for the moment
// the library goes, and is called by nothing outside its own test. So a deleted
// library's grants outlive it — rows naming a subject nobody can reach, which
// is harmless right up until something takes the id back.
func TestDeletingALibraryTakesItsGrants(t *testing.T) {
	base, token, _, _ := enrolledAccount(t)

	code, body := call(t, "POST", base+"/api/silo/v1/libraries", token, `{"name":"Shared then deleted"}`)
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("creating = %d (%s)", code, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatal(err)
	}

	code, body = call(t, "POST", base+"/api/silo/v1/libraries/"+created.ID+"/shares", token,
		`{"email":"guest@example.com","perm":"rw"}`)
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("sharing = %d (%s)", code, body)
	}
	if grants, err := share.ForLibrary(adminCtx(t), created.ID); err != nil || len(grants) == 0 {
		t.Fatalf("the share did not land: %d grants, %v", len(grants), err)
	}

	if code, body := call(t, "DELETE", base+"/api/silo/v1/libraries/"+created.ID, token, ""); code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("deleting = %d (%s)", code, body)
	}

	grants, err := share.ForLibrary(adminCtx(t), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 0 {
		t.Errorf("%d grants outlived the library they were on", len(grants))
	}
}

func createSeededLibrary(t *testing.T, base, token, name string, seed client.EncryptedSeed) (int, string) {
	t.Helper()
	payload, err := json.Marshal(struct {
		Name string `json:"name"`
		E2EE bool   `json:"e2ee"`
		client.EncryptedSeed
	}{name, true, seed})
	if err != nil {
		t.Fatal(err)
	}
	return call(t, "POST", base+"/api/silo/v1/libraries", token, string(payload))
}
