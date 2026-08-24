package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dkam/silo/client"
)

// clientListing returns a client whose only surface is the libraries listing,
// which is all resolveLibrary reads.
func clientListing(t *testing.T, libraries []client.Library) *client.APIClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/silo/v1/libraries" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(libraries)
	}))
	t.Cleanup(srv.Close)
	return client.NewClient(srv.URL)
}

const (
	photosID = "4e5d525b-38a0-4198-95c7-2fde63a9b91d"
	otherID  = "d2432066-1f5f-47d7-a85e-cd4bc5a3c1cc"
)

// A name reaches the library it names.
func TestALibraryCanBeNamedByItsName(t *testing.T) {
	c := clientListing(t, []client.Library{
		{ID: otherID, Name: "Documents"},
		{ID: photosID, Name: "Photos"},
	})
	got, err := resolveLibrary(c, "Photos")
	if err != nil {
		t.Fatalf("resolveLibrary: %v", err)
	}
	if got != photosID {
		t.Errorf("resolved to %s, want %s", got, photosID)
	}
}

// An id is used as it stands, without a request.
//
// Asserted by giving the client a server that would fail the lookup: if an id
// were resolved like a name, this could not pass.
func TestAnIDIsNotLookedUp(t *testing.T) {
	c := client.NewClient("http://127.0.0.1:1") // nothing listening
	got, err := resolveLibrary(c, photosID)
	if err != nil {
		t.Fatalf("resolveLibrary: %v", err)
	}
	if got != photosID {
		t.Errorf("resolved to %s, want it unchanged", got)
	}
}

// An ambiguous name is refused, and the error carries what is needed to
// disambiguate.
//
// Picking the first match is the tempting shortcut and the dangerous one: it
// works for as long as the name is unique, and the day a second library takes
// the same name it silently writes to the wrong one.
func TestAnAmbiguousNameIsRefusedRatherThanGuessed(t *testing.T) {
	c := clientListing(t, []client.Library{
		{ID: photosID, Name: "Photos"},
		{ID: otherID, Name: "Photos"},
	})
	_, err := resolveLibrary(c, "Photos")
	if err == nil {
		t.Fatal("an ambiguous name resolved to something; it must not")
	}
	for _, want := range []string{photosID, otherID} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s, so there is no way to act on it: %v", want, err)
		}
	}
}

// A name nobody has says so, rather than becoming a request the server rejects
// for a different reason. Typing a name used to produce "403 Forbidden", which
// is the permission check answering before anything looked for the library.
func TestAnUnknownNameSaysSo(t *testing.T) {
	c := clientListing(t, []client.Library{{ID: photosID, Name: "Photos"}})
	_, err := resolveLibrary(c, "Videos")
	if err == nil {
		t.Fatal("an unknown name resolved to something")
	}
	if !strings.Contains(err.Error(), "Videos") {
		t.Errorf("the error does not repeat what was typed: %v", err)
	}
}
