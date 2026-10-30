package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/futo-org/external-dns-netbird-webhook/internal/config"
	"github.com/futo-org/external-dns-netbird-webhook/internal/metrics"
	"github.com/futo-org/external-dns-netbird-webhook/internal/webhook"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
)

func TestCachedProbe(t *testing.T) {
	var calls atomic.Int32
	probeErr := errors.New("upstream down")
	probe := newCachedProbe(func(context.Context) error {
		calls.Add(1)

		return probeErr
	}, time.Hour)

	for range 3 {
		if err := probe.Check(t.Context()); !errors.Is(err, probeErr) {
			t.Fatalf("Check = %v, want %v", err, probeErr)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("probe calls = %d, want 1 within the TTL", got)
	}

	probe.ttl = 0
	_ = probe.Check(t.Context())
	if got := calls.Load(); got != 2 {
		t.Errorf("probe calls = %d, want 2 once the TTL lapsed", got)
	}
}

func TestCachedProbeIgnoresCallerCancellation(t *testing.T) {
	probe := newCachedProbe(func(ctx context.Context) error { return ctx.Err() }, time.Hour)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := probe.Check(ctx); err != nil {
		t.Fatalf("Check with a cancelled caller = %v, want nil", err)
	}
}

func TestReadyz(t *testing.T) {
	tests := []struct {
		name     string
		probeErr error
		want     int
		wantBody string
	}{
		{"ready", nil, http.StatusOK, "OK"},
		{"not ready hides the cause", errors.New("netbird.internal:443 refused"), http.StatusServiceUnavailable, "not ready"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := readyzHandler(newCachedProbe(func(context.Context) error { return tt.probeErr }, time.Hour))
			rec := httptest.NewRecorder()
			handler(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			if rec.Code != tt.want || rec.Body.String() != tt.wantBody {
				t.Errorf("readyz = %d %q, want %d %q", rec.Code, rec.Body, tt.want, tt.wantBody)
			}
		})
	}
}

type panicProvider struct{}

func (panicProvider) Records(context.Context) ([]*endpoint.Endpoint, error) { panic("boom") }
func (panicProvider) ApplyChanges(context.Context, *plan.Changes) error     { return nil }
func (panicProvider) AdjustEndpoints(e []*endpoint.Endpoint) ([]*endpoint.Endpoint, error) {
	return e, nil
}
func (panicProvider) GetDomainFilter() endpoint.DomainFilterInterface {
	return &endpoint.DomainFilter{}
}

func TestMainHandler(t *testing.T) {
	cfg := &config.Config{ServerMaxBodyBytes: 16}
	readyz := readyzHandler(newCachedProbe(func(context.Context) error { return nil }, time.Hour))
	srv := httptest.NewServer(mainHandler(cfg, webhook.New(panicProvider{}), readyz))
	t.Cleanup(srv.Close)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
		want   int
	}{
		{"negotiate", http.MethodGet, "/", "", http.StatusOK},
		{"healthz", http.MethodGet, "/healthz", "", http.StatusOK},
		{"readyz", http.MethodGet, "/readyz", "", http.StatusOK},
		{"panic becomes 500", http.MethodGet, "/records", "", http.StatusInternalServerError},
		{"body limit", http.MethodPost, "/records", `{"create":[{"dnsName":"www.example.com"}]}`, http.StatusRequestEntityTooLarge},
		{"unknown route", http.MethodGet, "/nope", "", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, srv.URL+tt.path, strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Accept", "application/external.dns.webhook+json;version=1")
			req.Header.Set("Content-Type", "application/external.dns.webhook+json;version=1")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tt.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}
}

func TestMainHandlerMetrics(t *testing.T) {
	cfg := &config.Config{ServerMaxBodyBytes: 16}
	readyz := readyzHandler(newCachedProbe(func(context.Context) error { return nil }, time.Hour))
	srv := httptest.NewServer(mainHandler(cfg, webhook.New(panicProvider{}), readyz))
	t.Cleanup(srv.Close)

	tests := []struct {
		name   string
		method string
		path   string
		labels []string
	}{
		{"panic counts as 500", http.MethodGet, "/records", []string{http.MethodGet, "/records", "500"}},
		{"unknown method", "FOO", "/records", []string{"other", "other", "405"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			counter := metrics.Get().HTTPRequestsTotal.WithLabelValues(append([]string{metrics.ProviderName}, tt.labels...)...)
			before := testutil.ToFloat64(counter)

			req, err := http.NewRequestWithContext(t.Context(), tt.method, srv.URL+tt.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Accept", "application/external.dns.webhook+json;version=1")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()

			if got := testutil.ToFloat64(counter) - before; got != 1 {
				t.Errorf("requests counted with labels %v = %v, want 1", tt.labels, got)
			}
		})
	}
}
