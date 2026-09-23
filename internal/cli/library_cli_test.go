package cli

import (
	"encoding/json"
	"github.com/SiloDrive/silo/internal/lexicon"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The CLI says library.
//
// The word a person types is the one tier of this rename with no machine on
// the other end to complain, which is why it is the tier most likely to be
// left half-done. `silo libraries` would go on working forever, and the only
// symptom would be documentation that describes a command nobody can run.

// cliServer answers login and the library listing, which is as much as a
// dispatch test needs: Run logs in before it looks at the subcommand, so a
// test of subcommand routing still has to get past that.
func cliServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/silo/v1/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "t"})
		case r.URL.Path == "/api/silo/v1/libraries" && r.Method == "GET":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": photosID, "name": "Photos"},
			})
		case r.URL.Path == "/api/silo/v1/libraries" && r.Method == "POST":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": otherID, "name": "new"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestTheCLIListsLibraries(t *testing.T) {
	if err := Run(cliServer(t), "a@b.c", "pw", []string{"libraries"}); err != nil {
		t.Errorf("silo libraries: %v", err)
	}
}

func TestTheCLICreatesALibrary(t *testing.T) {
	if err := Run(cliServer(t), "a@b.c", "pw", []string{"library", "create", "new"}); err != nil {
		t.Errorf("silo library create: %v", err)
	}
}

// The old words are gone rather than kept as aliases. An alias is a second
// spelling that outlives the reason for it, and the point of the sweep was to
// have one.
func TestTheRetiredCLIWordsAreGone(t *testing.T) {
	url := cliServer(t)
	// From the published constant, not written out: the sweep rewrote the
	// literals in the first draft of this test into the new spelling, leaving
	// it asserting that the commands it had just added did not exist.
	for _, args := range [][]string{
		{lexicon.RetiredSegment},
		{lexicon.RetiredNoun, "create", "new"},
	} {
		err := Run(url, "a@b.c", "pw", args)
		if err == nil {
			t.Errorf("silo %s: succeeded, want an unknown-subcommand error",
				strings.Join(args, " "))
			continue
		}
		if !strings.Contains(err.Error(), "unknown subcommand") {
			t.Errorf("silo %s: %v, want an unknown-subcommand error",
				strings.Join(args, " "), err)
		}
	}
}

// TestUsageStringsSayLibrary covers what a person reads when they get it
// wrong. Every one of these is a sentence printed straight at somebody, and a
// usage line naming an argument the docs no longer use is worse than no usage
// line — it is the CLI arguing with its own manual.
func TestUsageStringsSayLibrary(t *testing.T) {
	url := cliServer(t)
	for _, args := range [][]string{
		{"ls"},
		{"get"},
		{"put"},
		{"mkdir"},
		{"rm"},
		{"mv"},
		{"rename"},
		{"changes"},
		{"library"},
	} {
		err := Run(url, "a@b.c", "pw", args)
		if err == nil {
			t.Errorf("silo %s with no arguments: no error, so no usage line", args[0])
			continue
		}
		if lexicon.SaysOldWord(err.Error()) {
			t.Errorf("silo %s: %q still says library", args[0], err.Error())
		}
	}
}
