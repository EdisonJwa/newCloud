package server

// Request instrumentation (§11.1).
//
// The requirement names specific signals — request error rate, DAV conflict rate, upload
// failure rate — and this middleware is what produces the first of them. It wraps the
// response writer to see the status code the handler actually wrote, which the router
// cannot report afterwards.

import (
	"net/http"
	"strings"
)

// statusRecorder captures the status code and whether the response was a WebDAV one.
//
// Flush and Unwrap are forwarded deliberately: chunked downloads and SSE-style responses
// need the underlying Flusher, and http.ResponseController (used by the content handler)
// reaches through Unwrap to find it. A recorder that swallowed those would turn every
// large download into a buffered one.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.written {
		r.status = code
		r.written = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.written {
		// A handler that writes without calling WriteHeader implies 200.
		r.status = http.StatusOK
		r.written = true
	}
	return r.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer when it supports flushing.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the wrapped writer to http.ResponseController.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// observeRequest records one request's outcome.
//
// It is a no-op when metrics are not configured, so the middleware chain costs one nil
// check per request in tests and development.
func (s *Server) observeRequest(next http.Handler) http.Handler {
	if s.deps.Metrics == nil {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// WebDAV is identified by path prefix rather than by method: PROPFIND on /api
		// would be a bug, but the metric should follow where the client actually went.
		isDAV := strings.HasPrefix(r.URL.Path, "/dav/")

		// A 1xx status is not a final outcome; recording it would double-count. The
		// recorder only keeps the first WriteHeader, which is the final one in practice
		// because Go does not allow a second.
		status := rec.status
		if status < 200 {
			status = http.StatusOK
		}
		s.deps.Metrics.ObserveRequest(r.Method, status, isDAV)
	})
}
