// Package promote lands a release in an env: the stack file at the
// release's config commit becomes the env's tiles, domains, volumes and
// params, and the release's images become what those tiles run. Plan is the
// dry run; Apply recomputes the same plan and carries it out, so the two
// cannot disagree (B20). The one ladder rule serves promote and rollback (B2).
package promote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domainres"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Flow holds no Docker handle: containers change through D, which runs
// inside the caller's job.
type Flow struct {
	D *deploy.Flow
	// Resources is leaf/domainres: the stack file's domains: rows and what
	// auto and apex domains resolve against.
	Resources *domainres.Leaf
	// Config reads the stack file (and a fetcher for its includes) at a
	// commit of the stack's config repo.
	Config func(ctx context.Context, st store.Stack, commit string, log io.Writer) ([]byte, Fetcher, error)
	// DNS01 reports whether a DNS-01 provider is configured (wildcards).
	DNS01 func(ctx context.Context) bool
	// RouteHeld reports whether an external route holds a host: a stack
	// file domain on it is blocked, as a UI domain is refused.
	RouteHeld func(ctx context.Context, host string) (bool, error)
	// Build builds one service tile at a commit and returns the image row id.
	Build func(ctx context.Context, st store.Stack, t store.Tile, commit string, log io.Writer) (string, error)
}

// Plan is the dry run of promoting releaseID into envID.
func (f *Flow) Plan(ctx context.Context, envID, releaseID string, log io.Writer) (*Plan, error) {
	p, _, err := f.plan(ctx, envID, releaseID, log)
	return p, err
}

// Apply promotes. Blocked: a conflict naming every blocker, the same text
// the dry run shows.
func (f *Flow) Apply(ctx context.Context, envID, releaseID string, log io.Writer, swap func() error) (*Plan, error) {
	p, w, err := f.plan(ctx, envID, releaseID, log)
	if err != nil {
		return nil, err
	}
	if p.Blocked() {
		if err := needsApproval(p, w.st); err != nil {
			return p, err
		}
		return p, errs.Conflictf("%s", strings.Join(p.Blockers, "; "))
	}
	if len(p.Changes) == 0 {
		logf(log, "plan: nothing to change\n")
	}
	for _, c := range p.Changes {
		logf(log, "plan: %s\n", c.Line())
	}
	late, err := f.swapOrDefer(ctx, w, swap)
	if err != nil {
		return p, err
	}
	err = f.apply(ctx, w, log, late)
	p.Deployed, p.Removed = w.deployed, w.removed
	return p, err
}

func (f *Flow) apply(ctx context.Context, w *work, log io.Writer, swap func() error) error {
	d, e := f.D, w.e
	var err error
	if w.envEdit != nil {
		if w.envEdit.Color != e.Color {
			if e, err = d.Envs.SetColor(ctx, e, w.envEdit.Color); err != nil {
				return err
			}
		}
		if w.envEdit.FromKind != e.FromKind || w.envEdit.FromBranch != e.FromBranch || w.envEdit.Auto != e.Auto {
			if e, err = d.Envs.SetFrom(ctx, e, w.envEdit.FromKind, w.envEdit.FromBranch, w.envEdit.Auto); err != nil {
				return err
			}
		}
	}
	if w.envBlob != "" {
		if e, err = d.Envs.SetSettings(ctx, e, w.envBlob); err != nil {
			return err
		}
	}
	st := w.st
	if w.stackBlob != nil {
		if st, err = d.Stacks.SetSettings(ctx, st, *w.stackBlob); err != nil {
			return err
		}
	}
	// Resources before the tile domains that name them.
	for _, r := range w.resCreate {
		spec := domainres.Spec{
			ID:                  r.ID,
			Level:               domainres.Stack,
			OwnerID:             st.ID,
			Host:                r.Host,
			IncludeEnvOnDefault: r.IncludeEnvOnDefault,
			ACMEEmail:           r.ACMEEmail,
		}
		if _, err := f.Resources.Create(ctx, spec, st.OrgID, w.orgs); err != nil {
			return fmt.Errorf("domains: %s: %w", r.Host, err)
		}
	}
	for _, r := range w.resUpdate {
		if _, err := f.Resources.Update(ctx, r, r.IncludeEnvOnDefault, r.ACMEEmail); err != nil {
			return fmt.Errorf("domains: %s: %w", r.Host, err)
		}
	}
	if len(w.params) > 0 {
		if err := d.Params.Merge(ctx, params.Scope{Kind: "env", ID: e.ID}, w.params); err != nil {
			return err
		}
	}
	scope := volume.Scope{Kind: "env", ID: e.ID}
	for _, n := range slices.Sorted(maps.Keys(w.declare)) {
		if _, _, err := d.Volumes.Declare(ctx, scope, n, w.declare[n].MaxSizeMB, nil); err != nil {
			return err
		}
	}
	for _, v := range w.orphan {
		if _, err := d.Volumes.Orphan(ctx, v); err != nil {
			return err
		}
	}

	for _, row := range w.creates {
		t, err := d.Tiles.Create(ctx, row)
		if err != nil {
			return fmt.Errorf("create %s: %w", row.Slug, err)
		}
		if t.Kind == tile.Managed {
			if _, err := d.Managed.Create(ctx, t.ID, w.re.Tiles[t.Slug].Engine, "stackr", ""); err != nil {
				return err
			}
		}
		logf(log, "created %s\n", t.Slug)
	}
	for _, u := range w.updates {
		if _, _, err := d.Tiles.Update(ctx, u[0], u[1]); err != nil {
			return fmt.Errorf("update %s: %w", u[0].Slug, err)
		}
	}
	if err := f.applyInstances(ctx, w, e); err != nil {
		return err
	}
	dns01 := f.DNS01 != nil && f.DNS01(ctx)
	for _, name := range slices.Sorted(maps.Keys(w.domains)) {
		t, err := d.Tiles.GetBySlug(ctx, e.ID, name)
		if err != nil {
			return err
		}
		dw := w.domains[name]
		for _, row := range dw.remove {
			if err := d.Domains.Detach(ctx, row); err != nil {
				return err
			}
		}
		for id, sp := range dw.update {
			row, err := d.Domains.Get(ctx, id)
			if err != nil {
				return err
			}
			if _, err := d.Domains.Update(ctx, row, sp, dns01); err != nil {
				return err
			}
		}
		cs, err := d.Tiles.Replicas(ctx, t)
		if err != nil {
			return err
		}
		var reps []string
		for _, c := range cs {
			if !c.HostNetwork { // on the host's network: no ingress network to join
				reps = append(reps, c.ID)
			}
		}
		for _, sp := range dw.add {
			if _, err := d.Domains.Attach(ctx, t.ID, sp, dns01, reps); err != nil {
				return fmt.Errorf("%s: domain %s: %w", name, sp.Host, err)
			}
		}
	}
	if err := f.deleteTiles(ctx, w, e, log); err != nil {
		return err
	}

	if e, err = d.Envs.SetRelease(ctx, e, w.rel.ID); err != nil {
		return err
	}
	if w.sync && d.Sync != nil {
		if err := d.Sync(ctx); err != nil {
			return err
		}
	}
	return f.rollout(ctx, w, e, log, swap)
}

// swapOrDefer marks the job swapping before the first write, unless a tile of
// the rollout waits on a dependency: the wait must stay cancellable, so the
// swap is handed on (late) and comes per tile, once its wait is over.
// ponytail: with a wait in the env the row writes are cancellable too; a
// re-run re-plans from whatever landed.
func (f *Flow) swapOrDefer(ctx context.Context, w *work, swap func() error) (late func() error, err error) {
	if swap == nil {
		return nil, nil
	}
	live, err := f.D.Tiles.List(ctx, w.e.ID)
	if err != nil {
		return nil, err
	}
	rows := append(slices.Clone(live), w.creates...)
	for _, u := range w.updates {
		rows = append(rows, u[1])
	}
	if slices.ContainsFunc(rows, deploy.Waits) {
		return swap, nil
	}
	return nil, swap()
}

// partly is a tile's rollout failure once the env already points at the new
// release: some tiles run it and some do not. A park keeps its own text.
func partly(slug string, err error) error {
	_, unset := errs.IsUnset(err)
	_, approval := errs.IsNeedsApproval(err)
	if unset || approval {
		return fmt.Errorf("deploy %s: %w", slug, err)
	}
	return fmt.Errorf("deploy %s: %w; the environment is partly rolled out on the new release, promote again to finish it", slug, err)
}

// applyInstances writes the file's allow list and env pairs onto each
// managed tile's instance row whose either moved; nothing redeploys.
func (f *Flow) applyInstances(ctx context.Context, w *work, e store.Environment) error {
	d := f.D
	o, err := d.Orgs.Get(ctx, w.st.OrgID)
	if err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(w.instances)) {
		iw := w.instances[name]
		t, err := d.Tiles.GetBySlug(ctx, e.ID, name)
		if err != nil {
			return err
		}
		m, err := d.Managed.GetByTile(ctx, t.ID)
		if err != nil {
			return err
		}
		if m, err = d.Managed.SetAllow(ctx, m, o.Slug, iw.allow); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if _, err = d.Managed.SetEnvPairs(ctx, m, iw.pairs); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// deleteTiles: consumers first, so an instance goes after its slices.
func (f *Flow) deleteTiles(ctx context.Context, w *work, e store.Environment, log io.Writer) error {
	var err error
	w.removed, err = f.Remove(ctx, e, w.deletes, log)
	return err
}

// Remove takes tiles of env e off the box and out of the store: containers,
// ingress, a consumer's bindings, a slice tile's provision (its data dropped
// or kept by on_remove), a managed tile's instance (volumes orphaned, its
// network last). Consumers go first, then slices, then managed tiles, so
// nothing goes while something in this env still binds it. An engine
// failure on a binding is logged, not fatal: its row goes with the tile.
// It returns the ids of the tiles it removed, also on an error.
func (f *Flow) Remove(ctx context.Context, e store.Environment, ts []store.Tile, log io.Writer) (removed []string, err error) {
	d := f.D
	order := slices.Clone(ts)
	slices.SortStableFunc(order, func(a, b store.Tile) int {
		return removeRank(a) - removeRank(b)
	})
	for _, t := range order {
		net := ""
		if t.Kind == tile.Managed {
			m, err := d.Managed.GetByTile(ctx, t.ID)
			switch {
			case err == nil:
				// Refused before anything moves: an instance still holding
				// slices keeps its volumes, container and rows.
				// ponytail: refuse, never drain; removing the slice tiles first
				// (or a forced Engines.Teardown) is the way out, a UI verb for
				// the forced drop when someone needs it.
				if _, err := d.Managed.Teardown(ctx, m, false); err != nil {
					return removed, err
				}
				if err := f.orphan(ctx, e, m); err != nil {
					return removed, err
				}
				// Before the container, so the engine can still reach it.
				if err := d.Engines.Teardown(ctx, t, false, log); err != nil {
					return removed, err
				}
				net = managed.Network(m.ID)
			case !errors.Is(err, errs.ErrNotFound):
				return removed, err
			}
		}
		if err := d.Tiles.Teardown(ctx, t, e.Network); err != nil {
			return removed, err
		}
		if err := d.Domains.CloseIngress(ctx, t.ID); err != nil {
			return removed, err
		}
		switch {
		case net != "":
			if err := d.Envs.DropShared(ctx, net); err != nil {
				logf(log, "warning: network %s stays: %v\n", net, err)
			}
		case d.Engines == nil:
		case t.Kind == tile.Slice:
			p, ok, err := d.Managed.ProvisionOf(ctx, t.ID)
			if err != nil {
				return removed, err
			}
			if ok {
				drop := deref(t.OnRemove) == tile.Drop || e.Type == environment.Ephemeral
				if err := d.Engines.Drop(ctx, p, drop, log); err != nil {
					return removed, err
				}
			}
		default:
			bound, err := d.Managed.Bound(ctx, t.ID)
			if err != nil {
				return removed, err
			}
			for _, id := range slices.Sorted(maps.Keys(bound)) {
				if err := d.Engines.Unbind(ctx, bound[id]); err != nil {
					logf(log, "warning: unbind %s: %v\n", bound[id].DBUser, err)
				}
			}
		}
		if t.Kind == tile.Slice {
			if err := f.forgetSlice(ctx, e, t.Slug); err != nil {
				return removed, err
			}
		}
		if err := d.Tiles.Delete(ctx, t.ID); err != nil {
			return removed, err
		}
		removed = append(removed, t.ID)
		logf(log, "removed %s\n", t.Slug)
	}
	if len(removed) > 0 {
		// Share volumes have no row; the ones no remaining tile row names go.
		st, err := d.Stacks.Get(ctx, e.StackID)
		if err != nil {
			return removed, err
		}
		uses, err := f.shareUses(ctx, st.OrgID)
		if err != nil {
			return removed, err
		}
		held, err := d.Volumes.SweepShares(ctx, st.OrgID, uses)
		if err != nil {
			return removed, err
		}
		if len(held) > 0 {
			logf(log, "warning: %d share volumes stay: a container holds them\n", len(held))
		}
	}
	return removed, nil
}

// shareUses is every share line of every tile row in the org: the keep set
// of a share sweep.
func (f *Flow) shareUses(ctx context.Context, orgID string) ([]volume.ShareUse, error) {
	d := f.D
	sts, err := d.Stacks.List(ctx, orgID)
	if err != nil {
		return nil, err
	}
	var uses []volume.ShareUse
	for _, st := range sts {
		ts, err := d.Tiles.ListByStack(ctx, st.ID)
		if err != nil {
			return nil, err
		}
		for _, t := range ts {
			for _, l := range tile.Lines(t.Volumes) {
				if m, err := tile.ParseMount(l); err == nil && m.Kind == tile.MountShare {
					uses = append(uses, volume.ShareUse{Share: m.Share, Sub: m.Sub})
				}
			}
		}
	}
	return uses, nil
}

// forgetSlice drops every slice_access entry naming slice slug from the
// other tiles of env e, so no consumer keeps naming a slice that is gone.
func (f *Flow) forgetSlice(ctx context.Context, e store.Environment, slug string) error {
	ts, err := f.D.Tiles.List(ctx, e.ID)
	if err != nil {
		return err
	}
	for _, t := range ts {
		cur := t
		cur.SliceAccess = slices.DeleteFunc(slices.Clone(t.SliceAccess), func(a store.SliceAccess) bool {
			return a.From == slug
		})
		if len(cur.SliceAccess) == len(t.SliceAccess) {
			continue
		}
		if _, _, err := f.D.Tiles.Update(ctx, t, cur); err != nil {
			return err
		}
	}
	return nil
}

// removeRank: consumers, then slices, then managed tiles.
func removeRank(t store.Tile) int {
	switch t.Kind {
	case tile.Slice:
		return 1
	case tile.Managed:
		return 2
	}
	return 0
}

// orphan marks the instance's data volumes orphaned: kept, and reclaimed
// only after retention.
func (f *Flow) orphan(ctx context.Context, e store.Environment, m store.ManagedInstance) error {
	vs, err := f.D.Volumes.List(ctx, volume.Scope{Kind: "env", ID: e.ID})
	if err != nil {
		return err
	}
	for _, v := range vs {
		if v.InstanceID == nil || *v.InstanceID != m.ID {
			continue
		}
		if _, err := f.D.Volumes.Orphan(ctx, v); err != nil {
			return err
		}
	}
	return nil
}

// rollout runs every tile the plan touched: managed tiles, then slice tiles
// (their provision needs the instance up), then the rest, each pass in
// depends_on order. Image tiles whose tag moved run the tag and are pinned
// again in one derived release.
func (f *Flow) rollout(ctx context.Context, w *work, e store.Environment, log io.Writer, swap func() error) error {
	d := f.D
	live, err := d.Tiles.List(ctx, e.ID)
	if err != nil {
		return err
	}
	bySlug := map[string]store.Tile{}
	var slugs []string
	for _, t := range live {
		bySlug[t.Slug] = t
		slugs = append(slugs, t.Slug)
	}
	order := topo(slugs, depsOf(live))
	if order == nil {
		return errors.New("depends_on has a cycle")
	}
	var run []string
	for pass := range 3 {
		for _, s := range order {
			if w.redeploy[s] && rolloutPass(bySlug[s]) == pass {
				run = append(run, s)
			}
		}
	}
	pins, err := d.Releases.Pins(ctx, w.rel.ID)
	if err != nil {
		return err
	}
	var repin []release.Pin
	for _, s := range run {
		t := bySlug[s]
		ref, pinned := t.ImageRef, false
		if !w.unpin[s] {
			if ref, pinned, err = d.Current(ctx, t, e); err != nil {
				return err
			}
		}
		// A tile whose tag was edited outside the stack file takes the
		// pinned tag back, so a later redeploy stays on what the release
		// runs (DECIDE 140). A bare-repo pin names no tag to take.
		if p := pins[s]; pinned && tile.Pulls(t) && p.Repo != t.ImageRef && deploy.RepoOf(p.Repo) != p.Repo {
			cur := t
			cur.ImageRef = p.Repo
			if t, _, err = d.Tiles.Update(ctx, t, cur); err != nil {
				return err
			}
		}
		logf(log, "deploying %s\n", s)
		digest, err := d.Run(ctx, t, ref, log, swap)
		if err != nil {
			return partly(s, err)
		}
		w.deployed = append(w.deployed, t.ID)
		if tile.Pulls(t) && !pinned && digest != "" {
			repin = append(repin, release.Pin{Slug: s, Repo: t.ImageRef, Digest: digest})
		}
	}
	if len(repin) == 0 {
		return nil
	}
	r, err := d.Releases.Derive(ctx, e.StackID, w.rel.ID, "promote", repin...)
	if err != nil {
		return err
	}
	_, err = d.Envs.SetRelease(ctx, e, r.ID)
	return err
}

// rolloutPass: managed tiles, then slices, then everything else.
func rolloutPass(t store.Tile) int {
	switch t.Kind {
	case tile.Managed:
		return 0
	case tile.Slice:
		return 1
	}
	return 2
}

func replicaIDs(ctx context.Context, d *deploy.Flow, t store.Tile) ([]string, error) {
	cs, err := d.Tiles.Replicas(ctx, t)
	var ids []string
	for _, c := range cs {
		ids = append(ids, c.ID)
	}
	return ids, err
}

func depsOf(ts []store.Tile) map[string]TileConf {
	out := map[string]TileConf{}
	for _, t := range ts {
		out[t.Slug] = TileConf{DependsOn: tile.Lines(t.DependsOn)}
	}
	return out
}

func logf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
