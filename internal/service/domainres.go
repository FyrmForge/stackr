package service

import (
	"context"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domainres"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// DomainResource is a host stackr names tiles under, at instance, org or
// stack level (REWRITE.md "Domain resources").
type DomainResource = store.DomainResource

// CreateDomainResource adds a declared resource. level is instance, org or
// stack; ownerID the org or stack id ("" at instance level). The squat
// check counts the host against the owner's org. A resource with its own
// ACME email re-pushes the proxy, where the account lives.
func (o *Orchestrator) CreateDomainResource(
	ctx context.Context,
	level, ownerID, host string,
	includeEnvOnDefault bool,
	acmeEmail string,
) (DomainResource, error) {
	ownOrgID := ""
	switch level {
	case domainres.Org:
		og, err := o.orgs.Get(ctx, ownerID)
		if err != nil {
			return DomainResource{}, err
		}
		ownOrgID = og.ID
	case domainres.Stack:
		st, err := o.stacks.Get(ctx, ownerID)
		if err != nil {
			return DomainResource{}, err
		}
		ownOrgID = st.OrgID
	}
	orgs, err := o.orgs.ListAll(ctx)
	if err != nil {
		return DomainResource{}, err
	}
	spec := domainres.Spec{
		Level:               level,
		OwnerID:             ownerID,
		Host:                host,
		IncludeEnvOnDefault: includeEnvOnDefault,
		ACMEEmail:           acmeEmail,
	}
	if err := o.checkRouteHost(ctx, host); err != nil {
		return DomainResource{}, err
	}
	r, err := o.domainres.Create(ctx, spec, ownOrgID, orgs)
	if err != nil || r.ACMEEmail == "" {
		return r, err
	}
	return r, o.sync.Sync(ctx)
}

// UpdateDomainResource sets the env flag and the ACME email; a moved email
// re-pushes the proxy. Auto names already written keep their host until
// their stack's next promote.
func (o *Orchestrator) UpdateDomainResource(
	ctx context.Context,
	id string,
	includeEnvOnDefault bool,
	acmeEmail string,
) (DomainResource, error) {
	r, err := o.domainres.Get(ctx, id)
	if err != nil {
		return r, err
	}
	was := r.ACMEEmail
	r, err = o.domainres.Update(ctx, r, includeEnvOnDefault, acmeEmail)
	if err != nil || r.ACMEEmail == was {
		return r, err
	}
	return r, o.sync.Sync(ctx)
}

// DeleteDomainResource is refused while tile domains carry its name.
func (o *Orchestrator) DeleteDomainResource(ctx context.Context, id string) error {
	r, err := o.domainres.Get(ctx, id)
	if err != nil {
		return err
	}
	ds, err := o.domains.List(ctx)
	if err != nil {
		return err
	}
	named := 0
	for _, d := range ds {
		if d.ResourceID != nil && *d.ResourceID == r.ID {
			named++
		}
	}
	err = o.domainres.Delete(ctx, r, named)
	if err != nil || r.ACMEEmail == "" {
		return err
	}
	return o.sync.Sync(ctx)
}

// DomainResources is the org drawer's list: the org's own rows, then the
// instance's, nearest first. Stack rows belong to their stack file.
func (o *Orchestrator) DomainResources(ctx context.Context, orgID string) ([]DomainResource, error) {
	return o.domainres.Visible(ctx, "", orgID)
}

// AllDomainResources is every resource on the server (admin).
func (o *Orchestrator) AllDomainResources(ctx context.Context) ([]DomainResource, error) {
	return o.domainres.ListAll(ctx)
}

// RenameImpact is what renaming a resource moves, for the plan's impact
// line: resources renamed, tile hosts rewritten (each keeps a redirect from
// its old name) and the certificates those hosts need, one per HTTPS host.
type RenameImpact struct {
	Resources int `json:"resources"`
	Hosts     int `json:"hosts"`
	Certs     int `json:"certs"`
}

// renamePlan is one rename, checked and not yet written.
type renamePlan struct {
	res      []DomainResource // the resources that move, r first
	to       []string         // their new hosts
	rows     []Domain         // tile domains rewritten
	rowTo    []string
	reclaim  []Domain          // generated redirects on a host a moved row takes
	repoint  []Domain          // generated redirects to a host that moves
	repointT map[string]string // old host -> new, for repoint
}

// ownerOf is the org a resource's host counts against for the squat check.
func ownerOf(r DomainResource) string {
	if r.OrgID == nil {
		return ""
	}
	return *r.OrgID
}

// generatedRedirect is the row a rename leaves on an old host: auto with a
// redirect_to, which the stack file grammar forbids together, so a promote
// can tell it from a redirect the file declares.
func generatedRedirect(d Domain) bool { return d.Auto && d.RedirectTo != "" }

// renameSet is what one rename covers: r, and for the instance row the
// undeclared <org slug>.<host> rows FinishOrg made under it (the org's tiles
// nest under those, so leaving them would move nothing).
func (o *Orchestrator) renameSet(ctx context.Context, r DomainResource) ([]DomainResource, []store.Org, error) {
	orgs, err := o.orgs.ListAll(ctx)
	if err != nil {
		return nil, nil, err
	}
	set := []DomainResource{r}
	if r.Level != domainres.Instance {
		return set, orgs, nil
	}
	all, err := o.domainres.ListAll(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, c := range all {
		if c.Level != domainres.Org || c.Declared || c.OrgID == nil {
			continue
		}
		i := slices.IndexFunc(orgs, func(og store.Org) bool { return og.ID == *c.OrgID })
		if i >= 0 && c.Host == orgs[i].Slug+"."+r.Host {
			set = append(set, c)
		}
	}
	return set, orgs, nil
}

// rowsOf are the tile domains the set names: the auto and apex hosts that
// move. Literal hosts carry no resource; a generated redirect carries none
// either.
func rowsOf(set []DomainResource, ds []Domain) []Domain {
	var out []Domain
	for _, d := range ds {
		named := d.ResourceID != nil && slices.ContainsFunc(set, func(r DomainResource) bool { return r.ID == *d.ResourceID })
		if named && d.RedirectTo == "" {
			out = append(out, d)
		}
	}
	return out
}

// DomainResourceImpact counts what renaming id moves: the numbers behind
// the plan's "moves N hosts, issues N certificates" line.
func (o *Orchestrator) DomainResourceImpact(ctx context.Context, id string) (RenameImpact, error) {
	r, err := o.domainres.Get(ctx, id)
	if err != nil {
		return RenameImpact{}, err
	}
	set, _, err := o.renameSet(ctx, r)
	if err != nil {
		return RenameImpact{}, err
	}
	ds, err := o.domains.List(ctx)
	if err != nil {
		return RenameImpact{}, err
	}
	imp := RenameImpact{Resources: len(set)}
	for _, d := range rowsOf(set, ds) {
		imp.Hosts++
		if d.HTTPS {
			imp.Certs++
		}
	}
	return imp, nil
}

// planRename checks the whole rename and writes nothing: the new host of
// every resource and every tile domain against the squat check, the
// external routes, the panel host and the other rows that hold a name.
func (o *Orchestrator) planRename(ctx context.Context, r DomainResource, host string) (renamePlan, error) {
	var p renamePlan
	set, orgs, err := o.renameSet(ctx, r)
	if err != nil {
		return p, err
	}
	newRoot, err := domainres.CheckRename(r, host, ownerOf(r), orgs)
	if err != nil {
		return p, err
	}
	panel := installspec.CleanHost(o.panelDomain(ctx))
	all, err := o.domainres.ListAll(ctx)
	if err != nil {
		return p, err
	}
	for _, c := range set {
		to, _ := domainres.Rehost(r.Host, newRoot, c.Host)
		if to, err = domainres.CheckRename(c, to, ownerOf(c), orgs); err != nil {
			return p, err
		}
		if err := o.checkRenamedHost(ctx, to, panel); err != nil {
			return p, err
		}
		held := slices.ContainsFunc(all, func(x DomainResource) bool {
			return x.Host == to && !slices.ContainsFunc(set, func(s DomainResource) bool { return s.ID == x.ID })
		})
		if held {
			return p, errs.Conflictf("%s is already a domain resource.", to)
		}
		p.res, p.to = append(p.res, c), append(p.to, to)
	}
	ds, err := o.domains.List(ctx)
	if err != nil {
		return p, err
	}
	p.rows = rowsOf(set, ds)
	moved := map[string]string{}
	for _, d := range p.rows {
		var to string
		for i, c := range p.res {
			if d.ResourceID != nil && *d.ResourceID == c.ID {
				to, _ = domainres.Rehost(c.Host, p.to[i], d.Host)
			}
		}
		if d.RawCaddy != "" {
			return p, errs.Conflictf("%s has a raw Caddy route that names its host; edit that first.", d.Host)
		}
		if err := o.checkRenamedHost(ctx, to, panel); err != nil {
			return p, err
		}
		p.rowTo = append(p.rowTo, to)
		moved[d.Host] = to
	}
	for _, d := range ds {
		if slices.ContainsFunc(p.rows, func(m Domain) bool { return m.ID == d.ID }) {
			continue
		}
		for i, m := range p.rows {
			if d.Host != p.rowTo[i] || d.Path != m.Path {
				continue
			}
			// Only a redirect on the renamer's own moved tiles is theirs to take
			// back; another tile's, another org's included, still holds its host.
			own := slices.ContainsFunc(p.rows, func(r Domain) bool { return r.TileID == d.TileID })
			if !generatedRedirect(d) || !own {
				return p, errs.Conflictf("%s%s is already attached to a tile.", d.Host, d.Path)
			}
			p.reclaim = append(p.reclaim, d)
		}
	}
	for _, d := range ds {
		to, ok := moved[d.RedirectTo]
		if ok && generatedRedirect(d) && !slices.ContainsFunc(p.reclaim, func(x Domain) bool { return x.ID == d.ID }) {
			p.repoint = append(p.repoint, d)
			if p.repointT == nil {
				p.repointT = map[string]string{}
			}
			p.repointT[d.ID] = to
		}
	}
	return p, nil
}

// checkRenamedHost refuses a new name an external route holds or the panel
// answers on.
func (o *Orchestrator) checkRenamedHost(ctx context.Context, host, panel string) error {
	if host == panel {
		return errs.Conflictf("%s is the panel's own host.", host)
	}
	return o.checkRouteHost(ctx, host)
}

// panelDomain is the host the panel answers on, "" when none is set.
func (o *Orchestrator) panelDomain(ctx context.Context) string {
	v, _ := o.settings.Get(ctx, "panel_domain")
	return v
}

// applyRename writes a checked rename: the resources, then the tile domains
// (each old host keeps a generated redirect to its new one), no proxy push.
// ponytail: no transaction across the leaves; the checks above leave a
// failed write only to a store error, and the caller still pushes so the
// proxy matches whatever landed.
func (o *Orchestrator) applyRename(ctx context.Context, p renamePlan, orgs []store.Org) error {
	dns01 := o.dns01(ctx)
	for i, c := range p.res {
		if _, err := o.domainres.Rename(ctx, c, p.to[i], ownerOf(c), orgs); err != nil {
			return err
		}
	}
	for _, d := range p.reclaim {
		if err := o.domains.Detach(ctx, d); err != nil {
			return err
		}
	}
	for i, d := range p.rows {
		s, err := specOf(d)
		if err != nil {
			return err
		}
		s.Host = p.rowTo[i]
		if _, err := o.domains.Update(ctx, d, s, dns01); err != nil {
			return err
		}
	}
	for _, d := range p.repoint {
		s, err := specOf(d)
		if err != nil {
			return err
		}
		s.RedirectTo = p.repointT[d.ID]
		if _, err := o.domains.Update(ctx, d, s, dns01); err != nil {
			return err
		}
	}
	for i, d := range p.rows {
		_, err := o.domains.Attach(ctx, d.TileID, DomainSpec{
			Host:       d.Host,
			Path:       d.Path,
			HTTPS:      &d.HTTPS,
			ForceHTTPS: &d.ForceHTTPS,
			RedirectTo: p.rowTo[i],
			Auto:       true,
		}, dns01, nil)
		if err != nil {
			return err
		}
	}
	return nil
}

// RenameDomainResource moves a domain resource to a new host: every auto and
// apex host it named is rewritten (literal hosts stay), each old host stays
// as a redirect to its new one until removed, one proxy push. Instance and
// org level only: a stack's domains are its stack file's. The instance row
// takes the undeclared org rows under it along, and root_domain follows.
// Everything is checked before the first write.
func (o *Orchestrator) RenameDomainResource(ctx context.Context, id, host string) (DomainResource, error) {
	r, err := o.domainres.Get(ctx, id)
	if err != nil {
		return r, err
	}
	p, err := o.planRename(ctx, r, host)
	if err != nil {
		return r, err
	}
	orgs, err := o.orgs.ListAll(ctx)
	if err != nil {
		return r, err
	}
	err = o.applyRename(ctx, p, orgs)
	if err == nil && r.Level == domainres.Instance {
		err = o.followRoot(ctx, r.Host, p.to[0])
	}
	// Push even after a failed write: the proxy should match what landed.
	if perr := o.sync.Sync(ctx); err == nil {
		err = perr
	}
	r.Host = p.to[0]
	return r, err
}

// followRoot points the root_domain setting at a renamed instance row, so a
// later boot cannot read the old root as a new one. A root that named some
// other host is left alone.
func (o *Orchestrator) followRoot(ctx context.Context, oldHost, newHost string) error {
	root, err := o.settings.Get(ctx, "root_domain")
	if err != nil || strings.TrimPrefix(installspec.CleanHost(root), "*.") != oldHost {
		return err
	}
	if strings.HasPrefix(installspec.CleanHost(root), "*.") {
		newHost = "*." + newHost
	}
	return o.settings.Set(ctx, "root_domain", newHost)
}

// instanceRow is the instance resource whose host is host.
func (o *Orchestrator) instanceRow(ctx context.Context, host string) (DomainResource, bool, error) {
	all, err := o.domainres.ListAll(ctx)
	i := slices.IndexFunc(all, func(r DomainResource) bool { return r.Level == domainres.Instance && r.Host == host })
	if err != nil || i < 0 {
		return DomainResource{}, false, err
	}
	return all[i], true, nil
}

// checkRootDomain is SetSettings' check for a root_domain value: a domain
// the installer would take, and a rename of the instance row that passes
// planRename. Nothing is written.
func (o *Orchestrator) checkRootDomain(ctx context.Context, raw string) error {
	old, err := o.settings.Get(ctx, "root_domain")
	if err != nil {
		return err
	}
	root, err := installspec.CheckRoot(raw)
	if err != nil && raw != "" {
		return errs.Invalidf("root_domain", "%s", err.Error())
	}
	oldBare, bare := strings.TrimPrefix(installspec.CleanHost(old), "*."), strings.TrimPrefix(root, "*.")
	switch {
	case raw == "" && oldBare != "":
		return errs.Invalidf("root_domain", "Name the new root domain; the instance domain is removed from the domains list.")
	case bare == oldBare:
		return nil
	}
	inst, ok, err := o.rootRow(ctx, oldBare, bare)
	if err != nil || !ok {
		return err
	}
	_, err = o.planRename(ctx, inst, bare)
	return err
}

// rootRow is the instance row a root_domain move from oldBare to bare
// renames. ok false means nothing to rename: the row already sits on bare,
// or there is no instance row at all (the new root seeds one). An instance
// row on neither host is refused: the setting would move and the row stay.
func (o *Orchestrator) rootRow(ctx context.Context, oldBare, bare string) (DomainResource, bool, error) {
	all, err := o.domainres.ListAll(ctx)
	if err != nil {
		return DomainResource{}, false, err
	}
	var inst, onNew *DomainResource
	found := false
	for i, r := range all {
		if r.Level != domainres.Instance {
			continue
		}
		found = true
		switch r.Host {
		case oldBare:
			inst = &all[i]
		case bare:
			onNew = &all[i]
		}
	}
	switch {
	case inst != nil:
		return *inst, true, nil
	case onNew == nil && found:
		return DomainResource{}, false, errs.Conflictf("The instance domain is not %s; rename it from the domains list.", oldBare)
	}
	return DomainResource{}, false, nil
}

// setRootDomain writes a checked root_domain value, stored cleaned (SetSettings
// pushes once): the instance row whose host is the old root is renamed, or,
// with no instance row at all, the new root seeds one.
func (o *Orchestrator) setRootDomain(ctx context.Context, raw string) error {
	old, err := o.settings.Get(ctx, "root_domain")
	if err != nil {
		return err
	}
	clean := ""
	if raw != "" {
		if clean, err = installspec.CheckRoot(raw); err != nil {
			return errs.Invalidf("root_domain", "%s", err.Error())
		}
	}
	oldBare, bare := strings.TrimPrefix(installspec.CleanHost(old), "*."), strings.TrimPrefix(clean, "*.")
	if bare != oldBare {
		inst, ok, err := o.rootRow(ctx, oldBare, bare)
		if err != nil {
			return err
		}
		if ok {
			p, err := o.planRename(ctx, inst, bare)
			if err != nil {
				return err
			}
			orgs, err := o.orgs.ListAll(ctx)
			if err != nil {
				return err
			}
			if err := o.applyRename(ctx, p, orgs); err != nil {
				_ = o.sync.Sync(ctx)
				return err
			}
		} else if err := o.domainres.SeedInstance(ctx, clean); err != nil {
			return err
		}
	}
	return o.settings.Set(ctx, "root_domain", clean)
}
