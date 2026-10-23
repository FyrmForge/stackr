package service

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/connector"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// connusage.go is where a connection is used, each capability saying its
// own: DNS the tile domains with records under it, Git the stacks, orgs
// and tiles that clone through it. The settings dialog's Used by tab and
// a refused Remove list them.

// Usage is one place a connection is used: Path its org / stack / env /
// tile by name, What what it does there. Level is the drawer it opens
// (org, stack or tile), ID that row's id; Org, Stack and Env the slugs of
// the pages above it ("" past the level).
type Usage struct {
	Path            string `json:"path"`
	What            string `json:"what"`
	Level           string `json:"level"`
	ID              string `json:"id"`
	OrgID           string `json:"org_id"`
	Org, Stack, Env string `json:"-"`
}

// InUse is a refusal because a connection (or one of its domains) is used:
// the usages say where. It is a Conflict.
type InUse struct {
	Msg, Tail string // before and after the list
	Usages    []Usage
}

func (e *InUse) Error() string {
	paths := make([]string, len(e.Usages))
	for i, u := range e.Usages {
		paths[i] = u.Path + " (" + u.What + ")"
	}
	return e.Msg + " " + strings.Join(paths, ", ") + e.Tail
}

// As makes it an errs.Conflict, its paths in the message.
func (e *InUse) As(target any) bool {
	if c, ok := target.(*errs.Conflict); ok {
		*c = errs.Conflict{Msg: e.Error()}
		return true
	}
	return false
}

// ConnectorUsages is where connection id is used, as viewerOrg sees it
// (nil = a server admin, who sees every org's): an org sees its own rows
// only, a shared server connection included.
func (o *Orchestrator) ConnectorUsages(ctx context.Context, viewerOrg *string, id string) ([]Usage, error) {
	c, err := o.store.Connectors.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	var out []Usage
	if connector.Has(c, CapDNS) {
		us, err := o.dnsUsages(ctx, c, "")
		if err != nil {
			return nil, err
		}
		out = append(out, us...)
	}
	if connector.Has(c, CapGit) {
		us, err := o.gitUsages(ctx, c)
		if err != nil {
			return nil, err
		}
		out = append(out, us...)
	}
	if viewerOrg != nil {
		out = slices.DeleteFunc(out, func(u Usage) bool { return u.OrgID != *viewerOrg })
	}
	slices.SortStableFunc(out, func(a, b Usage) int { return cmp.Compare(a.Path, b.Path) })
	return out, nil
}

// usageOf is the Usage of a scope (tile, env, stack or org) doing what.
func (o *Orchestrator) usageOf(ctx context.Context, kind, id, what string) (Usage, error) {
	ch, og, err := o.chain(ctx, kind, id)
	if err != nil {
		return Usage{}, err
	}
	u := Usage{What: what, Level: kind, ID: id, OrgID: og.ID, Org: og.Slug}
	names := []string{og.Name}
	for i := len(ch) - 1; i >= 0; i-- {
		s := ch[i]
		if s.kind == ScopeOrg {
			continue
		}
		names = append(names, s.name)
		switch s.kind {
		case ScopeStack:
			st, err := o.store.Stacks.Get(ctx, s.id)
			if err != nil {
				return Usage{}, err
			}
			u.Stack = st.Slug
		case ScopeEnv:
			e, err := o.store.Environments.Get(ctx, s.id)
			if err != nil {
				return Usage{}, err
			}
			u.Env = e.Slug
		}
	}
	u.Path = strings.Join(names, " / ")
	return u, nil
}

// dnsUsages is one row per tile domain with records stackr made under c,
// zone "" = every domain c manages.
func (o *Orchestrator) dnsUsages(ctx context.Context, c store.Connector, zone string) ([]Usage, error) {
	rs, err := o.connectorRecords(ctx, c)
	if err != nil {
		return nil, err
	}
	types := map[string][]string{}
	var order []string
	for _, r := range rs {
		if r.DomainID == nil || zone != "" && r.Zone != zone {
			continue
		}
		if types[*r.DomainID] == nil {
			order = append(order, *r.DomainID)
		}
		types[*r.DomainID] = append(types[*r.DomainID], r.Type)
	}
	var out []Usage
	for _, did := range order {
		d, err := o.store.Domains.Get(ctx, did)
		if errors.Is(err, errs.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		slices.Sort(types[did])
		u, err := o.usageOf(ctx, ScopeTile, d.TileID, strings.Join(slices.Compact(types[did]), ", ")+" record "+d.Host)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

// connectorRecords is the records stackr made on c's managed domains.
func (o *Orchestrator) connectorRecords(ctx context.Context, c store.Connector) ([]store.DNSRecord, error) {
	zs, err := o.dns.ByConnector(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for _, z := range zs {
		ids[z.ID] = true
	}
	rs, err := o.dns.Records(ctx)
	return slices.DeleteFunc(rs, func(r store.DNSRecord) bool { return r.DNSDomainID == nil || !ids[*r.DNSDomainID] }), err
}

// gitUsages is the server file's binding, each org's config file and each
// stack's config repo that name c, and each tile whose repo clones
// through it.
func (o *Orchestrator) gitUsages(ctx context.Context, c store.Connector) ([]Usage, error) {
	var out []Usage
	ogs, err := o.orgs.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	for _, og := range ogs {
		if c.OrgID != nil && *c.OrgID != og.ID {
			continue
		}
		if og.ConfigConnectorID == c.ID {
			u, err := o.usageOf(ctx, ScopeOrg, og.ID, "config file from "+repoAt(og.ConfigRepo, og.ConfigBranch))
			if err != nil {
				return nil, err
			}
			out = append(out, u)
		}
		conns, err := o.conns.ListConnected(ctx, og.ID)
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(conns, func(x store.Connector) bool { return x.ID == c.ID }) {
			continue // a server connection not shared with this org
		}
		sts, err := o.stacks.List(ctx, og.ID)
		if err != nil {
			return nil, err
		}
		for _, st := range sts {
			if st.ConfigRepo != "" && (st.ConfigConnectorID == c.ID ||
				st.ConfigConnectorID == "" && resolvesTo(conns, st.ConfigRepo, c.ID)) {
				u, err := o.usageOf(ctx, ScopeStack, st.ID, "clones "+repoAt(st.ConfigRepo, st.ConfigBranch))
				if err != nil {
					return nil, err
				}
				out = append(out, u)
			}
			ts, err := o.tiles.ListByStack(ctx, st.ID)
			if err != nil {
				return nil, err
			}
			for _, t := range ts {
				if t.GitURL == "" || !resolvesTo(conns, t.GitURL, c.ID) {
					continue
				}
				u, err := o.usageOf(ctx, ScopeTile, t.ID, "clones "+repoAt(t.GitURL, t.GitBranch))
				if err != nil {
					return nil, err
				}
				out = append(out, u)
			}
		}
	}
	return out, nil
}

// resolvesTo: a clone of repo through conns (an org's) picks connection id.
func resolvesTo(conns []store.Connector, repo, id string) bool {
	c, err := connector.Resolve(conns, "", connector.Host(repo))
	return err == nil && c.ID == id
}

// repoAt words repo at branch: owner/name@branch.
func repoAt(repo, branch string) string {
	s := strings.TrimSuffix(repo, ".git")
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if _, rest, ok := strings.Cut(s, "/"); ok {
		s = rest
	}
	if branch != "" {
		s += "@" + branch
	}
	return s
}

// RecordRow is one record stackr made under a connection: Domain the
// managed domain it sits in, Host the tile domain it serves (Usage its
// tile, nil when none wants it any more), Node the ingress node it points
// at.
type RecordRow struct {
	Domain, Name, Type, Value, Host, Node string
	Usage                                 *Usage
}

// ConnectorRecords is every record stackr made under connection id of
// orgID (nil = a server one), from its own records, by domain then name:
// never a provider call, never a record stackr did not make.
func (o *Orchestrator) ConnectorRecords(ctx context.Context, orgID *string, id string) ([]RecordRow, error) {
	c, err := o.conns.Of(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	rs, err := o.connectorRecords(ctx, c)
	if err != nil {
		return nil, err
	}
	def, err := o.DefaultIngress(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RecordRow, 0, len(rs))
	for _, r := range rs {
		row := RecordRow{Domain: r.Zone, Name: r.Name, Type: r.Type, Value: r.Value}
		if r.DomainID != nil {
			if d, err := o.store.Domains.Get(ctx, *r.DomainID); err == nil {
				row.Host = d.Host
				if u, err := o.usageOf(ctx, ScopeTile, d.TileID, ""); err == nil {
					row.Usage = &u
				}
				nodeID, err := o.IngressFor(ctx, d.TileID)
				if err != nil {
					return nil, err
				}
				if n, err := o.store.Nodes.Get(ctx, cmp.Or(nodeID, def)); err == nil {
					row.Node = n.Name
				}
			}
		}
		out = append(out, row)
	}
	slices.SortStableFunc(out, func(a, b RecordRow) int {
		return cmp.Or(cmp.Compare(a.Domain, b.Domain), cmp.Compare(a.Name, b.Name), cmp.Compare(a.Type, b.Type))
	})
	return out, nil
}
