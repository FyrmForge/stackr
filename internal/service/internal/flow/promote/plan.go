package promote

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/address"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domainres"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/planfile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Change is one line of a plan, planfile's shape. Kind is env | stack |
// param | volume | orphan | create | update | delete | domain | slice |
// managed | image. Values that may be credentials (env, params) never
// appear, only that they moved.
type Change = planfile.Change

// Plan is what a promote would do. Blockers is the one answer to "what
// blocks this promote" (B20): the dry run shows it, the real run refuses
// with it, whoever calls.
type Plan struct {
	Stack    string   `json:"stack"`
	Env      string   `json:"env"`
	Release  int      `json:"release,omitempty"`
	Changes  []Change `json:"changes"`
	Blockers []string `json:"blockers,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	// Deployed is the ids of the tiles Apply rolled out, in order (the
	// orchestrator queues on_deploy runs from it). Empty on a dry run.
	Deployed []string `json:"-"`
	// Removed is the ids of the tiles Apply took out (their run logs go).
	Removed []string `json:"-"`
}

// shownValues are the tile keys whose old and new values the plan prints:
// never a credential, and what a reader of a cron, function or setting diff
// needs. Never env, build_args, command or files (they carry secrets), nor
// the healthcheck command, so "healthcheck" prints its timings only.
var shownValues = map[string]func(store.Tile) string{
	"limits": func(t store.Tile) string {
		var b []string
		if t.MemLimitMB > 0 {
			b = append(b, strconv.Itoa(t.MemLimitMB)+" MB")
		}
		if t.CPULimit > 0 {
			b = append(b, strconv.FormatFloat(t.CPULimit, 'f', -1, 64)+" cpus")
		}
		if len(b) == 0 {
			return "none"
		}
		return strings.Join(b, ", ")
	},
	"replicas":        func(t store.Tile) string { return strconv.Itoa(t.Replicas) },
	"port":            func(t store.Tile) string { return strconv.Itoa(t.ContainerPort) },
	"published_ports": func(t store.Tile) string { return t.PublishedPorts },
	"lan":             func(t store.Tile) string { return t.Lan },
	"host_network":    func(t store.Tile) string { return strconv.FormatBool(t.HostNetwork) },
	"update_policy":   func(t store.Tile) string { return t.UpdatePolicy },
	"tag_policy":      func(t store.Tile) string { return t.TagPolicy },
	"restart":         func(t store.Tile) string { return t.RestartPolicy },
	"user":            func(t store.Tile) string { return t.User },
	"shm_size_mb":     func(t store.Tile) string { return strconv.Itoa(t.ShmSizeMB) },
	"health_path":     func(t store.Tile) string { return t.HealthPath },
	"healthcheck": func(t store.Tile) string {
		return fmt.Sprintf("every %ds, timeout %ds, %d retries, start %ds",
			t.HealthcheckIntervalS, t.HealthcheckTimeoutS, t.HealthcheckRetries, t.HealthcheckStartPeriodS)
	},
	"watch_paths":     func(t store.Tile) string { return t.WatchPaths },
	"dockerfile":      func(t store.Tile) string { return t.DockerfilePath },
	"build_context":   func(t store.Tile) string { return t.BuildContext },
	"git_url":         func(t store.Tile) string { return t.GitURL },
	"depends_on":      func(t store.Tile) string { return t.DependsOn },
	"schedule":        func(t store.Tile) string { return t.Schedule },
	"trigger":         func(t store.Tile) string { return t.Trigger },
	"timeout_minutes": func(t store.Tile) string { return strconv.Itoa(t.TimeoutMinutes) },
	"provision_from":  func(t store.Tile) string { return deref(t.ProvisionFrom) },
	"default_access":  func(t store.Tile) string { return deref(t.DefaultAccess) },
	"on_remove":       func(t store.Tile) string { return deref(t.OnRemove) },
}

func (p *Plan) Blocked() bool { return len(p.Blockers) > 0 }

func (p *Plan) block(format string, a ...any) {
	p.Blockers = append(p.Blockers, fmt.Sprintf(format, a...))
}

func (p *Plan) add(c Change) { p.Changes = append(p.Changes, c) }

// work is everything the real run needs, computed once by plan so the dry
// run and the real run cannot drift apart.
type work struct {
	e      store.Environment
	st     store.Stack
	rel    store.Release
	bottom bool
	re     *ResolvedEnv // nil: a stack with no config repo, live state is desired state

	envEdit   *store.Environment
	envBlob   string
	stackBlob *string
	orgs      []store.Org            // every org, for the squat check
	resCreate []store.DomainResource // the file's new stack resources, ids made here
	resUpdate []store.DomainResource // env flag or ACME email moved
	visible   []store.DomainResource // what auto and apex resolve against, the above included
	isDefault bool                   // the env is the ladder's top rung (DECIDE 192)
	params    []params.Entry
	// paramScope (set by planParams) is where params land: the env's own scope.
	paramScope params.Scope
	prParams   []params.Entry // a static env's promote also writes the file's pr block to the stack pr scope
	drops      []paramDrop    // names the file no longer sets
	ticked     []string       // secret removal keys the caller ticked
	declare    map[string]VolumeConf
	orphan     []store.Volume
	creates    []store.Tile
	updates    [][2]store.Tile // old, new
	deletes    []store.Tile
	domains    map[string]domainWork   // by tile slug
	instances  map[string]instanceWork // managed tiles whose allow or env_pairs moved, by slug
	list       Lister                  // reads the config repo's tree at the pinned commit
	sliced     []string                // slice tiles created or moved: their consumers redeploy
	base       string                  // a PR env's base env slug; "" on a static env
	redeploy   map[string]bool
	unpin      map[string]bool // image tiles whose tag moved: run the tag, pin again
	sync       bool
	deployed   []string // tile ids rolled out, filled by apply
	removed    []string // tile ids actually removed, filled by apply
	owed       []string // tile ids a parked sync rollout still has to deploy
}

// instanceWork is a managed tile's allow list and env pairs as the file
// says; apply writes both onto its instance row.
type instanceWork struct {
	allow []string
	pairs map[string]string
}

type domainWork struct {
	add    []domain.Spec
	update map[string]domain.Spec // row id → spec
	remove []store.Domain
}

// plan diffs the release against live state for one env.
func (f *Flow) plan(ctx context.Context, envID, releaseID string, log io.Writer) (*Plan, *work, error) {
	d := f.D
	e, err := d.Envs.Get(ctx, envID)
	if err != nil {
		return nil, nil, err
	}
	st, err := d.Stacks.Get(ctx, e.StackID)
	if err != nil {
		return nil, nil, err
	}
	rel, err := d.Releases.Get(ctx, releaseID)
	if err != nil {
		return nil, nil, err
	}
	p := &Plan{
		Stack:   st.Slug,
		Env:     e.Slug,
		Release: rel.Number,
		Changes: []Change{},
	}
	w := &work{
		e:         e,
		st:        st,
		rel:       rel,
		domains:   map[string]domainWork{},
		instances: map[string]instanceWork{},
		declare:   map[string]VolumeConf{},
		redeploy:  map[string]bool{},
		unpin:     map[string]bool{},
	}
	if rel.StackID != e.StackID {
		p.block("release #%d belongs to another stack", rel.Number)
		return p, w, nil
	}
	if err := f.ladderRule(ctx, p, e, rel); err != nil {
		return nil, nil, err
	}
	if _, err := d.Envs.Below(ctx, e); errors.Is(err, errs.ErrNotFound) {
		w.bottom = e.Type == environment.Static
	} else if err != nil {
		return nil, nil, err
	}

	pins, err := d.Releases.Pins(ctx, rel.ID)
	if err != nil {
		return nil, nil, err
	}
	if cp, ok := pins[release.ConfigSlug]; ok {
		if f.Config == nil {
			return nil, nil, errors.New("promote: no config reader wired")
		}
		data, fetch, err := f.Config(ctx, st, cp.CommitSHA, log)
		if err != nil {
			p.block("read the stack file at %s: %v", short(cp.CommitSHA), err)
			return p, w, nil
		}
		o, err := d.Orgs.Get(ctx, st.OrgID)
		if err != nil {
			return nil, nil, err
		}
		r, err := Load(data, fetch, o.Slug)
		if err != nil {
			p.block("stack file at %s: %v", short(cp.CommitSHA), err)
			return p, w, nil
		}
		w.list = listOf(fetch)
		if e.BaseEnvID != nil {
			if base, err := d.Envs.Get(ctx, *e.BaseEnvID); err == nil {
				w.base = base.Slug
			}
		}
		re, ok := r.Envs[e.Slug]
		if !ok && w.base != "" {
			// A PR env is shaped like its base env's section.
			re, ok = r.Envs[w.base]
		}
		if !ok {
			p.block("the stack file at %s has no environment %s", short(cp.CommitSHA), e.Slug)
			return p, w, nil
		}
		w.re = &re
		f.dropGrantTiles(p, w, log)
		if err := f.planConfig(ctx, p, w, r); err != nil {
			return nil, nil, err
		}
	}
	if err := f.planImages(ctx, p, w, pins); err != nil {
		return nil, nil, err
	}
	return p, w, nil
}

// ladderRule is the one rule for what may land in a `from: promote` env,
// rollbacks included (B2): never a release ahead of the env below.
// DECIDE 29: no history lookup; an older release is always allowed.
func (f *Flow) ladderRule(ctx context.Context, p *Plan, e store.Environment, rel store.Release) error {
	if e.FromKind != environment.FromPromote {
		return nil
	}
	below, err := f.D.Envs.Below(ctx, e)
	if errors.Is(err, errs.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if below.ReleaseID == nil {
		p.block("%s promotes from %s, which runs nothing yet", e.Slug, below.Slug)
		return nil
	}
	br, err := f.D.Releases.Get(ctx, *below.ReleaseID)
	if err != nil {
		return err
	}
	if rel.Number > br.Number {
		p.block(
			"release #%d has not reached %s yet (it runs #%d); %s promotes from %s",
			rel.Number,
			below.Slug,
			br.Number,
			e.Slug,
			below.Slug,
		)
	}
	return nil
}

func (f *Flow) planConfig(ctx context.Context, p *Plan, w *work, r *Resolved) error {
	d, e, re := f.D, w.e, w.re

	// The env's own knobs and settings.
	ed := e
	if re.Color != "" && re.Color != e.Color {
		p.add(Change{
			Kind:  "env",
			Field: "color",
			Old:   e.Color,
			New:   re.Color,
		})
		ed.Color = re.Color
	}
	if re.FromKind != "" && (re.FromKind != e.FromKind || re.FromBranch != e.FromBranch || re.Auto != e.Auto) {
		p.add(Change{
			Kind:  "env",
			Field: "from",
			Old:   fromLabel(e.FromKind, e.FromBranch, e.Auto),
			New:   fromLabel(re.FromKind, re.FromBranch, re.Auto),
		})
		ed.FromKind, ed.FromBranch, ed.Auto = re.FromKind, re.FromBranch, re.Auto
	}
	if ed != e {
		w.envEdit = &ed
	}
	have, _ := settings.Parse(e.Settings)
	planfile.KeepSecretPair(&re.Defaults.ProtectUser, &re.Defaults.ProtectPassword, have.ProtectUser, have.ProtectPassword)
	if blob := defaultsJSON(re.Defaults); blob != canonSettings(e.Settings) {
		p.add(Change{Kind: "env", Field: "defaults"})
		w.envBlob = blob
	}
	orgs, err := d.Orgs.ListAll(ctx)
	if err != nil {
		return err
	}
	w.orgs = orgs
	// Stack-level keys land with the bottom rung, where new config enters
	// the ladder, so a rollback higher up never rewrites them (DECIDE 31).
	if w.bottom {
		have, _ := settings.Parse(w.st.Settings)
		planfile.KeepSecretPair(&r.Defaults.ProtectUser, &r.Defaults.ProtectPassword, have.ProtectUser, have.ProtectPassword)
		if blob := defaultsJSON(r.Defaults); blob != canonSettings(w.st.Settings) {
			p.add(Change{Kind: "stack", Field: "defaults"})
			w.stackBlob = &blob
		}
		if err := f.planResources(ctx, p, w, r); err != nil {
			return err
		}
	}
	// Auto and apex resolve against the rows this plan writes too, so a
	// file that declares a resource and names tiles under it lands in one
	// promote.
	pending := slices.Concat(w.resCreate, w.resUpdate)
	if w.visible, err = f.Resources.Visible(ctx, w.st.ID, w.st.OrgID, pending...); err != nil {
		return err
	}
	if w.isDefault, err = d.Envs.IsDefault(ctx, e); err != nil {
		return err
	}

	if err := f.planParams(ctx, p, w, r); err != nil {
		return err
	}
	if err := f.planVolumes(ctx, p, w); err != nil {
		return err
	}

	// Tiles.
	live, err := d.Tiles.List(ctx, e.ID)
	if err != nil {
		return err
	}
	byslug := map[string]store.Tile{}
	for _, t := range live {
		byslug[t.Slug] = t
	}
	for _, name := range slices.Sorted(maps.Keys(re.Tiles)) {
		tc := re.Tiles[name]
		if tc.Type == tile.Managed && !deploy.KnownEngine(tc.Engine) {
			p.block("tile %s: engine %q is not supported (postgres or s3)", name, tc.Engine)
			continue
		}
		row := toRow(name, tc, w.st, e)
		old, exists := byslug[name]
		if exists {
			// paused is the panel's stored intent, never the file's.
			row.ID = old.ID
			row.Name = old.Name
			row.CreatedAt = old.CreatedAt
			row.Paused = old.Paused
		}
		if err := tile.Validate(&row); err != nil {
			p.block("tile %s: %v", name, err)
			continue
		}
		if !exists {
			p.add(Change{Kind: "create", Tile: name, New: row.Kind})
			w.creates = append(w.creates, row)
			w.redeploy[name] = true
			w.unpin[name] = tile.Pulls(row)
		} else if err := f.planUpdate(ctx, p, w, old, row, tc); err != nil {
			return err
		}
		if err := f.planDomains(ctx, p, w, name, tc, old, exists); err != nil {
			return err
		}
		switch row.Kind {
		case tile.Managed:
			if err := f.planInstance(ctx, p, w, name, tc, old, exists); err != nil {
				return err
			}
		case tile.Slice:
			if err := f.planSlice(ctx, p, w, row, old, exists); err != nil {
				return err
			}
		}
	}
	// A slice that is new or moved reaches its consumers through their refs.
	for _, sl := range w.sliced {
		for _, n := range slices.Sorted(maps.Keys(re.Tiles)) {
			if refsTile(re.Tiles[n], sl) {
				w.redeploy[n] = true
			}
		}
	}
	for _, y := range slices.Sorted(maps.Keys(re.Tiles)) {
		if re.Tiles[y].Network != "host" {
			continue
		}
		for _, x := range slices.Sorted(maps.Keys(re.Tiles)) {
			if x != y && refsTile(re.Tiles[x], y) {
				p.Warnings = append(p.Warnings, fmt.Sprintf(
					"tile %s refs %s, which runs on the host network; reach it by the server address and a granted port", x, y))
			}
		}
	}
	if err := f.planDeletes(ctx, p, w, live); err != nil {
		return err
	}
	if err := f.planFiles(ctx, p, w); err != nil {
		return err
	}
	return f.checkHostAccess(ctx, p, w)
}

// refsTile: tc's env or command refs ${{ tile.<slugName>.<output> }}.
func refsTile(tc TileConf, slugName string) bool {
	return deploy.Refs(tc.Env, tc.Command, slugName)
}

func (f *Flow) planUpdate(ctx context.Context, p *Plan, w *work, old, row store.Tile, tc TileConf) error {
	name := old.Slug
	if old.Kind != row.Kind {
		p.block(
			"tile %s changes kind (%s to %s); remove it in one release and add it back in the next",
			name,
			old.Kind,
			row.Kind,
		)
		return nil
	}
	if old.Kind == tile.Managed {
		m, err := f.D.Managed.GetByTile(ctx, old.ID)
		if err != nil && !errors.Is(err, errs.ErrNotFound) {
			return err
		}
		if err == nil && m.Engine != tc.Engine {
			p.block(
				"tile %s changes engine (%s to %s); a new engine is a new instance, add it under another name",
				name,
				m.Engine,
				tc.Engine,
			)
			return nil
		}
	}
	c := tile.Diff(old, row)
	if !c.Any() {
		return nil
	}
	for _, k := range slices.Sorted(maps.Keys(c)) {
		if row.Kind == tile.Slice && (k == "provision_from" || k == "default_access") {
			continue // planSlice's one row says both
		}
		ch := Change{Kind: "update", Tile: name, Field: k}
		if v, ok := shownValues[k]; ok {
			ch.Old, ch.New = v(old), v(row)
		}
		p.add(ch)
	}
	w.updates = append(w.updates, [2]store.Tile{old, row})
	for _, fx := range tile.Effects(row.Kind, c) {
		switch fx {
		case tile.Redeploy:
			w.redeploy[name] = true
		case tile.Route:
			w.sync = true
		}
	}
	if c["image"] && tile.Pulls(row) {
		w.unpin[name] = true
	}
	return nil
}

// planResources: the file's domains: are the stack's own domain resources.
// A missing one is created, a moved env flag or ACME email updates it, and
// one the file no longer lists stays: the file never deletes a row (it may
// still name tiles, and the drawer adds rows too). Every check Create makes
// is a blocker here, so apply never stops half way.
func (f *Flow) planResources(ctx context.Context, p *Plan, w *work, r *Resolved) error {
	if len(r.Domains) == 0 {
		return nil
	}
	all, err := f.Resources.ListAll(ctx)
	if err != nil {
		return err
	}
	for _, x := range r.Domains {
		spec := domainres.Spec{
			Level:               domainres.Stack,
			OwnerID:             w.st.ID,
			Host:                x.Host,
			IncludeEnvOnDefault: x.IncludeEnvOnDefault,
			ACMEEmail:           x.ACMEEmail,
		}
		row, err := domainres.Prepare(spec, w.st.OrgID, w.orgs)
		if err != nil {
			p.block("domains: %s: %v", x.Host, err)
			continue
		}
		i := slices.IndexFunc(all, func(h store.DomainResource) bool { return h.Host == row.Host })
		if i < 0 {
			if f.RouteHeld != nil {
				held, err := f.RouteHeld(ctx, row.Host)
				if err != nil {
					return err
				}
				if held {
					p.block("domains: %s is an external route", row.Host)
					continue
				}
			}
			p.add(Change{Kind: "stack", Field: "domains", New: row.Host})
			w.resCreate = append(w.resCreate, row)
			w.sync = true // its ACME account
			continue
		}
		have := all[i]
		switch {
		case have.StackID == nil || *have.StackID != w.st.ID:
			p.block("domains: %s is already a domain resource", row.Host)
		case have.IncludeEnvOnDefault != row.IncludeEnvOnDefault || have.ACMEEmail != row.ACMEEmail:
			p.add(Change{
				Kind:  "stack",
				Field: "domains",
				Old:   row.Host,
				New:   row.Host,
				Note:  "settings change",
			})
			have.IncludeEnvOnDefault = row.IncludeEnvOnDefault
			have.ACMEEmail = row.ACMEEmail
			w.resUpdate = append(w.resUpdate, have)
			w.sync = true
		}
	}
	return nil
}

// claimHost is v0's: a literal host expands params. refs (the resolver
// refuses anything else there), apex must name a visible resource, auto is
// AutoHost under the nearest one. Only a literal takes the squat check: an
// auto or apex host derives from a resource that passed it (its first label
// is the tile's slug, which may match another org's). The id is the resource
// that named it, nil for a literal; false means blocked.
func claimHost(p *Plan, w *work, rr *params.Resolver, name string, dc DomainConf) (string, *string, bool) {
	host, resID := "", (*string)(nil)
	switch {
	case dc.Auto:
		if len(w.visible) == 0 {
			p.block(
				"tile %s: auto domain, but no domain resource is visible to this stack; add one at stack, org or server level",
				name,
			)
			return "", nil, false
		}
		res := w.visible[0]
		host = domainres.AutoHost(res, orgSlug(w), w.st.Slug, w.e.Slug, name, w.isDefault)
		resID = &res.ID
	case dc.Apex != "":
		i := slices.IndexFunc(w.visible, func(r store.DomainResource) bool {
			return r.Host == strings.ToLower(strings.TrimSpace(dc.Apex))
		})
		if i < 0 {
			p.block("tile %s: apex %q is not a domain resource visible to this stack", name, dc.Apex)
			return "", nil, false
		}
		host = w.visible[i].Host
		resID = &w.visible[i].ID
	default:
		var err error
		host, err = rr.Expand(params.InDomain, dc.Host)
		var unset errs.Unset
		switch {
		case errors.As(err, &unset):
			p.block("tile %s: domain %s needs params.%s set first", name, dc.Host, unset.Param)
			return "", nil, false
		case err != nil:
			p.block("tile %s: domain %s: %v", name, dc.Host, err)
			return "", nil, false
		}
		if domainres.CheckOrgSquat(host, w.st.OrgID, w.orgs) != nil {
			p.block("tile %s: host %q starts with another organization's slug", name, host)
			return "", nil, false
		}
	}
	return host, resID, true
}

func orgSlug(w *work) string {
	i := slices.IndexFunc(w.orgs, func(o store.Org) bool { return o.ID == w.st.OrgID })
	if i < 0 {
		return ""
	}
	return w.orgs[i].Slug
}

// planDomains: the file's domains vs the tile's rows, by host+path.
func (f *Flow) planDomains(
	ctx context.Context,
	p *Plan,
	w *work,
	name string,
	tc TileConf,
	old store.Tile,
	exists bool,
) error {
	if tc.Type == tile.Managed || tc.Type == tile.Slice {
		return nil
	}
	var have []store.Domain
	if exists {
		var err error
		if have, err = f.D.Domains.ListByTile(ctx, old.ID); err != nil {
			return err
		}
	}
	rr, err := f.resolver(ctx, w)
	if err != nil {
		return err
	}
	all, err := f.D.Domains.List(ctx)
	if err != nil {
		return err
	}
	dw := domainWork{update: map[string]domain.Spec{}}
	want := map[string]bool{}
	for _, dc := range tc.Domains {
		host, resID, ok := claimHost(p, w, rr, name, dc)
		if !ok {
			continue
		}
		sp := specOf(dc, host, tc.Port)
		sp.Auto = dc.Auto
		sp.ResourceID = resID
		key := strings.ToLower(host) + normPath(dc.Path)
		want[key] = true
		for _, o := range all {
			if o.Host+o.Path == key && o.TileID != old.ID {
				p.block("tile %s: %s is already routed to another tile", name, key)
			}
		}
		row, ok := findDomain(have, key)
		if !ok && f.RouteHeld != nil {
			held, err := f.RouteHeld(ctx, host)
			if err != nil {
				return err
			}
			if held {
				p.block("tile %s: %s is an external route", name, host)
			}
		}
		switch {
		case !ok:
			p.add(Change{Kind: "domain", Tile: name, New: key})
			dw.add = append(dw.add, sp)
		case sigOf(sp) != sigRow(row):
			p.add(Change{
				Kind: "domain",
				Tile: name,
				Old:  key,
				New:  key,
				Note: "settings change",
			})
			dw.update[row.ID] = sp
		}
	}
	for _, row := range have {
		// A generated redirect (a resource rename's old host) is no file row:
		// auto with redirect_to, which the file grammar forbids together.
		if row.Auto && row.RedirectTo != "" {
			continue
		}
		if !want[row.Host+row.Path] {
			p.add(Change{Kind: "domain", Tile: name, Old: row.Host + row.Path})
			dw.remove = append(dw.remove, row)
		}
	}
	if len(dw.add)+len(dw.update)+len(dw.remove) > 0 {
		w.domains[name] = dw
		w.sync = true
	}
	return nil
}

// planInstance: a managed tile's allow list and env pairs against its
// instance row. Either moving is a plan row and a row write on apply,
// never a redeploy: the instance runs the same either way.
func (f *Flow) planInstance(
	ctx context.Context,
	p *Plan,
	w *work,
	name string,
	tc TileConf,
	old store.Tile,
	exists bool,
) error {
	var have store.ManagedInstance
	if exists {
		m, err := f.D.Managed.GetByTile(ctx, old.ID)
		if err != nil && !errors.Is(err, errs.ErrNotFound) {
			return err
		}
		have = m
	}
	var pairsOld, pairsNew []string
	for k, v := range have.EnvPairs {
		pairsOld = append(pairsOld, k+"→"+v)
	}
	for k, v := range tc.EnvPairs {
		pairsNew = append(pairsNew, k+"→"+v)
	}
	moved := false
	for _, x := range []struct {
		field    string
		old, cur []string
	}{
		{"allow", have.Allow, tc.Allow},
		{"env_pairs", pairsOld, pairsNew},
	} {
		if d := signedDiff(x.old, x.cur); d != "" {
			p.add(Change{
				Kind:  "managed",
				Tile:  name,
				Field: x.field,
				New:   d,
			})
			moved = true
		}
	}
	if moved {
		w.instances[name] = instanceWork{allow: tc.Allow, pairs: tc.EnvPairs}
	}
	return nil
}

// signedDiff is "+added −removed", each sorted; "" when the sets match.
func signedDiff(old, cur []string) string {
	var out []string
	for _, v := range slices.Sorted(slices.Values(cur)) {
		if !slices.Contains(old, v) {
			out = append(out, "+"+v)
		}
	}
	for _, v := range slices.Sorted(slices.Values(old)) {
		if !slices.Contains(cur, v) {
			out = append(out, "−"+v)
		}
	}
	return strings.Join(out, " ")
}

// planSlice: a slice tile's target resolves at plan time (DECIDE 194), so a
// slice that cannot reach its instance blocks the promote with the reason
// instead of failing a deploy. A new slice, a moved target or a moved
// default access is a row, and every tile that refs the slice redeploys.
// ponytail: an env_pairs edit upstream that moves the target under an
// unchanged provision_from shows no row; the slice's next deploy refuses
// the move (flow/managed Provision). Compare the provision's instance here
// if that surprise needs to come earlier.
func (f *Flow) planSlice(ctx context.Context, p *Plan, w *work, row, old store.Tile, exists bool) error {
	target, ok, err := f.sliceTarget(ctx, p, w, row)
	if err != nil || !ok {
		return err
	}
	now := target + " (" + deref(row.DefaultAccess) + ")"
	c := Change{
		Kind: "slice",
		Tile: row.Slug,
		New:  now,
	}
	if exists {
		sameFrom := deref(old.ProvisionFrom) == deref(row.ProvisionFrom)
		if sameFrom && deref(old.DefaultAccess) == deref(row.DefaultAccess) {
			return nil
		}
		c.Old = deref(old.ProvisionFrom)
		if sameFrom {
			c.Old = target
		}
		c.Old += " (" + deref(old.DefaultAccess) + ")"
	}
	p.add(c)
	w.sliced = append(w.sliced, row.Slug)
	return nil
}

// sliceTarget resolves row's provision_from to "stack/env/tile" by the
// deploy's own rule (address.Resolve over deploy.Place), with this stack's
// part laid over from the file: its env_pairs, and this env's instance,
// which may land in this very promote. false: a blocker says why.
func (f *Flow) sliceTarget(ctx context.Context, p *Plan, w *work, row store.Tile) (string, bool, error) {
	name := row.Slug
	o := orgSlug(w)
	block := func(format string, a ...any) (string, bool, error) {
		p.block("slice %s: "+format, append([]any{name}, a...)...)
		return "", false, nil
	}
	rr, err := f.resolver(ctx, w)
	if err != nil {
		return "", false, err
	}
	raw, err := rr.Expand(params.InProvisionFrom, deref(row.ProvisionFrom))
	var unset errs.Unset
	switch {
	case errors.As(err, &unset):
		return block("provision_from needs params.%s set first", unset.Param)
	case err != nil:
		return block("provision_from: %v", err)
	}
	tg, err := address.ParseTarget(raw)
	if err != nil {
		return block("%v", err)
	}
	st, err := f.D.Stacks.GetBySlug(ctx, w.st.OrgID, tg.Stack)
	if errors.Is(err, errs.ErrNotFound) {
		return block("stack %s not found", tg.Stack)
	}
	if err != nil {
		return "", false, err
	}
	pl, err := f.D.Place(ctx, st, tg.Tile)
	if err != nil {
		return "", false, err
	}
	if st.ID == w.st.ID {
		tc, ok := w.re.Tiles[tg.Tile]
		if ok && tc.Type == tile.Managed {
			pl.Pairs = tc.EnvPairs
		}
		var h *address.Host
		if ok {
			h = &address.Host{
				Managed: tc.Type == tile.Managed,
				Ready:   true,
				Allow:   tc.Allow,
			}
		}
		pl.Envs[w.e.Slug] = h
	}
	me := address.Address{
		Org:   o,
		Stack: w.st.Slug,
		Env:   w.e.Slug,
		Tile:  name,
	}
	env, err := address.Resolve(o, me, w.base, tg, pl)
	if err != nil {
		return block("%v", err)
	}
	written := false
	if w.re != nil {
		_, written = w.re.Tiles[tg.Tile]
	}
	if st.ID != w.st.ID || env != w.e.Slug || !written {
		up, err := f.sourceRunning(ctx, st, env, tg.Tile)
		if err != nil {
			return "", false, err
		}
		if !up {
			return block("%s/%s/%s is not running; start it first", st.Slug, env, tg.Tile)
		}
	}
	return st.Slug + "/" + env + "/" + tg.Tile, true, nil
}

// sourceRunning: the instance a slice provisions from has a running replica.
// A missing row counts as running: the address rules own "unreachable".
func (f *Flow) sourceRunning(ctx context.Context, st store.Stack, env, slug string) (bool, error) {
	e, err := f.D.Envs.GetBySlug(ctx, st.ID, env)
	if errors.Is(err, errs.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	t, err := f.D.Tiles.GetBySlug(ctx, e.ID, slug)
	if errors.Is(err, errs.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	cs, err := f.D.Tiles.Replicas(ctx, t)
	if err != nil {
		return false, err
	}
	for _, c := range cs {
		if c.State == "running" {
			return true, nil
		}
	}
	return false, nil
}

// planDeletes: live tiles the file no longer declares. Never destructive:
// volumes are orphaned, not removed; an instance still serving slices
// outside this promote blocks it (there is no force in a promote).
func (f *Flow) planDeletes(ctx context.Context, p *Plan, w *work, live []store.Tile) error {
	gone := map[string]bool{}
	for _, t := range live {
		if _, ok := w.re.Tiles[t.Slug]; !ok {
			gone[t.ID] = true
		}
	}
	for _, t := range live {
		if !gone[t.ID] {
			continue
		}
		c := Change{Kind: "delete", Tile: t.Slug}
		if t.Kind == tile.Managed {
			m, err := f.D.Managed.GetByTile(ctx, t.ID)
			if err != nil && !errors.Is(err, errs.ErrNotFound) {
				return err
			}
			if err == nil {
				ps, err := f.D.Managed.ByInstance(ctx, m.ID)
				if err != nil {
					return err
				}
				for _, pr := range ps {
					if !gone[pr.TileID] {
						p.block("tile %s still holds a slice that stays; remove its slice tile first", t.Slug)
						break
					}
				}
				c.Note = "its data volume is orphaned and kept"
			}
		}
		p.add(c)
		w.deletes = append(w.deletes, t)
		w.sync = true
	}
	return nil
}

// pscope is where this work's params land; a sync with no file never sets it.
func (w *work) pscope() params.Scope {
	if w.paramScope.Kind != "" {
		return w.paramScope
	}
	return params.Scope{Kind: "env", ID: w.e.ID}
}

// paramDrop is a param the file no longer sets: apply deletes it. A secret
// is an optional plan row (key) and goes only when ticked.
type paramDrop struct {
	scope      params.Scope
	coll, name string
	secret     bool
	key        string
}

// planParams diffs the file's block for this env against the env's own scope
// (a PR env too: its copy of the stack's pr block). A static env also diffs
// the file's pr block against the stack's pr scope, the template new PR envs
// copy. Everything in a file-bound scope is managed: a name the block does
// not set is removed (see diffParams).
func (f *Flow) planParams(ctx context.Context, p *Plan, w *work, r *Resolved) error {
	w.paramScope = params.Scope{Kind: "env", ID: w.e.ID}
	if w.e.Type == environment.Ephemeral {
		if err := f.diffParams(ctx, p, w, w.paramScope, r.Params[planfile.PR], true, "", &w.params); err != nil {
			return err
		}
		return f.planLock(ctx, p, w)
	}
	if err := f.diffParams(ctx, p, w, w.paramScope, r.Params[w.e.Slug], false, "", &w.params); err != nil {
		return err
	}
	pr := params.Scope{Kind: "stack_pr", ID: w.st.ID}
	if err := f.diffParams(ctx, p, w, pr, r.Params[planfile.PR], false, planfile.PR, &w.prParams); err != nil {
		return err
	}
	return f.planLock(ctx, p, w)
}

// diffParams diffs block against scope, appending the writes to out. A plain
// value the panel holds differently is drift: the row shows both and applying
// sets the file's. A secret has no value in the file, so it never drifts.
// fromBranch (a PR env): a secret entry only declares, a missing one warns
// and none is ever written; a value never comes from the branch.
// A name the live scope holds and block does not set is a removal: plain
// ones apply with the plan, secrets are optional rows that need a tick.
// label names a non-env scope in the rows ("pr").
func (f *Flow) diffParams(
	ctx context.Context,
	p *Plan,
	w *work,
	scope params.Scope,
	block map[string]planfile.Entry,
	fromBranch bool,
	label string,
	out *[]params.Entry,
) error {
	have, err := f.D.Params.Values(ctx, scope, true)
	if err != nil {
		return err
	}
	where := ""
	if label != "" {
		where = label + "."
	}
	for _, key := range slices.Sorted(maps.Keys(block)) {
		decl := block[key]
		c, n, _ := strings.Cut(key, ".")
		old, ok := have[key]
		switch {
		case !decl.Secret && ok && old.Secret:
			p.block("params.%s%s is a secret; a secret is never turned back into a param", where, key)
		case decl.Secret && fromBranch && !ok:
			p.Warnings = append(p.Warnings, "params."+key+" is declared and not set; tiles that read it wait until it is")
		case decl.Secret && fromBranch:
		case decl.Secret && ok && !old.Secret:
			p.add(Change{Kind: "param", Field: where + key, Note: "becomes a secret"})
			*out = append(*out, params.Entry{Collection: c, Name: n, Kind: params.Secret})
		case decl.Secret && !ok && decl.Generate > 0:
			p.add(Change{Kind: "param", Field: where + key, Note: "generated"})
			*out = append(*out, params.Entry{
				Collection: c, Name: n, Kind: params.Secret, Value: params.Generate(decl.Generate),
			})
		case decl.Secret && !ok:
			p.Warnings = append(p.Warnings, "params."+where+key+" is declared and not set; tiles that read it wait until it is")
		case decl.Secret:
		case !ok:
			p.add(Change{Kind: "param", Field: where + key, New: decl.Value})
			*out = append(*out, params.Entry{Collection: c, Name: n, Kind: params.Param, Value: decl.Value})
		case old.V != decl.Value:
			p.add(Change{
				Kind: "param", Field: where + key, Old: old.V, New: decl.Value,
				Note: "panel has " + old.V + ", file says " + decl.Value,
			})
			*out = append(*out, params.Entry{Collection: c, Name: n, Kind: params.Param, Value: decl.Value})
		}
	}
	for _, key := range slices.Sorted(maps.Keys(have)) {
		if _, set := block[key]; set {
			continue
		}
		c, n, _ := strings.Cut(key, ".")
		d := paramDrop{scope: scope, coll: c, name: n, secret: have[key].Secret, key: "param:" + where + key}
		ch := Change{Kind: "param", Field: where + key, Note: "removed: the file no longer sets it"}
		if d.secret {
			ch.Note = "secret removed: the file no longer sets it; a generated value cannot be recovered, tick to delete"
			ch.Optional, ch.Key = true, d.key
		} else {
			ch.Old = have[key].V
		}
		p.add(ch)
		w.drops = append(w.drops, d)
	}
	return nil
}

// planLock: a file lock that differs from the env's is a row that does not
// apply; locking is a member's call, made in the panel (SetEnvLock). A tiered
// env has its tier's lock, so the file's says nothing there.
func (f *Flow) planLock(ctx context.Context, p *Plan, w *work) error {
	if w.re == nil || w.re.Locked == nil || w.e.Type == environment.Ephemeral {
		return nil
	}
	if _, in, err := f.D.Tiers.Of(ctx, w.st.OrgID, w.e.Slug); err != nil {
		return err
	} else if in {
		p.Warnings = append(p.Warnings, "environments."+w.e.Slug+".locked is ignored; "+w.e.Slug+" is in an org tier, which holds the lock")
		return nil
	}
	if *w.re.Locked != w.e.Locked {
		word := func(l bool) string { return map[bool]string{true: "locked", false: "unlocked"}[l] }
		p.add(Change{
			Kind: "lock", Tile: w.e.Slug, Old: word(w.e.Locked), New: word(*w.re.Locked),
			Note: "needs a member to apply in the panel",
		})
	}
	return nil
}

func (f *Flow) planVolumes(ctx context.Context, p *Plan, w *work) error {
	scope := volume.Scope{Kind: "env", ID: w.e.ID}
	live, err := f.D.Volumes.List(ctx, scope)
	if err != nil {
		return err
	}
	byslug := map[string]store.Volume{}
	for _, v := range live {
		if v.InstanceID == nil {
			byslug[v.Slug] = v
		}
	}
	for _, n := range slices.Sorted(maps.Keys(w.re.Volumes)) {
		vc := w.re.Volumes[n]
		v, ok := byslug[n]
		switch {
		case !ok:
			p.add(Change{Kind: "volume", New: n})
		case v.OrphanedAt != nil:
			p.add(Change{Kind: "volume", New: n, Note: "re-adopts the orphaned volume and its data"})
		case v.MaxSizeMB != vc.MaxSizeMB:
			p.add(Change{
				Kind:  "volume",
				Field: "max_size_mb",
				Old:   strconv.Itoa(v.MaxSizeMB),
				New:   strconv.Itoa(vc.MaxSizeMB),
				Tile:  n,
			})
		default:
			continue
		}
		w.declare[n] = vc
	}
	for _, n := range slices.Sorted(maps.Keys(byslug)) {
		if _, ok := w.re.Volumes[n]; !ok && byslug[n].OrphanedAt == nil {
			p.add(Change{Kind: "orphan", Old: n, Note: "the volume and its data stay; retention deletes it later"})
			w.orphan = append(w.orphan, byslug[n])
		}
	}
	return nil
}

// planImages: what the env runs now vs what the release pins.
func (f *Flow) planImages(ctx context.Context, p *Plan, w *work, pins map[string]release.Pin) error {
	cur := map[string]release.Pin{}
	if w.e.ReleaseID != nil {
		var err error
		if cur, err = f.D.Releases.Pins(ctx, *w.e.ReleaseID); err != nil {
			return err
		}
	}
	tiles, err := f.desiredTiles(ctx, w)
	if err != nil {
		return err
	}
	// With no stack file a release only moves the pins of tiles the env
	// already has: one with none of them would land nothing.
	if w.re == nil && !slices.ContainsFunc(slices.Collect(maps.Keys(pins)), func(s string) bool {
		_, ok := tiles[s]
		return ok
	}) {
		p.block("%s has none of release #%d's tiles; add them first", w.e.Slug, w.rel.Number)
	}
	for _, c := range release.Diff(cur, pins) {
		if _, ok := tiles[c.Slug]; !ok || c.Slug == release.ConfigSlug {
			continue
		}
		p.add(Change{Kind: "image", Tile: c.Slug, Note: c.Kind})
		w.redeploy[c.Slug] = true
	}
	// A tile added after the env took this release has no diff row but
	// never ran: deploy it. A stopped tile keeps its containers, so it is
	// left alone.
	live, err := f.D.Tiles.List(ctx, w.e.ID)
	if err != nil {
		return err
	}
	for _, t := range live {
		if t.Kind == tile.Function && t.Trigger == tile.OnDeploy && !w.redeploy[t.Slug] {
			// A parked promote already moved the env's release, so its resume
			// sees an empty diff: a function whose pin moved since its last
			// run is deployed now, and its run queued from plan.Deployed.
			moved, err := f.pinMovedSinceRun(ctx, t, pins[t.Slug])
			if err != nil {
				return err
			}
			if moved {
				p.add(Change{Kind: "image", Tile: t.Slug, Note: "changed since its last run"})
				w.redeploy[t.Slug] = true
			}
		}
		if _, ok := pins[t.Slug]; !ok || w.redeploy[t.Slug] || t.Slug == release.ConfigSlug || tile.RunToCompletion(t.Kind) {
			continue
		}
		cs, err := f.D.Tiles.Replicas(ctx, t)
		if err != nil {
			return err
		}
		if len(cs) == 0 {
			p.add(Change{Kind: "image", Tile: t.Slug, Note: "not deployed yet"})
			w.redeploy[t.Slug] = true
			continue
		}
		// A promote that parked at deploy time already moved the env's
		// release, so its resume sees an empty diff: a tile still running
		// another image than the release pins is deployed now.
		if f.runsOther(ctx, t, w.e, cs) {
			p.add(Change{Kind: "image", Tile: t.Slug, Note: "runs another image"})
			w.redeploy[t.Slug] = true
		}
	}
	for _, n := range slices.Sorted(maps.Keys(tiles)) {
		if tiles[n] && pins[n].ImageID == nil {
			p.block("tile %s has no build in release #%d; build a commit first", n, w.rel.Number)
		}
	}
	return nil
}

// pinMovedSinceRun reports whether t's newest run was made on another pin
// than cur. No runs, or none that recorded a release, read as not moved.
func (f *Flow) pinMovedSinceRun(ctx context.Context, t store.Tile, cur release.Pin) (bool, error) {
	if f.D.Runs == nil || cur.Slug == "" {
		return false, nil
	}
	r, ok, err := f.D.Runs.Last(ctx, t.ID)
	if err != nil || !ok || r.ReleaseID == nil {
		return false, err
	}
	old, err := f.D.Releases.Pins(ctx, *r.ReleaseID)
	if err != nil {
		return false, err
	}
	o, ok := old[t.Slug]
	if !ok {
		return false, nil
	}
	return len(release.Diff(map[string]release.Pin{t.Slug: o}, map[string]release.Pin{t.Slug: cur})) > 0, nil
}

// runsOther reports whether a running replica of t was started on an image
// other than the one e's release pins for it.
// A replica names the ref it was started for in its stackr.ref label; one
// without it (started before the label) falls back to the image it names.
// ponytail: that fallback cannot compare a tag-started tile with a digest
// pin, so it reads as current; the label is the fix and ages the case out.
func (f *Flow) runsOther(ctx context.Context, t store.Tile, e store.Environment, cs []docker.Container) bool {
	ref, pinned, err := f.D.Current(ctx, t, e)
	if err != nil || !pinned || ref == "" {
		return false
	}
	return slices.ContainsFunc(cs, func(c docker.Container) bool {
		if c.State != "running" {
			return false
		}
		if l := c.Labels[deploy.LabelRef]; l != "" {
			return l != ref
		}
		return c.Image != "" && c.Image != ref &&
			(strings.Contains(c.Image, "@") || !strings.Contains(ref, "@"))
	})
}

// desiredTiles is slug → "builds from git" of what the env will run after
// the promote.
func (f *Flow) desiredTiles(ctx context.Context, w *work) (map[string]bool, error) {
	out := map[string]bool{}
	if w.re != nil {
		for n, tc := range w.re.Tiles {
			out[n] = builds(tc)
		}
		return out, nil
	}
	live, err := f.D.Tiles.List(ctx, w.e.ID)
	for _, t := range live {
		out[t.Slug] = tile.Builds(t)
	}
	return out, err
}

// builds: the file's tile is a git build (a service, or a cron or function
// without an image).
func builds(tc TileConf) bool {
	return tc.Type == tile.Service || (tile.RunToCompletion(tc.Type) && tc.Image == "")
}

func (f *Flow) resolver(ctx context.Context, w *work) (*params.Resolver, error) {
	s, err := f.D.ParamSnapshot(ctx, w.e, w.st, true)
	if err != nil {
		return nil, err
	}
	for _, e := range w.params { // this promote's own values count
		s.EnvParams[e.Collection+"."+e.Name] = params.Value{V: e.Value, Secret: e.Kind == params.Secret}
	}
	for _, d := range w.drops { // and so do its plain removals
		if !d.secret && d.scope == w.pscope() {
			delete(s.EnvParams, d.coll+"."+d.name)
		}
	}
	return params.NewResolver(s), nil
}

// toRow is the tile row the file asks for. A built tile with no git_url
// builds from the config repo, on the config branch unless it names one.
func toRow(name string, tc TileConf, st store.Stack, e store.Environment) store.Tile {
	t := store.Tile{
		StackID:                 e.StackID,
		EnvironmentID:           e.ID,
		Name:                    name,
		Slug:                    name,
		Kind:                    tc.Type,
		ImageRef:                tc.Image,
		Command:                 tc.Command,
		ContainerPort:           tc.Port,
		PublishedPorts:          lines(tc.PublishedPorts),
		EndpointProtocol:        tc.EndpointProtocol,
		HealthPath:              tc.HealthPath,
		HealthcheckCmd:          tc.Healthcheck,
		HealthcheckIntervalS:    tc.HealthInterval,
		HealthcheckTimeoutS:     tc.HealthTimeout,
		HealthcheckRetries:      tc.HealthRetries,
		HealthcheckStartPeriodS: tc.HealthStart,
		User:                    tc.User,
		ShmSizeMB:               tc.ShmSizeMB,
		Privileged:              tc.Privileged,
		Devices:                 lines(tc.Devices),
		Lan:                     lines(tc.LAN),
		HostNetwork:             tc.Network == "host",
		RestartPolicy:           tc.Restart,
		DependsOn:               lines(tc.DependsOn),
		Files:                   lines(tc.Files),
		Volumes:                 lines(tc.Volumes),
		Replicas:                tc.Replicas,
		UpdatePolicy:            tc.UpdatePolicy,
		TagPolicy:               tc.TagPolicy,
		EnvJSON:                 jsonMap(tc.Env),
		Schedule:                tc.Schedule,
		Trigger:                 tc.Trigger,
		TimeoutMinutes:          tc.TimeoutMinutes,
	}
	if tc.Type == tile.Slice {
		access := cmp.Or(tc.DefaultAccess, tile.Write)
		onRemove := cmp.Or(tc.OnRemove, tile.Keep)
		t.ProvisionFrom = &tc.ProvisionFrom
		t.DefaultAccess = &access
		t.OnRemove = &onRemove
	}
	for _, a := range tc.SliceAccess {
		t.SliceAccess = append(t.SliceAccess, store.SliceAccess{
			From:   a.From,
			Access: a.Access,
		})
	}
	if tc.Limits != nil {
		t.CPULimit = tc.Limits.CPU
		t.MemLimitMB = tc.Limits.MemoryMB
	}
	if builds(tc) {
		t.GitURL = or(tc.GitURL, st.ConfigRepo)
		t.GitBranch = or(tc.Branch, st.ConfigBranch)
		t.BuildArgs = jsonMap(tc.BuildArgs)
		t.WatchPaths = lines(tc.WatchPaths)
		if tc.Build != nil {
			t.BuildContext = tc.Build.Context
			t.DockerfilePath = tc.Build.Dockerfile
		}
	}
	return t
}

func specOf(dc DomainConf, host string, port int) domain.Spec {
	sp := domain.Spec{
		Host:       host,
		Path:       dc.Path,
		Port:       or(dc.Port, port),
		HTTPS:      dc.HTTPS,
		ForceHTTPS: dc.ForceHTTPS,
		RedirectTo: dc.RedirectTo,
	}
	if x := dc.Proxy; x != nil {
		sp.Extras = domain.Extras{
			Websockets:  x.Websockets,
			MaxBodyMB:   x.MaxBodyMB,
			Headers:     x.Headers,
			Methods:     x.Methods,
			StripPrefix: x.StripPrefix,
			SecHeaders:  x.SecHeaders,
		}
		if x.BasicAuth != nil {
			sp.Extras.BasicAuth = &domain.BasicAuth{User: x.BasicAuth.User, Password: x.BasicAuth.Password}
		}
		if x.ForwardAuth != nil {
			sp.Extras.ForwardAuth = &domain.ForwardAuth{URL: x.ForwardAuth.URL, CopyHeaders: x.ForwardAuth.CopyHeaders}
		}
		if x.Timeouts != nil {
			sp.Extras.Timeouts = &domain.Timeouts{Dial: x.Timeouts.Dial, Read: x.Timeouts.Read, Write: x.Timeouts.Write}
		}
	}
	return sp
}

// sigOf and sigRow put a spec and a stored row in one comparable form.
func sigOf(s domain.Spec) string {
	x, _ := json.Marshal(s.Extras)
	return fmt.Sprint(
		s.Port,
		domain.On(s.HTTPS),
		domain.On(s.ForceHTTPS),
		strings.ToLower(s.RedirectTo),
		string(x),
		s.Auto,
		deref(s.ResourceID),
	)
}

func sigRow(d store.Domain) string {
	return fmt.Sprint(d.ContainerPort, d.HTTPS, d.ForceHTTPS, d.RedirectTo, d.ProxyJSON, d.Auto, deref(d.ResourceID))
}

func findDomain(ds []store.Domain, key string) (store.Domain, bool) {
	i := slices.IndexFunc(ds, func(d store.Domain) bool { return d.Host+d.Path == key })
	if i < 0 {
		return store.Domain{}, false
	}
	return ds[i], true
}

func normPath(p string) string {
	if p = strings.TrimSpace(p); p == "/" {
		return ""
	}
	return p
}

func defaultsJSON(d Defaults) string { return jsonOf(d) }

// canonSettings folds a stored blob to the form defaultsJSON writes.
func canonSettings(blob string) string {
	s, err := settings.Parse(blob)
	if err != nil {
		return blob
	}
	return jsonOf(Defaults(s))
}

func fromLabel(kind, branch string, auto bool) string {
	s := kind
	if kind == environment.FromBranch {
		s = branch
	}
	if auto {
		s += ", auto"
	}
	return s
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func jsonMap(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	return jsonOf(m)
}

func lines(l []string) string { return strings.Join(l, "\n") }

func or[T comparable](a, b T) T {
	var zero T
	if a != zero {
		return a
	}
	return b
}

func short(sha string) string { return sha[:min(7, len(sha))] }
