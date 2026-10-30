package netbird

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/futo-org/external-dns-netbird-webhook/internal/config"
	"github.com/futo-org/external-dns-netbird-webhook/internal/metrics"
	"github.com/netbirdio/netbird/shared/management/http/api"
	"golang.org/x/net/idna"
	"golang.org/x/sync/errgroup"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider"
)

const (
	// defaultTTL is the TTL for a record that external-dns sends without one.
	// NetBird gives the TTL of a record to the peers as it is, and a TTL of
	// zero stops caching on the clients.
	defaultTTL = 300

	// zoneLookupTimeout bounds the zone lookup in AdjustEndpoints, whose
	// signature carries no context. It stays below the 15s that external-dns
	// gives a webhook call by default.
	zoneLookupTimeout = 10 * time.Second

	opCreate = "create"
	opUpdate = "update"
	opDelete = "delete"
)

// supportedRecordTypes are the only types NetBird custom zones accept.
var supportedRecordTypes = []string{endpoint.RecordTypeA, endpoint.RecordTypeAAAA, endpoint.RecordTypeCNAME}

// errApplyTimeout is the cause of the deadline that ApplyChanges sets. The
// cause tells this deadline apart from a cancellation by the caller.
var errApplyTimeout = errors.New("NETBIRD_APPLY_TIMEOUT reached")

// Provider implements the external-dns provider.Provider interface on top of
// NetBird custom DNS zones. It keeps the empty domain filter of BaseProvider.
// AdjustEndpoints keeps the provider inside the zones.
type Provider struct {
	provider.BaseProvider

	client       *client
	workers      int
	applyTimeout time.Duration
	// managed holds the normalized domains of the zones to manage, or is nil
	// to manage every zone the token can see.
	managed map[string]bool
}

// NewProvider returns a Provider for the NetBird management API in cfg.
func NewProvider(cfg config.NetBird) *Provider {
	p := &Provider{client: newClient(cfg), workers: cfg.ApplyWorkers, applyTimeout: cfg.ApplyTimeout}
	if len(cfg.Zones) > 0 {
		p.managed = make(map[string]bool, len(cfg.Zones))
		for _, domain := range cfg.Zones {
			p.managed[normalizeName(domain)] = true
		}
	}

	return p
}

// zoneSet is the zones the token can see and which of them are managed.
type zoneSet struct {
	zones   []api.Zone
	managed map[string]bool
}

// owner returns the zone that holds the records of a name. This is the most
// specific zone that contains the name. If no zone contains the name, or if
// that zone is not managed, owner returns nil. owner picks from all zones, not
// only the managed ones. As a result, a name under an unmanaged zone never
// goes into a managed parent zone, where the unmanaged zone hides it.
func (s zoneSet) owner(name string) *api.Zone {
	var found *api.Zone
	longest := -1
	for i, z := range s.zones {
		d := normalizeName(z.Domain)
		if (name == d || strings.HasSuffix(name, "."+d)) && len(d) > longest {
			found, longest = &s.zones[i], len(d)
		}
	}
	if found == nil || s.managed != nil && !s.managed[normalizeName(found.Domain)] {
		return nil
	}

	return found
}

type recordKey struct {
	name       string
	recordType string
}

func (k recordKey) compare(o recordKey) int {
	return cmp.Or(strings.Compare(k.name, o.name), strings.Compare(k.recordType, o.recordType))
}

type zonedRecord struct {
	zoneID string
	api.DNSRecord
}

type change struct {
	op string
	zonedRecord
}

// Records returns every A, AAAA, and CNAME record in the managed zones,
// grouped into one endpoint per name and type, in a stable order.
func (p *Provider) Records(ctx context.Context) (_ []*endpoint.Endpoint, err error) {
	m := metrics.Get()
	defer func() { m.RecordOperation(err) }()

	zones, err := p.listZones(ctx)
	if err != nil {
		return nil, err
	}

	index := indexRecords(zones)
	counts := make(map[string]int, len(supportedRecordTypes))
	endpoints := make([]*endpoint.Endpoint, 0, len(index))
	for _, key := range slices.SortedFunc(maps.Keys(index), recordKey.compare) {
		records := index[key]
		counts[key.recordType] += len(records)

		// After a partial update or a manual edit, the records of one name can
		// have different TTLs. Then Records reports a TTL of 0. If external-dns
		// wants a specific TTL, it plans an update that rewrites all the records.
		ttl := records[0].Ttl
		targets := make([]string, len(records))
		for i, r := range records {
			targets[i] = canonicalTarget(r.Content)
			if r.Ttl != ttl {
				ttl = 0
			}
		}
		if ep := endpoint.NewEndpointWithTTL(key.name, key.recordType, endpoint.TTL(ttl), targets...); ep != nil {
			endpoints = append(endpoints, ep)
		}
	}
	for _, t := range supportedRecordTypes {
		m.UpdateRecordsByType(t, counts[t])
	}

	return endpoints, nil
}

// AdjustEndpoints removes the endpoints outside the managed zones. It rewrites
// the other endpoints into the form that NetBird stores. As a result,
// external-dns compares equal forms and does not plan the same change on each
// sync.
//
// AdjustEndpoints looks up the zones on each sync. GetDomainFilter does not
// report them, because external-dns negotiates only once, at startup. A zone
// that you create later then stays out of scope until external-dns restarts.
func (p *Provider) AdjustEndpoints(endpoints []*endpoint.Endpoint) ([]*endpoint.Endpoint, error) {
	ctx, cancel := context.WithTimeout(context.Background(), zoneLookupTimeout)
	defer cancel()

	zones, err := p.listZones(ctx)
	if err != nil {
		return nil, err
	}

	adjusted := make([]*endpoint.Endpoint, 0, len(endpoints))
	for _, ep := range endpoints {
		name := normalizeName(ep.DNSName)
		if zones.owner(name) == nil {
			slog.Debug("no managed NetBird zone contains this name; ignoring it", "name", ep.DNSName)

			continue
		}
		adjusted = append(adjusted, ep)

		// The CRD registry of external-dns names each DNSRecord object after the
		// DNS name, without IDNA normalization. If the source of a hostname
		// changes between Unicode and punycode spelling, the hostname loses its
		// object.
		ep.DNSName = name

		// NetBird has no routing policies. Without the identifier, the endpoints
		// that share a name become one row in the plan. Then external-dns always
		// picks the same endpoint, and the record does not change between them.
		if ep.SetIdentifier != "" {
			slog.Warn("NetBird has no routing policies; ignoring the set identifier",
				"name", ep.DNSName, "set_identifier", ep.SetIdentifier)
			ep.SetIdentifier = ""
		}

		// Records never returns provider-specific properties. If a property
		// stays here, external-dns plans an update on each sync.
		if len(ep.ProviderSpecific) > 0 {
			slog.Debug("NetBird has no provider-specific properties; ignoring them",
				"name", ep.DNSName, "properties", ep.ProviderSpecific)
			ep.ProviderSpecific = nil
		}

		for i, target := range ep.Targets {
			ep.Targets[i] = canonicalTarget(target)
		}

		if ep.RecordType == endpoint.RecordTypeCNAME && len(ep.Targets) > 1 {
			metrics.Get().IgnoredCNAMETargetsTotal.WithLabelValues(metrics.ProviderName).Inc()
			slog.Warn("NetBird allows a single CNAME target per name; ignoring the rest",
				"name", ep.DNSName, "ignored_targets", ep.Targets[1:])
			ep.Targets = ep.Targets[:1]
		}
	}

	return adjusted, nil
}

// ApplyChanges reconciles each name and type in changes against one snapshot
// of the zones. Deletes run before creates and updates, because NetBird
// rejects a CNAME next to any other record of the same name. ApplyChanges
// tries each change and returns all the failures together.
//
// The apply timeout limits the whole call. If a webhook misses the deadline of
// external-dns, external-dns stops. But it retries a 500 on the next sync, and
// that sync continues from where this one stopped.
func (p *Provider) ApplyChanges(ctx context.Context, changes *plan.Changes) (err error) {
	m := metrics.Get()
	defer func() { m.RecordOperation(err) }()

	for _, ep := range slices.Concat(changes.Create, changes.UpdateNew) {
		if !slices.Contains(supportedRecordTypes, ep.RecordType) {
			return fmt.Errorf("netbird: %s record %s is not supported: NetBird zones only hold A, AAAA, and CNAME "+
				"records, so external-dns must run with --registry=noop or --registry=crd", ep.RecordType, ep.DNSName)
		}
	}

	ctx, cancel := context.WithTimeoutCause(ctx, p.applyTimeout, errApplyTimeout)
	defer cancel()
	defer func() {
		if err != nil && errors.Is(context.Cause(ctx), errApplyTimeout) {
			err = fmt.Errorf("netbird: changes not finished within NETBIRD_APPLY_TIMEOUT (%s); "+
				"the rest are retried on the next sync: %w", p.applyTimeout, err)
		}
	}()

	zones, err := p.listZones(ctx)
	if err != nil {
		return err
	}

	m.BatchSize.WithLabelValues(metrics.ProviderName, opCreate).Observe(float64(len(changes.Create)))
	m.BatchSize.WithLabelValues(metrics.ProviderName, opUpdate).Observe(float64(len(changes.UpdateNew)))
	m.BatchSize.WithLabelValues(metrics.ProviderName, opDelete).Observe(float64(len(changes.Delete)))

	deletes, writes := diff(zones, changes)
	deleteErr := runAll(ctx, p.workers, deletes, p.apply)

	return errors.Join(deleteErr, runAll(ctx, p.workers, writes, p.apply))
}

func (p *Provider) listZones(ctx context.Context) (zoneSet, error) {
	zones, err := p.client.listZones(ctx)
	if err != nil {
		return zoneSet{}, fmt.Errorf("netbird: listing zones: %w", err)
	}

	return zoneSet{zones: zones, managed: p.managed}, nil
}

func (p *Provider) apply(ctx context.Context, c change) error {
	var err error
	switch c.op {
	case opCreate:
		err = p.client.createRecord(ctx, c.zoneID, c.DNSRecord)
	case opUpdate:
		err = p.client.updateRecord(ctx, c.zoneID, c.DNSRecord)
	case opDelete:
		err = p.client.deleteRecord(ctx, c.zoneID, c.Id)
	}
	if err != nil {
		return fmt.Errorf("netbird: %s %s record %s: %w", c.op, c.Type, c.Name, err)
	}

	slog.Debug("applied change", "op", c.op, "zone", c.zoneID, "name", c.Name,
		"type", c.Type, "content", c.Content, "ttl", c.Ttl)
	metrics.Get().RecordChange(c.op, string(c.Type))

	return nil
}

// diff turns the plan into API calls for single records. external-dns owns
// full names and types. As a result, diff reconciles each name and type in
// the plan against the zones, and does not replay the plan word for word.
// This also lets a sync recover after the CRD registry forgets a failed
// update. The retry comes as a create, and diff still repoints the old record.
func diff(zones zoneSet, changes *plan.Changes) (deletes, writes []change) {
	index := indexRecords(zones)

	desired := make(map[recordKey]*endpoint.Endpoint)
	for _, ep := range slices.Concat(changes.Create, changes.UpdateNew) {
		desired[keyOf(ep)] = ep
	}

	seen := make(map[recordKey]bool)
	for _, ep := range slices.Concat(changes.Delete, changes.UpdateOld, changes.Create, changes.UpdateNew) {
		key := keyOf(ep)
		if seen[key] {
			continue
		}
		seen[key] = true

		d, w := diffKey(zones, key, index[key], desired[key])
		deletes = append(deletes, d...)
		writes = append(writes, w...)
	}

	return deletes, writes
}

// diffKey reconciles the records of one name and type with want. If
// external-dns wants no records, want is nil. diffKey keeps the records whose
// content is still wanted. It repoints the other records to the remaining
// targets, so that the name always resolves. Then it deletes the records that
// are left, and creates the targets that are left.
func diffKey(zones zoneSet, key recordKey, existing []zonedRecord, want *endpoint.Endpoint) (deletes, writes []change) {
	var unmatched []string
	if want != nil {
		unmatched = slices.Clone(want.Targets)
	}

	var stale []zonedRecord
	for _, r := range existing {
		i := slices.IndexFunc(unmatched, func(t string) bool { return sameContent(t, r.Content) })
		if i < 0 {
			stale = append(stale, r)

			continue
		}
		unmatched = slices.Delete(unmatched, i, i+1)
		if ttl := ttlFor(want, r.Ttl); ttl != r.Ttl {
			r.Ttl = ttl
			writes = append(writes, change{opUpdate, r})
		}
	}

	for len(stale) > 0 && len(unmatched) > 0 {
		r := stale[0]
		r.Content = unmatched[0]
		r.Ttl = ttlFor(want, r.Ttl)
		writes = append(writes, change{opUpdate, r})
		stale, unmatched = stale[1:], unmatched[1:]
	}

	for _, r := range stale {
		deletes = append(deletes, change{opDelete, r})
	}

	if len(unmatched) == 0 {
		return deletes, writes
	}
	z := zones.owner(key.name)
	if z == nil {
		slog.Warn("no managed NetBird zone contains this name; skipping", "name", key.name, "type", key.recordType)

		return deletes, writes
	}
	// NetBird makes sure that a name ends in the domain of the zone. The
	// comparison is case-sensitive and uses the domain exactly as the zone
	// stores it.
	name := strings.TrimSuffix(key.name, normalizeName(z.Domain)) + strings.TrimSuffix(z.Domain, ".")
	for _, target := range unmatched {
		writes = append(writes, change{opCreate, zonedRecord{
			zoneID:  z.Id,
			Name:    name,
			Type:    api.DNSRecordType(key.recordType),
			Content: target,
			Ttl:     ttlFor(want, defaultTTL),
		}})
	}

	return deletes, writes
}

// indexRecords groups the managed records by name and type. A record is
// managed only in the owner zone of its name, where creates also go.
// external-dns never sees or changes copies in a parent zone. As a result,
// zones with different distribution groups cannot lose records to each other.
func indexRecords(zones zoneSet) map[recordKey][]zonedRecord {
	index := make(map[recordKey][]zonedRecord)
	for _, z := range zones.zones {
		for _, r := range z.Records {
			if !slices.Contains(supportedRecordTypes, string(r.Type)) {
				continue
			}
			key := recordKey{name: normalizeName(r.Name), recordType: string(r.Type)}
			if owner := zones.owner(key.name); owner == nil || owner.Id != z.Id {
				continue
			}
			index[key] = append(index[key], zonedRecord{zoneID: z.Id, DNSRecord: r})
		}
	}

	return index
}

func keyOf(ep *endpoint.Endpoint) recordKey {
	return recordKey{name: normalizeName(ep.DNSName), recordType: ep.RecordType}
}

// nameProfile is the IDNA profile that external-dns uses for its plan keys.
// With it, a Unicode hostname and its punycode record are also one name here.
var nameProfile = idna.New(idna.MapForLookup(), idna.Transitional(true), idna.StrictDomainName(false))

// normalizeName returns name as lowercase ASCII without a trailing dot.
func normalizeName(name string) string {
	// If a name is not valid IDNA, keep the best-effort result, as
	// external-dns does. Do not fail the sync for it.
	ascii, _ := nameProfile.ToASCII(strings.TrimSpace(name))

	return strings.ToLower(strings.TrimSuffix(ascii, "."))
}

// canonicalTarget writes a target in the form that the provider compares and
// stores. An IP gets its canonical form. In external-dns v0.23.0, Targets.Same
// stops at the first pair of equal IPs with different spellings, and does not
// see a change to another target. A name loses its trailing dot. NetBird
// rejects the dot, but the DNSEndpoint source keeps it for CNAMEs.
func canonicalTarget(target string) string {
	if addr, err := netip.ParseAddr(target); err == nil {
		return addr.String()
	}

	return strings.TrimSuffix(target, ".")
}

// sameContent reports whether two record contents are the same target, with
// names compared case-insensitively.
func sameContent(a, b string) bool {
	return strings.EqualFold(canonicalTarget(a), canonicalTarget(b))
}

// If external-dns configured a TTL on want, ttlFor returns that TTL. If not,
// ttlFor returns fallback.
func ttlFor(want *endpoint.Endpoint, fallback int) int {
	if want != nil && want.RecordTTL.IsConfigured() {
		return int(want.RecordTTL)
	}

	return fallback
}

// runAll calls fn for each item, with at most limit calls at the same time,
// and joins the errors. A failure does not stop the other calls. As a result,
// one record that NetBird rejects cannot block all other changes on each sync.
func runAll[T any](ctx context.Context, limit int, items []T, fn func(context.Context, T) error) error {
	errs := make([]error, len(items))
	var group errgroup.Group
	group.SetLimit(limit)
	for i, item := range items {
		group.Go(func() error {
			errs[i] = fn(ctx, item)

			return nil
		})
	}
	// Each call reports through errs, so Wait has nothing to return.
	_ = group.Wait()

	return errors.Join(errs...)
}
