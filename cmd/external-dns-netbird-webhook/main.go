package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/futo-org/external-dns-netbird-webhook/internal/config"
	"github.com/futo-org/external-dns-netbird-webhook/internal/metrics"
	"github.com/futo-org/external-dns-netbird-webhook/internal/netbird"
	"github.com/futo-org/external-dns-netbird-webhook/internal/server"
	"github.com/futo-org/external-dns-netbird-webhook/internal/webhook"
)

var (
	Version = "local"
	Gitsha  = "?"
)

func main() {
	initLogger()
	slog.Info("starting external-dns-netbird-webhook", "version", Version, "gitsha", Gitsha)

	metrics.New(Version)

	cfg, err := config.Load()
	if err != nil {
		slog.Error("loading configuration", "error", err)
		os.Exit(1)
	}
	slog.Info("using NetBird management API", "url", cfg.NetBird.APIURL)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		// After the first signal, restore the default signal handling. Then a
		// second signal kills a shutdown that is stuck.
		<-ctx.Done()
		stop()
	}()

	provider := netbird.NewProvider(cfg.NetBird)
	probe := func(ctx context.Context) error {
		_, err := provider.Records(ctx)

		return err
	}

	if err := server.Run(ctx, &cfg, webhook.New(provider), probe); err != nil {
		slog.Error("running server", "error", err)
		os.Exit(1)
	}
}

func initLogger() {
	var level slog.Level
	if err := level.UnmarshalText([]byte(os.Getenv("LOG_LEVEL"))); err != nil {
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{AddSource: level <= slog.LevelDebug, Level: level}

	var handler slog.Handler = slog.NewJSONHandler(os.Stdout, opts)
	if os.Getenv("LOG_FORMAT") == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	slog.SetDefault(slog.New(handler))
}
