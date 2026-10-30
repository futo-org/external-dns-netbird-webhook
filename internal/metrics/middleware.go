package metrics

import (
	"net/http"
	"strings"
	"time"

	"github.com/futo-org/external-dns-netbird-webhook/internal/httpx"
)

// RouteLabel turns a request into a metric label from a fixed set: the path of
// the matched route, or "other" for a request that matched no route. It reads
// r.Pattern, which http.ServeMux sets only during routing. Call it after
// next.ServeHTTP.
func RouteLabel(r *http.Request) string {
	// r.Pattern is "[METHOD ][HOST]/path". The path starts at the first slash.
	if i := strings.IndexByte(r.Pattern, '/'); i >= 0 {
		return r.Pattern[i:]
	}

	return "other"
}

// methodLabel turns a request method into a metric label from a fixed set.
// net/http accepts any token as a method. Without this, each new method from a
// client adds a label value.
func methodLabel(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
		return method
	}

	return "other"
}

// HTTPMetricsMiddleware records request count, duration, in-flight count and
// response size for every wrapped HTTP handler.
func HTTPMetricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := Get()

		m.HTTPRequestsInFlight.WithLabelValues(ProviderName).Inc()
		defer m.HTTPRequestsInFlight.WithLabelValues(ProviderName).Dec()

		rec := httpx.NewResponseRecorder(w)
		start := time.Now()

		next.ServeHTTP(rec, r)

		m.RecordHTTPRequest(methodLabel(r.Method), RouteLabel(r), rec.Status(), time.Since(start), rec.Written())
	})
}
