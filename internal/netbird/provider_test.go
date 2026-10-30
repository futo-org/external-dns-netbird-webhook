package netbird

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/futo-org/external-dns-netbird-webhook/internal/netbirdtest"
	"github.com/netbirdio/netbird/shared/management/http/api"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
)

func exampleZones() []api.Zone {
	return []api.Zone{
		{Id: "z1", Domain: "example.com"},
		{Id: "z2", Domain: "sub.example.com"},
	}
}

func TestRecords(t *testing.T) {
	zones := exampleZones()
	zones[0].Records = []api.DNSRecord{
		{Id: "1", Name: "www.example.com", Type: "A", Content: "192.0.2.1", Ttl: 300},
		{Id: "2", Name: "www.example.com", Type: "A", Content: "192.0.2.2", Ttl: 300},
		{Id: "3", Name: "WWW.example.com", Type: "AAAA", Content: "2001:db8::1", Ttl: 60},
		{Id: "4", Name: "alias.example.com", Type: "CNAME", Content: "www.example.com", Ttl: 3600},
		{Id: "5", Name: "txt.example.com", Type: "TXT", Content: "ignored", Ttl: 300},
		{Id: "7", Name: "mixed.example.com", Type: "A", Content: "192.0.2.4", Ttl: 60},
		{Id: "8", Name: "mixed.example.com", Type: "A", Content: "192.0.2.5", Ttl: 300},
		// The more specific sub.example.com zone hides this record.
		{Id: "9", Name: "app.sub.example.com", Type: "A", Content: "192.0.2.99", Ttl: 120},
	}
	zones[1].Records = []api.DNSRecord{
		{Id: "6", Name: "app.sub.example.com", Type: "A", Content: "192.0.2.3", Ttl: 120},
	}
	p, _ := newTestProvider(t, zones...)

	endpoints, err := p.Records(t.Context())
	if err != nil {
		t.Fatalf("Records: %v", err)
	}

	got := make([]string, 0, len(endpoints))
	for _, ep := range endpoints {
		got = append(got, fmt.Sprintf("%s %s %d %s", ep.DNSName, ep.RecordType, ep.RecordTTL, strings.Join(ep.Targets, ",")))
	}
	// Ordered by name, then type, so identical zones give an identical response.
	want := []string{
		"alias.example.com CNAME 3600 www.example.com",
		"app.sub.example.com A 120 192.0.2.3",
		"mixed.example.com A 0 192.0.2.4,192.0.2.5",
		"www.example.com A 300 192.0.2.1,192.0.2.2",
		"www.example.com AAAA 60 2001:db8::1",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Records =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestRecordsUnauthorized(t *testing.T) {
	f := netbirdtest.NewServer(t)
	cfg := testConfig(f.URL)
	cfg.APIToken = "wrong"
	p := NewProvider(cfg)

	_, err := p.Records(t.Context())
	if err == nil || !strings.Contains(err.Error(), "status 401: token invalid") {
		t.Fatalf("Records error = %v, want a 401 with the API message", err)
	}
	if n := f.Calls("GET"); n != 1 {
		t.Errorf("GET count = %d, want 1 (401 is not retried)", n)
	}
}

func TestApplyChanges(t *testing.T) {
	tests := []struct {
		name     string
		existing []api.DNSRecord // all in zone z1 (example.com)
		changes  plan.Changes
		want     []string
		calls    map[string]int
	}{
		{
			name: "create picks the most specific zone and the default TTL",
			changes: plan.Changes{Create: []*endpoint.Endpoint{
				endpoint.NewEndpoint("app.sub.example.com", "A", "192.0.2.1"),
				endpoint.NewEndpointWithTTL("www.example.com", "AAAA", 60, "2001:db8::1"),
			}},
			want: []string{
				"z1 www.example.com AAAA 2001:db8::1 60",
				"z2 app.sub.example.com A 192.0.2.1 300",
			},
			calls: map[string]int{"POST": 2},
		},
		{
			name:     "changed target is updated in place",
			existing: []api.DNSRecord{{Id: "a", Name: "www.example.com", Type: "A", Content: "192.0.2.1", Ttl: 300}},
			changes: plan.Changes{
				UpdateOld: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "192.0.2.1")},
				UpdateNew: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "192.0.2.9")},
			},
			want:  []string{"z1 www.example.com A 192.0.2.9 300"},
			calls: map[string]int{"PUT": 1, "POST": 0, "DELETE": 0},
		},
		{
			name: "TTL change keeps unchanged targets",
			existing: []api.DNSRecord{
				{Id: "a", Name: "www.example.com", Type: "A", Content: "192.0.2.1", Ttl: 300},
				{Id: "b", Name: "www.example.com", Type: "A", Content: "192.0.2.2", Ttl: 300},
			},
			changes: plan.Changes{
				UpdateOld: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "192.0.2.1", "192.0.2.2")},
				UpdateNew: []*endpoint.Endpoint{
					endpoint.NewEndpointWithTTL("www.example.com", "A", 60, "192.0.2.2", "192.0.2.3"),
				},
			},
			want: []string{
				"z1 www.example.com A 192.0.2.2 60",
				"z1 www.example.com A 192.0.2.3 60",
			},
			calls: map[string]int{"PUT": 2, "POST": 0, "DELETE": 0},
		},
		{
			name: "mixed TTLs are all rewritten",
			existing: []api.DNSRecord{
				{Id: "a", Name: "www.example.com", Type: "A", Content: "192.0.2.1", Ttl: 60},
				{Id: "b", Name: "www.example.com", Type: "A", Content: "192.0.2.2", Ttl: 300},
			},
			changes: plan.Changes{
				UpdateOld: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "192.0.2.1", "192.0.2.2")},
				UpdateNew: []*endpoint.Endpoint{
					endpoint.NewEndpointWithTTL("www.example.com", "A", 60, "192.0.2.1", "192.0.2.2"),
				},
			},
			want: []string{
				"z1 www.example.com A 192.0.2.1 60",
				"z1 www.example.com A 192.0.2.2 60",
			},
			calls: map[string]int{"PUT": 1, "POST": 0, "DELETE": 0},
		},
		{
			name: "unconfigured TTL leaves existing TTL alone",
			existing: []api.DNSRecord{
				{Id: "a", Name: "www.example.com", Type: "A", Content: "192.0.2.1", Ttl: 120},
			},
			changes: plan.Changes{
				UpdateOld: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "192.0.2.1")},
				UpdateNew: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "192.0.2.1", "192.0.2.2")},
			},
			want: []string{
				"z1 www.example.com A 192.0.2.1 120",
				"z1 www.example.com A 192.0.2.2 300",
			},
			calls: map[string]int{"PUT": 0, "POST": 1},
		},
		{
			name: "removed target is deleted",
			existing: []api.DNSRecord{
				{Id: "a", Name: "www.example.com", Type: "A", Content: "192.0.2.1", Ttl: 300},
				{Id: "b", Name: "www.example.com", Type: "A", Content: "192.0.2.2", Ttl: 300},
			},
			changes: plan.Changes{
				UpdateOld: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "192.0.2.1", "192.0.2.2")},
				UpdateNew: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "192.0.2.2")},
			},
			want:  []string{"z1 www.example.com A 192.0.2.2 300"},
			calls: map[string]int{"DELETE": 1, "PUT": 0, "POST": 0},
		},
		{
			name: "delete removes every record of the name and type",
			existing: []api.DNSRecord{
				{Id: "a", Name: "www.example.com", Type: "A", Content: "192.0.2.1", Ttl: 300},
				{Id: "b", Name: "www.example.com", Type: "A", Content: "192.0.2.2", Ttl: 300},
				{Id: "c", Name: "www.example.com", Type: "AAAA", Content: "2001:db8::1", Ttl: 300},
			},
			changes: plan.Changes{
				Delete: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "192.0.2.1", "192.0.2.2")},
			},
			want:  []string{"z1 www.example.com AAAA 2001:db8::1 300"},
			calls: map[string]int{"DELETE": 2},
		},
		{
			name: "A records replaced by a CNAME at the same name",
			existing: []api.DNSRecord{
				{Id: "a", Name: "www.example.com", Type: "A", Content: "192.0.2.1", Ttl: 300},
				{Id: "b", Name: "www.example.com", Type: "A", Content: "192.0.2.2", Ttl: 300},
			},
			changes: plan.Changes{
				Delete: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "192.0.2.1", "192.0.2.2")},
				Create: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "CNAME", "lb.example.net")},
			},
			want: []string{"z1 www.example.com CNAME lb.example.net 300"},
		},
		{
			name: "CNAME replaced by an A record at the same name",
			existing: []api.DNSRecord{
				{Id: "a", Name: "www.example.com", Type: "CNAME", Content: "lb.example.net", Ttl: 300},
			},
			changes: plan.Changes{
				Create: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "192.0.2.1")},
				Delete: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "CNAME", "lb.example.net")},
			},
			want: []string{"z1 www.example.com A 192.0.2.1 300"},
		},
		{
			name:     "create of an existing record is a no-op",
			existing: []api.DNSRecord{{Id: "a", Name: "www.example.com", Type: "AAAA", Content: "2001:db8:0::1", Ttl: 300}},
			changes: plan.Changes{Create: []*endpoint.Endpoint{
				endpoint.NewEndpoint("www.example.com", "AAAA", "2001:db8::1"),
			}},
			want:  []string{"z1 www.example.com AAAA 2001:db8:0::1 300"},
			calls: map[string]int{"POST": 0, "PUT": 0, "DELETE": 0},
		},
		{
			name: "name outside every zone is skipped",
			changes: plan.Changes{Create: []*endpoint.Endpoint{
				endpoint.NewEndpoint("www.example.org", "A", "192.0.2.1"),
				endpoint.NewEndpoint("www.example.com", "A", "192.0.2.1"),
			}},
			want:  []string{"z1 www.example.com A 192.0.2.1 300"},
			calls: map[string]int{"POST": 1},
		},
		// The CRD registry does not report the records that it forgot, such as
		// the old target of a failed update or a hand-made record. As a result,
		// a create must take over the full name and type.
		{
			name:     "create repoints a record external-dns didn't report",
			existing: []api.DNSRecord{{Id: "h", Name: "www.example.com", Type: "A", Content: "10.0.0.7", Ttl: 60}},
			changes: plan.Changes{Create: []*endpoint.Endpoint{
				endpoint.NewEndpoint("www.example.com", "A", "192.0.2.5"),
			}},
			want:  []string{"z1 www.example.com A 192.0.2.5 60"},
			calls: map[string]int{"PUT": 1, "POST": 0, "DELETE": 0},
		},
		{
			name: "update replaces every record of the name and type",
			existing: []api.DNSRecord{
				{Id: "h", Name: "www.example.com", Type: "A", Content: "10.0.0.7", Ttl: 60},
				{Id: "a", Name: "www.example.com", Type: "A", Content: "192.0.2.1", Ttl: 300},
			},
			changes: plan.Changes{
				UpdateOld: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "192.0.2.1")},
				UpdateNew: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "192.0.2.2")},
			},
			want:  []string{"z1 www.example.com A 192.0.2.2 60"},
			calls: map[string]int{"PUT": 1, "DELETE": 1, "POST": 0},
		},
		{
			name:     "identical unclaimed record is adopted",
			existing: []api.DNSRecord{{Id: "h", Name: "www.example.com", Type: "A", Content: "192.0.2.1", Ttl: 300}},
			changes: plan.Changes{Create: []*endpoint.Endpoint{
				endpoint.NewEndpointWithTTL("www.example.com", "A", 60, "192.0.2.1"),
			}},
			want:  []string{"z1 www.example.com A 192.0.2.1 60"},
			calls: map[string]int{"PUT": 1, "POST": 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			zones := exampleZones()
			zones[0].Records = tt.existing
			p, f := newTestProvider(t, zones...)

			if err := p.ApplyChanges(t.Context(), &tt.changes); err != nil {
				t.Fatalf("ApplyChanges: %v", err)
			}

			if got := f.Records(); !slices.Equal(got, tt.want) {
				t.Errorf("records =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tt.want, "\n"))
			}
			if n := f.Calls("GET"); n != 1 {
				t.Errorf("GET count = %d, want 1 snapshot", n)
			}
			for method, want := range tt.calls {
				if got := f.Calls(method); got != want {
					t.Errorf("%s count = %d, want %d", method, got, want)
				}
			}
		})
	}
}

func TestApplyChangesRejectsUnsupportedTypes(t *testing.T) {
	p, f := newTestProvider(t, exampleZones()...)

	err := p.ApplyChanges(t.Context(), &plan.Changes{Create: []*endpoint.Endpoint{
		endpoint.NewEndpoint("www.example.com", "A", "192.0.2.1"),
		endpoint.NewEndpoint("a-www.example.com", "TXT", "heritage=external-dns"),
	}})
	if err == nil || !strings.Contains(err.Error(), "--registry=noop") {
		t.Fatalf("ApplyChanges error = %v, want one pointing at --registry=noop", err)
	}
	if n := f.Calls(""); n != 0 {
		t.Errorf("NetBird calls = %d, want none before validation passes", n)
	}
}

func TestApplyChangesSurfacesAPIErrors(t *testing.T) {
	zones := exampleZones()
	zones[0].Records = []api.DNSRecord{{Id: "a", Name: "www.example.com", Type: "CNAME", Content: "lb.example.net"}}
	p, _ := newTestProvider(t, zones...)

	// Without a matching delete, NetBird refuses an A next to the CNAME.
	err := p.ApplyChanges(t.Context(), &plan.Changes{Create: []*endpoint.Endpoint{
		endpoint.NewEndpoint("www.example.com", "A", "192.0.2.1"),
	}})
	if err == nil || !strings.Contains(err.Error(), "status 422") {
		t.Fatalf("ApplyChanges error = %v, want the 422 from NetBird", err)
	}
}

func TestAdjustEndpoints(t *testing.T) {
	// Literal endpoints, because endpoint.NewEndpoint already strips the
	// trailing dots that the DNSEndpoint source passes through.
	tests := []struct {
		name string
		in   endpoint.Endpoint
		want endpoint.Endpoint
	}{
		{
			name: "CNAME keeps only its first target",
			in:   endpoint.Endpoint{DNSName: "alias.example.com", RecordType: "CNAME", Targets: endpoint.Targets{"a.example.net", "b.example.net"}},
			want: endpoint.Endpoint{DNSName: "alias.example.com", RecordType: "CNAME", Targets: endpoint.Targets{"a.example.net"}},
		},
		{
			name: "CNAME target loses its trailing dot",
			in:   endpoint.Endpoint{DNSName: "alias.example.com", RecordType: "CNAME", Targets: endpoint.Targets{"lb.example.net."}},
			want: endpoint.Endpoint{DNSName: "alias.example.com", RecordType: "CNAME", Targets: endpoint.Targets{"lb.example.net"}},
		},
		{
			name: "A targets are kept",
			in:   endpoint.Endpoint{DNSName: "www.example.com", RecordType: "A", Targets: endpoint.Targets{"192.0.2.1", "192.0.2.2"}},
			want: endpoint.Endpoint{DNSName: "www.example.com", RecordType: "A", Targets: endpoint.Targets{"192.0.2.1", "192.0.2.2"}},
		},
		{
			name: "set identifier is dropped",
			in:   endpoint.Endpoint{DNSName: "www.example.com", RecordType: "A", SetIdentifier: "blue", Targets: endpoint.Targets{"192.0.2.1"}},
			want: endpoint.Endpoint{DNSName: "www.example.com", RecordType: "A", Targets: endpoint.Targets{"192.0.2.1"}},
		},
		{
			name: "name is spelled as lowercase punycode",
			in:   endpoint.Endpoint{DNSName: "Bücher.Example.com.", RecordType: "A", Targets: endpoint.Targets{"192.0.2.1"}},
			want: endpoint.Endpoint{DNSName: "xn--bcher-kva.example.com", RecordType: "A", Targets: endpoint.Targets{"192.0.2.1"}},
		},
	}

	p, _ := newTestProvider(t, exampleZones()...)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.in
			got, err := p.AdjustEndpoints([]*endpoint.Endpoint{&in})
			if err != nil {
				t.Fatalf("AdjustEndpoints: %v", err)
			}
			if len(got) != 1 || got[0].DNSName != tt.want.DNSName || got[0].SetIdentifier != tt.want.SetIdentifier ||
				!slices.Equal(got[0].Targets, tt.want.Targets) {
				t.Errorf("AdjustEndpoints = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestAdjustedDottedCNAMEIsCreated(t *testing.T) {
	p, f := newTestProvider(t, exampleZones()...)

	desired, err := p.AdjustEndpoints([]*endpoint.Endpoint{
		{DNSName: "alias.example.com", RecordType: "CNAME", Targets: endpoint.Targets{"lb.example.net."}},
	})
	if err != nil {
		t.Fatalf("AdjustEndpoints: %v", err)
	}
	if err := p.ApplyChanges(t.Context(), &plan.Changes{Create: desired}); err != nil {
		t.Fatalf("ApplyChanges: %v", err)
	}

	current, err := p.Records(t.Context())
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(current) != 1 || !current[0].Targets.Same(desired[0].Targets) {
		t.Errorf("Records = %v, want targets matching desired %v so no update is planned", current, desired[0].Targets)
	}
	if got := f.Records(); !slices.Equal(got, []string{"z1 alias.example.com CNAME lb.example.net 300"}) {
		t.Errorf("records = %v", got)
	}
}

func TestApplyChangesLeavesParentZoneCopies(t *testing.T) {
	zones := exampleZones()
	zones[0].Records = []api.DNSRecord{{Id: "p", Name: "app.sub.example.com", Type: "A", Content: "192.0.2.1", Ttl: 300}}
	zones[1].Records = []api.DNSRecord{{Id: "c", Name: "app.sub.example.com", Type: "A", Content: "192.0.2.1", Ttl: 300}}
	p, f := newTestProvider(t, zones...)

	current, err := p.Records(t.Context())
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(current) != 1 || !slices.Equal(current[0].Targets, endpoint.Targets{"192.0.2.1"}) {
		t.Fatalf("Records = %v, want only the sub.example.com copy", current)
	}

	err = p.ApplyChanges(t.Context(), &plan.Changes{
		UpdateOld: current,
		UpdateNew: []*endpoint.Endpoint{endpoint.NewEndpoint("app.sub.example.com", "A", "192.0.2.1", "192.0.2.2")},
	})
	if err != nil {
		t.Fatalf("ApplyChanges update: %v", err)
	}
	want := []string{
		"z1 app.sub.example.com A 192.0.2.1 300",
		"z2 app.sub.example.com A 192.0.2.1 300",
		"z2 app.sub.example.com A 192.0.2.2 300",
	}
	if got := f.Records(); !slices.Equal(got, want) {
		t.Errorf("after update records =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	err = p.ApplyChanges(t.Context(), &plan.Changes{
		Delete: []*endpoint.Endpoint{endpoint.NewEndpoint("app.sub.example.com", "A", "192.0.2.1", "192.0.2.2")},
	})
	if err != nil {
		t.Fatalf("ApplyChanges delete: %v", err)
	}
	if got := f.Records(); !slices.Equal(got, want[:1]) {
		t.Errorf("after delete records = %v, want only the untouched parent copy", got)
	}
}

func TestAdjustEndpointsFollowsZones(t *testing.T) {
	p, f := newTestProvider(t, api.Zone{Id: "z1", Domain: "Example.com."})

	if !p.GetDomainFilter().Match("anything.example.org") {
		t.Fatal("negotiated domain filter should be empty; zones are applied in AdjustEndpoints")
	}

	names := func() []string {
		t.Helper()

		got, err := p.AdjustEndpoints([]*endpoint.Endpoint{
			endpoint.NewEndpoint("example.com", "A", "192.0.2.1"),
			endpoint.NewEndpoint("WWW.example.com", "A", "192.0.2.1"),
			endpoint.NewEndpoint("notexample.com", "A", "192.0.2.1"),
			endpoint.NewEndpoint("app.internal.test", "A", "192.0.2.1"),
		})
		if err != nil {
			t.Fatalf("AdjustEndpoints: %v", err)
		}
		out := make([]string, len(got))
		for i, ep := range got {
			out[i] = ep.DNSName
		}

		return out
	}

	if got, want := names(), []string{"example.com", "www.example.com"}; !slices.Equal(got, want) {
		t.Errorf("before the zone exists: kept %v, want %v", got, want)
	}

	f.AddZone(api.Zone{Id: "z2", Domain: "internal.test"})

	if got, want := names(), []string{"example.com", "www.example.com", "app.internal.test"}; !slices.Equal(got, want) {
		t.Errorf("after the zone is created: kept %v, want %v", got, want)
	}
}

func TestAdjustEndpointsFailsWhenZonesUnavailable(t *testing.T) {
	f := netbirdtest.NewServer(t)
	cfg := testConfig(f.URL)
	cfg.APIToken = "wrong"

	_, err := NewProvider(cfg).AdjustEndpoints([]*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "192.0.2.1")})
	if err == nil || !strings.Contains(err.Error(), "status 401") {
		t.Fatalf("AdjustEndpoints error = %v, want the 401 from NetBird", err)
	}
}

// planOnce runs one external-dns planning cycle against the provider, the way
// the controller does: read current records, adjust the desired ones, plan.
func planOnce(t *testing.T, p *Provider, desired ...*endpoint.Endpoint) *plan.Changes {
	t.Helper()

	current, err := p.Records(t.Context())
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	adjusted, err := p.AdjustEndpoints(desired)
	if err != nil {
		t.Fatalf("AdjustEndpoints: %v", err)
	}

	return (&plan.Plan{
		Policies:       []plan.Policy{plan.Policies["sync"]},
		Current:        current,
		Desired:        adjusted,
		ManagedRecords: supportedRecordTypes,
	}).Calculate().Changes
}

func TestPlanConverges(t *testing.T) {
	tests := []struct {
		name     string
		zone     api.Zone
		desired  []*endpoint.Endpoint
		want     []string
		changing bool // whether the first cycle plans changes
	}{
		{
			name: "Unicode hostname matches its punycode record",
			zone: api.Zone{Id: "z1", Domain: "xn--bcher-kva.example", Records: []api.DNSRecord{
				{Id: "a", Name: "www.xn--bcher-kva.example", Type: "A", Content: "192.0.2.1", Ttl: 300},
			}},
			desired: []*endpoint.Endpoint{endpoint.NewEndpoint("www.bücher.example", "A", "192.0.2.1")},
			want:    []string{"z1 www.xn--bcher-kva.example A 192.0.2.1 300"},
		},
		{
			name:     "Unicode hostname is created as punycode",
			zone:     api.Zone{Id: "z1", Domain: "xn--bcher-kva.example"},
			desired:  []*endpoint.Endpoint{endpoint.NewEndpoint("www.bücher.example", "A", "192.0.2.1")},
			want:     []string{"z1 www.xn--bcher-kva.example A 192.0.2.1 300"},
			changing: true,
		},
		{
			name:     "two spellings of one IPv6 target create one record",
			zone:     api.Zone{Id: "z1", Domain: "example.com"},
			desired:  []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "AAAA", "2001:db8::1", "2001:db8:0::1")},
			want:     []string{"z1 www.example.com AAAA 2001:db8::1 300"},
			changing: true,
		},
		{
			name: "differently spelled IPv6 does not hide another target change",
			zone: api.Zone{Id: "z1", Domain: "example.com", Records: []api.DNSRecord{
				{Id: "a", Name: "www.example.com", Type: "AAAA", Content: "2001:db8:0::1", Ttl: 300},
				{Id: "b", Name: "www.example.com", Type: "AAAA", Content: "2001:db8::2", Ttl: 300},
			}},
			desired: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "AAAA", "2001:db8::1", "2001:db8::3")},
			want: []string{
				"z1 www.example.com AAAA 2001:db8:0::1 300",
				"z1 www.example.com AAAA 2001:db8::3 300",
			},
			changing: true,
		},
		{
			name: "provider-specific properties are ignored",
			zone: api.Zone{Id: "z1", Domain: "example.com", Records: []api.DNSRecord{
				{Id: "a", Name: "alias.example.com", Type: "CNAME", Content: "lb.example.net", Ttl: 300},
			}},
			desired: []*endpoint.Endpoint{
				endpoint.NewEndpoint("alias.example.com", "CNAME", "lb.example.net").
					WithProviderSpecific("alias", "false"),
			},
			want: []string{"z1 alias.example.com CNAME lb.example.net 300"},
		},
		{
			name:     "record is created in a zone whose domain has capitals",
			zone:     api.Zone{Id: "z1", Domain: "Example.com"},
			desired:  []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "192.0.2.1")},
			want:     []string{"z1 www.Example.com A 192.0.2.1 300"},
			changing: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, f := newTestProvider(t, tt.zone)
			clone := func() []*endpoint.Endpoint {
				out := make([]*endpoint.Endpoint, len(tt.desired))
				for i, ep := range tt.desired {
					out[i] = ep.DeepCopy()
				}

				return out
			}

			changes := planOnce(t, p, clone()...)
			if changes.HasChanges() != tt.changing {
				t.Fatalf("first cycle changes = %+v, want changes: %v", changes, tt.changing)
			}
			if err := p.ApplyChanges(t.Context(), changes); err != nil {
				t.Fatalf("ApplyChanges: %v", err)
			}
			if got := f.Records(); !slices.Equal(got, tt.want) {
				t.Errorf("records =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tt.want, "\n"))
			}
			if changes := planOnce(t, p, clone()...); changes.HasChanges() {
				t.Errorf("second cycle planned %+v, want no changes", changes)
			}
		})
	}
}

func TestApplyChangesStopsAtApplyTimeout(t *testing.T) {
	f := netbirdtest.NewServer(t, exampleZones()...)
	f.SetWriteDelay(40 * time.Millisecond)
	cfg := testConfig(f.URL)
	cfg.ApplyWorkers = 1
	cfg.ApplyTimeout = 100 * time.Millisecond
	p := NewProvider(cfg)

	start := time.Now()
	err := p.ApplyChanges(t.Context(), &plan.Changes{Create: []*endpoint.Endpoint{
		endpoint.NewEndpoint("a.example.com", "A", "192.0.2.1"),
		endpoint.NewEndpoint("b.example.com", "A", "192.0.2.1"),
		endpoint.NewEndpoint("c.example.com", "A", "192.0.2.1"),
		endpoint.NewEndpoint("d.example.com", "A", "192.0.2.1"),
		endpoint.NewEndpoint("e.example.com", "A", "192.0.2.1"),
	}})
	if err == nil || !strings.Contains(err.Error(), "NETBIRD_APPLY_TIMEOUT") {
		t.Fatalf("ApplyChanges error = %v, want the apply timeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("ApplyChanges took %s, want it to stop near the 100ms timeout", elapsed)
	}
	if n := len(f.Records()); n == 0 || n == 5 {
		t.Errorf("records created = %d, want some but not all before the timeout", n)
	}
}

func TestApplyChangesAttemptsEveryChange(t *testing.T) {
	zones := exampleZones()
	zones[0].Records = []api.DNSRecord{{Id: "h", Name: "alias.example.com", Type: "A", Content: "10.0.0.8", Ttl: 60}}
	nb := netbirdtest.NewServer(t, zones...)
	// One worker runs the failing change first. If a failure does not stop the
	// batch, the second change lands.
	cfg := testConfig(nb.URL)
	cfg.ApplyWorkers = 1
	p := NewProvider(cfg)

	// The CNAME conflicts with a record that external-dns does not own, and
	// NetBird rejects it. The unrelated create must still land.
	err := p.ApplyChanges(t.Context(), &plan.Changes{Create: []*endpoint.Endpoint{
		endpoint.NewEndpoint("alias.example.com", "CNAME", "lb.example.net"),
		endpoint.NewEndpoint("www.example.com", "A", "192.0.2.1"),
	}})
	if err == nil || !strings.Contains(err.Error(), "status 422") {
		t.Fatalf("ApplyChanges error = %v, want the 422 for the conflicting CNAME", err)
	}
	want := []string{
		"z1 alias.example.com A 10.0.0.8 60",
		"z1 www.example.com A 192.0.2.1 300",
	}
	if got := nb.Records(); !slices.Equal(got, want) {
		t.Errorf("records =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestManagedZones(t *testing.T) {
	nb := netbirdtest.NewServer(t,
		api.Zone{Id: "z1", Domain: "example.com", Records: []api.DNSRecord{
			{Id: "a", Name: "www.example.com", Type: "A", Content: "192.0.2.1", Ttl: 300},
			// Its name belongs to the unmanaged sub.example.com zone.
			{Id: "b", Name: "app.sub.example.com", Type: "A", Content: "192.0.2.9", Ttl: 300},
		}},
		api.Zone{Id: "z2", Domain: "sub.example.com", Records: []api.DNSRecord{
			{Id: "c", Name: "app.sub.example.com", Type: "A", Content: "192.0.2.2", Ttl: 300},
		}},
		api.Zone{Id: "z3", Domain: "other.test", Records: []api.DNSRecord{
			{Id: "d", Name: "x.other.test", Type: "A", Content: "192.0.2.3", Ttl: 300},
		}},
	)
	cfg := testConfig(nb.URL)
	cfg.Zones = []string{"Example.com"}
	p := NewProvider(cfg)
	untouched := nb.Records()

	current, err := p.Records(t.Context())
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(current) != 1 || current[0].DNSName != "www.example.com" {
		t.Errorf("Records = %v, want only www.example.com", current)
	}

	adjusted, err := p.AdjustEndpoints([]*endpoint.Endpoint{
		endpoint.NewEndpoint("new.example.com", "A", "192.0.2.4"),
		endpoint.NewEndpoint("new.sub.example.com", "A", "192.0.2.4"),
		endpoint.NewEndpoint("new.other.test", "A", "192.0.2.4"),
	})
	if err != nil {
		t.Fatalf("AdjustEndpoints: %v", err)
	}
	if len(adjusted) != 1 || adjusted[0].DNSName != "new.example.com" {
		t.Errorf("AdjustEndpoints kept %v, want only new.example.com", adjusted)
	}

	err = p.ApplyChanges(t.Context(), &plan.Changes{
		Create: []*endpoint.Endpoint{endpoint.NewEndpoint("new.sub.example.com", "A", "192.0.2.4")},
		Delete: []*endpoint.Endpoint{
			endpoint.NewEndpoint("app.sub.example.com", "A", "192.0.2.2", "192.0.2.9"),
			endpoint.NewEndpoint("x.other.test", "A", "192.0.2.3"),
		},
	})
	if err != nil {
		t.Fatalf("ApplyChanges: %v", err)
	}
	if got := nb.Records(); !slices.Equal(got, untouched) {
		t.Errorf("records changed outside the managed zone:\n%s", strings.Join(got, "\n"))
	}
}
