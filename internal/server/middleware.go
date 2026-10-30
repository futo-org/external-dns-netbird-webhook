package server

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/futo-org/external-dns-netbird-webhook/internal/httpx"
	"github.com/futo-org/external-dns-netbird-webhook/internal/metrics"
)

// recovery turns a panic in a handler into a 500 and logs it. It must run
// inside HTTPMetricsMiddleware, so that the metrics count the 500.
func recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rv := recover(); rv != nil {
				metrics.Get().PanicsTotal.WithLabelValues(metrics.ProviderName, metrics.RouteLabel(r)).Inc()
				slog.Error("panic in HTTP handler",
					"panic", rv, "method", r.Method, "path", r.URL.Path, "stack", string(debug.Stack()))
				// If the handler already wrote a status, the client keeps it.
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()

		next.ServeHTTP(w, r)
	})
}

// accessLog writes one access log line for each request, at a level that
// matches the status class.
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := httpx.NewResponseRecorder(w)

		next.ServeHTTP(rec, r)

		status := rec.Status()
		level := slog.LevelInfo
		switch {
		case status >= http.StatusInternalServerError:
			level = slog.LevelError
		case status >= http.StatusBadRequest:
			level = slog.LevelWarn
		case isProbePath(r.URL.Path):
			level = slog.LevelDebug
		}

		slog.LogAttrs(r.Context(), level, "request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", status),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
		)
	})
}

// isProbePath reports whether a scraper calls path on a fixed schedule.
// Successful calls to such a path log at debug, so that they do not hide the
// real traffic.
func isProbePath(path string) bool {
	switch path {
	case "/healthz", "/readyz", "/metrics":
		return true
	}

	return false
}

// limitBody limits each request body to maxBytes. A read past the limit fails
// with *http.MaxBytesError.
func limitBody(maxBytes int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		next.ServeHTTP(w, r)
	})
}
