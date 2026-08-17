package observability_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dkam/silo/internal/observability"
	log "github.com/sirupsen/logrus"
)

// A flush that returns must mean the events are gone, every time.
//
// This is not paranoia about the network: sentry-go 0.48's telemetry buffer
// loses events that Flush has already reported as delivered, at a rate of
// roughly one in two hundred, and worse when the flush follows the capture
// closely — which is precisely the shape of the flush after log.Fatal, on the
// way out of the process. Init turns that buffer off; this is what says so.
// Run enough times to catch it if the option is ever dropped.
func TestFlushLosesNothing(t *testing.T) {
	const rounds = 200

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	savedHooks := log.StandardLogger().Hooks
	restoredHooks := make(log.LevelHooks, len(savedHooks))
	for level, hooks := range savedHooks {
		restoredHooks[level] = hooks
	}
	savedOut := log.StandardLogger().Out
	log.SetOutput(discard{}) // 200 error lines are not worth reading

	t.Setenv("SILO_SENTRY_DSN", strings.Replace(srv.URL, "://", "://k@", 1)+"/1")
	flush := observability.Init("test", "0.0.0")
	t.Cleanup(func() {
		flush()
		log.SetOutput(savedOut)
		log.StandardLogger().ReplaceHooks(restoredHooks)
		observability.ResetForTest()
	})

	for i := range rounds {
		log.Errorf("round %d failed", i)
		observability.Flush()
		if got := hits.Load(); got != int64(i+1) {
			t.Fatalf("after %d flushed errors the receiver had seen %d", i+1, got)
		}
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
