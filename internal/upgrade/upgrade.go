// Package upgrade answers one question: this binary is version X, the latest
// release is version Y, so what exactly should the operator type?
//
// It deliberately does not upgrade anything. A silo installed from a .deb has
// its binary at /usr/bin/silo, owned by dpkg; writing over that leaves the
// package database describing a file that is no longer there, and the next
// package operation either reverts the upgrade or fails a checksum. The same
// goes for rpm, for pacman and for Homebrew's Cellar. So this package prints
// the command belonging to the package manager that owns the file, and the one
// case it could safely act on -- a loose binary from install.sh -- is left to
// install.sh, which already does it correctly.
package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// Method is how this binary got onto the machine. It is stamped at build time
// via -ldflags "-X main.InstallMethod=...", because nothing observable at
// runtime distinguishes a binary dpkg placed at /usr/bin/silo from one somebody
// copied there by hand, and guessing wrong means printing a command that
// damages a working install.
type Method string

const (
	Deb      Method = "deb"
	RPM      Method = "rpm"
	AUR      Method = "aur"
	Homebrew Method = "homebrew"
	Tarball  Method = "tarball"
	// Unknown covers an unstamped build -- `go build ./cmd/silo` -- and any
	// stamp this version does not recognise. It is not an error: it means the
	// advice has to stay general rather than name a command.
	Unknown Method = "unknown"
)

// ParseMethod is lenient about case and surrounding whitespace because the
// value arrives from a Makefile, an nfpm environment variable or a PKGBUILD,
// where a stray newline is a plausible accident and not worth failing over.
func ParseMethod(s string) Method {
	switch Method(strings.ToLower(strings.TrimSpace(s))) {
	case Deb:
		return Deb
	case RPM:
		return RPM
	case AUR:
		return AUR
	case Homebrew:
		return Homebrew
	case Tarball:
		return Tarball
	default:
		return Unknown
	}
}

// Comparison is the relationship between the running version and the latest
// release.
type Comparison int

const (
	ComparisonUnknown Comparison = iota
	UpToDate
	Behind
	Ahead
	// Development is a `git describe` build sitting on commits after a tag, or
	// a dirty tree. It is ahead of its base tag, not behind it.
	Development
)

func (c Comparison) String() string {
	switch c {
	case UpToDate:
		return "UpToDate"
	case Behind:
		return "Behind"
	case Ahead:
		return "Ahead"
	case Development:
		return "Development"
	default:
		return "ComparisonUnknown"
	}
}

// describeSuffix matches the "3 commits past the tag" part of `git describe`
// output: -<count>-g<abbrev>.
var describeSuffix = regexp.MustCompile(`-[0-9]+-g[0-9a-f]+$`)

// parseVersion splits a reported version into the tag it is based on and
// whether it is a build past that tag.
//
// The distinction matters because semver reads everything after the first
// hyphen as a pre-release, which sorts *before* the release: "0.7.0-3-gabc1234"
// would compare as older than "0.7.0", so a developer on a build newer than the
// last release would be told to upgrade to the release they are already past,
// and the command offered would move them backwards.
func parseVersion(v string) (base string, dev bool, ok bool) {
	v = strings.TrimSpace(v)
	if len(v) > 1 && (v[0] == 'v' || v[0] == 'V') && v[1] >= '0' && v[1] <= '9' {
		v = v[1:]
	}
	if v == "" {
		return "", false, false
	}
	if s := strings.TrimSuffix(v, "-dirty"); s != v {
		v, dev = s, true
	}
	if s := describeSuffix.ReplaceAllString(v, ""); s != v {
		v, dev = s, true
	}
	if !semver.IsValid("v" + v) {
		// `git describe --always` on a history with no tags gives a bare short
		// SHA, which is a real thing to be running and not a thing to order.
		return "", false, false
	}
	return v, dev, true
}

// Compare reports where current sits relative to latest. Either side may carry
// a leading "v": the binary reports bare and the releases API reports tagged.
func Compare(current, latest string) Comparison {
	cur, dev, okCur := parseVersion(current)
	rel, _, okRel := parseVersion(latest)
	if !okCur || !okRel {
		return ComparisonUnknown
	}
	switch c := semver.Compare("v"+cur, "v"+rel); {
	case c < 0:
		return Behind
	case c > 0:
		return Ahead
	case dev:
		return Development
	default:
		return UpToDate
	}
}

// Asset is one published file on a release.
type Asset struct {
	Name string
	URL  string
}

// Release is the subset of the releases API this package reads.
type Release struct {
	Tag    string
	URL    string
	Assets []Asset
}

// archSuffix is the tail of the published filename for a format and a
// runtime.GOARCH, or "" when that combination is not something the release
// publishes.
//
// deb and rpm disagree on both the separator and the spelling of the
// architecture -- silo_0.8.0_amd64.deb against silo-0.8.0.x86_64.rpm -- and
// neither rpm spelling is what GOARCH says. Matching the whole tail rather than
// searching for ".deb" also excludes the .sha256 published beside each package,
// which would otherwise match and hand back a hundred bytes of text that dpkg
// rejects only after the download has succeeded.
func archSuffix(m Method, goarch string) string {
	switch m {
	case Deb:
		switch goarch {
		case "amd64", "arm64":
			return "_" + goarch + ".deb"
		}
	case RPM:
		switch goarch {
		case "amd64":
			return ".x86_64.rpm"
		case "arm64":
			return ".aarch64.rpm"
		}
	}
	return ""
}

// AssetFor finds the package this method and architecture should install. It
// reports false for methods with nothing to download -- the AUR, Homebrew and
// install.sh each resolve their own artefact -- and for an architecture this
// release did not build.
func (r Release) AssetFor(m Method, goarch string) (Asset, bool) {
	suffix := archSuffix(m, goarch)
	if suffix == "" {
		return Asset{}, false
	}
	for _, a := range r.Assets {
		if strings.HasSuffix(a.Name, suffix) {
			return a, true
		}
	}
	return Asset{}, false
}

// noReplace is worth saying out loud because it is the fear that stops people
// upgrading: both config files are marked config|noreplace in
// packaging/nfpm.yaml, so neither package manager silently overwrites an edit.
const noReplaceDeb = "\n/etc/silo/silo.conf and /etc/silo/silo.env are marked noreplace, so dpkg\nprompts before replacing a file you have edited.\n"

const noReplaceRPM = "\n/etc/silo/silo.conf and /etc/silo/silo.env are marked noreplace, so a file\nyou have edited is kept and the new one is written beside it as .rpmnew.\n"

// Advise renders what to do about the gap between current and r.Tag.
//
// goos and goarch are parameters rather than reads of runtime.GOOS/GOARCH so
// the rendering is testable, which matters more here than usual: every branch
// of this function is a command somebody will paste into a root shell.
func Advise(m Method, current string, r Release, goos, goarch string) string {
	latest := strings.TrimPrefix(r.Tag, "v")

	switch Compare(current, r.Tag) {
	case UpToDate:
		return fmt.Sprintf("%s is the latest release.\n", current)
	case Development:
		return fmt.Sprintf("%s is a development build, ahead of the latest release (%s).\n", current, latest)
	case Ahead:
		return fmt.Sprintf("%s is newer than the latest release (%s).\n", current, latest)
	case ComparisonUnknown:
		return fmt.Sprintf("This build reports its version as %q, which cannot be ordered against\nthe latest release (%s).\n\n  %s\n", current, latest, r.URL)
	}

	header := fmt.Sprintf("%s → %s", current, latest)
	restart := ""
	if goos == "linux" {
		restart = "  sudo systemctl restart silo\n"
	}

	switch m {
	case Deb, RPM:
		asset, ok := r.AssetFor(m, goarch)
		if !ok {
			// A constructed URL for an architecture the release never built
			// 404s, and it 404s after the operator has trusted it enough to
			// paste it. The release page is the honest answer.
			return fmt.Sprintf("%s\n\nThis release publishes no package for %s. What it does publish is listed at:\n\n  %s\n",
				header, goarch, r.URL)
		}
		if m == Deb {
			return fmt.Sprintf("%s  (installed from a .deb)\n\n  curl -sSfLO %s\n  sudo dpkg -i %s\n%s%s",
				header, asset.URL, asset.Name, restart, noReplaceDeb)
		}
		return fmt.Sprintf("%s  (installed from an .rpm)\n\n  curl -sSfLO %s\n  sudo rpm -Uvh %s\n%s%s",
			header, asset.URL, asset.Name, restart, noReplaceRPM)

	case AUR:
		// The AUR recipe is published from packaging/aur by hand, so it can
		// legitimately trail a GitHub release by a while. Saying so beats
		// having the command appear to do nothing.
		return fmt.Sprintf("%s  (installed from the AUR)\n\n  yay -Syu silo-bin\n  systemctl --user restart silo\n\nsilo-bin is updated by hand after each release, so it may not carry %s yet.\n",
			header, latest)

	case Homebrew:
		return fmt.Sprintf("%s  (installed with Homebrew)\n\n  brew update && brew upgrade silo\n", header)

	case Tarball:
		return fmt.Sprintf("%s  (installed from a release tarball)\n\n  curl -sSfL https://raw.githubusercontent.com/dkam/silo/main/install.sh | sh\n%s",
			header, restart)

	default:
		// Every command above is the damaging one for somebody: the pipe
		// writes a second silo to /usr/local/bin, which precedes /usr/bin on
		// most PATHs, so the shell would get the new version while the systemd
		// unit -- which names /usr/bin/silo absolutely -- kept running the old
		// one, with nothing reporting a problem. Not knowing which install
		// this is means not being able to rule that out.
		return fmt.Sprintf("%s\n\nThis build does not record how it was installed, so upgrade it the same way\nyou installed it. Doing it another way can leave two copies on the machine,\nwith your shell and your service running different ones.\n\n  %s\n",
			header, r.URL)
	}
}

// DefaultLatestURL is the GitHub releases API. install.sh names the same
// endpoint and can be pointed elsewhere; so can this, for the same reason --
// a Gitea instance serves the identical shape at
// /api/v1/repos/<owner>/<repo>/releases/latest.
const DefaultLatestURL = "https://api.github.com/repos/dkam/silo/releases/latest"

// FetchLatest reads the latest release, including the real names of the files
// it published.
//
// The asset names are not reconstructed from a convention: nfpm's defaults
// decide the deb and rpm names, packaging/nfpm.yaml sets no name_template, and
// a convention guessed here would agree with the build only by coincidence.
// The symptom of disagreement is a URL that 404s after the operator has already
// trusted it.
func FetchLatest(ctx context.Context, c *http.Client, url string) (Release, error) {
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("checking for the latest release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Named rather than swallowed: 403 here is almost always GitHub's
		// unauthenticated rate limit, 60 an hour per IP. Parsing that body as a
		// release yields an empty tag, which Compare reads as "cannot be
		// ordered" -- a confusing way to say "try again later".
		return Release{}, fmt.Errorf("checking for the latest release: %s returned %d %s",
			url, resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	// Capped: this is an unauthenticated endpoint over the network, and a
	// release with a great many assets is still kilobytes.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Release{}, fmt.Errorf("reading the release: %w", err)
	}

	var payload struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Release{}, fmt.Errorf("parsing the release: %w", err)
	}
	if payload.TagName == "" {
		return Release{}, fmt.Errorf("%s described a release with no tag_name", url)
	}

	r := Release{Tag: payload.TagName, URL: payload.HTMLURL}
	for _, a := range payload.Assets {
		r.Assets = append(r.Assets, Asset{Name: a.Name, URL: a.URL})
	}
	return r, nil
}

// MarkerPath is where the package that installed exe records which package
// manager it was: <prefix>/share/silo/install-method, derived from the binary's
// own location by the ordinary prefix convention. /usr/bin/silo gives
// /usr/share/silo/install-method, /opt/homebrew/bin/silo gives the Homebrew
// prefix, and a binary somewhere with no <prefix>/bin shape gives "".
//
// Deriving it from exe rather than hard-coding /usr/share is what keeps a loose
// binary in /usr/local/bin from reading the marker a deb left in /usr/share and
// concluding that dpkg owns it.
func MarkerPath(exe string) string {
	if exe == "" {
		return ""
	}
	bin := filepath.Dir(exe)
	if bin == "." || bin == string(filepath.Separator) {
		return ""
	}
	return filepath.Join(filepath.Dir(bin), "share", "silo", "install-method")
}

// FileMarker reads a marker off the real filesystem. Any error is reported as
// absence: an unreadable marker and a missing one lead to the same place, which
// is the build-time stamp.
func FileMarker(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// Resolve decides how this binary was installed, from the marker its installer
// left beside it and the stamp its build gave it, in that order.
//
// The marker wins because it is the stronger evidence: a package manager put it
// there as part of the same transaction that placed the binary, and it goes
// away when the package does. The stamp is what a build asserted about itself,
// which for the .deb, the .rpm and silo-bin is "tarball" -- true of how the
// binary was produced, wrong about how it arrived.
func Resolve(stamp, exe string, read func(string) (string, bool)) Method {
	if p := MarkerPath(exe); p != "" && read != nil {
		if contents, ok := read(p); ok {
			if m := ParseMethod(contents); m != Unknown {
				return m
			}
		}
	}
	return ParseMethod(stamp)
}
