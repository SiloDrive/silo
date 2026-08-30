package middleware

import (
	"bufio"
	"io"
	"net"
	"net/http"

	"github.com/dkam/silo/fileserver/traffic"
	"github.com/dkam/silo/internal/observability"
)

// CountTraffic records the wire bytes of every request and response.
//
// Its own middleware rather than a few lines inside DebugLogger, which already
// wraps the ResponseWriter and would have been the obvious place. DebugLogger
// is only installed when debug logging is on (see server.go), so a counter
// hung off it reads zero on every install that is not being debugged -- which
// is every install an operator would ask this question about.
//
// It wraps rather than instruments the handlers, so a route added later is
// counted without anybody remembering to count it. The cost is that this sees
// bytes rather than chunks: a request refused at the credential check still
// moved its body across the socket, and that is the honest thing for a
// throughput figure to include.
func CountTraffic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lane := traffic.LaneFor(r.URL.Path)

		body := &countingBody{ReadCloser: r.Body}
		if r.Body != nil {
			r.Body = body
		}
		rec := &countingWriter{ResponseWriter: w}

		// Deferred so that a handler that panics still has its bytes counted:
		// observability.Middleware sits outside this and will recover, and a
		// panic in a large upload is exactly the transfer worth having a
		// record of.
		defer func() {
			traffic.Default.Record(lane, body.n, rec.n)
			// The same two numbers, for a different question. The counters are
			// the unsampled total and the rate; this puts the per-request size
			// on the trace, where it can be ranked and correlated but must
			// never be summed. See observability.RecordRequestBytes.
			observability.RecordRequestBytes(r, body.n, rec.n)
		}()

		next.ServeHTTP(rec, r)
	})
}

// countingBody totals what was read off the request.
//
// Read rather than Content-Length, because a client can declare a length it
// does not send, and a chunked upload declares none at all.
type countingBody struct {
	io.ReadCloser
	n int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n += int64(n)
	return n, err
}

// countingWriter totals what was written to the response.
//
// Flush and Hijack pass through for the reason statusRecorder's do, and here
// it is load-bearing rather than tidy: /notification is a WebSocket, so a
// wrapper that swallowed Hijack would not slow the notification lane down, it
// would break it. After a Hijack the connection is the handler's and its bytes
// stop being visible here, which is correct -- what a WebSocket carries is not
// a request body and counting it as one would put a long-lived idle socket
// into a transfer rate.
type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)
	return n, err
}

func (w *countingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *countingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}
