package observability_test

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dkam/silo/internal/observability"
	log "github.com/sirupsen/logrus"
)

// envelope is one decoded Sentry envelope: the header line, then alternating
// item-header / item-payload lines.
type envelope struct {
	header  map[string]any
	items   []map[string]any
	payload []map[string]any
}

// sentryStub stands in for Splat: it accepts envelopes on the ingestion
// endpoint and decodes enough of them to assert on.
type sentryStub struct {
	*httptest.Server

	mu        sync.Mutex
	received  []envelope
	authSeen  []string
	pathsSeen []string
}

func newSentryStub(t *testing.T) *sentryStub {
	t.Helper()
	s := &sentryStub{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := decodeBody(r)
		if err != nil {
			t.Errorf("stub could not read envelope: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		env, err := parseEnvelope(body)
		if err != nil {
			t.Errorf("stub could not parse envelope: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.received = append(s.received, env)
		s.authSeen = append(s.authSeen, r.Header.Get("X-Sentry-Auth"))
		s.pathsSeen = append(s.pathsSeen, r.URL.Path)
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s.Close)
	return s
}

// dsn points a client at the stub as project 1, the way Splat's "Copy External
// DSN" button would.
func (s *sentryStub) dsn() string {
	return strings.Replace(s.URL, "://", "://testpublickey@", 1) + "/1"
}

func (s *sentryStub) envelopes() []envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]envelope(nil), s.received...)
}

func decodeBody(r *http.Request) ([]byte, error) {
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, err
		}
		defer func() { _ = zr.Close() }()
		return io.ReadAll(zr)
	}
	return io.ReadAll(r.Body)
}

func parseEnvelope(body []byte) (envelope, error) {
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	var env envelope
	if err := json.Unmarshal([]byte(lines[0]), &env.header); err != nil {
		return env, err
	}
	for i := 1; i+1 < len(lines); i += 2 {
		var itemHeader, payload map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &itemHeader); err != nil {
			return env, err
		}
		if err := json.Unmarshal([]byte(lines[i+1]), &payload); err != nil {
			return env, err
		}
		env.items = append(env.items, itemHeader)
		env.payload = append(env.payload, payload)
	}
	return env, nil
}

// enable points the package at the stub for the duration of one test and puts
// the process back the way it was afterwards. sentry.Init and logrus's hooks
// are both process-wide, so leaving either of them set would have this test
// reporting the next one's log lines.
func enable(t *testing.T, s *sentryStub) {
	t.Helper()
	savedHooks := log.StandardLogger().Hooks
	restoredHooks := make(log.LevelHooks, len(savedHooks))
	for level, hooks := range savedHooks {
		restoredHooks[level] = hooks
	}

	t.Setenv("SILO_SENTRY_DSN", s.dsn())
	t.Setenv("SILO_SENTRY_ENVIRONMENT", "test")
	flush := observability.Init("test", "9.9.9")

	t.Cleanup(func() {
		flush()
		log.StandardLogger().ReplaceHooks(restoredHooks)
		observability.ResetForTest()
	})
	if !observability.Enabled() {
		t.Fatal("Init did not enable reporting despite a DSN")
	}
}

// The whole point of the exercise: an error the server logs turns up at the
// far end as an event, addressed and authenticated the way the receiver
// expects.
func TestLoggedErrorIsDelivered(t *testing.T) {
	stub := newSentryStub(t)
	enable(t, stub)

	log.Errorf("failed to read block %s", "a94a8fe5ccb19ba61c4c0873d391e987982fbbd3")
	observability.Flush()

	envs := stub.envelopes()
	if len(envs) != 1 {
		t.Fatalf("stub received %d envelopes, want 1", len(envs))
	}
	env := envs[0]

	if got, want := stub.pathsSeen[0], "/api/1/envelope/"; got != want {
		t.Errorf("posted to %q, want %q", got, want)
	}
	if auth := stub.authSeen[0]; !strings.Contains(auth, "sentry_key=testpublickey") {
		t.Errorf("X-Sentry-Auth = %q, want it to carry the public key", auth)
	}
	if got, want := env.items[0]["type"], "event"; got != want {
		t.Errorf("item type = %v, want %v", got, want)
	}

	payload := env.payload[0]
	if got, want := payload["message"], "failed to read block a94a8fe5ccb19ba61c4c0873d391e987982fbbd3"; got != want {
		t.Errorf("message = %v, want %v", got, want)
	}
	if got, want := payload["level"], "error"; got != want {
		t.Errorf("level = %v, want %v", got, want)
	}
	if got, want := payload["environment"], "test"; got != want {
		t.Errorf("environment = %v, want %v", got, want)
	}
	if got, want := payload["release"], "silo@9.9.9"; got != want {
		t.Errorf("release = %v, want %v", got, want)
	}

	fingerprint, _ := payload["fingerprint"].([]any)
	if len(fingerprint) != 2 {
		t.Fatalf("fingerprint = %v, want a call site and a normalised message", payload["fingerprint"])
	}
	if got, want := fingerprint[1], "failed to read block <hash>"; got != want {
		t.Errorf("fingerprint message = %v, want %v", got, want)
	}
	// The site must be the test, not the hook that forwarded the entry.
	if site, _ := fingerprint[0].(string); !strings.Contains(site, "TestLoggedErrorIsDelivered") {
		t.Errorf("fingerprint site = %v, want the logging function", fingerprint[0])
	}
}

// Two failures of one statement are one issue; a different statement is a
// different issue. That is what makes the receiver's issue list readable.
func TestFingerprintGroupsBySite(t *testing.T) {
	stub := newSentryStub(t)
	enable(t, stub)

	log.Errorf("failed to read block %s", "a94a8fe5ccb19ba61c4c0873d391e987982fbbd3")
	log.Errorf("failed to read block %s", "da39a3ee5e6b4b0d3255bfef95601890afd80709")
	log.Errorf("quota exceeded for %s", "someone@example.com")
	observability.Flush()

	prints := map[string]int{}
	for _, env := range stub.envelopes() {
		fp, _ := env.payload[0]["fingerprint"].([]any)
		key := ""
		for _, part := range fp {
			key += part.(string) + "::"
		}
		prints[key]++
	}
	if len(prints) != 2 {
		t.Errorf("got %d distinct fingerprints, want 2: %v", len(prints), prints)
	}
	for key, n := range prints {
		if strings.Contains(key, "read block") && n != 2 {
			t.Errorf("the two block failures produced %d fingerprints, want them grouped", n)
		}
	}
}

// Warnings are the server's normal noise — a 404 from a sync client logs one.
// Sending them would make the issue list useless.
func TestWarningsAreNotSent(t *testing.T) {
	stub := newSentryStub(t)
	enable(t, stub)

	log.Warn("HTTP GET /nope -> 404")
	log.Info("Silo server listening")
	observability.Flush()

	if envs := stub.envelopes(); len(envs) != 0 {
		t.Errorf("stub received %d envelopes for non-error logs, want 0", len(envs))
	}
}

// Panic reports the panic itself, once — not once as a panic and again as the
// error line it also writes to the log.
func TestPanicIsReportedOnce(t *testing.T) {
	stub := newSentryStub(t)
	enable(t, stub)

	func() {
		defer func() {
			if r := recover(); r != nil {
				observability.Panic(t.Context(), "testSite", r)
			}
		}()
		panic("something came apart")
	}()
	observability.Flush()

	// One envelope, not two: the log line Panic also writes carries a marker
	// that keeps the logrus hook from filing it a second time.
	envs := stub.envelopes()
	if len(envs) != 1 {
		t.Fatalf("stub received %d envelopes for one panic, want 1", len(envs))
	}
	payload := envs[0].payload[0]
	if got, want := payload["message"], "something came apart"; got != want {
		t.Errorf("message = %v, want %v", got, want)
	}
	if got, want := payload["level"], "fatal"; got != want {
		t.Errorf("level = %v, want %v", got, want)
	}
	// The stack has to reach back to the function that panicked, which is what
	// calling Panic from inside the deferred recover buys — the frames are
	// still live there.
	if !strings.Contains(strings.Join(stackFunctions(payload), " "), "TestPanicIsReportedOnce") {
		t.Errorf("stack does not reach the panicking function: %v", stackFunctions(payload))
	}
	if tags, _ := payload["tags"].(map[string]any); tags["panic_site"] != "testSite" {
		t.Errorf("panic_site tag = %v, want testSite", tags["panic_site"])
	}
	fp, _ := payload["fingerprint"].([]any)
	if len(fp) != 3 || fp[0] != "panic" {
		t.Errorf("fingerprint = %v, want a panic fingerprint", payload["fingerprint"])
	}
}

// A site named after the thing that panicked — "notif client 12" — must still
// group as one issue however many clients hit it.
func TestPanicSitesGroupAcrossInstances(t *testing.T) {
	stub := newSentryStub(t)
	enable(t, stub)

	for _, id := range []int{12, 4098} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					observability.Panic(t.Context(), fmt.Sprintf("notif client %d", id), r)
				}
			}()
			panic("connection went away")
		}()
	}
	observability.Flush()

	prints := map[string]bool{}
	for _, env := range stub.envelopes() {
		fp, _ := env.payload[0]["fingerprint"].([]any)
		prints[fmt.Sprint(fp...)] = true
	}
	if len(prints) != 1 {
		t.Errorf("two clients panicking produced %d fingerprints, want 1: %v", len(prints), prints)
	}
}

// A panic carrying an error — the common shape when a library re-panics a
// wrapped failure — arrives as an exception, so the receiver can group it by
// type and show it as one.
func TestPanicWithErrorBecomesException(t *testing.T) {
	stub := newSentryStub(t)
	enable(t, stub)

	func() {
		defer func() {
			if r := recover(); r != nil {
				observability.Panic(t.Context(), "testSite", r)
			}
		}()
		panic(errors.New("nil block store"))
	}()
	observability.Flush()

	envs := stub.envelopes()
	if len(envs) != 1 {
		t.Fatalf("stub received %d envelopes, want 1", len(envs))
	}
	exceptions, _ := envs[0].payload[0]["exception"].([]any)
	if len(exceptions) == 0 {
		t.Fatalf("panic with an error arrived without an exception: %v", envs[0].payload[0])
	}
	first, _ := exceptions[0].(map[string]any)
	if got, want := first["value"], "nil block store"; got != want {
		t.Errorf("exception value = %v, want %v", got, want)
	}
}

// Every credential Silo accepts is a bearer token in a header or a query
// parameter, and an error tracker holding one is a credential leak. The SDK
// scrubs on a substring deny list that happens to cover all of them today;
// this pins that down, because the day it stops covering Seafile-Repo-Token is
// the day sync tokens start arriving in the issue list.
func TestCredentialsAreScrubbedFromReports(t *testing.T) {
	stub := newSentryStub(t)
	enable(t, stub)

	handler := observability.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	req := httptest.NewRequest(http.MethodPost, "/repo/abc/block/def?token=querysecret&op=upload",
		strings.NewReader("the contents of somebody's file"))
	req.Header.Set("Authorization", "Bearer jwtsecret")
	req.Header.Set("Seafile-Repo-Token", "synctokensecret")
	req.Header.Set("Cookie", "sessionid=cookiesecret")
	req.Header.Set("User-Agent", "Seafile/9.0.0")

	func() {
		defer func() { _ = recover() }()
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}()
	observability.Flush()

	var event map[string]any
	for _, env := range stub.envelopes() {
		if env.items[0]["type"] == "event" {
			event = env.payload[0]
		}
	}
	if event == nil {
		t.Fatal("no event captured")
	}
	blob, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"jwtsecret", "synctokensecret", "cookiesecret", "querysecret",
		"the contents of somebody's file", // bodies are never read at all
	} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("event carries %q:\n%s", secret, blob)
		}
	}

	// Scrubbing must not have thrown out the context that makes the report
	// useful: the route, the method and the harmless headers stay.
	request, _ := event["request"].(map[string]any)
	if request == nil {
		t.Fatal("event carries no request at all")
	}
	if !strings.Contains(request["url"].(string), "/repo/abc/block/def") {
		t.Errorf("request url = %v, want the path", request["url"])
	}
	headers, _ := request["headers"].(map[string]any)
	if headers["User-Agent"] != "Seafile/9.0.0" {
		t.Errorf("User-Agent = %v, want it kept", headers["User-Agent"])
	}
}

// stackFunctions lists the function names of whichever stack an event carries
// — the exception's when there is one, the current thread's otherwise.
func stackFunctions(payload map[string]any) []string {
	var frames []any
	if exceptions, _ := payload["exception"].([]any); len(exceptions) > 0 {
		first, _ := exceptions[0].(map[string]any)
		st, _ := first["stacktrace"].(map[string]any)
		frames, _ = st["frames"].([]any)
	}
	if len(frames) == 0 {
		if threads, _ := payload["threads"].([]any); len(threads) > 0 {
			first, _ := threads[0].(map[string]any)
			st, _ := first["stacktrace"].(map[string]any)
			frames, _ = st["frames"].([]any)
		}
	}
	var names []string
	for _, f := range frames {
		frame, _ := f.(map[string]any)
		name, _ := frame["function"].(string)
		names = append(names, name)
	}
	return names
}

// A request through the middleware is timed and named after its route, not
// after the library id in its URL.
func TestRequestBecomesNamedTransaction(t *testing.T) {
	stub := newSentryStub(t)
	t.Setenv("SILO_SENTRY_TRACES_SAMPLE_RATE", "1")
	enable(t, stub)

	handler := observability.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observability.NameTransaction(r, "/repo/{repoid}/block/{id}")
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet,
		"/repo/7c8a1f2e-3b4d-4c5a-9e6f-0a1b2c3d4e5f/block/a94a8fe5ccb19ba61c4c0873d391e987982fbbd3", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	observability.Flush()

	envs := stub.envelopes()
	if len(envs) != 1 {
		t.Fatalf("stub received %d envelopes for one request, want 1", len(envs))
	}
	if got, want := envs[0].items[0]["type"], "transaction"; got != want {
		t.Fatalf("item type = %v, want %v", got, want)
	}
	payload := envs[0].payload[0]
	if got, want := payload["transaction"], "GET /repo/{repoid}/block/{id}"; got != want {
		t.Errorf("transaction = %v, want %v", got, want)
	}
	if payload["start_timestamp"] == nil || payload["timestamp"] == nil {
		t.Errorf("transaction has no timing: %v", payload)
	}

	// contexts.response.status_code is where the protocol puts the status and
	// where a receiver reads it; the Go SDK records it only as span data, so
	// this is ours to set. Without it every transaction lands with a blank
	// status and a page of 500s looks like a page of 200s.
	contexts, _ := payload["contexts"].(map[string]any)
	response, _ := contexts["response"].(map[string]any)
	if response == nil {
		t.Fatalf("transaction carries no response context: %v", contexts)
	}
	if got, want := response["status_code"], float64(http.StatusOK); got != want {
		t.Errorf("contexts.response.status_code = %v, want %v", got, want)
	}
}

// The status has to be the one the handler actually wrote, not the default.
func TestTransactionCarriesRealStatus(t *testing.T) {
	stub := newSentryStub(t)
	t.Setenv("SILO_SENTRY_TRACES_SAMPLE_RATE", "1")
	enable(t, stub)

	handler := observability.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/repo/abc/block/def", nil))
	observability.Flush()

	envs := stub.envelopes()
	if len(envs) != 1 {
		t.Fatalf("stub received %d envelopes, want 1", len(envs))
	}
	contexts, _ := envs[0].payload[0]["contexts"].(map[string]any)
	response, _ := contexts["response"].(map[string]any)
	if response == nil {
		t.Fatalf("transaction carries no response context: %v", contexts)
	}
	if got, want := response["status_code"], float64(http.StatusInternalServerError); got != want {
		t.Errorf("contexts.response.status_code = %v, want %v", got, want)
	}
}

// The endpoints excluded from tracing stay excluded — /notification above all,
// since it is a WebSocket that would otherwise report a transaction lasting as
// long as the client is running.
func TestUntracedEndpointsProduceNoTransaction(t *testing.T) {
	stub := newSentryStub(t)
	t.Setenv("SILO_SENTRY_TRACES_SAMPLE_RATE", "1")
	enable(t, stub)

	handler := observability.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for _, path := range []string{"/notification", "/seafhttp/notification", "/debug/pprof/heap"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	observability.Flush()

	if envs := stub.envelopes(); len(envs) != 0 {
		t.Errorf("stub received %d envelopes for untraced endpoints, want 0", len(envs))
	}
}

// A handler that panics is reported with the request attached, and the panic
// still propagates so net/http tears the connection down as it always did.
func TestHandlerPanicIsReportedAndRepanics(t *testing.T) {
	stub := newSentryStub(t)
	enable(t, stub)

	handler := observability.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("handler came apart")
	}))

	repanicked := false
	func() {
		defer func() { repanicked = recover() != nil }()
		handler.ServeHTTP(httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/repo/abc/block/def", nil))
	}()
	if !repanicked {
		t.Error("Middleware swallowed the panic; net/http never saw it")
	}

	// The transaction for the panicking request may also arrive; find the event.
	waitFor(t, func() bool { return len(stub.envelopes()) > 0 })
	observability.Flush()
	var event map[string]any
	for _, env := range stub.envelopes() {
		if env.items[0]["type"] == "event" {
			event = env.payload[0]
		}
	}
	if event == nil {
		t.Fatal("handler panic produced no event")
	}
	request, _ := event["request"].(map[string]any)
	if request == nil || !strings.Contains(request["url"].(string), "/repo/abc/block/def") {
		t.Errorf("event does not carry the request: %v", event["request"])
	}
}

func waitFor(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
