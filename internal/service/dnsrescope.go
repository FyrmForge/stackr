package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/dnscap"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domainres"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// dnsrescope.go changes what part of a managed domain stackr manages
// without unmanaging it: no tile gives up a hostname. Widening offers the
// records now inside that point at our nodes for adoption; narrowing hands
// the records stackr made outside back to the user (they stay in the zone,
// stackr stops updating them). The domain resource named after the scope
// follows it.

// DNSRescopePlan is what changing a managed domain's scope does: the
// records handed back (outside the new scope, they become the user's), the
// ones stackr writes, the existing ones it may adopt, and the domain
// resource generated names hang under when it moves ("" = it stays). From
// is the scope it was planned from, which the confirm sends back.
type DNSRescopePlan struct {
	From     string     `json:"from"`
	Scope    string     `json:"scope,omitempty"`
	HandBack []DNSWrite `json:"hand_back"`
	Writes   []DNSWrite `json:"writes"`
	Adopt    []DNSAdopt `json:"adopt"`
	Resource string     `json:"resource,omitempty"`
}

// rescopeRes is what a rescope does to the domain resource: rename the one
// stackr made, drop it for a row of ours already at the new host, or make
// one there. All nil: nothing.
type rescopeRes struct {
	rename, drop *DomainResource
	make         *domainres.Spec
	host         string
}

// rescopeOf is connector id's managed domain and the scope asked for ("" =
// the whole domain).
func (o *Orchestrator) rescopeOf(ctx context.Context, orgID *string, id, domain, under string) (Connector, dnscap.Provider, store.DNSDomain, string, error) {
	c, p, err := o.dnsAccount(ctx, orgID, id)
	if err != nil {
		return c, p, store.DNSDomain{}, "", err
	}
	rows, err := o.dns.ByConnector(ctx, c.ID)
	if err != nil {
		return c, p, store.DNSDomain{}, "", err
	}
	i := slices.IndexFunc(rows, func(r store.DNSDomain) bool { return r.Name == dnscap.Normal(domain) })
	if i < 0 {
		return c, p, store.DNSDomain{}, "", errs.ErrNotFound
	}
	scope, err := dnscap.Scope(domain, under)
	return c, p, rows[i], scope, err
}

// PlanRescopeDNSDomain is what RescopeDNSDomain would do, for the confirm
// that comes first. under is the new scope ("" = the whole domain). A
// change it would refuse is refused here, before anything is asked.
func (o *Orchestrator) PlanRescopeDNSDomain(ctx context.Context, orgID *string, id, domain, under string) (DNSRescopePlan, error) {
	c, p, row, scope, err := o.rescopeOf(ctx, orgID, id, domain, under)
	if err != nil {
		return DNSRescopePlan{}, err
	}
	plan, _, err := o.rescopePlan(ctx, c, p, row, scope)
	return plan, err
}

// RescopeDNSDomain changes managed domain's scope to under ("" = the whole
// domain) and adopts the records in adopt the plan lists. Refused while a
// generated or wildcard hostname would fall outside the new scope, or
// while tile domains carry the name of a domain resource that would move.
// from is the plan's From (nil = no plan): refused once the scope moved
// on since. One rescope of a domain at a time.
func (o *Orchestrator) RescopeDNSDomain(ctx context.Context, orgID *string, id, domain, under string, from *string, adopt []DNSRef) (DNSDomain, error) {
	// ponytail: per-domain lock in the repo lock map, one process; a db lock if rescopes ever run on two controllers
	m, _ := o.repoLocks.LoadOrStore("rescope "+dnscap.Normal(domain), &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	c, p, row, scope, err := o.rescopeOf(ctx, orgID, id, domain, under)
	if err != nil {
		return DNSDomain{}, err
	}
	if from != nil && dnscap.Normal(*from) != row.Scope {
		return DNSDomain{}, errs.Conflictf("The scope of %s changed since this plan. Review the plan again.", row.Name)
	}
	plan, res, err := o.rescopePlan(ctx, c, p, row, scope)
	if err != nil {
		return DNSDomain{}, err
	}
	var take []DNSRef
	for _, a := range plan.Adopt {
		if slices.Contains(adopt, a.DNSRef) {
			take = append(take, a.DNSRef)
		}
	}
	d, err := o.dns.Rescope(ctx, c.ID, row.Name, scope, take)
	if err != nil {
		return DNSDomain{}, err
	}
	if err := o.moveResource(ctx, c, row.Name, res); err != nil {
		return DNSDomain{}, err
	}
	o.dnsChanged(ctx)
	ids := []string(d.OrgIDs)
	if ids == nil {
		ids = []string{}
	}
	return DNSDomain{Name: d.Name, State: DNSManaged, ShareAll: d.ShareAll, OrgIDs: ids, Scope: d.Scope}, nil
}

// moveResource does what res says to managed domain zone's resource.
func (o *Orchestrator) moveResource(ctx context.Context, c Connector, zone string, res rescopeRes) error {
	switch {
	case res.rename != nil: // no squat check: the domain's own name, as managing makes it
		_, err := o.domainres.Rename(ctx, *res.rename, res.host, ownerOf(*res.rename), nil)
		return err
	case res.drop != nil:
		if err := o.dns.Linked(ctx, c.ID, zone, ""); err != nil {
			return err
		}
		return o.domainres.Delete(ctx, *res.drop, 0)
	case res.make != nil:
		r, err := o.domainres.Adopt(ctx, *res.make, ownerOf(DomainResource{OrgID: c.OrgID}), nil)
		if err != nil {
			return err
		}
		return o.dns.Linked(ctx, c.ID, zone, r.ID)
	}
	return nil
}

// rescopePlan is row's change to scope: the plan, what happens to its
// domain resource, or the refusal.
func (o *Orchestrator) rescopePlan(ctx context.Context, c Connector, p dnscap.Provider, row store.DNSDomain, scope string) (DNSRescopePlan, rescopeRes, error) {
	to := row
	to.Scope = scope
	ds, err := o.domains.List(ctx)
	if err != nil {
		return DNSRescopePlan{}, rescopeRes{}, err
	}
	// a generated name cannot become the user's, and a wildcard outside
	// the scope loses its certificate
	var users []Usage
	for _, d := range ds {
		name := dnscap.Name(d.Host, row.Name)
		if name == "" || !dnscap.InScope(row, name) || dnscap.InScope(to, name) {
			continue
		}
		if d.Auto || d.ResourceID != nil || strings.HasPrefix(d.Host, "*.") && d.HTTPS {
			u, err := o.usageOf(ctx, ScopeTile, d.TileID, d.Host)
			if err != nil {
				return DNSRescopePlan{}, rescopeRes{}, err
			}
			users = append(users, u)
		}
	}
	if len(users) > 0 {
		return DNSRescopePlan{}, rescopeRes{}, &InUse{
			Msg: "These hostnames would fall outside " + dnscap.Under(to) + ":", Tail: ". Remove them first, or pick a scope that covers them", Usages: users,
		}
	}
	res, err := o.rescopeResource(ctx, c, row, dnscap.Under(to), ds)
	if err != nil {
		return DNSRescopePlan{}, rescopeRes{}, err
	}
	mp, err := o.planZone(ctx, p, to)
	if err != nil {
		return DNSRescopePlan{}, rescopeRes{}, err
	}
	plan := DNSRescopePlan{From: row.Scope, Scope: scope,HandBack: []DNSWrite{}, Writes: mp.Writes, Adopt: mp.Adopt}
	if res.rename != nil || res.drop != nil || res.make != nil {
		plan.Resource = res.host
	}
	recs, err := o.dns.Records(ctx)
	if err != nil {
		return DNSRescopePlan{}, rescopeRes{}, err
	}
	for _, r := range dnscap.HandBack(to, recs) {
		plan.HandBack = append(plan.HandBack, DNSWrite{Host: zoneHost(r.Name, row.Name), Type: r.Type, Value: r.Value})
	}
	return plan, res, nil
}

// rescopeResource is what moving row's scope to host does to its domain
// resource. A row stackr made is renamed, or dropped when a row of ours
// already holds host; none made (one existed when it was managed) leaves
// that one and makes one at host, as managing does. Refused while tile
// domains carry the made row's name: their names would change on the
// next promote.
func (o *Orchestrator) rescopeResource(ctx context.Context, c Connector, row store.DNSDomain, host string, ds []Domain) (rescopeRes, error) {
	res := rescopeRes{host: host}
	var cur *DomainResource
	if row.ResourceID != nil {
		r, err := o.domainres.Get(ctx, *row.ResourceID)
		switch {
		case err == nil:
			cur = &r
		case !errors.Is(err, errs.ErrNotFound):
			return res, err
		}
	}
	if cur != nil && cur.Host == host {
		return res, nil
	}
	spec, made, err := o.managedResource(ctx, c.OrgID, host)
	if err != nil {
		return res, err
	}
	if cur == nil {
		if made {
			if _, err := domainres.Prepare(spec, ownerOf(DomainResource{OrgID: c.OrgID}), nil); err != nil {
				return res, err
			}
			res.make = &spec
		}
		return res, nil
	}
	var users []Usage
	for _, d := range ds {
		if d.ResourceID != nil && *d.ResourceID == cur.ID {
			u, err := o.usageOf(ctx, ScopeTile, d.TileID, d.Host)
			if err != nil {
				return res, err
			}
			users = append(users, u)
		}
	}
	if len(users) > 0 {
		return res, &InUse{
			Msg: "The domain resource " + cur.Host + " names", Tail: ". Their names would change on the next promote; remove them first", Usages: users,
		}
	}
	if !made {
		res.drop = cur
		return res, nil
	}
	if _, err := domainres.CheckRename(*cur, host, ownerOf(*cur), nil); err != nil {
		return res, err
	}
	res.rename = cur
	return res, nil
}
