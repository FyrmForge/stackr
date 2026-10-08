package service

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/serverconfig"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domainres"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// serverLive is what the server file is diffed against: facts read from the
// store. f is the file being diffed (nil for an export); it only decides
// whether the tiles a cascade change redeploys are counted, which costs a
// Docker call per tile.
func (o *Orchestrator) serverLive(ctx context.Context, f *serverconfig.File) (serverconfig.Live, error) {
	live := serverconfig.Live{
		Settings:      map[string]string{},
		DestSchedules: map[string]int{},
		DomainHosts:   map[string]int{},
		TLSOff:        o.cfg.TLSOff,
	}
	var err error
	for _, k := range settings.Catalogue {
		if k.Scopes != settings.Flat || k.ConfigOnly {
			continue
		}
		if live.Settings[k.Key], err = o.settings.Get(ctx, k.Key); err != nil {
			return live, err
		}
	}
	if live.Defaults, err = o.settings.Defaults(ctx); err != nil {
		return live, err
	}
	if f != nil && f.Defaults != nil {
		if live.Tiles, err = o.runningTiles(ctx); err != nil {
			return live, err
		}
	}
	if live.Params, err = o.params.Values(ctx, params.ServerScope, true); err != nil {
		return live, err
	}
	if live.Routes, err = o.routes.List(ctx); err != nil {
		return live, err
	}
	if live.Dests, err = o.globalDestsWithKeys(ctx); err != nil {
		return live, err
	}
	scheds, err := o.backups.AllSchedules(ctx)
	if err != nil {
		return live, err
	}
	for _, s := range scheds {
		if s.DestID != nil {
			live.DestSchedules[*s.DestID]++
		}
	}
	if live.Domains, err = o.domainres.ListAll(ctx); err != nil {
		return live, err
	}
	ds, err := o.domains.List(ctx)
	if err != nil {
		return live, err
	}
	for _, d := range ds {
		if d.ResourceID != nil {
			live.DomainHosts[*d.ResourceID]++
		}
	}
	if live.Taken, err = o.domainHosts(ctx); err != nil {
		return live, err
	}
	if live.Orgs, err = o.orgs.ListAll(ctx); err != nil {
		return live, err
	}
	if live.Claims, err = o.claims(ctx); err != nil {
		return live, err
	}
	live.Connectors, err = o.connectorsLive(ctx)
	return live, err
}

// runningTiles is how many tiles a cascade change would redeploy.
func (o *Orchestrator) runningTiles(ctx context.Context) (int, error) {
	orgs, err := o.orgs.ListAll(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, og := range orgs {
		ts, err := o.scopeTiles(ctx, ParamScope{Kind: "org", ID: og.ID})
		if err != nil {
			return 0, err
		}
		for _, t := range ts {
			ok, err := o.reachedByRedeploy(ctx, t)
			if err != nil {
				return 0, err
			}
			if ok {
				n++
			}
		}
	}
	return n, nil
}

// globalDestsWithKeys is the global backup destinations with their keys: the
// diff compares them with the file's refs, never shows them.
func (o *Orchestrator) globalDestsWithKeys(ctx context.Context) ([]store.BackupDest, error) {
	ds, err := o.backups.Global(ctx)
	if err != nil {
		return nil, err
	}
	for i := range ds {
		if ds[i], err = o.backups.Get(ctx, ds[i].ID); err != nil {
			return nil, err
		}
	}
	return ds, nil
}

// connectorsLive is the server connectors with who they are shared with and
// which orgs bind to them.
func (o *Orchestrator) connectorsLive(ctx context.Context) ([]serverconfig.ConnectorLive, error) {
	cs, err := o.conns.ListServer(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]serverconfig.ConnectorLive, 0, len(cs))
	for _, c := range cs {
		cl := serverconfig.ConnectorLive{Connector: c}
		if cl.OrgIDs, err = o.conns.SharedOrgs(ctx, c.ID); err != nil {
			return nil, err
		}
		bs, err := o.connectorBindings(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		for _, b := range bs {
			if b.orgID != "" && !slices.Contains(cl.Bound, b.orgID) {
				cl.Bound = append(cl.Bound, b.orgID)
			}
		}
		out = append(out, cl)
	}
	return out, nil
}

// seedDestParams stores the keys of each global S3 destination as server
// params (the names serverconfig.DestParamCollection and DestKeys give), for
// the ones not stored yet: the exported file refers to them. An existing
// param is the admin's and stays.
func (o *Orchestrator) seedDestParams(ctx context.Context) error {
	ds, err := o.globalDestsWithKeys(ctx)
	if err != nil {
		return err
	}
	have, err := o.params.Values(ctx, params.ServerScope, true)
	if err != nil {
		return err
	}
	var es []ParamEntry
	seen := map[string]bool{}
	for _, d := range ds {
		coll := serverconfig.DestParamCollection(d.Name)
		if d.Kind != backup.S3 || d.OrgID != nil || seen[coll] {
			continue // a name clash is Export's error to name
		}
		seen[coll] = true
		for i, v := range []string{d.AccessKey, d.SecretKey, d.ArchiveKey} {
			key := serverconfig.DestKeys[i]
			if v != "" && have[coll+"."+key].V == "" {
				es = append(es, ParamEntry{Collection: coll, Name: key, Kind: params.Secret, Value: v})
			}
		}
	}
	if len(es) == 0 {
		return nil
	}
	_, err = o.SetParams(ctx, ServerParamScope, es)
	return err
}

// applyServerPlan fetches the plan's file (the stored bytes of a local plan,
// the repo at the plan's commit otherwise), diffs it again (the server may
// have moved since the plan), and walks the changes the approver let through.
func (o *Orchestrator) applyServerPlan(ctx context.Context, job serverApplyJob, log io.Writer) error {
	ctx, queued := withRedeploys(ctx)
	pl, err := o.serverPlans.Get(ctx, job.PlanID)
	if err != nil {
		return err
	}
	data := []byte(pl.File)
	if pl.Source != sourceLocal {
		if data, _, err = o.serverFile(ctx, pl.Commit, log); err != nil {
			return err
		}
	}
	f, err := serverconfig.Parse(data)
	if err != nil {
		return err
	}
	live, err := o.serverLive(ctx, f)
	if err != nil {
		return err
	}
	plan := o.limitBlock(ctx, f, serverconfig.Diff(f, live))
	if plan.Blocked() {
		return fmt.Errorf("blocked: %s", strings.Join(plan.Blockers, "; "))
	}
	// a removal row applies only when the approver ticked it
	plan.Plan = plan.OnlyTicked(pl.Ticked)
	if err := newRisk(pl, plan); err != nil {
		return err
	}
	w := &serverWalk{o: o, ctx: ctx, f: f, live: live, plan: plan, approver: job.ApproverID, log: log, orgIDs: map[string]string{}}
	for _, og := range live.Orgs {
		w.orgIDs[og.Slug] = og.ID
	}
	if err := w.run(); err != nil {
		return err
	}
	return queued.await(ctx, o, log)
}

// newRisk refuses an apply whose re-diff holds a change with an impact line
// the approved plan did not: the approver confirmed (or the plan needed no
// confirm, auto-applied ones included) only for what the plan showed.
func newRisk(pl ServerPlan, now serverconfig.Plan) error {
	var was serverconfig.Plan
	_ = json.Unmarshal([]byte(pl.Plan), &was) // an unreadable stored plan knows nothing: every risk is new
	seen := map[string]bool{}
	if pl.Confirmed {
		for _, c := range was.Changes {
			if c.Impact != "" {
				seen[c.Kind+"|"+c.Tile+"|"+c.Field] = true
			}
		}
	}
	for _, c := range now.Changes {
		if c.Impact != "" && !seen[c.Kind+"|"+c.Tile+"|"+c.Field] {
			return fmt.Errorf("the server changed since the approve (%s); plan again", c.Impact)
		}
	}
	return nil
}

// serverWalk applies a server plan step by step, each change through its
// verb, in the order the plan documents: server params, connector shares,
// settings and the cascade rung, backup destinations (then the panel's
// backup destination, which may name one just made), domains, routes, orgs,
// then the removals that were ticked.
type serverWalk struct {
	o        *Orchestrator
	ctx      context.Context
	f        *serverconfig.File
	live     serverconfig.Live
	plan     serverconfig.Plan
	approver string
	log      io.Writer
	orgIDs   map[string]string // org slug -> id; grows as the walk creates orgs
}

func (w *serverWalk) run() error {
	for _, step := range []struct {
		name string
		do   func() error
	}{
		{"params", w.params},
		{"connector shares", w.shares},
		{"settings", w.settings},
		{"defaults", w.defaults},
		{"backup destinations", w.dests},
		{"panel backup destination", w.panelDest},
		{"domains", w.domains},
		{"routes", w.routes},
		{"orgs", w.orgs},
		{"removals", w.removals},
	} {
		if err := step.do(); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
	}
	return nil
}

// pick is the plan's lines of these kinds, removal rows aside, in plan order.
func (w *serverWalk) pick(kinds ...string) []serverconfig.Change {
	var out []serverconfig.Change
	for _, c := range w.plan.Changes {
		if !c.Optional && slices.Contains(kinds, c.Kind) {
			out = append(out, c)
		}
	}
	return out
}

func (w *serverWalk) say(c serverconfig.Change) {
	_, _ = fmt.Fprintf(w.log, "%s %s\n", c.Kind, cmp.Or(c.Tile, c.Field, c.New))
}

func (w *serverWalk) params() error {
	var es []ParamEntry
	for _, c := range w.pick("param", "param-update") {
		w.say(c)
		col, name, _ := strings.Cut(c.Field, ".")
		decl := w.f.Params[col][name]
		e := ParamEntry{Collection: col, Name: name, Kind: decl.Type}
		if decl.Value != nil {
			e.Value = *decl.Value
		}
		es = append(es, e)
	}
	if len(es) == 0 {
		return nil
	}
	_, err := w.o.SetParams(w.ctx, ServerParamScope, es)
	return err
}

// shares grants the file's new connector shares to the orgs that exist; a
// share to an org this apply creates waits for the orgs step (applyShares).
func (w *serverWalk) shares() error {
	_, err := w.applyShares(w.pick("connector-share"))
	return err
}

// applyShares adds each row's org (Field "org") or share-all (Field "share") to its connector's shares and
// answers the rows whose org does not exist yet.
func (w *serverWalk) applyShares(rows []serverconfig.Change) (later []serverconfig.Change, err error) {
	byConn := map[string][]serverconfig.Change{}
	var order []string
	for _, c := range rows {
		if _, ok := byConn[c.Tile]; !ok {
			order = append(order, c.Tile)
		}
		byConn[c.Tile] = append(byConn[c.Tile], c)
	}
	for _, name := range order {
		id, ok := w.connectorID(name)
		if !ok {
			return nil, fmt.Errorf("no server connector named %s", name)
		}
		cur, err := w.o.conns.Server(w.ctx, id)
		if err != nil {
			return nil, err
		}
		named, err := w.o.conns.SharedOrgs(w.ctx, id)
		if err != nil {
			return nil, err
		}
		all, changed := cur.ShareAll, false
		for _, c := range byConn[name] {
			w.say(c)
			// the row's Field tells share-all from an org whose slug is "all"
			switch oid, known := w.orgIDs[c.New]; {
			case c.Field == "share":
				all, changed = true, true
			case all:
				// already shared with every org
			case !known:
				later = append(later, c)
			case !slices.Contains(named, oid):
				named, changed = append(named, oid), true
			}
		}
		if changed {
			if _, err := w.o.ShareConnector(w.ctx, id, named, all); err != nil {
				return nil, fmt.Errorf("share %s: %w", name, err)
			}
		}
	}
	return later, nil
}

func (w *serverWalk) connectorID(name string) (string, bool) {
	i := slices.IndexFunc(w.live.Connectors, func(c serverconfig.ConnectorLive) bool { return c.Connector.Name == name })
	if i < 0 {
		return "", false
	}
	return w.live.Connectors[i].Connector.ID, true
}

// settings writes the file's flat knobs in one save, so the proxy is pushed
// once. A knob's value is the file's, not the plan line's (which is cut).
func (w *serverWalk) settings() error {
	vals := map[string]string{}
	for _, c := range w.pick("settings") {
		if c.Field == "panel_backup_dest" {
			continue // names a destination: after the destinations step
		}
		w.say(c)
		v, _ := w.f.Settings[c.Field].(string)
		vals[c.Field] = v
	}
	if len(vals) == 0 {
		return nil
	}
	return w.o.SetSettings(w.ctx, vals)
}

// defaults writes the server rung of the cascade from the file's block; a
// field the block leaves out is cleared.
func (w *serverWalk) defaults() error {
	rows := w.pick("defaults")
	if len(rows) == 0 {
		return nil
	}
	w.say(rows[0])
	d := w.f.Defaults
	if d == nil {
		d = &serverconfig.Defaults{}
	}
	vals := map[string]string{
		"cpu_limit":        "",
		"mem_limit_mb":     "",
		"protect":          "",
		"protect_user":     "",
		"protect_password": "",
	}
	if d.CPULimit != nil {
		vals["cpu_limit"] = strconv.FormatFloat(*d.CPULimit, 'f', -1, 64)
	}
	if d.MemLimitMB != nil {
		vals["mem_limit_mb"] = strconv.Itoa(*d.MemLimitMB)
	}
	if d.Protect != nil {
		vals["protect"] = strconv.FormatBool(*d.Protect)
	}
	if d.ProtectUser != nil {
		vals["protect_user"] = *d.ProtectUser
	}
	if d.ProtectPassword != nil {
		vals["protect_password"] = *d.ProtectPassword
	}
	return w.o.SetSettingDefaults(w.ctx, vals)
}

// globalDest is the global S3 destination named name, as it is now.
func (w *serverWalk) globalDest(name string) (BackupDest, bool, error) {
	ds, err := w.o.GlobalBackupDests(w.ctx)
	i := slices.IndexFunc(ds, func(d BackupDest) bool { return d.Name == name && d.Kind == backup.S3 })
	if err != nil || i < 0 {
		return BackupDest{}, false, err
	}
	return ds[i], true, nil
}

// dests creates and updates the destinations the file names, once each,
// whatever number of its fields changed. The keys are read from the server
// params the file refers to.
func (w *serverWalk) dests() error {
	done := map[string]bool{}
	for _, c := range w.pick("dest", "dest-update") {
		if done[c.Tile] {
			continue
		}
		done[c.Tile] = true
		w.say(c)
		i := slices.IndexFunc(w.f.BackupDests, func(d serverconfig.Dest) bool { return d.Name == c.Tile })
		if i < 0 {
			return fmt.Errorf("no backup_dests entry for %s", c.Tile)
		}
		fd := w.f.BackupDests[i]
		spec := BackupDestSpec{
			Name:     fd.Name,
			Endpoint: fd.Endpoint,
			Region:   fd.Region,
			Bucket:   fd.Bucket,
			Shared:   fd.Shared,
		}
		var err error
		if spec.AccessKey, err = w.o.ExpandServerRefs(w.ctx, fd.AccessKey); err != nil {
			return fmt.Errorf("%s: %w", fd.Name, err)
		}
		if spec.SecretKey, err = w.o.ExpandServerRefs(w.ctx, fd.SecretKey); err != nil {
			return fmt.Errorf("%s: %w", fd.Name, err)
		}
		cur, exists, err := w.globalDest(fd.Name)
		if err != nil {
			return err
		}
		if exists {
			_, err = w.o.UpdateBackupDest(w.ctx, "", cur.ID, spec)
		} else {
			err = w.createDest(fd, spec)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", fd.Name, err)
		}
	}
	return nil
}

// createDest makes the destination. The leaf gives every one a random archive
// key; a file that carries the key (a rebuilt box reading the old archives)
// has it written over that one.
// ponytail: straight to the store, since the leaf's Dest has no archive key;
// give it one if a second caller needs it.
func (w *serverWalk) createDest(fd serverconfig.Dest, spec BackupDestSpec) error {
	d, err := w.o.CreateBackupDest(w.ctx, nil, spec)
	if err != nil || fd.ArchiveKey == "" {
		return err
	}
	if d, err = w.o.backups.Get(w.ctx, d.ID); err != nil {
		return err
	}
	if d.ArchiveKey, err = w.o.ExpandServerRefs(w.ctx, fd.ArchiveKey); err != nil {
		return err
	}
	return w.o.store.BackupDests.Update(w.ctx, d)
}

// panelDest points the panel's scheduled backup at the destination the file
// names ("" or local = the install's own).
func (w *serverWalk) panelDest() error {
	for _, c := range w.pick("settings") {
		if c.Field != "panel_backup_dest" {
			continue
		}
		w.say(c)
		name, _ := w.f.Settings[c.Field].(string)
		id := ""
		if name != "" && name != backup.Local {
			d, ok, err := w.globalDest(name)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("no backup destination named %s", name)
			}
			id = d.ID
		}
		return w.o.SetSetting(w.ctx, c.Field, id)
	}
	return nil
}

func (w *serverWalk) domains() error {
	done := map[string]bool{}
	for _, c := range w.pick("domain-rename", "domain", "domain-update") {
		w.say(c)
		var err error
		switch c.Kind {
		case "domain-rename":
			err = w.renameDomain(c.Old, c.New)
		case "domain":
			err = w.createDomain(c.New)
		case "domain-update":
			if done[c.Tile] {
				continue
			}
			done[c.Tile] = true
			err = w.updateDomain(c.Tile)
		}
		if err != nil {
			return fmt.Errorf("%s %s: %w", c.Kind, cmp.Or(c.Tile, c.New), err)
		}
	}
	return nil
}

func (w *serverWalk) domainEntry(host string) (serverconfig.Domain, error) {
	i := slices.IndexFunc(w.f.Domains, func(d serverconfig.Domain) bool { return d.Host == installspec.CleanHost(host) })
	if i < 0 {
		return serverconfig.Domain{}, fmt.Errorf("no domains: entry for %s", host)
	}
	return w.f.Domains[i], nil
}

func (w *serverWalk) renameDomain(from, to string) error {
	r, ok, err := w.o.instanceRow(w.ctx, from)
	if err != nil || !ok {
		return cmp.Or(err, fmt.Errorf("no instance domain %s", from))
	}
	_, err = w.o.RenameDomainResource(w.ctx, r.ID, to)
	return err
}

func (w *serverWalk) createDomain(host string) error {
	fd, err := w.domainEntry(host)
	if err != nil {
		return err
	}
	_, err = w.o.CreateDomainResource(w.ctx, domainres.Instance, "", fd.Host, fd.IncludeEnvOnDefault, fd.ACMEEmail)
	return err
}

func (w *serverWalk) updateDomain(host string) error {
	fd, err := w.domainEntry(host)
	if err != nil {
		return err
	}
	r, ok, err := w.o.instanceRow(w.ctx, fd.Host)
	if err != nil || !ok {
		return cmp.Or(err, fmt.Errorf("no instance domain %s", fd.Host))
	}
	_, err = w.o.UpdateDomainResource(w.ctx, r.ID, fd.IncludeEnvOnDefault, fd.ACMEEmail)
	return err
}

func (w *serverWalk) routes() error {
	done := map[string]bool{}
	for _, c := range w.pick("route", "route-update") {
		if done[c.Tile] {
			continue
		}
		done[c.Tile] = true
		w.say(c)
		i := slices.IndexFunc(w.f.Routes, func(r serverconfig.Route) bool { return r.Host == c.Tile })
		if i < 0 {
			return fmt.Errorf("no routes: entry for %s", c.Tile)
		}
		fr := w.f.Routes[i]
		if c.Kind == "route-update" { // the service has no route update: replace it
			if err := w.deleteRoute(fr.Host); err != nil {
				return fmt.Errorf("route-update %s: %w", fr.Host, err)
			}
		}
		_, err := w.o.CreateExternalRoute(w.ctx, ExternalRouteSpec{
			Host:     fr.Host,
			Mode:     fr.Mode,
			Target:   fr.Target,
			Insecure: fr.Insecure,
		})
		if err != nil {
			return fmt.Errorf("%s %s: %w", c.Kind, fr.Host, err)
		}
	}
	return nil
}

func (w *serverWalk) deleteRoute(host string) error {
	rs, err := w.o.ExternalRoutes(w.ctx)
	i := slices.IndexFunc(rs, func(r ExternalRoute) bool { return r.Host == host })
	if err != nil || i < 0 {
		return cmp.Or(err, errs.ErrNotFound)
	}
	return w.o.DeleteExternalRoute(w.ctx, rs[i].ID)
}

// orgs creates the orgs the file names (the approver is the owner), grants
// the shares that waited for them, then binds every org the file binds to
// its org file, which plans it.
func (w *serverWalk) orgs() error {
	var bind []string
	for _, c := range w.pick("org-create") {
		w.say(c)
		if w.approver == "" {
			return fmt.Errorf("org-create %s: no approver to own the new org; approve this plan with a session or key", c.Tile)
		}
		og, err := w.o.CreateNamedOrg(w.ctx, w.approver, c.New)
		if err != nil {
			return fmt.Errorf("org-create %s: %w", c.Tile, err)
		}
		if og.Slug != c.Tile {
			return fmt.Errorf("org-create %s: the org came out as %s", c.Tile, og.Slug)
		}
		w.orgIDs[og.Slug] = og.ID
		if i := slices.IndexFunc(w.f.Orgs, func(o serverconfig.Org) bool { return o.Slug == c.Tile }); i >= 0 && w.f.Orgs[i].Repo != "" {
			bind = append(bind, c.Tile)
		}
	}
	later, err := w.applyShares(w.pick("connector-share"))
	if err != nil {
		return err
	}
	for _, c := range later {
		if _, ok := w.orgIDs[c.New]; !ok {
			return fmt.Errorf("connector-share %s: no organization %q", c.Tile, c.New)
		}
	}
	for _, c := range w.pick("org-bind") {
		if !slices.Contains(bind, c.Tile) {
			bind = append(bind, c.Tile)
		}
	}
	for _, slug := range bind {
		_, _ = fmt.Fprintf(w.log, "org-bind %s\n", slug)
		if err := w.bindOrg(slug); err != nil {
			return fmt.Errorf("org-bind %s: %w", slug, err)
		}
	}
	return nil
}

// bindOrg binds the org to the repo the file gives it. A file that names no
// connector keeps the org's own, and drops a server one.
func (w *serverWalk) bindOrg(slug string) error {
	i := slices.IndexFunc(w.f.Orgs, func(o serverconfig.Org) bool { return o.Slug == slug })
	if i < 0 {
		return fmt.Errorf("no orgs: entry for %s", slug)
	}
	fo := w.f.Orgs[i]
	og, err := w.o.orgs.Get(w.ctx, w.orgIDs[slug])
	if err != nil {
		return err
	}
	connID := og.ConfigConnectorID
	if fo.Connector != "" {
		id, ok := w.connectorID(fo.Connector)
		if !ok {
			return fmt.Errorf("no server connector named %s", fo.Connector)
		}
		connID = id
	} else if w.connectorName(connID) != "" {
		connID = ""
	}
	_, err = w.o.SetOrgConfigRepo(w.ctx, og.ID, connID, fo.Repo, fo.Branch, fo.Path, fo.Auto)
	return err
}

// connectorName is a server connector's name by id; "" when it is not one.
func (w *serverWalk) connectorName(id string) string {
	i := slices.IndexFunc(w.live.Connectors, func(c serverconfig.ConnectorLive) bool { return c.Connector.ID == id })
	if i < 0 {
		return ""
	}
	return w.live.Connectors[i].Connector.Name
}

// removals applies the removal rows the approver ticked (the plan was cut to
// those), each through the verb that refuses what is still in use.
func (w *serverWalk) removals() error {
	for _, c := range w.plan.Changes {
		if !c.Optional {
			continue
		}
		w.say(c)
		var err error
		switch c.Kind {
		case "route-delete":
			err = w.deleteRoute(c.Tile)
		case "dest-delete":
			err = w.deleteDest(c.Tile)
		case "domain-delete":
			err = w.deleteDomain(c.Tile)
		case "connector-share-delete":
			err = w.unshare(c.Tile, c.Field, c.Old)
		case "org-unbind":
			_, err = w.o.SetOrgConfigRepo(w.ctx, w.orgIDs[c.Tile], "", "", "", "", false)
		default:
			err = fmt.Errorf("no verb for a %s row", c.Kind)
		}
		if err != nil {
			return fmt.Errorf("%s %s: %w", c.Kind, c.Tile, err)
		}
	}
	return nil
}

func (w *serverWalk) deleteDest(name string) error {
	d, ok, err := w.globalDest(name)
	if err != nil || !ok {
		return cmp.Or(err, errs.ErrNotFound)
	}
	return w.o.DeleteBackupDest(w.ctx, "", d.ID)
}

func (w *serverWalk) deleteDomain(host string) error {
	r, ok, err := w.o.instanceRow(w.ctx, host)
	if err != nil || !ok {
		return cmp.Or(err, errs.ErrNotFound)
	}
	return w.o.DeleteDomainResource(w.ctx, r.ID)
}

// unshare takes one org (field "org") or share-all (field "share") out of a
// connector's shares. Leaving share-all falls back to the orgs the file lists.
func (w *serverWalk) unshare(conn, field, from string) error {
	id, ok := w.connectorID(conn)
	if !ok {
		return fmt.Errorf("no server connector named %s", conn)
	}
	named, err := w.o.conns.SharedOrgs(w.ctx, id)
	if err != nil {
		return err
	}
	if field == "share" {
		i := slices.IndexFunc(w.f.Connectors, func(c serverconfig.Connector) bool { return c.Name == conn })
		if i >= 0 && w.f.Connectors[i].Share != nil {
			for _, slug := range w.f.Connectors[i].Share.Orgs {
				if oid, ok := w.orgIDs[slug]; ok && !slices.Contains(named, oid) {
					named = append(named, oid)
				}
			}
		}
	} else {
		named = slices.DeleteFunc(named, func(oid string) bool { return oid == w.orgIDs[from] })
	}
	_, err = w.o.ShareConnector(w.ctx, id, named, false)
	return err
}
