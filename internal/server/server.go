package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"strconv"
	"time"

	"github.com/futo-org/external-dns-netbird-webhook/internal/config"
	"github.com/futo-org/external-dns-netbird-webhook/internal/metrics"
	"github.com/futo-org/external-dns-netbird-webhook/internal/webhook"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"
)

const shutdownTimeout = 30 * time.Second

// Run serves the webhook and health servers until ctx is done or either
// server fails. probe backs /readyz.
func Run(ctx context.Context, cfg *config.Config, whk *webhook.Webhook, probe ProbeFunc) error {
	// Both ports share this handler, so that probes on each port use the same
	// cache.
	readyz := readyzHandler(newCachedProbe(probe, cfg.ReadinessCacheTTL))

	mainSrv := newServer(net.JoinHostPort(cfg.ServerHost, strconv.Itoa(cfg.ServerPort)), mainHandler(cfg, whk, readyz), cfg)
	healthSrv := newServer(cfg.HealthServerAddr, healthHandler(cfg, readyz), cfg)

	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error { return serve(ctx, mainSrv, "webhook") })
	group.Go(func() error { return serve(ctx, healthSrv, "health") })

	return group.Wait()
}

func mainHandler(cfg *config.Config, whk *webhook.Webhook, readyz http.HandlerFunc) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", whk.Negotiate)
	mux.HandleFunc("GET /records", whk.Records)
	mux.HandleFunc("POST /records", whk.ApplyChanges)
	mux.HandleFunc("POST /adjustendpoints", whk.AdjustEndpoints)
	// The external-dns chart exposes one port for the webhook sidecar, so the
	// probes must also be on this port.
	mountHealth(mux, readyz)

	// Recovery is the outermost layer, so that it catches panics from every
	// layer.
	return recoveryAndAccessLog(metrics.HTTPMetricsMiddleware(limitBody(cfg.ServerMaxBodyBytes, mux)))
}

func healthHandler(cfg *config.Config, readyz http.HandlerFunc) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.Handler())
	mountHealth(mux, readyz)

	if cfg.PprofEnabled {
		slog.Warn("pprof endpoints enabled on the health server; disable in production unless investigating")
		mux.HandleFunc("GET /debug/pprof/", pprof.Index)
		mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	}

	return recoveryAndAccessLog(mux)
}

func mountHealth(mux *http.ServeMux, readyz http.HandlerFunc) {
	mux.HandleFunc("GET /healthz", writeOK)
	mux.HandleFunc("GET /readyz", readyz)
}

func writeOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if _, err := w.Write([]byte("OK")); err != nil {
		slog.Error("writing health response", "error", err)
	}
}

func readyzHandler(probe *cachedProbe) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := probe.Check(r.Context()); err != nil {
			// The health port has no authentication. As a result, the probe
			// logs the cause, which names the upstream, and does not return it.
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			if _, err := w.Write([]byte("not ready")); err != nil {
				slog.Error("writing readiness response", "error", err)
			}

			return
		}
		writeOK(w, r)
	}
}

func newServer(addr string, handler http.Handler, cfg *config.Config) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadTimeout:       cfg.ServerReadTimeout,
		ReadHeaderTimeout: cfg.ServerReadHeaderTimeout,
		WriteTimeout:      cfg.ServerWriteTimeout,
		IdleTimeout:       cfg.ServerIdleTimeout,
		MaxHeaderBytes:    cfg.ServerMaxHeaderBytes,
	}
}

// serve runs srv until ctx is canceled. Then it shuts srv down and lets the
// open requests finish.
func serve(ctx context.Context, srv *http.Server, name string) error {
	errCh := make(chan error, 1)
	go func() {
		slog.Info("starting server", "name", name, "address", srv.Addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}

		return fmt.Errorf("server: %s: %w", name, err)
	case <-ctx.Done():
		slog.Info("shutting down server", "name", name)
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("server: shutting down %s: %w", name, err)
		}

		return nil
	}
}
