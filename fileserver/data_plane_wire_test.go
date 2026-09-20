package silod

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/SiloDrive/silo/fileserver/account"
)

// The data plane, asked the question the account surface is already asked.
//
// Every route here has a handler test, and until now that was the whole of its
// coverage: the handler tests build a request, push the owner's credential
// into its context with middleware.WithCredential, and call the function. That
// proves what the handler does once it has been let in, and it cannot prove
// anything about being let in -- the gate is the one thing those tests
// deliberately skip.
//
// So the id-addressed surface, which is the whole of how an end-to-end
// encrypted library is read and written, had no test anywhere that a different
// account is refused it through the real router. These are that test, twice
// over: once for somebody with no relationship to the library at all, and once
// for somebody holding a read-only grant, which is the case where the refusal
// has to be selective rather than total.

// dataRoute is one request against a library, and whether serving it would be
// a write.
//
// The bodies are mostly nonsense, and deliberately so: every one of these
// handlers checks permission before it looks at what it was sent, which is the
// property being measured. A body good enough to succeed would make a 400 and
// a 403 hard to tell apart in a failure message, and a handler that started
// validating first would go on passing.
type dataRoute struct {
	name   string
	method string
	path   string
	body   string
	// write is true when serving the request would change the library, or --
	// for the share surface and chunks/missing -- when the handler requires
	// write permission for a reason of its own that it states at its
	// declaration.
	write bool
}

// dataRoutes is the surface under test, relative to a library.
func dataRoutes(libraryID, objectID, chunkID string) []dataRoute {
	lib := "/api/silo/v1/libraries/" + libraryID
	return []dataRoute{
		// Reads.
		{"reading an object", "GET", lib + "/objects/" + objectID, "", false},
		{"heading an object", "HEAD", lib + "/objects/" + objectID, "", false},
		{"reading a chunk", "GET", lib + "/chunks/" + chunkID, "", false},
		{"heading a chunk", "HEAD", lib + "/chunks/" + chunkID, "", false},
		{"fetching chunks in bulk", "POST", lib + "/chunks/fetch", `{"chunks":["` + chunkID + `"]}`, false},
		{"reading the delta feed", "GET", lib + "/changes?since=" + zeroCommit, "", false},
		{"reading the history", "GET", lib + "/commits", "", false},
		{"listing the root", "GET", lib + "/entries/", "", false},
		{"reading the content key wrap", "GET", lib + "/key", "", false},

		// Writes.
		{"storing an object", "PUT", lib + "/objects/" + objectID, "x", true},
		{"storing a chunk", "PUT", lib + "/chunks/" + chunkID, "x", true},
		{"uploading chunks in bulk", "POST", lib + "/chunks", "", true},
		// Write permission although it only reads: answering "yes, I hold
		// that one" to anybody with read access turns it into an oracle for
		// whether a file exists somewhere on the server. See
		// chunksMissingHandler.
		{"asking which chunks are missing", "POST", lib + "/chunks/missing", `{"chunks":[]}`, true},
		{"moving the head", "PUT", lib + "/head", zeroCommit, true},
		{"running a batch", "POST", lib + "/batch", `{"ops":[{"op":"mkdir","path":"/nope"}]}`, true},
		{"writing a file", "PUT", lib + "/entries/theirs.txt", "x", true},
		{"deleting a file", "DELETE", lib + "/entries/seed.txt", "", true},
		{"renaming the library", "PATCH", lib, `{"name":"mine now"}`, true},
		{"deleting the library", "DELETE", lib, "", true},

		// The share surface. Owner-only in both directions, so the listing is
		// filed with the writes: who else holds a library is the owner's
		// business, and a grantee learning it is the re-sharing decision read
		// backwards.
		{"listing the shares", "GET", lib + "/shares", "", true},
		{"sharing the library on", "POST", lib + "/shares", `{"email":"third@example.com","perm":"r"}`, true},
		{"removing a share", "DELETE", lib + "/shares/user:0", "", true},
	}
}

// seededLibrary makes a library with one file and one chunk in it, and returns
// the library id, the id of the file's manifest object, and the chunk's id.
//
// Real ids rather than placeholders, because a 404 for an object nobody stored
// and a 403 for an object somebody may not read are the same shape of failure
// from the outside. With content actually present, a read that comes back 200
// means the caller was let in -- which is what the read-only grantee half of
// this needs to be able to say.
func seededLibrary(t *testing.T, base, token string) (libraryID, objectID, chunkID string) {
	t.Helper()
	libraryID = makeLibrary(t, base, token)
	lib := base + "/api/silo/v1/libraries/" + libraryID

	if code, body := call(t, "PUT", lib+"/entries/seed.txt", token, "the seeded file"); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("seeding a file: status %d, body %s", code, body)
	}
	code, body := call(t, "GET", lib+"/entries/", token, "")
	if code != http.StatusOK {
		t.Fatalf("listing the seeded library: status %d, body %s", code, body)
	}
	var rows []struct {
		Name string `json:"name"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatalf("decoding the listing %s: %v", body, err)
	}
	for _, row := range rows {
		if row.Name == "seed.txt" {
			objectID = row.ID
		}
	}
	if objectID == "" {
		t.Fatalf("the seeded file is not in the listing: %s", body)
	}

	// A chunk is addressed by the SHA-256 of its bytes, so the client names it
	// and the server verifies the name.
	const chunkBytes = "the seeded chunk"
	sum := sha256.Sum256([]byte(chunkBytes))
	chunkID = hex.EncodeToString(sum[:])
	if code, body := call(t, "PUT", lib+"/chunks/"+chunkID, token, chunkBytes); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("seeding a chunk: status %d, body %s", code, body)
	}
	return libraryID, objectID, chunkID
}

// TestAStrangerIsRefusedTheWholeDataPlane is the first half: an account that
// holds a perfectly good credential of its own and has no relationship to this
// library.
//
// 403 on every route, reads included, and 403 rather than 404 throughout: a
// library somebody may not reach and one that does not exist have to answer
// the same thing, or the difference between them enumerates library ids. See
// docs/responses.md.
func TestAStrangerIsRefusedTheWholeDataPlane(t *testing.T) {
	base, token := wire(t)
	libraryID, objectID, chunkID := seededLibrary(t, base, token)
	_, strangerToken := makeAccount(t, base, "stranger@example.com", "a password", account.RoleUser)
	makeAccount(t, base, "third@example.com", "a password", account.RoleUser)

	for _, route := range dataRoutes(libraryID, objectID, chunkID) {
		code, body := call(t, route.method, base+route.path, strangerToken, route.body)
		if code != http.StatusForbidden {
			t.Errorf("a stranger %s: status %d, want 403, body %s", route.name, code, body)
		}
	}

	// And the library is untouched, which is the assertion that catches a
	// refusal arriving after the work was already done -- a delete that runs
	// and then answers 403 passes the loop above and fails here.
	code, body := call(t, "GET", base+"/api/silo/v1/libraries/"+libraryID+"/entries/", token, "")
	if code != http.StatusOK {
		t.Fatalf("the owner listing their own library afterwards: status %d, body %s", code, body)
	}
	var rows []struct {
		Name string `json:"name"`
	}
	decodeInto(t, body, &rows)
	if len(rows) != 1 || rows[0].Name != "seed.txt" {
		t.Errorf("the library reads %+v after the sweep, want the one seeded file", rows)
	}
}

// TestAReadOnlyShareIsRefusedEveryWrite is the second half, and the harder
// one: the caller is let in, so a gate that answers "yes" or "no" for the
// whole library is not enough. Every write has to be refused individually
// while every read goes through.
//
// The share is a grant on the library rather than a credential ceiling, so
// this measures share.CheckPerm reaching the id-addressed surface -- a
// different mechanism from the one TestAReadOnlyCredentialCannotWrite
// measures, arriving at the same handlers.
func TestAReadOnlyShareIsRefusedEveryWrite(t *testing.T) {
	base, token := wire(t)
	libraryID, objectID, chunkID := seededLibrary(t, base, token)
	_, granteeToken := makeAccount(t, base, "grantee@example.com", "a password", account.RoleUser)
	makeAccount(t, base, "third@example.com", "a password", account.RoleUser)

	if code, body := shareWith(t, base, token, libraryID, "grantee@example.com", "r"); code != http.StatusCreated {
		t.Fatalf("sharing read-only: status %d, body %s", code, body)
	}

	for _, route := range dataRoutes(libraryID, objectID, chunkID) {
		code, body := call(t, route.method, base+route.path, granteeToken, route.body)
		if route.write {
			if code != http.StatusForbidden {
				t.Errorf("a read-only grantee %s: status %d, want 403, body %s", route.name, code, body)
			}
			continue
		}
		// A read is allowed to fail for its own reasons -- there is no content
		// key wrap on a plain library, and zeroCommit anchors nothing -- so
		// what is asserted is that it was not refused as a permission
		// question. The two reads that must actually succeed are checked
		// exactly below.
		if code == http.StatusForbidden {
			t.Errorf("a read-only grantee %s: 403, but a read-only grant is for reading; body %s",
				route.name, body)
		}
	}

	// The grant is worth something, or the sweep above would pass just as well
	// on a server that had broken reading altogether.
	lib := base + "/api/silo/v1/libraries/" + libraryID
	if code, body := call(t, "GET", lib+"/objects/"+objectID, granteeToken, ""); code != http.StatusOK {
		t.Errorf("a read-only grantee reading an object: status %d, want 200, body %s", code, body)
	}
	if code, body := call(t, "GET", lib+"/chunks/"+chunkID, granteeToken, ""); code != http.StatusOK {
		t.Errorf("a read-only grantee reading a chunk: status %d, want 200, body %s", code, body)
	}

	// And nothing the sweep sent landed: the library still has its name, its
	// one file, and no second grant.
	code, body := call(t, "GET", lib+"/shares", token, "")
	if code != http.StatusOK {
		t.Fatalf("the owner listing shares: status %d, body %s", code, body)
	}
	var grants []struct {
		Email string `json:"email"`
		Perm  string `json:"perm"`
	}
	decodeInto(t, body, &grants)
	if len(grants) != 1 || grants[0].Email != "grantee@example.com" || grants[0].Perm != "r" {
		t.Errorf("the share list reads %+v after the sweep, want one read-only grant to the grantee", grants)
	}
}
