// Package httpx holds small HTTP helpers shared across the webhook's
// middleware layers.
package httpx

import "net/http"

// ResponseRecorder wraps an http.ResponseWriter and records the status code
// and the number of body bytes that the handler writes. The status starts as
// http.StatusOK. If a handler calls Write without WriteHeader, net/http
// writes that status itself. Like net/http, the recorder keeps the first
// final status and ignores a later WriteHeader.
type ResponseRecorder struct {
	http.ResponseWriter

	status    int
	written   int
	committed bool
}

// NewResponseRecorder wraps w with the status pre-set to http.StatusOK.
func NewResponseRecorder(w http.ResponseWriter) *ResponseRecorder {
	return &ResponseRecorder{ResponseWriter: w, status: http.StatusOK}
}

func (r *ResponseRecorder) WriteHeader(code int) {
	// A 1xx status other than 101 is informational: the final status follows.
	informational := code >= 100 && code <= 199 && code != http.StatusSwitchingProtocols
	if !r.committed && !informational {
		r.status = code
		r.committed = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *ResponseRecorder) Write(b []byte) (int, error) {
	r.committed = true
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
