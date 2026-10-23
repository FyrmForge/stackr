// Package dnscap is the DNS capability's stackr side: dns_domains, the
// domains of a DNS connection's account that stackr manages, the records it
// made (records.go), and every provider call on a zone behind one
// process-wide lock. The provider (connectors.DNS, opened from the
// connector row by the service) comes in as an argument.
package dnscap

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/connectors"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Provider is one DNS account; Zone a domain on it.
type (
	Provider = connectors.DNS
	Zone     = connectors.Zone
)

// Normal is a domain as stored: lower case, no trailing dot.
func Normal(d string) string { return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d), ".")) }

// Domain states in a connector's list.
const (
	Managed    = "managed"
	NotManaged = "not_managed"
	Elsewhere  = "dns_elsewhere"
)

// Domain is a domain on a connector's account as its manager sees it.
// ShareAll and OrgIDs: who may use a managed domain of a server connector.
type Domain struct {
	Name     string   `json:"name"`
	State    string   `json:"state"`
	ShareAll bool     `json:"share_all"`
	OrgIDs   []string `json:"org_ids"`
}

type Leaf struct {
	rows  store.DNSDomainStore
	recs  store.DNSRecordStore
	locks sync.Map   // zone -> *sync.Mutex
	recMu sync.Mutex // one reconcile at a time: the table and the zones agree
}

func New(rows store.DNSDomainStore, recs store.DNSRecordStore) *Leaf {
	return &Leaf{rows: rows, recs: recs}
}

// Locked is p with every zone call behind the zone's lock: a provider may
// rewrite a whole zone per change (some do), so two writers would
// drop each other's records.
func (l *Leaf) Locked(p Provider) Provider { return locked{p, l} }

func (l *Leaf) lock(zone string) func() {
	m, _ := l.locks.LoadOrStore(Normal(zone), &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// locked is a Provider whose zone calls hold the zone's lock.
type locked struct {
	Provider
	l *Leaf
}

func (p locked) Get(ctx context.Context, zone string) ([]libdns.Record, error) {
	defer p.l.lock(zone)()
	return p.Provider.Get(ctx, zone)
}

func (p locked) Append(ctx context.Context, zone string, rs []libdns.Record) ([]libdns.Record, error) {
	defer p.l.lock(zone)()
	return p.Provider.Append(ctx, zone, rs)
}

func (p locked) Set(ctx context.Context, zone string, rs []libdns.Record) ([]libdns.Record, error) {
	defer p.l.lock(zone)()
	return p.Provider.Set(ctx, zone, rs)
}

func (p locked) Delete(ctx context.Context, zone string, rs []libdns.Record) ([]libdns.Record, error) {
	defer p.l.lock(zone)()
	return p.Provider.Delete(ctx, zone, rs)
}

func (p locked) TakeOver(ctx context.Context, zone string) error {
	defer p.l.lock(zone)()
	return p.Provider.TakeOver(ctx, zone)
}

// Domains is every domain on connector id's account with its state, plus
// managed ones the account no longer lists.
func (l *Leaf) Domains(ctx context.Context, id string, p Provider) ([]Domain, error) {
	zs, err := p.Zones(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := l.rows.ListByConnector(ctx, id)
	if err != nil {
		return nil, err
	}
	return merge(zs, rows), nil
}

// Managed is how many domains connector id manages, from stackr's own
// rows: no provider call.
func (l *Leaf) Managed(ctx context.Context, id string) (int, error) {
	rows, err := l.rows.ListByConnector(ctx, id)
	return len(rows), err
}

// States is a new connector's list: zs, none managed yet.
func States(zs []Zone) []Domain { return merge(zs, nil) }

func merge(zs []Zone, rows []store.DNSDomain) []Domain {
	out := make([]Domain, 0, len(zs))
	for _, z := range zs {
		d := Domain{Name: z.Name, State: NotManaged, OrgIDs: []string{}}
		if !z.OurDNS {
			d.State = Elsewhere
		}
		if i := slices.IndexFunc(rows, func(r store.DNSDomain) bool { return r.Name == z.Name }); i >= 0 {
			d.State, d.ShareAll, d.OrgIDs = Managed, rows[i].ShareAll, orEmpty(rows[i].OrgIDs)
			rows = slices.Delete(rows, i, i+1)
		}
		out = append(out, d)
	}
	for _, r := range rows {
		out = append(out, Domain{Name: r.Name, State: Managed, ShareAll: r.ShareAll, OrgIDs: orEmpty(r.OrgIDs)})
	}
	return out
}

func orEmpty(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

// zone finds name on the account; not there is an Invalid on domain.
func (l *Leaf) zone(ctx context.Context, p Provider, name string) (Zone, error) {
	zs, err := p.Zones(ctx)
	if err != nil {
		return Zone{}, err
	}
	i := slices.IndexFunc(zs, func(z Zone) bool { return z.Name == name })
	if i < 0 {
		return Zone{}, errs.Invalidf("domain", "%s is not on this account", name)
	}
	return zs[i], nil
}

// Manage makes stackr manage name through connector id of orgID (nil = a
// server connector), or for a managed one sets who may use it. A server
// connector's domain is available to orgIDs, or every org when none; an
// org connector's is only ever its org's. The domain must be on the
// account, served by the provider's DNS.
func (l *Leaf) Manage(ctx context.Context, id string, orgID *string, p Provider, name string, orgIDs []string) (Domain, error) {
	name = Normal(name)
	if orgID != nil && len(orgIDs) > 0 {
		return Domain{}, errs.Invalidf("org_ids", "An organization's connector serves that organization only")
	}
	d := Domain{Name: name, State: Managed, ShareAll: len(orgIDs) == 0, OrgIDs: orEmpty(orgIDs)}
	if orgID != nil {
		d.ShareAll = false
	}
	rows, err := l.rows.ListByConnector(ctx, id)
	if err != nil {
		return Domain{}, err
	}
	if i := slices.IndexFunc(rows, func(r store.DNSDomain) bool { return r.Name == name }); i >= 0 {
		r := rows[i]
		r.ShareAll, r.OrgIDs = d.ShareAll, store.StringList(orgIDs)
		return d, l.rows.Update(ctx, r)
	}
	z, err := l.zone(ctx, p, name)
	if err != nil {
		return Domain{}, err
	}
	if !z.OurDNS {
		return Domain{}, errs.Conflictf("%s uses DNS elsewhere; take over its DNS first", name)
	}
	err = l.rows.Create(ctx, store.DNSDomain{
		ID:          uuid.NewString(),
		ConnectorID: id,
		OrgID:       orgID,
		Name:        name,
		ShareAll:    d.ShareAll,
		OrgIDs:      store.StringList(orgIDs),
		CreatedAt:   time.Now().UTC(),
	})
	if _, ok := errs.IsConflict(err); ok {
		return Domain{}, errs.Conflictf("%s is managed through another connector", name)
	}
	return d, err
}

// Unmanage forgets a managed domain and returns its row. The caller has
// refused it while tiles use the domain and deleted stackr's records
// (Forget) first.
func (l *Leaf) Unmanage(ctx context.Context, id, name string) (store.DNSDomain, error) {
	name = Normal(name)
	rows, err := l.rows.ListByConnector(ctx, id)
	if err != nil {
		return store.DNSDomain{}, err
	}
	i := slices.IndexFunc(rows, func(r store.DNSDomain) bool { return r.Name == name })
	if i < 0 {
		return store.DNSDomain{}, errs.ErrNotFound
	}
	return rows[i], l.rows.Delete(ctx, rows[i].ID)
}

// ByConnector is the domains connector id manages.
func (l *Leaf) ByConnector(ctx context.Context, id string) ([]store.DNSDomain, error) {
	return l.rows.ListByConnector(ctx, id)
}

// Linked records the domain resource stackr made for managed domain name.
func (l *Leaf) Linked(ctx context.Context, id, name, resourceID string) error {
	rows, err := l.rows.ListByConnector(ctx, id)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(rows, func(r store.DNSDomain) bool { return r.Name == Normal(name) })
	if i < 0 {
		return errs.ErrNotFound
	}
	rows[i].ResourceID = &resourceID
	return l.rows.Update(ctx, rows[i])
}

// All is every managed domain on the server.
func (l *Leaf) All(ctx context.Context) ([]store.DNSDomain, error) { return l.rows.List(ctx) }

// MayUse says whether orgID may put tiles under managed domain d: an org
// connector's only its org, a server one its "Available to" orgs.
func MayUse(d store.DNSDomain, orgID string) bool {
	if d.OrgID != nil {
		return *d.OrgID == orgID
	}
	return d.ShareAll || slices.Contains(d.OrgIDs, orgID)
}

// TakeOver switches name to the provider's DNS. confirm must be the domain
// typed out: records at the old DNS host stop applying. label is the
// provider as the refusal names it.
func (l *Leaf) TakeOver(ctx context.Context, p Provider, label, name, confirm string) error {
	name = Normal(name)
	if strings.TrimSpace(confirm) != name || name == "" {
		return errs.Invalidf("confirm", "Type %s to confirm", name)
	}
	z, err := l.zone(ctx, p, name)
	if err != nil {
		return err
	}
	if z.OurDNS {
		return errs.Conflictf("%s already uses %s DNS", name, label)
	}
	return l.Locked(p).TakeOver(ctx, name)
}

// Usable is the managed domains orgID may use: its own connectors' and
// the server connectors' shared with it.
func (l *Leaf) Usable(ctx context.Context, orgID string) ([]store.DNSDomain, error) {
	return l.rows.ListFor(ctx, orgID)
}
