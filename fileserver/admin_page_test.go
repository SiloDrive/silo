package silod

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/dkam/silo/fileserver/account"
	"github.com/dkam/silo/fileserver/admin"
	"github.com/dkam/silo/fileserver/option"
	"github.com/dkam/silo/fileserver/traffic"
)

// The libraries panel, the storage panel, and the page that renders them.
//
// The assertion worth having in all three is what they refuse to say. An
// administrator may enumerate every library and reach into none of them; the
// storage panel names what it cannot measure instead of rendering a plausible
// number; and the page is served with no data in it at all.

func TestTheLibrariesListingSeesEveryLibraryAndNoContent(t *testing.T) {
	base, adminToken, userToken := adminWire(t)

	// A library belonging to somebody other than the administrator.
	mine := makeLibrary(t, base, userToken)

	code, body := call(t, "GET", base+"/api/silo/v1/admin/libraries", adminToken, "")
	if code != http.StatusOK {
		t.Fatalf("listing: %d, body %s", code, body)
	}
	var libs []struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Owner string `json:"owner"`
		E2EE  bool   `json:"e2ee"`
	}
	if err := json.Unmarshal([]byte(body), &libs); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, l := range libs {
		if l.ID == mine {
			found = true
			if l.Owner != "wire@example.com" {
				t.Errorf("the row names owner %q, want the account that owns it", l.Owner)
			}
		}
	}
	if !found {
		t.Fatalf("a library the administrator does not own is missing from the listing: %s", body)
	}

	// Metadata and no way in. The administrator holds no grant on this
	// library, so the ordinary content routes still refuse them -- which is the
	// structural line the listing does not cross.
	if code, _ := call(t, "GET", base+"/api/silo/v1/libraries/"+mine+"/entries/", adminToken, ""); code == http.StatusOK {
		t.Error("an administrator read the contents of a library nobody shared with them")
	}
}

// Gated by quota rather than users: a listing of every library with its owner
// and its size is usage information.
func TestTheLibrariesListingIsGatedByQuota(t *testing.T) {
	base, _, _ := adminWire(t)
	_, usersOnly := makeAccount(t, base, "onboarder@example.com", "an onboarder's password",
		account.RoleAdmin, admin.CapUsers)
	_, quotaOnly := makeAccount(t, base, "bursar@example.com", "a bursar's password",
		account.RoleAdmin, admin.CapQuota)

	if code, body := call(t, "GET", base+"/api/silo/v1/admin/libraries", usersOnly, ""); code != http.StatusForbidden {
		t.Errorf("the users capability opened the libraries listing = %d; body %s", code, body)
	}
	if code, body := call(t, "GET", base+"/api/silo/v1/admin/libraries", quotaOnly, ""); code != http.StatusOK {
		t.Errorf("the quota capability does not open the libraries listing = %d; body %s", code, body)
	}
}

// The panel that would have to lie, and does not.
func TestTheStoragePanelNamesWhatItCannotMeasure(t *testing.T) {
	base, adminToken, _ := adminWire(t)

	code, body := call(t, "GET", base+"/api/silo/v1/admin/storage", adminToken, "")
	if code != http.StatusOK {
		t.Fatalf("storage: %d, body %s", code, body)
	}
	var s struct {
		LogicalSize  int64            `json:"logical_size"`
		ServerQuota  *int64           `json:"server_quota"`
		DiskFree     *int64           `json:"disk_free"`
		AtRestSealed bool             `json:"at_rest_sealed"`
		Unmeasured   []string         `json:"unmeasured"`
		Throughput   traffic.Snapshot `json:"throughput"`
	}
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatal(err)
	}
	// No ceiling is configured in a test server, and the honest answer is null
	// rather than a zero that reads as "no bytes allowed".
	if s.ServerQuota != nil {
		t.Errorf("server_quota is %d on a server with no ceiling set, want null", *s.ServerQuota)
	}
	if s.DiskFree == nil || *s.DiskFree <= 0 {
		t.Error("the panel reports no free disk, which is the one wrong answer that looks like an emergency")
	}
	// At-rest sealing is not the same fact as E2EE, and the panel says so.
	if !s.AtRestSealed {
		t.Error("the panel says objects are not sealed at rest")
	}
	if len(s.Unmeasured) == 0 {
		t.Fatal("the panel claims to measure everything")
	}
	joined := strings.ToLower(strings.Join(s.Unmeasured, " "))
	for _, want := range []string{"storage locations", "cache"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the panel does not admit that %s is unmeasured: %v", want, s.Unmeasured)
		}
	}
	// Throughput used to be on that list and is not any more, which is the
	// half of this that a new measurement has to remember. A panel that
	// reports bytes and still says throughput is unmeasured is worse than
	// either one alone, so the list losing its entry is asserted rather than
	// left to whoever edits it next.
	if strings.Contains(joined, "throughput") {
		t.Errorf("the panel reports throughput and still calls it unmeasured: %v", s.Unmeasured)
	}
	if s.Throughput.WindowSeconds != traffic.WindowSeconds {
		t.Errorf("the throughput window is %d seconds, want %d", s.Throughput.WindowSeconds, traffic.WindowSeconds)
	}
	// Both lanes are always present, so a panel never has to distinguish "no
	// traffic" from "no such counter".
	for _, lane := range []string{"bulk", "control"} {
		if _, ok := s.Throughput.Total[lane]; !ok {
			t.Errorf("the throughput report has no %s lane: %+v", lane, s.Throughput.Total)
		}
		if _, ok := s.Throughput.Window[lane]; !ok {
			t.Errorf("the throughput window has no %s lane: %+v", lane, s.Throughput.Window)
		}
	}
}

// The request that fetched the panel is itself traffic, so a server that has
// served anything at all reports something. This is the end-to-end check that
// the middleware is installed: the counters can be perfect and still read zero
// if nothing calls them.
func TestTheStoragePanelReportsTrafficItActuallyServed(t *testing.T) {
	base, adminToken, _ := adminWire(t)

	code, body := call(t, "GET", base+"/api/silo/v1/admin/storage", adminToken, "")
	if code != http.StatusOK {
		t.Fatalf("storage: %d, body %s", code, body)
	}
	var s struct {
		Throughput traffic.Snapshot `json:"throughput"`
	}
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatal(err)
	}

	if s.Throughput.Total["control"].Out <= 0 {
		t.Errorf("the server has served requests and reports %d control bytes out: the counting middleware is not installed",
			s.Throughput.Total["control"].Out)
	}
}

// A configured ceiling is reported, so the panel is not simply always null.
func TestTheStoragePanelReportsAConfiguredCeiling(t *testing.T) {
	base, adminToken, _ := adminWire(t)

	orig := option.ServerQuota
	option.ServerQuota = 5 * option.GB
	t.Cleanup(func() { option.ServerQuota = orig })

	code, body := call(t, "GET", base+"/api/silo/v1/admin/storage", adminToken, "")
	if code != http.StatusOK {
		t.Fatalf("storage: %d, body %s", code, body)
	}
	var s struct {
		ServerQuota *int64 `json:"server_quota"`
	}
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatal(err)
	}
	if s.ServerQuota == nil || *s.ServerQuota != 5*option.GB {
		t.Errorf("server_quota = %v, want the configured ceiling", s.ServerQuota)
	}
}

func TestTheStoragePanelIsGatedByRetention(t *testing.T) {
	base, _, _ := adminWire(t)
	_, usersOnly := makeAccount(t, base, "onboarder@example.com", "an onboarder's password",
		account.RoleAdmin, admin.CapUsers)
	if code, _ := call(t, "GET", base+"/api/silo/v1/admin/storage", usersOnly, ""); code != http.StatusForbidden {
		t.Errorf("the users capability opened the storage panel = %d", code)
	}
}

// The page is served to anybody, because it holds nothing. Every number on it
// arrives from a fetch the browser makes with a credential the person typed in.
func TestTheAdminPageIsServedAndCarriesNoData(t *testing.T) {
	base, _, _ := adminWire(t)

	code, body := call(t, "GET", base+"/admin", "", "")
	if code != http.StatusOK {
		t.Fatalf("the admin page: %d", code)
	}
	if !strings.Contains(body, "<title>Silo") {
		t.Errorf("that is not the admin page: %.120s", body)
	}
	// No account or address is baked into the shell. The page is a form and two
	// empty tables; everything else arrives from a fetch.
	for _, leak := range []string{"root@example.com", "wire@example.com"} {
		if strings.Contains(body, leak) {
			t.Errorf("the page ships %q in its body", leak)
		}
	}
	// And it does not stash the credential anywhere that outlives the tab. The
	// check is for the writes rather than the words, because the page names
	// localStorage in a comment explaining why it does not use it -- and a test
	// that failed on the explanation would be a test that punished saying so.
	for _, write := range []string{
		"localStorage.setItem", "sessionStorage.setItem", "document.cookie =",
		"localStorage[", "sessionStorage[",
	} {
		if strings.Contains(body, write) {
			t.Errorf("the page writes the credential with %s", write)
		}
	}
	// Nothing is fetched from anywhere but this server.
	if strings.Contains(body, "//cdn") || strings.Contains(body, "https://") {
		t.Error("the page reaches off this server for an asset")
	}
}
