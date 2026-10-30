package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

const (
	maxPort = 65535

	// These limits stop a typo such as NETBIRD_APPLY_WORKERS=500 from sending
	// too many writes at the same time. Each write makes the management server
	// compute and send network maps to the peers of the zone.
	maxApplyWorkers  = 20
	maxRetryAttempts = 10
	// The retry jitter comes from [0, delay/2). As a result, the delay needs a
	// minimum that keeps delay/2 positive.
	minRetryInitialDelay = time.Millisecond
)

// Config holds the configuration from the process environment.
//
// Config has no domain filter on purpose. The provider ignores names outside
// the NetBird zones that it can see, and external-dns applies its own
// --domain-filter on top.
type Config struct {
	ServerHost              string        `env:"SERVER_HOST"                envDefault:"localhost"`
	ServerPort              int           `env:"SERVER_PORT"                envDefault:"8888"`
	ServerReadTimeout       time.Duration `env:"SERVER_READ_TIMEOUT"        envDefault:"60s"`
	ServerReadHeaderTimeout time.Duration `env:"SERVER_READ_HEADER_TIMEOUT" envDefault:"5s"`
	ServerWriteTimeout      time.Duration `env:"SERVER_WRITE_TIMEOUT"       envDefault:"60s"`
	ServerIdleTimeout       time.Duration `env:"SERVER_IDLE_TIMEOUT"        envDefault:"120s"`
	ServerMaxHeaderBytes    int           `env:"SERVER_MAX_HEADER_BYTES"    envDefault:"65536"`
	ServerMaxBodyBytes      int64         `env:"SERVER_MAX_BODY_BYTES"      envDefault:"5242880"`
	HealthServerAddr        string        `env:"HEALTH_SERVER_ADDR"         envDefault:":8080"`
	ReadinessCacheTTL       time.Duration `env:"READINESS_CACHE_TTL"        envDefault:"30s"`
	PprofEnabled            bool          `env:"PPROF_ENABLED"              envDefault:"false"`

	NetBird NetBird
}

// NetBird configures the NetBird management API client.
type NetBird struct {
	// APIURL is the base URL of the management server, without the /api suffix.
	APIURL string `env:"NETBIRD_API_URL" envDefault:"https://api.netbird.io"`
	// APIToken is a personal access token. After env reads the token, it
	// removes the token from the environment. As a result, the token does not
	// leak into child processes or crash dumps.
	APIToken          string        `env:"NETBIRD_API_TOKEN,notEmpty,unset"`
	ApplyWorkers      int           `env:"NETBIRD_APPLY_WORKERS"       envDefault:"5"`
	RetryAttempts     int           `env:"NETBIRD_RETRY_ATTEMPTS"      envDefault:"3"`
	RetryInitialDelay time.Duration `env:"NETBIRD_RETRY_INITIAL_DELAY" envDefault:"500ms"`
	RetryMaxDelay     time.Duration `env:"NETBIRD_RETRY_MAX_DELAY"     envDefault:"10s"`
	// RequestTimeout limits one attempt, which includes the read of the
	// response body. If it is zero, only the context of the caller limits an
	// attempt.
	RequestTimeout time.Duration `env:"NETBIRD_REQUEST_TIMEOUT" envDefault:"0"`
	// Zones lists the domains of the zones to manage. If the most specific
	// zone of a name is not in the list, the provider ignores the name and its
	// records. If Zones is empty, the provider manages every zone that the
	// token can see.
	Zones []string `env:"NETBIRD_ZONES"`
	// ApplyTimeout limits one batch of changes. If a webhook call takes longer
	// than --webhook-provider-read-timeout plus --webhook-provider-write-timeout
	// (15s by default), external-dns stops. But it retries a 500. As a result,
	// this timeout must stay below that sum.
	ApplyTimeout time.Duration `env:"NETBIRD_APPLY_TIMEOUT" envDefault:"12s"`
}

// Load reads Config from the environment and makes sure that it is valid. As
// a result, a bad configuration stops the startup before a server listens.
func Load() (Config, error) {
	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return Config{}, fmt.Errorf("config: reading environment: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}

	return cfg, nil
}

// Validate makes sure that the configuration is consistent.
func (c Config) Validate() error {
	if c.ServerPort < 1 || c.ServerPort > maxPort {
		return fmt.Errorf("SERVER_PORT must be between 1 and %d, got %d", maxPort, c.ServerPort)
	}
	if c.ServerMaxBodyBytes <= 0 {
		return fmt.Errorf("SERVER_MAX_BODY_BYTES must be positive, got %d", c.ServerMaxBodyBytes)
	}
	if c.ServerMaxHeaderBytes <= 0 {
		return fmt.Errorf("SERVER_MAX_HEADER_BYTES must be positive, got %d", c.ServerMaxHeaderBytes)
	}

	durations := []struct {
		name  string
		value time.Duration
	}{
		{"SERVER_READ_TIMEOUT", c.ServerReadTimeout},
		{"SERVER_READ_HEADER_TIMEOUT", c.ServerReadHeaderTimeout},
		{"SERVER_WRITE_TIMEOUT", c.ServerWriteTimeout},
		{"SERVER_IDLE_TIMEOUT", c.ServerIdleTimeout},
		{"READINESS_CACHE_TTL", c.ReadinessCacheTTL},
		{"NETBIRD_REQUEST_TIMEOUT", c.NetBird.RequestTimeout},
	}
	for _, d := range durations {
		if d.value < 0 {
			return fmt.Errorf("%s must not be negative, got %s", d.name, d.value)
		}
	}

	if _, _, err := net.SplitHostPort(c.HealthServerAddr); err != nil {
		return fmt.Errorf("HEALTH_SERVER_ADDR %q is not a valid host:port: %w", c.HealthServerAddr, err)
	}

	return c.NetBird.validate()
}

func (n NetBird) validate() error {
	u, err := url.Parse(n.APIURL)
	if err != nil {
		return fmt.Errorf("NETBIRD_API_URL %q is not a valid URL: %w", n.APIURL, err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("NETBIRD_API_URL %q must be an http(s) URL with a host (e.g. https://netbird.example.com)",
			n.APIURL)
	}
	if n.ApplyWorkers < 1 || n.ApplyWorkers > maxApplyWorkers {
		return fmt.Errorf("NETBIRD_APPLY_WORKERS must be between 1 and %d, got %d", maxApplyWorkers, n.ApplyWorkers)
	}
	if n.RetryAttempts < 1 || n.RetryAttempts > maxRetryAttempts {
		return fmt.Errorf("NETBIRD_RETRY_ATTEMPTS must be between 1 and %d, got %d", maxRetryAttempts, n.RetryAttempts)
	}
	if n.RetryInitialDelay < minRetryInitialDelay {
		return fmt.Errorf("NETBIRD_RETRY_INITIAL_DELAY must be at least %s, got %s",
			minRetryInitialDelay, n.RetryInitialDelay)
	}
	for _, zone := range n.Zones {
		if strings.TrimSpace(zone) == "" {
			return fmt.Errorf("NETBIRD_ZONES has an empty entry: %q", strings.Join(n.Zones, ","))
		}
	}
	if n.ApplyTimeout <= 0 {
		return fmt.Errorf("NETBIRD_APPLY_TIMEOUT must be positive, got %s", n.ApplyTimeout)
	}
	if n.RetryMaxDelay < n.RetryInitialDelay {
		return fmt.Errorf("NETBIRD_RETRY_MAX_DELAY (%s) must be at least NETBIRD_RETRY_INITIAL_DELAY (%s)",
			n.RetryMaxDelay, n.RetryInitialDelay)
	}

	return nil
}
