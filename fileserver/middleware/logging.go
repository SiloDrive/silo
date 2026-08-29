package middleware

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"
)

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	return r.ResponseWriter.Write(b)
}

// Flush / Hijack passthroughs so wrapping the writer doesn't break streaming
// downloads, SSE, or websocket-style upgrades. The fileserver streams large
// file bodies and uses chunked transfers; dropping these interfaces would
// buffer entire responses or break Hijack-based paths.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := r.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func DebugLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// Capture the original path before any downstream middleware
		// mutates r.URL.
		path := r.URL.Path
		if r.URL.RawQuery != "" {
			path = path + "?" + r.URL.RawQuery
		}

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		msg := fmt.Sprintf("HTTP %s %s -> %d (%s) from %s",
			r.Method, path, rec.status, time.Since(start), r.RemoteAddr)

		if rec.status == http.StatusNotFound {
			log.Warn(msg)
		} else {
			log.Info(msg)
		}
	})
}
