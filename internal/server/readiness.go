package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// ProbeFunc reports whether the upstream is usable. A non-nil error fails
// /readyz.
type ProbeFunc func(ctx context.Context) error

// probeTimeout limits one upstream probe. On purpose, it is shorter than the
// worst-case retry time of the client. As a result, an upstream that limits
// the rate or is down shows as not ready, and the probe does not wait through
// its backoff.
const probeTimeout = 10 * time.Second

// cachedProbe keeps the result of a ProbeFunc for ttl. Probes that run at the
// same time share one upstream call.
type cachedProbe struct {
	probe ProbeFunc
	ttl   time.Duration
	group singleflight.Group

	mu      sync.RWMutex
	checked time.Time
	err     error
}

func newCachedProbe(probe ProbeFunc, ttl time.Duration) *cachedProbe {
	return &cachedProbe{probe: probe, ttl: ttl}
}

func (c *cachedProbe) cached() (bool, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return !c.checked.IsZero() && time.Since(c.checked) < c.ttl, c.err
}

// Check returns the cached result while it is fresh. If not, it runs the probe.
func (c *cachedProbe) Check(ctx context.Context) error {
	if fresh, err := c.cached(); fresh {
		return err
	}

	_, err, _ := c.group.Do("probe", func() (any, error) {
		if fresh, err := c.cached(); fresh {
			return nil, err
		}

		// Every caller in the queue gets this result, and the cache keeps it
		// for the TTL. As a result, a cancellation of the first caller, such as
		// a dropped /readyz connection, must not become the result for all.
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), probeTimeout)
		err := c.probe(pctx)
		cancel()

		// /readyz answers with a generic body, so the cause shows only here.
		if err != nil {
			slog.Warn("readiness probe failed", "error", err)
		}

		c.mu.Lock()
		c.checked = time.Now()
		c.err = err
		c.mu.Unlock()

		return nil, err
	})

	return err
}
