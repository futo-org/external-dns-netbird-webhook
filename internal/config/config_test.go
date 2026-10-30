package config

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("NETBIRD_API_TOKEN", "nbp_test")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.NetBird.APIURL != "https://api.netbird.io" || cfg.NetBird.APIToken != "nbp_test" {
		t.Errorf("NetBird = %+v", cfg.NetBird)
	}
	if cfg.NetBird.ApplyWorkers != 5 || cfg.NetBird.RetryAttempts != 3 || cfg.NetBird.RequestTimeout != 0 ||
		cfg.NetBird.ApplyTimeout != 12*time.Second {
		t.Errorf("NetBird defaults = %+v", cfg.NetBird)
	}
	if cfg.ServerHost != "localhost" || cfg.ServerPort != 8888 || cfg.HealthServerAddr != ":8080" {
		t.Errorf("server defaults = %+v", cfg)
	}
	if _, set := os.LookupEnv("NETBIRD_API_TOKEN"); set {
		t.Error("NETBIRD_API_TOKEN should be unset from the environment after loading")
	}
}

func TestLoadZones(t *testing.T) {
	t.Setenv("NETBIRD_API_TOKEN", "nbp_test")
	t.Setenv("NETBIRD_ZONES", "example.com,internal.test")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := []string{"example.com", "internal.test"}; !slices.Equal(cfg.NetBird.Zones, want) {
		t.Errorf("Zones = %q, want %q", cfg.NetBird.Zones, want)
	}
}

func TestLoadRequiresToken(t *testing.T) {
	t.Setenv("NETBIRD_API_TOKEN", "")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "NETBIRD_API_TOKEN") {
		t.Fatalf("Load error = %v, want one naming NETBIRD_API_TOKEN", err)
	}
}

func TestValidate(t *testing.T) {
	valid := func() Config {
		return Config{
			ServerPort:           8888,
			ServerMaxHeaderBytes: 1,
			ServerMaxBodyBytes:   1,
			HealthServerAddr:     ":8080",
			NetBird: NetBird{
				APIURL:            "https://netbird.example.com",
				APIToken:          "nbp_test",
				ApplyWorkers:      5,
				RetryAttempts:     3,
				RetryInitialDelay: 500 * time.Millisecond,
				RetryMaxDelay:     10 * time.Second,
				ApplyTimeout:      12 * time.Second,
			},
		}
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"valid", func(*Config) {}, ""},
		{"port out of range", func(c *Config) { c.ServerPort = 70000 }, "SERVER_PORT"},
		{"negative timeout", func(c *Config) { c.NetBird.RequestTimeout = -time.Second }, "NETBIRD_REQUEST_TIMEOUT"},
		{"bad health addr", func(c *Config) { c.HealthServerAddr = "8080" }, "HEALTH_SERVER_ADDR"},
		{"url without scheme", func(c *Config) { c.NetBird.APIURL = "netbird.example.com" }, "NETBIRD_API_URL"},
		{"url with other scheme", func(c *Config) { c.NetBird.APIURL = "ftp://netbird.example.com" }, "NETBIRD_API_URL"},
		{"zero workers", func(c *Config) { c.NetBird.ApplyWorkers = 0 }, "NETBIRD_APPLY_WORKERS"},
		{"too many workers", func(c *Config) { c.NetBird.ApplyWorkers = 500 }, "NETBIRD_APPLY_WORKERS"},
		{"zero attempts", func(c *Config) { c.NetBird.RetryAttempts = 0 }, "NETBIRD_RETRY_ATTEMPTS"},
		{"tiny initial delay", func(c *Config) { c.NetBird.RetryInitialDelay = time.Nanosecond }, "NETBIRD_RETRY_INITIAL_DELAY"},
		{"max below initial", func(c *Config) { c.NetBird.RetryMaxDelay = time.Millisecond }, "NETBIRD_RETRY_MAX_DELAY"},
		{"zero apply timeout", func(c *Config) { c.NetBird.ApplyTimeout = 0 }, "NETBIRD_APPLY_TIMEOUT"},
		{"empty zone entry", func(c *Config) { c.NetBird.Zones = []string{"example.com", " "} }, "NETBIRD_ZONES"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid()
			tt.mutate(&cfg)

			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}

				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate error = %v, want one naming %s", err, tt.wantErr)
			}
		})
	}
}
