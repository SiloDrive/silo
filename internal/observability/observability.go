// Package observability reports the Silo server's failures to a
// Sentry-compatible endpoint — sentry.io, GlitchTip, or a self-hosted Splat.
//
// Three things go out: every error and fatal the server logs, every panic the
// recover sites catch, and a sampled share of HTTP requests as performance
// transactions. All of it is off unless a DSN is configured, and Init is a
// no-op without one, so nothing else in the tree has to test for it.
//
// Only the server reports. The TUI and the CLI subcommands run on someone
// else's machine and fail in front of the person who ran them, so there is
// nobody for a report to tell anything they cannot already see.
package observability

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	sentry "github.com/getsentry/sentry-go"
	sentryhttp "github.com/getsentry/sentry-go/http"
	log "github.com/sirupsen/logrus"
)

const (
	// flushTimeout bounds how long shutdown waits for queued events. The
	// transport is asynchronous, so without a flush the last event before
	// exit — usually the one worth having — dies with the process.
	flushTimeout = 5 * time.Second

	// defaultTracesSampleRate keeps a tenth of requests as performance
	// transactions. A sync client polls every few seconds and a single large
	// upload is thousands of chunk PUTs, so tracing everything would bury the
	// receiving end under data that says the same thing ten thousand times.
	defaultTracesSampleRate = 0.1

	// reportedField marks a logrus entry whose failure has already been sent
	// by hand, so the hook does not file it a second time.
	reportedField = "sentry_reported"

	// unwrapDepth caps how far SetException follows an error chain. Silo wraps
	// errors a few levels deep at most; the cap is only here so a cyclic or
	// pathological chain cannot turn one log line into an unbounded payload.
	unwrapDepth = 10
)

// enabled is written once by Init, before the server starts serving, and only
// read afterwards.
var enabled bool

// Enabled reports whether failures are being sent anywhere.
func Enabled() bool { return enabled }

// Init wires the process up to Sentry and returns a function that flushes
// whatever is still queued. With no DSN configured it does nothing and returns
// a no-op, so callers can defer the result unconditionally.
//
// Call it as early as the process can manage: it is the window between Init
// and the first successful request that produces the reports worth having, and
// a startup failure logged before Init is a startup failure nobody hears
// about.
func Init(component, version string) func() {
	dsn := firstEnv("SILO_SENTRY_DSN", "SENTRY_DSN")
	if dsn == "" {
		return func() {}
	}

	rate := tracesSampleRate()
	rel := release(version)
	environment := firstEnv("SILO_SENTRY_ENVIRONMENT", "SENTRY_ENVIRONMENT")
	if environment == "" {
		environment = "production"
	}

	if err := sentry.Init(clientOptions(dsn, environment, rel, rate)); err != nil {
		// A bad DSN must not stop the server: reporting is a convenience, and
		// refusing to start because the error tracker is misconfigured would
		// make the monitoring a bigger outage than anything it monitors.
		log.Warnf("Sentry disabled: %v", err)
		return func() {}
	}
	enabled = true

	sentry.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetTag("component", component)
	})
	log.AddHook(&logrusHook{})
	// log.Fatal calls os.Exit, which runs no deferred function; logrus's exit
	// handlers are the only place left from which the fatal itself can be
	// flushed.
	log.RegisterExitHandler(Flush)

	log.Infof("Sentry reporting to %s (environment %s, release %s, traces sample rate %g)",
		endpointOf(dsn), environment, rel, rate)
	return Flush
}

// Flush blocks until queued events have been sent, or flushTimeout passes.
func Flush() {
	if !enabled {
		return
	}
	if !sentry.Flush(flushTimeout) {
		log.Warnf("Sentry: gave up after %s with events still unsent", flushTimeout)
	}
}

// Panic reports a recovered panic and logs it.
//
// Call it from the deferred function that recovered, while the panicking
// frames are still on the stack — that is what gives Sentry a real stack
// trace instead of a string that looks like one. what names the site, so two
// unrelated goroutines panicking do not land in the same issue.
func Panic(ctx context.Context, what string, recovered any) {
	// Logged with the marker so the hook does not report the same panic a
	// second time as a plain error.
	log.WithField(reportedField, true).Errorf("%s panic: %v\n%s", what, recovered, debug.Stack())
	if !enabled {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	hub := hubFor(ctx)
	hub.WithScope(func(scope *sentry.Scope) {
		scope.SetTag("panic_site", what)
		// Normalised on both halves: a site is sometimes "notif client 12",
		// and grouping on that would file a separate issue per connection for
		// what is one bug.
		scope.SetFingerprint([]string{"panic", normalizeMessage(what), normalizeMessage(fmt.Sprint(recovered))})
		hub.RecoverWithContext(ctx, recovered)
	})
}

// Middleware wraps h so that each request gets its own hub, a panic is
// reported before net/http swallows it, and a sampled share of requests is
// timed as a performance transaction.
//
// It returns h untouched when reporting is off, so the disabled path costs
// nothing per request. The wrapper preserves http.Flusher, http.Hijacker and
// io.ReaderFrom, which streaming downloads and the WebSocket upgrade on
// /notification both depend on.
func Middleware(h http.Handler) http.Handler {
	if !enabled {
		return h
	}
	return sentryhttp.New(sentryhttp.Options{
		// Repanic so net/http still sees the panic and still tears the
		// connection down. Sentry is here to watch what the server does, not
		// to change it.
		Repanic: true,
	}).Handle(recordResponseStatus(h))
}

// recordResponseStatus copies the response status onto the transaction as the
// standard response context.
//
// The Go SDK records the status only as span data under contexts.trace, but
// the response context at contexts.response.status_code is where the protocol
// puts it and where a receiver looks — Splat reads exactly that, and without
// it every transaction arrives with a blank status, so nothing can tell a page
// of 200s from a page of 500s.
//
// It runs inside the Sentry handler, so its deferred write lands before that
// handler finishes the transaction. The status comes from the SDK's own
// ResponseWriter wrapper rather than another one of ours: that wrapper already
// implements Flusher, Hijacker and ReaderFrom, and adding a layer that missed
// ReaderFrom would quietly cost every file download its sendfile path.
func recordResponseStatus(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			status, ok := w.(interface{ Status() int })
			if !ok {
				return
			}
			if tx := sentry.TransactionFromContext(r.Context()); tx != nil {
				tx.SetContext("response", sentry.Context{"status_code": status.Status()})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// RecordRequestBytes puts one request's wire sizes on its transaction.
//
// Span data rather than tags. Tags are indexed strings meant for grouping, and
// a byte count is neither -- every request would be its own tag value, which
// is the cardinality problem NameTransaction exists to fix, reintroduced one
// field lower down.
//
// These are per-request sizes and must never be summed into a throughput
// figure. Traces are sampled -- a tenth by default -- so a total assembled
// from them is a tenth of the truth and looks entirely plausible, which is the
// worst way for a number to be wrong. The server's own counters in
// fileserver/traffic are the unsampled total; this is for asking which
// endpoint moves the fattest bodies and whether a slow request was slow
// because of its size.
//
// A no-op when Sentry is off or the request was not sampled, which is the
// common case and costs a nil check.
func RecordRequestBytes(r *http.Request, in, out int64) {
	if !enabled {
		return
	}
	tx := sentry.TransactionFromContext(r.Context())
	if tx == nil {
		return
	}
	tx.SetData("http.request_content_length", in)
	tx.SetData("http.response_content_length", out)

	// And a bucket as a tag, because span data is not enough on its own: the
	// Go SDK files it under contexts.trace.data, and a receiver reading the
	// transaction does not show it -- the same surprise, in the same place,
	// that recordResponseStatus exists to work around for the status code.
	// A tag is what can be seen and filtered on.
	//
	// Bucketed rather than exact, which is the whole reason a tag is safe
	// here. Tags are indexed strings meant for grouping; an exact byte count
	// would give every request its own value and reintroduce, one field lower
	// down, the cardinality problem NameTransaction exists to fix. Six buckets
	// answer "show me the slow requests carrying big bodies", which is the
	// question worth asking, and no more.
	tx.SetTag("body_size", sizeBucket(max(in, out)))
}

// sizeBucket names a size in a way that stays a small closed set.
//
// The boundaries follow what the format already treats as thresholds rather
// than round numbers for their own sake: 64 KiB is store.InlineThreshold, the
// point below which a file travels inside its manifest, and the megabyte
// steps above it are chunk-sized and file-sized. A bucket that lined up with
// nothing would sort requests into groups that mean nothing.
func sizeBucket(n int64) string {
	switch {
	case n == 0:
		return "empty"
	case n < 4<<10:
		return "under-4kb"
	case n < 64<<10:
		return "4kb-64kb"
	case n < 1<<20:
		return "64kb-1mb"
	case n < 16<<20:
		return "1mb-16mb"
	}
	return "over-16mb"
}

// NameTransaction replaces the URL-derived name of the request's transaction
// with a route template.
//
// Without it every library id and file path in the sync API becomes its own
// entry in the performance data: thousands of one-request "endpoints", no
// endpoint with enough samples to have a p95, and nothing to rank. It is a
// no-op when the request is not being traced.
func NameTransaction(r *http.Request, route string) {
	if !enabled || route == "" {
		return
	}
	tx := sentry.TransactionFromContext(r.Context())
	if tx == nil {
		return
	}
	tx.Name = r.Method + " " + route
	tx.Source = sentry.SourceRoute
}

// hubFor returns the hub scoped to ctx — carrying the request, its user and
// its transaction — falling back to the process-wide one for the background
// goroutines that have no request behind them.
func hubFor(ctx context.Context) *sentry.Hub {
	if ctx != nil {
		if hub := sentry.GetHubFromContext(ctx); hub != nil {
			return hub
		}
	}
	return sentry.CurrentHub()
}

// tracesSampler decides which requests are worth timing.
// clientOptions is the one description of how this process talks to Sentry.
//
// It is shared with SelfTest rather than copied there, because a self-test that
// exercised a different configuration from the server would answer a question
// nobody asked — and a comment promising the two stay in step cannot enforce
// it, while a shared function can. SelfTest overrides only the fields it needs
// to name.
func clientOptions(dsn, environment, release string, rate float64) sentry.ClientOptions {
	return sentry.ClientOptions{
		Dsn:              dsn,
		Environment:      environment,
		Release:          release,
		ServerName:       serverName(),
		AttachStacktrace: true,
		// A Silo request carries the account behind it in a bearer token and
		// the library behind it in the path, and the whole point of sending
		// any of this is to be able to say whose sync broke.
		//
		// Bodies are a different matter: they are file contents. This used to
		// say they were safe because the SDK never reads them, which was true
		// of an older sentry-go and is not true of 0.48 -- SendDefaultPII with
		// no DataCollection resolves to HTTPBodies: allBodyTypes()
		// (legacyDataCollection in the SDK), and the scope tees up to 10 KiB of
		// every body a handler reads. The SDK's key filter caught "password"
		// and "token" by name; it does not know that a library name or a path
		// is somebody's data too.
		//
		// So the collection is stated here rather than inherited. For the
		// reader who wants the one-line answer: PII is on -- the account
		// behind a request, not the bodies it carries. SendDefaultPII used to
		// be that line and is gone rather than kept as a summary: a non-nil
		// DataCollection supersedes it entirely, so it governed nothing while
		// reading as though it did, and the SDK now deprecates it outright.
		DataCollection: &sentry.DataCollection{
			// Not filtered -- not collected. There is no denylist that knows
			// which of a body's keys are file contents.
			HTTPBodies: []sentry.BodyType{},
			// Kept: naming whose sync broke is the point. The SDK's own
			// denylist redacts Authorization, which is the only sensitive
			// header Silo reads.
			HTTPHeaders: &sentry.HeaderCollectionConfig{},
			QueryParams: &sentry.KeyValueCollectionBehavior{},
			// Silo authenticates with bearer tokens and sets no cookies, so
			// anything here arrived from a proxy and is not ours to forward.
			Cookies:  &sentry.KeyValueCollectionBehavior{Mode: sentry.CollectionOff},
			UserInfo: sentry.Set(true),
		},
		// Last line rather than the only one. The setup token is the single
		// secret in this tree whose format a pattern can match without false
		// positives, and it is printed to the log -- at Warn, below this hook's
		// level, which is what actually keeps it out. This catches the day
		// somebody raises that level without noticing what it was holding back.
		BeforeSend:    redactSecrets,
		EnableTracing: rate > 0,
		TracesSampler: tracesSampler(rate),
		// sentry-go 0.48's telemetry buffer can lose an event that Flush has
		// already reported as sent — measurably, around one in two hundred,
		// and more than that when the flush follows the capture closely. The
		// flush that follows a capture closely is the one after log.Fatal, on
		// the way out of the process, which is exactly the event worth having.
		// The older transport path has no such race in the same test, so take
		// it: nothing here batches logs or metrics, which is what the buffer
		// exists to do.
		DisableTelemetryBuffer: true,
		Debug:                  envBool("SILO_SENTRY_DEBUG"),
	}
}

func tracesSampler(rate float64) sentry.TracesSampler {
	return func(ctx sentry.SamplingContext) float64 {
		if ctx.Span != nil && untraced(ctx.Span.Name) {
			return 0
		}
		return rate
	}
}

// untracedPaths are endpoints whose timings are noise or worse. /notification
// is a WebSocket held open for as long as the client runs, so its "request"
// would be a transaction hours long that finishes only when someone closes a
// laptop; the pprof handlers are diagnostics already being watched by whoever
// asked for them, and /protocol-version is a constant.
//
// 404s need no entry here: the SDK drops transactions that answered 404 by
// default, which is what keeps a port scanner from inventing an endpoint per
// probe.
var untracedPaths = []string{"/notification", "/debug/pprof", "/protocol-version"}

// untraced reports whether a span name — "GET /libraries/…", as sentryhttp builds
// it from the method and the path — names one of those endpoints.
func untraced(spanName string) bool {
	path := spanName
	if _, rest, ok := strings.Cut(spanName, " "); ok {
		path = rest
	}
	for _, p := range untracedPaths {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

// logrusHook turns the errors the server already logs into Sentry events.
// Silo reports failures by logging them — there is no second error channel to
// instrument — so hooking logrus is what makes the ~100 existing log.Error
// sites visible without touching any of them.
type logrusHook struct{}

func (*logrusHook) Levels() []log.Level {
	return []log.Level{log.PanicLevel, log.FatalLevel, log.ErrorLevel}
}

func (*logrusHook) Fire(entry *log.Entry) error {
	if reported, _ := entry.Data[reportedField].(bool); reported {
		return nil
	}

	event := sentry.NewEvent()
	event.Level = sentryLevel(entry.Level)
	event.Message = entry.Message
	event.Timestamp = entry.Time
	// Structured fields go in a context of their own rather than as tags: the
	// SDK dropped free-form "extra" in favour of contexts, and tags are for
	// the low-cardinality things you filter on, not for whatever a caller
	// happened to attach.
	for k, v := range entry.Data {
		if k == log.ErrorKey || k == reportedField {
			continue
		}
		if event.Contexts["logrus"] == nil {
			event.Contexts["logrus"] = make(sentry.Context, len(entry.Data))
		}
		event.Contexts["logrus"][k] = v
	}
	if err, ok := entry.Data[log.ErrorKey].(error); ok {
		event.SetException(err, unwrapDepth)
	}

	// Trimmed so the innermost frame is the code that actually logged, rather
	// than the hook and logrus frames that carried the message here.
	stack := trimPlumbing(sentry.NewStacktrace())
	if len(event.Exception) == 0 && stack != nil {
		event.Threads = []sentry.Thread{{Stacktrace: stack, Current: true}}
	}
	if site := callSite(stack); site != "" {
		// Group by the statement that logged and the shape of what it said,
		// not by the text itself. Nearly every message here interpolates a
		// library id, a path or an error string, and a receiver that groups on
		// raw text — Splat hashes the whole message — would file one issue
		// per file and bury the fact that a single line is failing over and
		// over.
		event.Fingerprint = []string{site, normalizeMessage(entry.Message)}
		event.Tags["log_site"] = site
	}

	hubFor(entry.Context).CaptureEvent(event)
	return nil
}

func sentryLevel(level log.Level) sentry.Level {
	switch level {
	case log.PanicLevel:
		return sentry.LevelFatal
	case log.FatalLevel:
		return sentry.LevelFatal
	default:
		return sentry.LevelError
	}
}

// trimPlumbing drops the hook and logrus frames from the innermost end of a
// stack, so what is left ends at the statement that logged. Sentry orders
// frames oldest-first, which puts the plumbing last.
func trimPlumbing(st *sentry.Stacktrace) *sentry.Stacktrace {
	if st == nil {
		return nil
	}
	for len(st.Frames) > 0 && isPlumbing(st.Frames[len(st.Frames)-1].Module) {
		st.Frames = st.Frames[:len(st.Frames)-1]
	}
	if len(st.Frames) == 0 {
		return nil
	}
	return st
}

func isPlumbing(module string) bool {
	return module == "github.com/sirupsen/logrus" ||
		module == "github.com/SiloDrive/silo/internal/observability"
}

// callSite names the innermost frame as package.Function, which survives the
// edits that move a line around. A file:line fingerprint would file a fresh
// issue every time an unrelated change shifted the statement down the file.
func callSite(st *sentry.Stacktrace) string {
	if st == nil || len(st.Frames) == 0 {
		return ""
	}
	f := st.Frames[len(st.Frames)-1]
	if f.Module == "" {
		return f.Function
	}
	return f.Module + "." + f.Function
}

// Volatile parts of a log message, in the order they have to be replaced:
// quoted strings first, since they can contain any of the rest, and UUIDs
// before bare hex, since a UUID's segments would otherwise be eaten piecemeal.
var (
	reQuoted = regexp.MustCompile(`"[^"]*"`)
	reEmail  = regexp.MustCompile(`[\w.+-]+@[\w-]+\.[\w.-]+`)
	reUUID   = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	reHex    = regexp.MustCompile(`\b[0-9a-fA-F]{8,}\b`)
	reNum    = regexp.MustCompile(`\b\d+\b`)
)

// normalizeMessage strips the varying parts out of a log message so that the
// same failing statement groups as one issue however many libraries, chunks or
// accounts it fires for.
func normalizeMessage(msg string) string {
	msg = reQuoted.ReplaceAllString(msg, `"<str>"`)
	msg = reEmail.ReplaceAllString(msg, "<email>")
	msg = reUUID.ReplaceAllString(msg, "<id>")
	msg = reHex.ReplaceAllString(msg, "<hash>")
	msg = reNum.ReplaceAllString(msg, "<n>")
	return msg
}

// release identifies the build an event came from, so a receiver that tracks
// releases can pin a regression to a version. Sentry's convention is
// name@version.
func release(version string) string {
	if v := firstEnv("SILO_SENTRY_RELEASE", "SENTRY_RELEASE"); v != "" {
		return v
	}
	if version == "" {
		return ""
	}
	return "silo@" + version
}

// serverName is what distinguishes two Silo instances reporting to the same
// project. The SDK falls back to the hostname on its own, so an empty value
// here is fine.
func serverName() string {
	return os.Getenv("SILO_SENTRY_SERVER_NAME")
}

func tracesSampleRate() float64 {
	v := os.Getenv("SILO_SENTRY_TRACES_SAMPLE_RATE")
	if v == "" {
		return defaultTracesSampleRate
	}
	rate, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || rate < 0 || rate > 1 {
		log.Warnf("Ignoring SILO_SENTRY_TRACES_SAMPLE_RATE=%q: want a number from 0 to 1. Using %g.",
			v, defaultTracesSampleRate)
		return defaultTracesSampleRate
	}
	return rate
}

// endpointOf renders a DSN as just its origin and project, so the startup line
// can say where reports are going without printing the key that authenticates
// them into the log.
func endpointOf(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "the configured DSN"
	}
	return fmt.Sprintf("%s://%s%s", u.Scheme, u.Host, u.Path)
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}
