package observability

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/getsentry/sentry-go"

	"github.com/SiloDrive/silo/fileserver/setup"
)

// eventFrom runs one capture through a real client built from clientOptions and
// hands back the event as it would have gone out.
//
// BeforeSend is the last thing the SDK calls before the transport, so
// intercepting there sees exactly what would be sent -- including whatever
// ApplyToEvent attached from the scope.
func eventFrom(t *testing.T, prepare func(*sentry.Scope), capture func(*sentry.Hub)) *sentry.Event {
	t.Helper()

	var got *sentry.Event
	opts := clientOptions("https://key@example.invalid/1", "test", "v0.0.0-test", 0)

	// Chained onto the real BeforeSend rather than replacing it. Replacing it
	// would quietly drop redactSecrets, which is one of the things under test:
	// the first version of this helper did exactly that, and the redaction test
	// failed against a redactor that was working.
	real := opts.BeforeSend
	opts.BeforeSend = func(e *sentry.Event, hint *sentry.EventHint) *sentry.Event {
		if real != nil {
			e = real(e, hint)
		}
		got = e
		return nil // never actually leaves the process
	}

	client, err := sentry.NewClient(opts)
	if err != nil {
		t.Fatalf("building a client from clientOptions: %v", err)
	}

	scope := sentry.NewScope()
	prepare(scope)
	capture(sentry.NewHub(client, scope))

	if got == nil {
		t.Fatal("nothing reached BeforeSend")
	}
	return got
}

// A request body must never leave this process. Bodies here are file contents:
// a crash report that carried one would be a copy of somebody's document in a
// third party's issue tracker.
//
// This is asserted rather than assumed because the assumption was written down
// and was wrong. clientOptions set SendDefaultPII with no DataCollection, and
// its comment said bodies were safe because the SDK never reads them. That was
// true of an older sentry-go. In v0.48, SendDefaultPII with a nil
// DataCollection resolves to HTTPBodies: allBodyTypes() -- see
// legacyDataCollection in the SDK -- and anything under the 10 KiB scope buffer
// is attached.
func TestRequestBodyIsNotCaptured(t *testing.T) {
	const secretDocument = "the-quarterly-numbers-nobody-should-see"

	event := eventFrom(t,
		func(scope *sentry.Scope) {
			req, err := http.NewRequest("POST", "https://silo.example/api/silo/v1/libraries",
				strings.NewReader(`{"name":"`+secretDocument+`"}`))
			if err != nil {
				t.Fatalf("building the request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			scope.SetRequest(req)

			// Read it, because that is what makes this test real. The SDK tees
			// the body into a buffer as the handler consumes it, so a body
			// nobody read is a body nobody can leak -- and a version of this
			// test that skipped the read passed against the very configuration
			// it was written to catch.
			if _, err := io.ReadAll(req.Body); err != nil {
				t.Fatalf("reading the body the way a handler would: %v", err)
			}
		},
		func(hub *sentry.Hub) { hub.CaptureMessage("something broke") },
	)

	if event.Request == nil {
		t.Fatal("no request attached at all; this test would prove nothing")
	}
	if strings.Contains(event.Request.Data, secretDocument) {
		t.Errorf("the request body was attached to the event:\n%s", event.Request.Data)
	}
	if event.Request.Data != "" {
		t.Errorf("a request body was attached, %d bytes: %q", len(event.Request.Data), event.Request.Data)
	}
}

// The URL, the method and the headers stay: naming whose sync broke is the
// whole point of sending a report. The SDK's own denylist is what keeps
// Authorization out, which is the only sensitive header Silo reads.
func TestTheRequestItselfIsStillReported(t *testing.T) {
	event := eventFrom(t,
		func(scope *sentry.Scope) {
			req, _ := http.NewRequest("GET", "https://silo.example/api/silo/v1/libraries/abc/entries/x", nil)
			req.Header.Set("Authorization", "Bearer silo_session_supersecrettokenvalue")
			scope.SetRequest(req)
		},
		func(hub *sentry.Hub) { hub.CaptureMessage("something broke") },
	)

	if event.Request == nil {
		t.Fatal("no request was attached at all; the report cannot say what broke")
	}
	if !strings.Contains(event.Request.URL, "/libraries/abc/entries/x") {
		t.Errorf("the URL is missing from the report: %q", event.Request.URL)
	}
	for k, v := range event.Request.Headers {
		if strings.Contains(v, "supersecrettokenvalue") {
			t.Errorf("header %s carried the bearer token: %q", k, v)
		}
	}
}

// A setup token must not reach an error reporter. The level is what normally
// keeps it out -- logSetupToken logs at Warn and the hook fires on Error and
// above -- so this is the second line, for the day someone raises that level
// without noticing what it was holding back.
func TestSetupTokenIsRedactedFromAMessage(t *testing.T) {
	// Minted rather than pasted, so this test fails if the token format and
	// the pattern that redacts it ever drift apart.
	minted, err := setup.Generate()
	if err != nil {
		t.Fatalf("minting a token: %v", err)
	}
	tok := minted.String()

	event := eventFrom(t,
		func(*sentry.Scope) {},
		func(hub *sentry.Hub) { hub.CaptureMessage("setup token: " + tok + " was refused") },
	)

	if strings.Contains(event.Message, tok) {
		t.Errorf("the setup token went out in the message: %q", event.Message)
	}
	if !strings.Contains(event.Message, "[redacted]") {
		t.Errorf("the message does not show the redaction: %q", event.Message)
	}
}

// The hook is what decides whether a log line becomes an event at all, and
// logSetupToken relies on Warn being below it. Assert the level rather than
// only the redaction: the redaction is a backstop, and a backstop that has
// quietly become the only defence should fail loudly.
func TestTheSentryHookIgnoresWarnings(t *testing.T) {
	for _, lvl := range (&logrusHook{}).Levels() {
		if lvl.String() == "warning" {
			t.Fatal("the Sentry hook now fires on warnings, which is the level the setup token is printed at")
		}
	}
}
