package netbird

import (
	"testing"
	"time"

	"github.com/futo-org/external-dns-netbird-webhook/internal/config"
	"github.com/futo-org/external-dns-netbird-webhook/internal/netbirdtest"
	"github.com/netbirdio/netbird/shared/management/http/api"
)

func newTestProvider(t *testing.T, zones ...api.Zone) (*Provider, *netbirdtest.Server) {
	t.Helper()

	nb := netbirdtest.NewServer(t, zones...)

	return NewProvider(testConfig(nb.URL)), nb
}

func testConfig(url string) config.NetBird {
	return config.NetBird{
		APIURL:            url,
		APIToken:          netbirdtest.Token,
		ApplyWorkers:      4,
		RetryAttempts:     3,
		RetryInitialDelay: time.Millisecond,
		RetryMaxDelay:     5 * time.Millisecond,
		ApplyTimeout:      5 * time.Second,
	}
}
