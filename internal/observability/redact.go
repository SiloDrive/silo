package observability

import (
	"github.com/SiloDrive/silo/fileserver/setup"
	"github.com/getsentry/sentry-go"
)

// The setup token's own package owns what one looks like: setup.Redact is built
// from the same alphabet, length and prefix that render a token in the first
// place, so an edit to the format cannot leave a redactor here that compiles,
// runs and no longer matches. What stays here is the policy question -- which
// fields of an event a secret can reach.
//
// It is the one secret in the tree with a format specific enough to match
// without false positives. A credential token is "silo_<kind>_<id>_<secret>"
// and could be matched too, but credential tokens are never logged -- their
// errors are written specifically not to carry them -- whereas the setup token
// is printed to the log on purpose, at every boot, which is what makes it worth
// a pattern.

// redactSecrets is the BeforeSend hook. It strips setup tokens from the places
// a log line can put one: the message, the tags logrusHook builds from a log
// entry's structured fields, exception values, and breadcrumbs.
//
// It is a backstop and is documented as one. What keeps the setup token out of
// Sentry is that logSetupToken writes at Warn and the logrus hook fires on
// Error and above -- see logrusHook.Levels. This exists for the change that
// raises a level, or the error path that interpolates a token into a message,
// neither of which would fail to compile.
func redactSecrets(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	if event == nil {
		return nil
	}

	event.Message = setup.Redact(event.Message)

	// Tags too: logrusHook copies entry.Data into them, so a token passed as a
	// structured field rather than interpolated into the message lands here.
	for k, v := range event.Tags {
		event.Tags[k] = setup.Redact(v)
	}

	for i := range event.Exception {
		event.Exception[i].Value = setup.Redact(event.Exception[i].Value)
	}

	for i := range event.Breadcrumbs {
		if event.Breadcrumbs[i] == nil {
			continue
		}
		event.Breadcrumbs[i].Message = setup.Redact(event.Breadcrumbs[i].Message)
	}

	return event
}
