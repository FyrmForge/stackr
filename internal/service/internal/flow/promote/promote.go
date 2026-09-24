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
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
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
	// Config reads the stack file (and a fetcher for its includes) at a
	// commit of the stack's config repo.
	Config func(ctx context.Context, st store.Stack, commit string, log io.Writer) ([]byte, Fetcher, error)
	// DNS01 reports whether a DNS-01 provider is configured (wildcards).
	DNS01 func(ctx context.Context) bool
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
		return p, errs.Conflictf("%s", strings.Join(p.Blockers, "; "))
	}
	if swap != nil {
		if err := swap(); err != nil {
			return p, err
		}
	}
	if err := f.apply(ctx, w, log); err != nil {
		return p, err
	}
	return p, nil
}

func (f *Flow) apply(ctx context.Context, w *work, log io.Writer) error {
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
	if w.stackRes != nil {
		if st, err = d.Stacks.SetReservations(ctx, st, w.stackRes); err != nil {
			return err
		}
	}
	if len(w.params) > 0 {
		if err := d.Params.Merge(ctx, params.Scope{Kind: "env", ID: e.ID}, w.params); err != nil {
			return err
		}
	}
	scope := volume.Scope{Kind: "env", ID: e.ID}
	for _, n := range sortedKeys(w.declare) {
		if _, _, err := d.Volumes.Declare(ctx, scope, n, w.declare[n].MaxSizeMB, nil); err != nil {
			return err
		}
	}
	for _, v := range w.orphan {
		if _, err := d.Volumes.Orphan(ctx, v); err != nil {
			return err
		}
	}

	home := managed.Home{EnvID: e.ID, StackID: st.ID, OrgID: st.OrgID}
	for _, row := range w.creates {
		t, err := d.Tiles.Create(ctx, row)
		if err != nil {
			return fmt.Errorf("create %s: %w", row.Slug, err)
		}
		if t.Kind == tile.Managed {
			if _, err := d.Managed.Create(ctx, t.ID, w.re.Tiles[t.Slug].Engine, "env", home, "stackr", ""); err != nil {
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
	dns01 := f.DNS01 != nil && f.DNS01(ctx)
	for _, name := range sortedKeys(w.domains) {
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
		reps, err := replicaIDs(ctx, d, t)
		if err != nil {
			return err
		}
		for _, sp := range dw.add {
			if _, err := d.Domains.Attach(ctx, t.ID, sp, dns01, reps); err != nil {
				return fmt.Errorf("%s: domain %s: %w", name, sp.Host, err)
			}
		}
	}
	ephemeral := e.Type == environment.Ephemeral
	for _, pr := range w.detach {
		if err := d.Engines.Detach(ctx, pr, ephemeral); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(w.attach) {
		consumer, err := d.Tiles.GetBySlug(ctx, e.ID, name)
		if err != nil {
			return err
		}
		for _, s := range w.attach[name] {
			it, err := f.instanceTile(ctx, home, e.ID, s.From)
			if err != nil {
				return err
			}
			if _, err := d.Engines.Attach(ctx, consumer, it, home, s.Name, s.Public, s.OnRemove); err != nil {
				return fmt.Errorf("%s: slice from %s: %w", name, s.From, err)
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
	return f.rollout(ctx, w, e, log)
}

// deleteTiles: consumers first, so an instance goes after its slices.
func (f *Flow) deleteTiles(ctx context.Context, w *work, e store.Environment, log io.Writer) error {
	d := f.D
	order := append([]store.Tile{}, w.deletes...)
	for i, t := range order { // managed last
		if t.Kind == tile.Managed {
			order = append(append(order[:i:i], order[i+1:]...), t)
		}
	}
	for _, t := range order {
		if err := d.Tiles.Teardown(ctx, t, e.Network); err != nil {
			return err
		}
		if err := d.Domains.CloseIngress(ctx, t.ID); err != nil {
			return err
		}
		if t.Kind == tile.Managed {
			m, err := d.Managed.GetByTile(ctx, t.ID)
			if err == nil {
				vs, err := d.Volumes.List(ctx, volume.Scope{Kind: m.ScopeKind, ID: m.ScopeID})
				if err != nil {
					return err
				}
				for _, v := range vs {
					if v.InstanceID != nil && *v.InstanceID == m.ID {
						if _, err := d.Volumes.Orphan(ctx, v); err != nil {
							return err
						}
					}
				}
				if err := d.Engines.Teardown(ctx, t, false, log); err != nil {
					return err
				}
			} else if !errors.Is(err, errs.ErrNotFound) {
				return err
			}
		} else if d.Engines != nil {
			ps, err := d.Managed.ForConsumer(ctx, t.ID)
			if err != nil {
				return err
			}
			for _, pr := range ps {
				if err := d.Engines.Detach(ctx, pr, e.Type == environment.Ephemeral); err != nil {
					return err
				}
			}
		}
		if err := d.Tiles.Delete(ctx, t.ID); err != nil {
			return err
		}
		logf(log, "removed %s\n", t.Slug)
	}
	return nil
}

// rollout runs every tile the plan touched: managed first, then in
// depends_on order. Image tiles whose tag moved run the tag and are pinned
// again in one derived release.
func (f *Flow) rollout(ctx context.Context, w *work, e store.Environment, log io.Writer) error {
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
	for _, pass := range []bool{true, false} {
		for _, s := range order {
			if w.redeploy[s] && (bySlug[s].Kind == tile.Managed) == pass {
				run = append(run, s)
			}
		}
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
		logf(log, "deploying %s\n", s)
		digest, err := d.Run(ctx, t, ref, log, nil)
		if err != nil {
			return fmt.Errorf("deploy %s: %w", s, err)
		}
		if t.Kind == tile.Image && !pinned && digest != "" {
			repin = append(repin, release.Pin{Slug: s, Repo: deploy.RepoOf(t.ImageRef), Digest: digest})
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

// instanceTile finds the managed tile a slice's from: names: the env's own
// first, then anything visible from here.
func (f *Flow) instanceTile(ctx context.Context, h managed.Home, envID, name string) (store.Tile, error) {
	if t, err := f.D.Tiles.GetBySlug(ctx, envID, name); err == nil && t.Kind == tile.Managed {
		return t, nil
	}
	ms, err := f.D.Managed.Visible(ctx, h)
	if err != nil {
		return store.Tile{}, err
	}
	for _, m := range ms {
		if t, err := f.D.Tiles.Get(ctx, m.TileID); err == nil && t.Slug == name {
			return t, nil
		}
	}
	return store.Tile{}, errs.ErrNotFound
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
