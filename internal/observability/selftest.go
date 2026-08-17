package observability

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	sentry "github.com/getsentry/sentry-go"
)

// SelfTest sends one error and one transaction through the same client the
// server uses, and says what the receiver made of them.
//
// It exists because the honest answer to "why is nothing showing up" is
// usually "nothing has gone wrong yet" — the server only reports failures, and
// a healthy server produces none. That is indistinguishable from a DSN
// pointing at a closed port, because the SDK's transport is asynchronous and
// swallows what it cannot deliver. This makes the two distinguishable in one
// command, by keeping the HTTP response the transport throws away.
func SelfTest(version string, out io.Writer) error {
	dsn := firstEnv("SILO_SENTRY_DSN", "SENTRY_DSN")
	if dsn == "" {
		return errors.New("no DSN configured: set SILO_SENTRY_DSN (or SENTRY_DSN) to the address reports should go to")
	}

	environment := firstEnv("SILO_SENTRY_ENVIRONMENT", "SENTRY_ENVIRONMENT")
	if environment == "" {
		environment = "production"
	}
	rel := release(version)

	rec := &recordingTransport{base: http.DefaultTransport}
	// Deliberately the same options as Init, save for three: the recorder, so
	// the status code survives; full trace sampling, so the transaction is
	// certain to be sent rather than probably; and a synchronous flush at the
	// end. A test that exercised a different configuration from the server
	// would answer a question nobody asked.
	err := sentry.Init(sentry.ClientOptions{
		Dsn:                    dsn,
		Environment:            environment,
		Release:                rel,
		ServerName:             serverName(),
		AttachStacktrace:       true,
		SendDefaultPII:         true,
		EnableTracing:          true,
		TracesSampleRate:       1.0,
		DisableTelemetryBuffer: true,
		Debug:                  envBool("SILO_SENTRY_DEBUG"),
		HTTPClient:             &http.Client{Transport: rec, Timeout: 15 * time.Second},
	})
	if err != nil {
		return fmt.Errorf("the DSN was not usable: %w", err)
	}
	defer sentry.Flush(flushTimeout)

	_, _ = fmt.Fprintf(out, "Reporting to %s\n", endpointOf(dsn))
	_, _ = fmt.Fprintf(out, "  environment %s, release %s\n\n", environment, orNone(rel))

	hub := sentry.CurrentHub()
	hub.WithScope(func(scope *sentry.Scope) {
		scope.SetTag("component", "sentry-test")
		scope.SetFingerprint([]string{"silo-sentry-test"})
		hub.CaptureMessage("Silo test event — reporting is configured correctly")
	})

	tx := sentry.StartTransaction(sentry.SetHubOnContext(context.Background(), hub), "GET /silo-sentry-test")
	tx.Finish()

	if !sentry.Flush(flushTimeout) {
		_, _ = fmt.Fprintf(out, "Timed out after %s waiting for delivery.\n", flushTimeout)
	}

	return report(out, rec.attempts())
}

// report turns what the receiver said into something worth acting on. The
// status codes are the ones a Sentry-compatible receiver distinguishes:
// anything 2xx accepted it, 401 means the key is wrong for that project, and
// 404 means no project of that name — which on Splat is the slug at the end of
// the DSN.
func report(out io.Writer, attempts []attempt) error {
	if len(attempts) == 0 {
		return errors.New("nothing was sent, which should not happen; re-run with SILO_SENTRY_DEBUG=true")
	}

	var failures int
	for _, a := range attempts {
		switch {
		case a.err != nil:
			failures++
			_, _ = fmt.Fprintf(out, "  %-12s could not reach the server: %v\n", a.kind, unwrapURLError(a.err))
		case a.status >= 200 && a.status < 300:
			_, _ = fmt.Fprintf(out, "  %-12s accepted (HTTP %d)\n", a.kind, a.status)
		case a.status == http.StatusUnauthorized || a.status == http.StatusForbidden:
			failures++
			_, _ = fmt.Fprintf(out, "  %-12s rejected (HTTP %d): the key in the DSN is not the one this project expects\n", a.kind, a.status)
		case a.status == http.StatusNotFound:
			failures++
			_, _ = fmt.Fprintf(out, "  %-12s rejected (HTTP %d): no project by that name — check the last path segment of the DSN\n", a.kind, a.status)
		default:
			failures++
			_, _ = fmt.Fprintf(out, "  %-12s rejected (HTTP %d)\n", a.kind, a.status)
		}
	}

	if failures > 0 {
		return fmt.Errorf("%d of %d deliveries failed", failures, len(attempts))
	}
	_, _ = fmt.Fprint(out, "\nDelivered. Look for the issue \"Silo test event — reporting is configured correctly\"\n"+
		"and the transaction \"GET /silo-sentry-test\"; both are safe to delete.\n")
	return nil
}

// attempt is one delivery, kept for the verdict.
type attempt struct {
	kind   string // what was being sent, as far as the URL reveals
	status int
	err    error
}

// recordingTransport keeps the responses the SDK discards. The SDK reports
// delivery failures only when Debug is on, and then only to stderr as prose;
// this is the same information in a form the command can act on.
type recordingTransport struct {
	base http.RoundTripper

	mu   sync.Mutex
	seen []attempt
}

func (t *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)

	a := attempt{kind: "envelope", err: err}
	if resp != nil {
		a.status = resp.StatusCode
	}

	t.mu.Lock()
	t.seen = append(t.seen, a)
	t.mu.Unlock()

	return resp, err
}

func (t *recordingTransport) attempts() []attempt {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]attempt(nil), t.seen...)
}

// unwrapURLError strips the wrapper net/http puts around a dial failure, which
// otherwise repeats the whole URL — key included — in front of the part that
// says what went wrong.
func unwrapURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return err
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(unset)"
	}
	return s
}
