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
	for _, want := range []string{"entries", "entries-copy", "conditional-writes", "changes", "repo-rename"} {
		if !slices.Contains(got.Features, want) {
			t.Errorf("feature %q is missing from %v", want, got.Features)
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
