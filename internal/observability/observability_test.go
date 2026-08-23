package observability

import (
	"net/http"
	"reflect"
	"testing"

	sentry "github.com/getsentry/sentry-go"
	log "github.com/sirupsen/logrus"
)

func TestNormalizeMessage(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want string
	}{
		{
			"repo id",
			"failed to get repo 7c8a1f2e-3b4d-4c5a-9e6f-0a1b2c3d4e5f: no such library",
			"failed to get repo <id>: no such library",
		},
		{
			"block id",
			"failed to read block a94a8fe5ccb19ba61c4c0873d391e987982fbbd3",
			"failed to read block <hash>",
		},
		{
			"account",
			"login failed for someone@example.com from 192.168.1.44",
			"login failed for <email> from <n>.<n>.<n>.<n>",
		},
		{
			"quoted path",
			`cannot open "/srv/silo/storage/commits/abc.txt": permission denied`,
			`cannot open "<str>": permission denied`,
		},
		{
			"nothing to strip",
			"failed to bind socket",
			"failed to bind socket",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeMessage(tt.msg); got != tt.want {
				t.Errorf("normalizeMessage(%q) = %q, want %q", tt.msg, got, tt.want)
			}
		})
	}
}

// Two failures of the same statement must land in the same issue however
// different the ids they mention are — that is the whole point of normalising.
func TestNormalizeMessageGroupsSiblings(t *testing.T) {
	a := normalizeMessage("failed to read block a94a8fe5ccb19ba61c4c0873d391e987982fbbd3")
	b := normalizeMessage("failed to read block da39a3ee5e6b4b0d3255bfef95601890afd80709")
	if a != b {
		t.Errorf("same statement normalised differently:\n %q\n %q", a, b)
	}
}

func TestUntraced(t *testing.T) {
	tests := []struct {
		spanName string
		want     bool
	}{
		{"GET /notification", true},
		{"GET /debug/pprof/heap", true},
		{"GET /protocol-version", true},
		{"GET /repo/7c8a1f2e-3b4d-4c5a-9e6f-0a1b2c3d4e5f/commit/HEAD", false},
		{"POST /api/silo/v1/repos", false},
		// A prefix match must not swallow an unrelated sibling route.
		{"GET /notifications-settings", false},
	}
	for _, tt := range tests {
		if got := untraced(tt.spanName); got != tt.want {
			t.Errorf("untraced(%q) = %v, want %v", tt.spanName, got, tt.want)
		}
	}
}

func TestReleaseFallsBackToVersion(t *testing.T) {
	t.Setenv("SILO_SENTRY_RELEASE", "")
	t.Setenv("SENTRY_RELEASE", "")
	if got, want := release("0.3.33"), "silo@0.3.33"; got != want {
		t.Errorf("release() = %q, want %q", got, want)
	}
	if got := release(""); got != "" {
		t.Errorf("release() with no version = %q, want empty", got)
	}
	t.Setenv("SILO_SENTRY_RELEASE", "silo@custom")
	if got, want := release("0.3.33"), "silo@custom"; got != want {
		t.Errorf("release() = %q, want %q", got, want)
	}
}

func TestTracesSampleRate(t *testing.T) {
	t.Setenv("SILO_SENTRY_TRACES_SAMPLE_RATE", "")
	if got := tracesSampleRate(); got != defaultTracesSampleRate {
		t.Errorf("unset rate = %v, want %v", got, defaultTracesSampleRate)
	}
	t.Setenv("SILO_SENTRY_TRACES_SAMPLE_RATE", "0.5")
	if got := tracesSampleRate(); got != 0.5 {
		t.Errorf("rate = %v, want 0.5", got)
	}
	// Out of range and unparseable both fall back rather than disabling
	// tracing silently or asking Sentry to sample 300% of requests.
	for _, bad := range []string{"3", "-1", "sometimes"} {
		t.Setenv("SILO_SENTRY_TRACES_SAMPLE_RATE", bad)
		if got := tracesSampleRate(); got != defaultTracesSampleRate {
			t.Errorf("rate for %q = %v, want %v", bad, got, defaultTracesSampleRate)
		}
	}
}

// The DSN's public key must not reach the log line that says where reports go.
func TestEndpointOfHidesTheKey(t *testing.T) {
	got := endpointOf("https://abc123secret@splat.example.com/4")
	if want := "https://splat.example.com/4"; got != want {
		t.Errorf("endpointOf() = %q, want %q", got, want)
	}
	if got := endpointOf("://nonsense"); got == "" {
		t.Error("endpointOf() on an unparseable DSN returned empty, want a placeholder")
	}
}

// With no DSN configured Init must not touch logrus or wrap the handler, so a
// server that never opts in pays nothing per request.
func TestInitWithoutDSNIsInert(t *testing.T) {
	t.Setenv("SILO_SENTRY_DSN", "")
	t.Setenv("SENTRY_DSN", "")

	before := len(log.StandardLogger().Hooks[log.ErrorLevel])
	flush := Init("test", "0.0.0")
	defer flush()

	if Enabled() {
		t.Error("Enabled() is true with no DSN configured")
	}
	if after := len(log.StandardLogger().Hooks[log.ErrorLevel]); after != before {
		t.Errorf("Init added %d logrus hook(s) with no DSN configured", after-before)
	}

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	if reflect.ValueOf(Middleware(h)).Pointer() != reflect.ValueOf(h).Pointer() {
		t.Error("Middleware wrapped the handler with no DSN configured")
	}
}

// The fingerprint has to name the code that logged, not the plumbing that
// carried the message — otherwise every error in the server groups into one
// issue named after logrus.
func TestCallSiteNamesTheCaller(t *testing.T) {
	// Sentry orders frames oldest-first, so the plumbing sits at the end.
	st := &sentry.Stacktrace{Frames: []sentry.Frame{
		{Module: "github.com/dkam/silo/fileserver", Function: "Run"},
		{Module: "github.com/dkam/silo/fileserver", Function: "blockOperCB"},
		{Module: "github.com/sirupsen/logrus", Function: "(*Entry).Errorf"},
		{Module: "github.com/sirupsen/logrus", Function: "LevelHooks.Fire"},
		{Module: "github.com/dkam/silo/internal/observability", Function: "(*logrusHook).Fire"},
	}}
	trimmed := trimPlumbing(st)
	if got, want := callSite(trimmed), "github.com/dkam/silo/fileserver.blockOperCB"; got != want {
		t.Errorf("callSite() = %q, want %q", got, want)
	}
	if n := len(trimmed.Frames); n != 2 {
		t.Errorf("trimPlumbing left %d frames, want 2", n)
	}
}

// A stack that is nothing but plumbing yields no fingerprint at all, rather
// than one naming the hook, which would group every error together.
func TestCallSiteOnAllPlumbing(t *testing.T) {
	st := &sentry.Stacktrace{Frames: []sentry.Frame{
		{Module: "github.com/sirupsen/logrus", Function: "LevelHooks.Fire"},
		{Module: "github.com/dkam/silo/internal/observability", Function: "(*logrusHook).Fire"},
	}}
	if got := callSite(trimPlumbing(st)); got != "" {
		t.Errorf("callSite() = %q, want empty", got)
	}
}

func TestSentryLevel(t *testing.T) {
	for level, want := range map[log.Level]sentry.Level{
		log.ErrorLevel: sentry.LevelError,
		log.FatalLevel: sentry.LevelFatal,
		log.PanicLevel: sentry.LevelFatal,
	} {
		if got := sentryLevel(level); got != want {
			t.Errorf("sentryLevel(%v) = %v, want %v", level, got, want)
		}
	}
}
