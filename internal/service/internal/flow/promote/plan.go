package promote

import (
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
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domainres"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Change is one line of a plan. Values that may be credentials (env,
// params) never appear, only that they moved.
type Change struct {
	Kind  string `json:"kind"` // env | stack | param | volume | orphan | create | update | delete | domain | slice | detach | image
	Tile  string `json:"tile,omitempty"`
	Field string `json:"field,omitempty"`
	Old   string `json:"old,omitempty"`
	New   string `json:"new,omitempty"`
	Note  string `json:"note,omitempty"`
}

// Plan is what a promote would do. Blockers is the one answer to "what
// blocks this promote" (B20): the dry run shows it, the real run refuses
// with it, whoever calls.
type Plan struct {
	Stack    string   `json:"stack"`
	Env      string   `json:"env"`
	Release  int      `json:"release"`
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
// never a credential, and what a reader of a cron or function diff needs.
var shownValues = map[string]func(store.Tile) string{
	"schedule":        func(t store.Tile) string { return t.Schedule },
	"trigger":         func(t store.Tile) string { return t.Trigger },
	"timeout_minutes": func(t store.Tile) string { return strconv.Itoa(t.TimeoutMinutes) },
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
	declare   map[string]VolumeConf
	orphan    []store.Volume
	creates   []store.Tile
	updates   [][2]store.Tile // old, new
	deletes   []store.Tile
	domains   map[string]domainWork // by tile slug
	attach    map[string][]SliceConf
	detach    []store.Provision
	redeploy  map[string]bool
	unpin     map[string]bool // image tiles whose tag moved: run the tag, pin again
	sync      bool
	deployed  []string // tile ids rolled out, filled by apply
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
		e:        e,
		st:       st,
		rel:      rel,
		domains:  map[string]domainWork{},
		attach:   map[string][]SliceConf{},
		declare:  map[string]VolumeConf{},
		redeploy: map[string]bool{},
		unpin:    map[string]bool{},
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
		r, err := Load(data, fetch)
		if err != nil {
			p.block("stack file at %s: %v", short(cp.CommitSHA), err)
			return p, w, nil
		}
		re, ok := r.Envs[e.Slug]
		if !ok && e.BaseEnvID != nil {
			// A PR env is shaped like its base env's section.
			if base, err := d.Envs.Get(ctx, *e.BaseEnvID); err == nil {
				re, ok = r.Envs[base.Slug]
			}
		}
		if !ok {
			p.block("the stack file at %s has no environment %s", short(cp.CommitSHA), e.Slug)
			return p, w, nil
		}
		w.re = &re
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
		if len(tc.Files) > 0 { // DECIDE 27
			p.block("tile %s: files: is not supported yet", name)
			continue
		}
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
		if err := f.planSlices(ctx, p, w, name, tc, old, exists); err != nil {
			return err
		}
	}
	return f.planDeletes(ctx, p, w, live)
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
	if tc.Type == tile.Managed {
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

// planSlices: the tile's slices: vs the provisions it holds, by instance.
// ponytail: on_remove and public changes on a held slice are not
// reconciled; they take effect when the slice is attached again.
func (f *Flow) planSlices(
	ctx context.Context,
	p *Plan,
	w *work,
	name string,
	tc TileConf,
	old store.Tile,
	exists bool,
) error {
	if tc.Type == tile.Managed {
		return nil
	}
	held := map[string]store.Provision{} // by instance tile slug
	if exists {
		ps, err := f.D.Managed.ForConsumer(ctx, old.ID)
		if err != nil {
			return err
		}
		for _, pr := range ps {
			m, err := f.D.Managed.Get(ctx, pr.InstanceID)
			if err != nil {
				return err
			}
			it, err := f.D.Tiles.Get(ctx, m.TileID)
			if err != nil {
				return err
			}
			held[it.Slug] = pr
		}
	}
	want := map[string]bool{}
	for _, s := range tc.Slices {
		want[s.From] = true
		if _, ok := held[s.From]; ok {
			continue
		}
		if f.D.Engines == nil {
			p.block("tile %s: slices need managed engines, and none are wired here", name)
			continue
		}
		if it, ok := w.re.Tiles[s.From]; !ok || it.Type != tile.Managed {
			if !f.visible(ctx, w, s.From) {
				p.block("tile %s: slice from %s: no managed instance of that name is visible here", name, s.From)
				continue
			}
		}
		p.add(Change{Kind: "slice", Tile: name, New: s.From})
		w.attach[name] = append(w.attach[name], s)
		w.redeploy[name] = true
	}
	for _, from := range slices.Sorted(maps.Keys(held)) {
		if want[from] {
			continue
		}
		pr := held[from]
		c := Change{
			Kind: "detach",
			Tile: name,
			Old:  from,
			Note: "the slice is orphaned and its data kept",
		}
		if pr.OnRemove == "drop" {
			c.Note = "on_remove: drop. The data behind this slice is destroyed"
		}
		p.add(c)
		w.detach = append(w.detach, pr)
		w.redeploy[name] = true
	}
	return nil
}

// visible: a managed tile of that slug in this env.
// step 7b task 3 replaces this: a slice tile's provision_from reaches any
// instance whose allow list admits it.
func (f *Flow) visible(ctx context.Context, w *work, slugName string) bool {
	it, err := f.D.Tiles.GetBySlug(ctx, w.e.ID, slugName)
	return err == nil && it.Kind == tile.Managed
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
						p.block("tile %s still serves a slice to another tile; detach it first", t.Slug)
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

func (f *Flow) planParams(ctx context.Context, p *Plan, w *work, r *Resolved) error {
	have, err := f.D.Params.Values(ctx, params.Scope{Kind: "env", ID: w.e.ID}, true)
	if err != nil {
		return err
	}
	stackHave, err := f.D.Params.Values(ctx, params.Scope{Kind: "stack", ID: w.st.ID}, true)
	if err != nil {
		return err
	}
	for _, c := range slices.Sorted(maps.Keys(r.Params)) {
		for _, n := range slices.Sorted(maps.Keys(r.Params[c])) {
			decl, key := r.Params[c][n], c+"."+n
			old, ok := have[key]
			switch {
			case decl.Type == params.Param && ok && old.Secret:
				p.block("params.%s is a secret; a secret is never turned back into a param", key)
			case decl.Type == params.Secret && ok && !old.Secret:
				p.add(Change{Kind: "param", Field: key, Note: "becomes a secret"})
				w.params = append(w.params, params.Entry{Collection: c, Name: n, Kind: params.Secret})
			case decl.Type == params.Secret && !ok:
				if _, atStack := stackHave[key]; !atStack {
					p.Warnings = append(
						p.Warnings,
						"params."+key+" is declared and not set; tiles that read it wait until it is",
					)
				}
			case decl.Type == params.Param && decl.Value != nil && (!ok || old.V != *decl.Value):
				p.add(Change{Kind: "param", Field: key})
				w.params = append(w.params, params.Entry{
					Collection: c,
					Name:       n,
					Kind:       params.Param,
					Value:      *decl.Value,
				})
			}
		}
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
	for _, c := range release.Diff(cur, pins) {
		if _, ok := tiles[c.Slug]; !ok || c.Slug == release.ConfigSlug {
			continue
		}
		p.add(Change{Kind: "image", Tile: c.Slug, Note: c.Kind})
		w.redeploy[c.Slug] = true
	}
	for _, n := range slices.Sorted(maps.Keys(tiles)) {
		if tiles[n] && pins[n].ImageID == nil {
			p.block("tile %s has no build in release #%d; build a commit first", n, w.rel.Number)
		}
	}
	return nil
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
	var s params.Snapshot
	var err error
	if s.EnvParams, err = f.D.Params.Values(ctx, params.Scope{Kind: "env", ID: w.e.ID}, true); err != nil {
		return nil, err
	}
	if s.StackParams, err = f.D.Params.Values(ctx, params.Scope{Kind: "stack", ID: w.st.ID}, true); err != nil {
		return nil, err
	}
	if s.OrgParams, err = f.D.Params.Values(ctx, params.Scope{Kind: "org", ID: w.st.OrgID}, true); err != nil {
		return nil, err
	}
	for _, e := range w.params { // this promote's own values count
		s.EnvParams[e.Collection+"."+e.Name] = params.Value{V: e.Value, Secret: e.Kind == params.Secret}
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
	if tc.Limits != nil {
		t.CPULimit, t.MemLimitMB = tc.Limits.CPU, tc.Limits.MemoryMB
	}
	if builds(tc) {
		t.GitURL, t.GitBranch = or(tc.GitURL, st.ConfigRepo), or(tc.Branch, st.ConfigBranch)
		t.BuildArgs, t.WatchPaths = jsonMap(tc.BuildArgs), lines(tc.WatchPaths)
		if tc.Build != nil {
			t.BuildContext, t.DockerfilePath = tc.Build.Context, tc.Build.Dockerfile
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
