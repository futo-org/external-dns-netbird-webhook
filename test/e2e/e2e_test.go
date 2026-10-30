//go:build e2e

// Package e2e runs the built webhook binary against an in-memory NetBird
// management API. It drives the binary with the webhook client and the
// planner of external-dns, as the external-dns controller does with
// --policy=sync. The e2e build tag keeps the package out of `go test ./...`.
// To run it, use `mise run test-e2e`.
package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/futo-org/external-dns-netbird-webhook/internal/netbirdtest"
	"github.com/netbirdio/netbird/shared/management/http/api"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/pkg/apis/externaldns"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider"
	extwebhook "sigs.k8s.io/external-dns/provider/webhook"
	webhookapi "sigs.k8s.io/external-dns/provider/webhook/api"
	"sigs.k8s.io/external-dns/registry"
	"sigs.k8s.io/external-dns/registry/noop"
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "external-dns-netbird-webhook-e2e")
	if err != nil {
		fmt.Fprintln(os.Stderr, "creating build dir:", err)
		os.Exit(1)
	}

	binary = filepath.Join(dir, "external-dns-netbird-webhook")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/external-dns-netbird-webhook")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	code := 1
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building webhook:", err)
	} else {
		code = m.Run()
	}

	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// server is a running webhook binary.
type server struct {
	url       string
	healthURL string
}

// startServer runs the webhook against nb, with env on top of the defaults,
// and waits until it serves. When the test ends, startServer stops the
// webhook with SIGTERM. If the webhook does not shut down cleanly, the test
// fails.
func startServer(t *testing.T, nb *netbirdtest.Server, env ...string) server {
	t.Helper()

	ports := freePorts(t, 2)
	port, healthPort := ports[0], ports[1]
	s := server{
		url:       "http://127.0.0.1:" + strconv.Itoa(port),
		healthURL: "http://127.0.0.1:" + strconv.Itoa(healthPort),
	}

	var output bytes.Buffer
	cmd := exec.Command(binary)
	// Later entries win, so env overrides these defaults.
	cmd.Env = append([]string{
		"SERVER_HOST=127.0.0.1",
		"SERVER_PORT=" + strconv.Itoa(port),
		"HEALTH_SERVER_ADDR=127.0.0.1:" + strconv.Itoa(healthPort),
		"NETBIRD_API_URL=" + nb.URL,
		"NETBIRD_API_TOKEN=" + netbirdtest.Token,
		"NETBIRD_RETRY_INITIAL_DELAY=10ms",
		"LOG_FORMAT=text",
		"LOG_LEVEL=debug",
	}, env...)
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting webhook: %v", err)
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("signalling webhook: %v", err)
		}
		select {
		case err := <-exited:
			if err != nil {
				t.Errorf("webhook did not exit cleanly on SIGTERM: %v", err)
			}
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
			t.Error("webhook did not exit within 10s of SIGTERM")
		}
		if t.Failed() {
			t.Logf("webhook output:\n%s", output.String())
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		if resp, err := http.Get(s.healthURL + "/healthz"); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return s
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("webhook never became healthy")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// freePorts returns n different free ports. Every listener stays open until
// freePorts has all the ports, so that no port comes back twice.
func freePorts(t *testing.T, n int) []int {
	t.Helper()

	ports := make([]int, n)
	for i := range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("finding a free port: %v", err)
		}
		defer func() { _ = l.Close() }()
		ports[i] = l.Addr().(*net.TCPAddr).Port
	}

	return ports
}

// client negotiates with s through the webhook provider of external-dns.
func (s server) client(t *testing.T) provider.Provider {
	t.Helper()

	cfg := externaldns.NewConfig()
	cfg.WebhookProviderURL = s.url
	cfg.WebhookProviderReadTimeout = 5 * time.Second
	cfg.WebhookProviderWriteTimeout = 10 * time.Second
	p, err := extwebhook.New(t.Context(), cfg, nil)
	if err != nil {
		t.Fatalf("negotiating with the webhook: %v", err)
	}

	return p
}

// reconcile runs one external-dns sync through reg, as the controller of
// external-dns does with the sync policy. It returns the planned changes and
// the error from the apply.
func reconcile(t *testing.T, reg registry.Registry, desired ...*endpoint.Endpoint) (*plan.Changes, error) {
	t.Helper()

	current, err := reg.Records(t.Context())
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	candidates := make([]*endpoint.Endpoint, len(desired))
	for i, ep := range desired {
		candidates[i] = ep.DeepCopy()
	}
	adjusted, err := reg.AdjustEndpoints(candidates)
	if err != nil {
		t.Fatalf("AdjustEndpoints: %v", err)
	}

	changes := (&plan.Plan{
		Policies:       []plan.Policy{plan.Policies["sync"]},
		Current:        current,
		Desired:        adjusted,
		DomainFilter:   endpoint.MatchAllDomainFilters{&endpoint.DomainFilter{}, reg.GetDomainFilter()},
		ManagedRecords: []string{endpoint.RecordTypeA, endpoint.RecordTypeAAAA, endpoint.RecordTypeCNAME},
		OwnerID:        reg.OwnerID(),
	}).Calculate().Changes
	if !changes.HasChanges() {
		return changes, nil
	}

	return changes, reg.ApplyChanges(t.Context(), changes)
}

func noopRegistry(t *testing.T, p provider.Provider) registry.Registry {
	t.Helper()

	reg, err := noop.New(nil, p)
	if err != nil {
		t.Fatalf("creating the noop registry: %v", err)
	}

	return reg
}

// ownedRegistry stands in for the CRD registry of external-dns, which needs a
// Kubernetes API server. Like that registry, it reports as current only the
// endpoints that it applied. As a result, the planner never sees records that
// it did not create. An endpoint counts as applied only after the provider
// accepts its batch.
type ownedRegistry struct {
	provider.Provider

	owned map[string]*endpoint.Endpoint
}

func ownedKey(ep *endpoint.Endpoint) string { return ep.DNSName + "/" + ep.RecordType }

func (r *ownedRegistry) OwnerID() string { return "e2e" }

func (r *ownedRegistry) Records(context.Context) ([]*endpoint.Endpoint, error) {
	current := make([]*endpoint.Endpoint, 0, len(r.owned))
	for _, key := range slices.Sorted(maps.Keys(r.owned)) {
		current = append(current, r.owned[key].DeepCopy())
	}

	return current, nil
}

func (r *ownedRegistry) ApplyChanges(ctx context.Context, changes *plan.Changes) error {
	if err := r.Provider.ApplyChanges(ctx, changes); err != nil {
		// The CRD registry marks all creates and updates of the batch as
		// failed. None of them count as current until a later batch succeeds.
		for _, ep := range slices.Concat(changes.Create, changes.UpdateOld) {
			delete(r.owned, ownedKey(ep))
		}

		return err
	}

	for _, ep := range slices.Concat(changes.Delete, changes.UpdateOld) {
		delete(r.owned, ownedKey(ep))
	}
	for _, ep := range slices.Concat(changes.Create, changes.UpdateNew) {
		r.owned[ownedKey(ep)] = ep.DeepCopy().WithLabel(endpoint.OwnerLabelKey, r.OwnerID())
	}

	// After creates and updates, the CRD registry reads the provider back to
	// copy labels. A failed read fails the apply, but the records stay current.
	if len(changes.Create)+len(changes.UpdateNew) > 0 {
		if _, err := r.Provider.Records(ctx); err != nil {
			return fmt.Errorf("unable to get records from provider: %w", err)
		}
	}

	return nil
}

func writes(nb *netbirdtest.Server) int {
	return nb.Calls(http.MethodPost) + nb.Calls(http.MethodPut) + nb.Calls(http.MethodDelete)
}

// step is one sync in a scenario. The steps share the state of NetBird, so
// each step starts where the last step stopped.
type step struct {
	name    string
	before  func()
	desired []*endpoint.Endpoint
	want    []string // NetBird's records afterwards
	writes  int      // NetBird writes made, failed ones included
	changes bool     // whether external-dns planned changes
	err     string   // part of the expected apply error, if any
}

func runSteps(t *testing.T, nb *netbirdtest.Server, reg registry.Registry, steps []step) {
	t.Helper()

	for _, step := range steps {
		if step.before != nil {
			step.before()
		}
		before := writes(nb)

		changes, err := reconcile(t, reg, step.desired...)

		if step.err == "" && err != nil {
			t.Fatalf("%s: applying changes: %v", step.name, err)
		}
		if step.err != "" && (err == nil || !strings.Contains(err.Error(), step.err)) {
			t.Fatalf("%s: apply error = %v, want one containing %q", step.name, err, step.err)
		}
		if got := nb.Records(); !slices.Equal(got, step.want) {
			t.Fatalf("%s: records =\n%s\nwant\n%s", step.name, strings.Join(got, "\n"), strings.Join(step.want, "\n"))
		}
		if got := writes(nb) - before; got != step.writes {
			t.Errorf("%s: NetBird writes = %d, want %d", step.name, got, step.writes)
		}
		if changes.HasChanges() != step.changes {
			t.Errorf("%s: planned changes = %+v, want changes: %v", step.name, changes, step.changes)
		}
	}
}

func TestReconcile(t *testing.T) {
	nb := netbirdtest.NewServer(t,
		api.Zone{Id: "z1", Domain: "example.com", Records: []api.DNSRecord{
			{Id: "manual", Name: "manual.example.com", Type: "A", Content: "10.0.0.9", Ttl: 60},
		}},
		api.Zone{Id: "z2", Domain: "internal.test"},
	)
	reg := noopRegistry(t, startServer(t, nb).client(t))

	initial := []*endpoint.Endpoint{
		endpoint.NewEndpoint("www.example.com", "A", "192.0.2.1", "192.0.2.2"),
		endpoint.NewEndpointWithTTL("app.internal.test", "CNAME", 60, "lb.example.net"),
		endpoint.NewEndpoint("multi.example.com", "CNAME", "a.example.net", "b.example.net"),
		endpoint.NewEndpoint("out.example.org", "A", "198.51.100.1"),
		endpoint.NewEndpoint("app.late.test", "A", "10.0.0.5"),
	}
	updated := []*endpoint.Endpoint{
		endpoint.NewEndpointWithTTL("www.example.com", "A", 120, "192.0.2.2", "192.0.2.3"),
		endpoint.NewEndpoint("app.internal.test", "A", "10.0.0.1"),
		endpoint.NewEndpoint("multi.example.com", "CNAME", "a.example.net", "b.example.net"),
		endpoint.NewEndpoint("app.late.test", "A", "10.0.0.5"),
	}
	initialRecords := []string{
		"z1 multi.example.com CNAME a.example.net 300",
		"z1 www.example.com A 192.0.2.1 300",
		"z1 www.example.com A 192.0.2.2 300",
		"z2 app.internal.test CNAME lb.example.net 60",
	}
	updatedRecords := []string{
		"z1 multi.example.com CNAME a.example.net 300",
		"z1 www.example.com A 192.0.2.2 120",
		"z1 www.example.com A 192.0.2.3 120",
		"z2 app.internal.test A 10.0.0.1 300",
		"z3 app.late.test A 10.0.0.5 300",
	}

	runSteps(t, nb, reg, []step{
		{
			// Without ownership records, sync removes the hand-made record and
			// skips the names outside every zone.
			name:    "initial sync",
			desired: initial,
			want:    initialRecords,
			writes:  5,
			changes: true,
		},
		{
			name:    "unchanged sources",
			desired: initial,
			want:    initialRecords,
		},
		{
			name:    "zone created while running",
			before:  func() { nb.AddZone(api.Zone{Id: "z3", Domain: "late.test"}) },
			desired: initial,
			want:    append(slices.Clone(initialRecords), "z3 app.late.test A 10.0.0.5 300"),
			writes:  1,
			changes: true,
		},
		{
			// Two in-place updates for www, a delete and a create for the
			// CNAME that became an A record.
			name:    "targets, TTL, and type change",
			desired: updated,
			want:    updatedRecords,
			writes:  4,
			changes: true,
		},
		{
			name:    "unchanged after the update",
			desired: updated,
			want:    updatedRecords,
		},
		{
			name:    "sources removed",
			want:    nil,
			writes:  5,
			changes: true,
		},
	})
}

func TestReconcileWithOwnership(t *testing.T) {
	nb := netbirdtest.NewServer(t, api.Zone{Id: "z1", Domain: "example.com", Records: []api.DNSRecord{
		{Id: "manual", Name: "manual.example.com", Type: "A", Content: "10.0.0.9", Ttl: 60},
		{Id: "shared", Name: "shared.example.com", Type: "A", Content: "10.0.0.7", Ttl: 60},
		{Id: "alias", Name: "alias.example.com", Type: "A", Content: "10.0.0.8", Ttl: 60},
	}})
	reg := &ownedRegistry{Provider: startServer(t, nb).client(t), owned: map[string]*endpoint.Endpoint{}}

	handMade := []string{
		"z1 alias.example.com A 10.0.0.8 60",
		"z1 manual.example.com A 10.0.0.9 60",
	}
	withHandMade := func(records ...string) []string {
		return slices.Sorted(slices.Values(slices.Concat(handMade, records)))
	}
	initial := []*endpoint.Endpoint{
		endpoint.NewEndpoint("www.example.com", "A", "192.0.2.1"),
		endpoint.NewEndpoint("shared.example.com", "A", "192.0.2.5"),
	}
	updated := []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "192.0.2.2")}
	repointed := []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "192.0.2.3")}
	conflicting := []*endpoint.Endpoint{
		endpoint.NewEndpoint("www.example.com", "A", "192.0.2.3"),
		endpoint.NewEndpoint("alias.example.com", "CNAME", "lb.example.net"),
		endpoint.NewEndpoint("new.example.com", "A", "192.0.2.4"),
	}
	resolved := []*endpoint.Endpoint{
		endpoint.NewEndpoint("www.example.com", "A", "192.0.2.3"),
		endpoint.NewEndpoint("new.example.com", "A", "192.0.2.4"),
	}

	// The provider never touches hand-made records under other names.
	// external-dns owns full names and types, so the provider takes over the
	// hand-made record of a managed name. This is also how a failed update
	// recovers.
	runSteps(t, nb, reg, []step{
		{
			name:    "initial sync takes over a managed name",
			desired: initial,
			want:    withHandMade("z1 shared.example.com A 192.0.2.5 60", "z1 www.example.com A 192.0.2.1 300"),
			writes:  2,
			changes: true,
		},
		{
			name:    "unchanged sources",
			desired: initial,
			want:    withHandMade("z1 shared.example.com A 192.0.2.5 60", "z1 www.example.com A 192.0.2.1 300"),
		},
		{
			name:    "update one record and delete another",
			desired: updated,
			want:    withHandMade("z1 www.example.com A 192.0.2.2 300"),
			writes:  2,
			changes: true,
		},
		{
			// The registry forgets the old target of an update NetBird rejects.
			name:    "failed update",
			before:  func() { nb.FailWrites(1) },
			desired: repointed,
			want:    withHandMade("z1 www.example.com A 192.0.2.2 300"),
			writes:  1,
			changes: true,
			err:     "failed to apply changes with code 500",
		},
		{
			// The retry comes as a create, and the provider still repoints the
			// old target. It does not leave the old target behind.
			name:    "next sync recovers the update",
			desired: repointed,
			want:    withHandMade("z1 www.example.com A 192.0.2.3 300"),
			writes:  1,
			changes: true,
		},
		{
			// NetBird rejects a CNAME next to the hand-made A record. The
			// provider reports the failure, but the unrelated create still lands.
			name:    "conflict with a hand-made record",
			desired: conflicting,
			want:    withHandMade("z1 new.example.com A 192.0.2.4 300", "z1 www.example.com A 192.0.2.3 300"),
			writes:  2,
			changes: true,
			err:     "failed to apply changes with code 500",
		},
		{
			name:    "conflict persists",
			desired: conflicting,
			want:    withHandMade("z1 new.example.com A 192.0.2.4 300", "z1 www.example.com A 192.0.2.3 300"),
			writes:  1,
			changes: true,
			err:     "failed to apply changes with code 500",
		},
		{
			// When a batch succeeds, the registry adopts the record from the
			// failed batch, without a new write.
			name:    "conflict removed from the sources",
			desired: resolved,
			want:    withHandMade("z1 new.example.com A 192.0.2.4 300", "z1 www.example.com A 192.0.2.3 300"),
			changes: true,
		},
		{
			name:    "sources removed",
			want:    handMade,
			writes:  2,
			changes: true,
		},
	})
}

func TestManagedZones(t *testing.T) {
	nb := netbirdtest.NewServer(t,
		api.Zone{Id: "z1", Domain: "example.com", Records: []api.DNSRecord{
			{Id: "manual", Name: "manual.example.com", Type: "A", Content: "10.0.0.9", Ttl: 60},
		}},
		api.Zone{Id: "z2", Domain: "internal.test"},
	)
	reg := noopRegistry(t, startServer(t, nb, "NETBIRD_ZONES=internal.test").client(t))

	// Even under sync without ownership, the unlisted zone is left alone.
	runSteps(t, nb, reg, []step{{
		name: "sync",
		desired: []*endpoint.Endpoint{
			endpoint.NewEndpoint("www.example.com", "A", "192.0.2.1"),
			endpoint.NewEndpoint("app.internal.test", "A", "192.0.2.2"),
		},
		want: []string{
			"z1 manual.example.com A 10.0.0.9 60",
			"z2 app.internal.test A 192.0.2.2 300",
		},
		writes:  1,
		changes: true,
	}})
}

// get fetches url. If accept is not empty, get sends it as the Accept header.
func get(t *testing.T, url, accept string) (*http.Response, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading %s: %v", url, err)
	}

	return resp, string(body)
}

func TestProtocol(t *testing.T) {
	nb := netbirdtest.NewServer(t, api.Zone{Id: "z1", Domain: "example.com"})
	s := startServer(t, nb, "SERVER_MAX_BODY_BYTES=1024")

	t.Run("negotiation returns an empty domain filter", func(t *testing.T) {
		resp, body := get(t, s.url+"/", webhookapi.MediaTypeFormatAndVersion)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != webhookapi.MediaTypeFormatAndVersion {
			t.Fatalf("negotiate = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		if strings.TrimSpace(body) != "{}" {
			t.Errorf("negotiate body = %s, want {}", body)
		}
	})

	t.Run("missing Accept header is rejected", func(t *testing.T) {
		if resp, _ := get(t, s.url+"/records", ""); resp.StatusCode != http.StatusNotAcceptable {
			t.Errorf("status = %d, want 406", resp.StatusCode)
		}
	})

	t.Run("unknown path is not found", func(t *testing.T) {
		if resp, _ := get(t, s.url+"/nope", webhookapi.MediaTypeFormatAndVersion); resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("oversized body is rejected", func(t *testing.T) {
		body := `{"create":[{"dnsName":"` + strings.Repeat("x", 2048) + `.example.com",` +
			`"recordType":"A","targets":["192.0.2.1"]}]}`
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.url+"/records", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", webhookapi.MediaTypeFormatAndVersion)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 413", resp.StatusCode)
		}
	})

	t.Run("TXT records fail as a retryable error", func(t *testing.T) {
		err := s.client(t).ApplyChanges(t.Context(), &plan.Changes{Create: []*endpoint.Endpoint{
			endpoint.NewEndpoint("a-www.example.com", "TXT", "heritage=external-dns"),
		}})
		if !errors.Is(err, provider.SoftError) {
			t.Errorf("ApplyChanges error = %v, want a soft error external-dns retries", err)
		}
		if n := writes(nb); n != 0 {
			t.Errorf("NetBird writes = %d, want none", n)
		}
	})

	t.Run("probes and metrics", func(t *testing.T) {
		for _, url := range []string{s.url + "/readyz", s.healthURL + "/readyz", s.healthURL + "/healthz"} {
			if resp, _ := get(t, url, ""); resp.StatusCode != http.StatusOK {
				t.Errorf("GET %s = %d, want 200", url, resp.StatusCode)
			}
		}
		_, metrics := get(t, s.healthURL+"/metrics", "")
		for _, name := range []string{"externaldns_webhook_info{", "externaldns_webhook_netbird_api_duration_seconds"} {
			if !strings.Contains(metrics, name) {
				t.Errorf("/metrics is missing %s", name)
			}
		}
	})
}

func TestNotReadyWhenNetBirdRejectsToken(t *testing.T) {
	nb := netbirdtest.NewServer(t)
	s := startServer(t, nb, "NETBIRD_API_TOKEN=nbp_wrong")

	if resp, body := get(t, s.healthURL+"/readyz", ""); resp.StatusCode != http.StatusServiceUnavailable ||
		strings.Contains(body, "401") {
		t.Errorf("/readyz = %d %q, want 503 without the upstream cause", resp.StatusCode, body)
	}
	if resp, _ := get(t, s.healthURL+"/healthz", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d, want 200 while NetBird is unusable", resp.StatusCode)
	}
}

func TestExitsOnInvalidConfig(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), binary)
	cmd.Env = []string{"LOG_FORMAT=text"}
	output, err := cmd.CombinedOutput()

	if exitErr, ok := errors.AsType[*exec.ExitError](err); !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("exit = %v, want status 1", err)
	}
	if !strings.Contains(string(output), "NETBIRD_API_TOKEN") {
		t.Errorf("output does not name the missing setting:\n%s", output)
	}
}
