package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/dkam/silo/fileserver/option"
)

// TestServerInfoAdvertisesFeatures covers the reason the endpoint exists: a
// client that has never seen this build can ask what it can call. A version
// string cannot answer that without a changelog.
func TestServerInfoAdvertisesFeatures(t *testing.T) {
	// Set explicitly: option.Version is stamped at startup, so it is empty in a
	// test binary and an assertion against it would only prove that.
	originalVersion := option.Version
	t.Cleanup(func() { option.Version = originalVersion })
	option.Version = "9.9.9"

	w := httptest.NewRecorder()
	ServerInfoHandler(w, httptest.NewRequest("GET", "/api/silo/v1/server-info", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var got siloServerInfo
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to decode %s: %v", w.Body.String(), err)
	}
	if got.Version != "9.9.9" {
		t.Errorf("version = %q, want %q", got.Version, "9.9.9")
	}
	// Named individually rather than compared as a set: a new capability must
	// not have to edit this test, but a removed one must break it, because
	// removing a feature name breaks every client that checked for it.
	for _, want := range []string{"entries", "entries-copy", "conditional-writes", "changes", "repo-rename", "blocks"} {
		if !slices.Contains(got.Features, want) {
			t.Errorf("feature %q is missing from %v", want, got.Features)
		}
	}
}

// TestServerInfoCarriesNoChunkerParameters pins the removal rather than the
// field. block_size was one server-wide fixed offset, and content-defined
// chunking made both halves of that wrong — the parameters are per library and
// there is no offset. A client reading a chunk size from here would be reading
// a number that cannot be right for every library the server holds, so the
// right answer is that there is nothing here to read.
func TestServerInfoCarriesNoChunkerParameters(t *testing.T) {
	w := httptest.NewRecorder()
	ServerInfoHandler(w, httptest.NewRequest("GET", "/api/silo/v1/server-info", nil))

	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to decode %s: %v", w.Body.String(), err)
	}
	for _, gone := range []string{"block_size", "chunk_size"} {
		if v, ok := got[gone]; ok {
			t.Errorf("server-info still reports %s = %v; chunker parameters belong to the library, not the server", gone, v)
		}
	}
}

// TestNotificationsFeatureFollowsConfiguration pins the conditional entry. It is
// the only one that depends on how this server was started, and it is the reason
// the list is computed per request rather than being a package-level literal.
func TestNotificationsFeatureFollowsConfiguration(t *testing.T) {
	original := option.EnableNotification
	t.Cleanup(func() { option.EnableNotification = original })

	for _, enabled := range []bool{true, false} {
		option.EnableNotification = enabled
		if got := slices.Contains(features(), "notifications"); got != enabled {
			t.Errorf("EnableNotification = %v, advertised = %v", enabled, got)
		}
	}
}
