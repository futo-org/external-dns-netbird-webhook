// Package httpx holds small HTTP helpers shared across the webhook's
// middleware layers.
package httpx

import "net/http"

// ResponseRecorder wraps an http.ResponseWriter and records the status code
// and the number of body bytes that the handler writes. The status starts as
// http.StatusOK. If a handler calls Write without WriteHeader, net/http
// writes that status itself.
type ResponseRecorder struct {
	http.ResponseWriter

	status  int
	written int
}

// NewResponseRecorder wraps w with the status pre-set to http.StatusOK.
func NewResponseRecorder(w http.ResponseWriter) *ResponseRecorder {
	return &ResponseRecorder{ResponseWriter: w, status: http.StatusOK}
}

func (r *ResponseRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *ResponseRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.written += n

	return n, err
}

// Unwrap lets http.NewResponseController reach the underlying writer, which
// net/http/pprof uses to extend the write deadline for long profiles.
func (r *ResponseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Status returns the captured status code.
func (r *ResponseRecorder) Status() int { return r.status }

// Written returns the number of body bytes written to the client.
func (r *ResponseRecorder) Written() int { return r.written }
