package upgrade

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCompareReadsDescribeBuildsAsAhead is the reason this does not just call
// semver.Compare on the two strings.
//
// `git describe` renders "three commits after v0.7.0" as "0.7.0-3-gabc1234",
// and semver reads anything after the hyphen as a pre-release, which sorts
// *before* 0.7.0. So a developer running a build newer than the last release
// would be told to upgrade to the release they are already past -- and the
// suggested command would move them backwards.
func TestCompareReadsDescribeBuildsAsAhead(t *testing.T) {
	cases := []struct {
		current, latest string
		want            Comparison
	}{
		{"0.7.0", "v0.8.0", Behind},
		{"0.7.0", "v0.7.0", UpToDate},

		// The trap. Both of these are newer than 0.7.0, not older.
		{"0.7.0-3-gabc1234", "v0.7.0", Development},
		{"0.7.0-dirty", "v0.7.0", Development},
		{"0.7.0-3-gabc1234-dirty", "v0.7.0", Development},

		// A describe build that really is behind: the tree is past 0.7.0, but
		// 0.8.0 has shipped since.
		{"0.7.0-3-gabc1234", "v0.8.0", Behind},

		// Numeric ordering, not lexical. "0.9.0" > "0.10.0" as strings.
		{"0.9.0", "v0.10.0", Behind},
		{"0.10.0", "v0.9.0", Ahead},

		// Leading v on either side must not change the answer; the binary
		// reports bare and the API reports tagged.
		{"v0.7.0", "0.8.0", Behind},

		// --always with no tag in history. Nothing to compare, so say so
		// rather than inventing an ordering.
		{"abc1234", "v0.8.0", ComparisonUnknown},
		{"0.7.0", "", ComparisonUnknown},
		{"", "v0.8.0", ComparisonUnknown},
	}

	for _, c := range cases {
		if got := Compare(c.current, c.latest); got != c.want {
			t.Errorf("Compare(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

func TestParseMethod(t *testing.T) {
	cases := map[string]Method{
		"deb": Deb, "rpm": RPM, "aur": AUR, "homebrew": Homebrew, "tarball": Tarball,
		// Case and surrounding whitespace come from build systems, not from
		// users, and are not worth failing over.
		"DEB": Deb, " homebrew\n": Homebrew,
		// An unstamped build and a stamp from a future version land in the
		// same place: general advice.
		"": Unknown, "snap": Unknown,
	}
	for in, want := range cases {
		if got := ParseMethod(in); got != want {
			t.Errorf("ParseMethod(%q) = %v, want %v", in, got, want)
		}
	}
}

// release is the shape the build workflow actually publishes: tarballs and
// sha256 sidecars from the `build` job, debs and rpms from `packages`.
func release() Release {
	r := Release{Tag: "v0.8.0", URL: "https://github.com/SiloDrive/silo/releases/tag/v0.8.0"}
	for _, n := range []string{
		"silo_0.8.0_amd64.deb",
		"silo_0.8.0_arm64.deb",
		"silo-0.8.0.x86_64.rpm",
		"silo-0.8.0.aarch64.rpm",
		"silo-v0.8.0-linux-amd64.tar.gz",
		"silo-v0.8.0-linux-amd64.tar.gz.sha256",
		"silo_0.8.0_amd64.deb.sha256",
		"silo-0.8.0.x86_64.rpm.sha256",
	} {
		r.Assets = append(r.Assets, Asset{Name: n, URL: "https://example.test/" + n})
	}
	return r
}

// TestAssetForNeverReturnsAChecksumFile is the specific way suffix matching
// goes wrong here. Every package ships a .sha256 beside it, so "ends in .deb"
// is right but "contains .deb" hands back silo_0.8.0_amd64.deb.sha256 -- a
// 100-byte text file that dpkg will refuse, after the download has succeeded
// and the URL has looked plausible.
func TestAssetForNeverReturnsAChecksumFile(t *testing.T) {
	r := release()
	for _, m := range []Method{Deb, RPM} {
		for _, arch := range []string{"amd64", "arm64"} {
			got, ok := r.AssetFor(m, arch)
			if !ok {
				t.Errorf("AssetFor(%v, %q) found nothing", m, arch)
				continue
			}
			if strings.HasSuffix(got.Name, ".sha256") {
				t.Errorf("AssetFor(%v, %q) = %q, which is a checksum file", m, arch, got.Name)
			}
		}
	}
}

// TestAssetForSpellsTheArchTheWayEachFormatDoes: deb says amd64/arm64, rpm says
// x86_64/aarch64, and runtime.GOARCH says neither for rpm. Matching on GOARCH
// alone finds no rpm at all; matching loosely finds the wrong one.
func TestAssetForSpellsTheArchTheWayEachFormatDoes(t *testing.T) {
	r := release()
	cases := []struct {
		m      Method
		goarch string
		want   string
	}{
		{Deb, "amd64", "silo_0.8.0_amd64.deb"},
		{Deb, "arm64", "silo_0.8.0_arm64.deb"},
		{RPM, "amd64", "silo-0.8.0.x86_64.rpm"},
		{RPM, "arm64", "silo-0.8.0.aarch64.rpm"},
	}
	for _, c := range cases {
		got, ok := r.AssetFor(c.m, c.goarch)
		if !ok || got.Name != c.want {
			t.Errorf("AssetFor(%v, %q) = %q (%v), want %q", c.m, c.goarch, got.Name, ok, c.want)
		}
	}

	// An architecture the release does not cover must report that, so the
	// caller falls back to the release page rather than printing a 404.
	if got, ok := r.AssetFor(Deb, "riscv64"); ok {
		t.Errorf("AssetFor(Deb, riscv64) = %q, want no match", got.Name)
	}
	// Methods with no downloadable package of their own.
	for _, m := range []Method{AUR, Homebrew, Tarball, Unknown} {
		if got, ok := r.AssetFor(m, "amd64"); ok {
			t.Errorf("AssetFor(%v, amd64) = %q, want no match", m, got.Name)
		}
	}
}

// TestAdviseNamesTheOwningPackageManager. The failure this prevents is telling
// somebody whose binary dpkg owns to pipe install.sh into sh: that writes a
// second silo to /usr/local/bin, which precedes /usr/bin on most PATHs, so the
// shell gets the new version while the systemd unit -- which names
// /usr/bin/silo absolutely -- keeps running the old one. Nothing errors.
func TestAdviseNamesTheOwningPackageManager(t *testing.T) {
	r := release()
	cases := []struct {
		m            Method
		wantContains []string
		wantAbsent   []string
	}{
		{Deb,
			[]string{"https://example.test/silo_0.8.0_amd64.deb", "dpkg -i", "silo_0.8.0_amd64.deb"},
			[]string{"install.sh", "rpm -", "brew "}},
		{RPM,
			[]string{"https://example.test/silo-0.8.0.x86_64.rpm", "rpm -U"},
			[]string{"install.sh", "dpkg", "brew "}},
		{AUR,
			[]string{"silo-bin"},
			[]string{"install.sh", "dpkg", "rpm -"}},
		{Homebrew,
			[]string{"brew upgrade"},
			[]string{"install.sh", "dpkg", "rpm -"}},
		{Tarball,
			[]string{"install.sh"},
			[]string{"dpkg", "rpm -", "brew "}},
	}
	for _, c := range cases {
		out := Advise(c.m, "0.7.0", r, "linux", "amd64")
		for _, want := range c.wantContains {
			if !strings.Contains(out, want) {
				t.Errorf("Advise(%v) omits %q:\n%s", c.m, want, out)
			}
		}
		for _, absent := range c.wantAbsent {
			if strings.Contains(out, absent) {
				t.Errorf("Advise(%v) suggests %q, which belongs to another install method:\n%s", c.m, absent, out)
			}
		}
		// Both versions belong in every one of them, or the operator cannot
		// tell what the command is going to do.
		for _, v := range []string{"0.7.0", "0.8.0"} {
			if !strings.Contains(out, v) {
				t.Errorf("Advise(%v) never mentions version %q:\n%s", c.m, v, out)
			}
		}
	}
}

// TestAdviseWithoutAKnownMethodSuggestsNoCommand. An unstamped build could be
// anything, including a package-managed one, so every command is potentially
// the damaging one. It gets the version delta and a link.
func TestAdviseWithoutAKnownMethodSuggestsNoCommand(t *testing.T) {
	out := Advise(Unknown, "0.7.0", release(), "linux", "amd64")
	for _, forbidden := range []string{"install.sh", "dpkg", "rpm -", "brew ", "yay "} {
		if strings.Contains(out, forbidden) {
			t.Errorf("Advise(Unknown) suggests %q without knowing it is safe:\n%s", forbidden, out)
		}
	}
	if !strings.Contains(out, "https://github.com/SiloDrive/silo/releases/tag/v0.8.0") {
		t.Errorf("Advise(Unknown) does not link the release page:\n%s", out)
	}
}

// TestAdviseFallsBackWhenTheArchHasNoPackage: a deb install on an architecture
// this release did not build. Printing a constructed URL would 404; the
// release page is the honest answer.
func TestAdviseFallsBackWhenTheArchHasNoPackage(t *testing.T) {
	out := Advise(Deb, "0.7.0", release(), "linux", "riscv64")
	if strings.Contains(out, "riscv64.deb") {
		t.Errorf("Advise invented a package name for an unbuilt arch:\n%s", out)
	}
	if !strings.Contains(out, "https://github.com/SiloDrive/silo/releases/tag/v0.8.0") {
		t.Errorf("Advise does not fall back to the release page:\n%s", out)
	}
}

// TestAdviseRestartsTheServiceOnlyWhereThereIsOne. Replacing the binary is an
// inode swap: the running process keeps executing the old code until it is
// restarted, so "upgraded" and "running the new version" are different events
// and the gap is silent. But `systemctl` on macOS is just a command that is
// not there.
func TestAdviseRestartsTheServiceOnlyWhereThereIsOne(t *testing.T) {
	if out := Advise(Deb, "0.7.0", release(), "linux", "amd64"); !strings.Contains(out, "systemctl restart silo") {
		t.Errorf("a linux package upgrade never mentions restarting the service:\n%s", out)
	}
	if out := Advise(Homebrew, "0.7.0", release(), "darwin", "arm64"); strings.Contains(out, "systemctl") {
		t.Errorf("Advise suggests systemctl on darwin:\n%s", out)
	}
}

// TestAdviseSaysNothingToDoWhenCurrent covers the common call. An up-to-date
// install must not be handed a command at all -- reinstalling the version you
// are running is how a config file gets clobbered for no reason.
func TestAdviseSaysNothingToDoWhenCurrent(t *testing.T) {
	out := Advise(Deb, "0.8.0", release(), "linux", "amd64")
	if strings.Contains(out, "dpkg") {
		t.Errorf("Advise suggests reinstalling an up-to-date silo:\n%s", out)
	}
	if !strings.Contains(out, "0.8.0") {
		t.Errorf("Advise does not say which version is current:\n%s", out)
	}
}

// TestFetchLatestReadsTheAssetListRatherThanGuessingIt.
//
// The asset names are decided by the build: nfpm's defaults for the deb and
// rpm, the workflow's own string for the tarball, and packaging/nfpm.yaml sets
// no name_template, so the rpm name is whatever nfpm's version produces.
// Reconstructing those names here would mean this package and the build agree
// by coincidence, and the symptom of them disagreeing is a printed URL that
// 404s after the operator has already trusted it. So the names come from the
// release itself.
func TestFetchLatestReadsTheAssetListRatherThanGuessingIt(t *testing.T) {
	const body = `{
	  "tag_name": "v0.8.0",
	  "html_url": "https://github.com/SiloDrive/silo/releases/tag/v0.8.0",
	  "assets": [
	    {"name": "silo_0.8.0_amd64.deb", "browser_download_url": "https://example.test/silo_0.8.0_amd64.deb"},
	    {"name": "silo-0.8.0.x86_64.rpm", "browser_download_url": "https://example.test/silo-0.8.0.x86_64.rpm"}
	  ]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	got, err := FetchLatest(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("FetchLatest: %v", err)
	}
	if got.Tag != "v0.8.0" {
		t.Errorf("Tag = %q, want v0.8.0", got.Tag)
	}
	if got.URL != "https://github.com/SiloDrive/silo/releases/tag/v0.8.0" {
		t.Errorf("URL = %q", got.URL)
	}
	if len(got.Assets) != 2 {
		t.Fatalf("got %d assets, want 2", len(got.Assets))
	}
	a, ok := got.AssetFor(Deb, "amd64")
	if !ok || a.URL != "https://example.test/silo_0.8.0_amd64.deb" {
		t.Errorf("AssetFor(Deb, amd64) = %+v (%v)", a, ok)
	}
}

// TestFetchLatestReportsAnHTTPFailure. The rate-limited case is the likely one
// -- GitHub allows 60 unauthenticated calls an hour per IP -- and a 403 body
// parsed as a release yields an empty tag, which Compare then reads as
// "cannot compare". That is a confusing way to say "try again later".
func TestFetchLatestReportsAnHTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"API rate limit exceeded"}`)
	}))
	defer srv.Close()

	if _, err := FetchLatest(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Fatal("FetchLatest returned no error for a 403")
	} else if !strings.Contains(err.Error(), "403") {
		t.Errorf("error does not name the status: %v", err)
	}
}

// TestFetchLatestRejectsAReleaseWithNoTag. A release with no tag_name compares
// against nothing; failing here names the cause, where returning it would
// surface three layers away as an unexplained "cannot be compared".
func TestFetchLatestRejectsAReleaseWithNoTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"assets":[]}`)
	}))
	defer srv.Close()

	if _, err := FetchLatest(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Fatal("FetchLatest accepted a release with no tag_name")
	}
}

// TestResolvePrefersTheMarkerTheInstallLeftBehind is the hole the ldflags stamp
// alone does not cover.
//
// The build workflow compiles each binary once, and the `packages` job
// repackages those same binaries into the .deb and .rpm rather than compiling
// again. silo-bin on the AUR extracts the published tarball. So all three
// package-managed installs carry whatever stamp the tarball build used --
// "tarball" -- and would be told to pipe install.sh into sh, which writes a
// second silo to /usr/local/bin and shadows the one the package manager owns.
//
// The fix is not to compile four more times. Each package installs a marker at
// <prefix>/share/silo/install-method, so the evidence comes from the package
// manager that actually placed the file, and the binary itself needs no
// special build.
func TestResolvePrefersTheMarkerTheInstallLeftBehind(t *testing.T) {
	// markers stands in for the filesystem: path -> contents.
	markers := map[string]string{
		"/usr/share/silo/install-method":          "deb",
		"/opt/homebrew/share/silo/install-method": "homebrew",
	}
	read := func(p string) (string, bool) {
		v, ok := markers[p]
		return v, ok
	}

	cases := []struct {
		name  string
		stamp string
		exe   string
		want  Method
	}{
		// The case above: a tarball-stamped binary that dpkg placed.
		{"deb binary carrying the tarball stamp", "tarball", "/usr/bin/silo", Deb},

		// The same machine, running the loose binary install.sh put in
		// /usr/local/bin. Its prefix has no marker, so the deb's marker one
		// directory over must not claim it -- that would send someone
		// upgrading their personal copy into dpkg.
		{"loose binary beside a deb install", "tarball", "/usr/local/bin/silo", Tarball},

		{"homebrew prefix", "", "/opt/homebrew/bin/silo", Homebrew},

		// No marker and no stamp: `go build ./cmd/silo`. General advice.
		{"unstamped development build", "", "/home/dan/go/bin/silo", Unknown},

		// A stamp with no marker is still authoritative; that is the path a
		// from-source formula or a future package takes.
		{"stamp with no marker", "homebrew", "/home/dan/silo", Homebrew},

		// Too short to have a <prefix>/bin, and must not panic looking.
		{"bare relative path", "tarball", "silo", Tarball},
		{"empty path", "tarball", "", Tarball},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Resolve(c.stamp, c.exe, read); got != c.want {
				t.Errorf("Resolve(%q, %q) = %v, want %v", c.stamp, c.exe, got, c.want)
			}
		})
	}
}

// TestResolveIgnoresAnUnreadableMarker. The marker is a file on disk that
// anything could have written; a truncated or foreign value must fall back to
// the stamp rather than be reported as a Method nothing handles.
func TestResolveIgnoresAnUnreadableMarker(t *testing.T) {
	cases := map[string]Method{
		// Trailing newline is what `echo deb > file` produces, and is the
		// normal case rather than the exceptional one.
		"deb\n":   Deb,
		"  rpm  ": RPM,
		"":        Homebrew, // empty marker, falls back to the stamp
		"snap":    Homebrew, // a method this build does not know
	}
	for contents, want := range cases {
		read := func(string) (string, bool) { return contents, true }
		if got := Resolve("homebrew", "/usr/bin/silo", read); got != want {
			t.Errorf("Resolve with marker %q = %v, want %v", contents, got, want)
		}
	}
}

// TestMarkerPathFollowsThePrefixConvention. The packaging has to write the file
// where this looks for it, and the two live in different repositories' worth of
// tooling -- nfpm.yaml, a PKGBUILD, a formula -- so the convention is pinned
// here rather than agreed by eye.
func TestMarkerPathFollowsThePrefixConvention(t *testing.T) {
	cases := map[string]string{
		"/usr/bin/silo":          "/usr/share/silo/install-method",
		"/usr/local/bin/silo":    "/usr/local/share/silo/install-method",
		"/opt/homebrew/bin/silo": "/opt/homebrew/share/silo/install-method",
		"silo":                   "",
		"":                       "",
	}
	for exe, want := range cases {
		if got := MarkerPath(exe); got != want {
			t.Errorf("MarkerPath(%q) = %q, want %q", exe, got, want)
		}
	}
}
