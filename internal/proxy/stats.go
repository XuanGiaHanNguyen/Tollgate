package proxy

import (
	"net/http"
	"sync"
	"time"
)

// reqStats accumulates per-request timings used to compute gateway overhead.
// It travels in the request context; only the request's own goroutines
// (ServeHTTP, then RoundTrip, then body copy) touch it, one after another.
type reqStats struct {
	upstream  time.Duration // time inside upstream RoundTrip calls, all attempts
	streaming time.Duration // time reading the response body from upstream
}

type statsKey struct{}

var statsPool = sync.Pool{New: func() any { return new(reqStats) }}

func statsFrom(r *http.Request) *reqStats {
	st, _ := r.Context().Value(statsKey{}).(*reqStats)
	return st
}

// statusWriter records the response status. Unwrap lets
// http.ResponseController (used by ReverseProxy to flush) reach the real writer.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 && code >= 200 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
