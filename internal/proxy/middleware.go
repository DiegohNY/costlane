package proxy

import (
	"net/http"
	"time"

	"github.com/google/uuid"
)

// responseWrapper records what a handler wrote while staying transparent to
// http.ResponseController.
//
// Unwrap is the load-bearing method. Without it the controller cannot reach
// the Flusher underneath, every chunk is buffered, and the stream arrives in
// one piece — a failure that is invisible to any test which only inspects the
// final body. Every wrapper in this package implements it for that reason.
type responseWrapper struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *responseWrapper) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWrapper) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}

// Unwrap exposes the underlying writer, so http.ResponseController can find
// its Flusher and its deadline setters.
func (w *responseWrapper) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// WithRequestID attaches an identifier to every response.
func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if w.Header().Get(HeaderRequestID) == "" {
			w.Header().Set(HeaderRequestID, uuid.NewString())
		}
		next.ServeHTTP(&responseWrapper{ResponseWriter: w}, r)
	})
}

// RequestMetrics receives the outcome of a request.
type RequestMetrics interface {
	HTTPRequest(method, path string, status int, d time.Duration)
}

// WithMetrics records status and duration.
func WithMetrics(next http.Handler, metrics RequestMetrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		wrapped := &responseWrapper{ResponseWriter: w}
		next.ServeHTTP(wrapped, r)
		if metrics != nil {
			metrics.HTTPRequest(r.Method, r.Pattern, wrapped.status, time.Since(started))
		}
	})
}
