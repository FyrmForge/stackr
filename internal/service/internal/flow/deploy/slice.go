package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/address"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Place is stack st as address.Resolve reads it for a target tile of slug
// tileSlug: every env of it with the tile there (PR envs included), and the
// tile's env_pairs. The promote plan reads the same Place and lays its file
// over this stack's part of it.
// ponytail: the first rung, bottom up, whose instance has a map wins; env
// overlays that give one tile different maps per env are not reconciled.
func (f *Flow) Place(ctx context.Context, st store.Stack, tileSlug string) (address.Place, error) {
	pl := address.Place{
		Stack: st.Slug,
		Envs:  map[string]*address.Host{},
	}
	envs, err := f.Envs.List(ctx, st.ID) // statics bottom rung first
	if err != nil {
		return pl, err
	}
	for _, e := range envs {
		pl.Envs[e.Slug] = nil
		it, err := f.Tiles.GetBySlug(ctx, e.ID, tileSlug)
		if errors.Is(err, errs.ErrNotFound) {
			continue
		}
		if err != nil {
			return pl, err
		}
		h := &address.Host{
			TileID:  it.ID,
			Managed: it.Kind == tile.Managed,
		}
		pl.Envs[e.Slug] = h
		if !h.Managed {
			continue
		}
		m, err := f.Managed.GetByTile(ctx, it.ID)
		if errors.Is(err, errs.ErrNotFound) {
			continue
		}
		if err != nil {
			return pl, err
		}
		h.Ready = true
		h.Allow = m.Allow
		if pl.Pairs == nil && e.Type == environment.Static && len(m.EnvPairs) > 0 {
			pl.Pairs = m.EnvPairs
		}
	}
	return pl, nil
}

// target is slice tile s's instance tile, re-resolved on every deploy by
// the plan's own rule (address.Resolve) against the rows as they are now:
// an instance whose allow list or env_pairs moved since the plan fails the
// deploy with the reason, and nothing is torn down. An unset param in
// provision_from comes back as errs.Unset, so the job parks.
func (f *Flow) target(
	ctx context.Context,
	s store.Tile,
	e store.Environment,
	st store.Stack,
	o store.Org,
) (store.Tile, error) {
	snap, err := f.snapshot(ctx, s, e, st)
	if err != nil {
		return store.Tile{}, err
	}
	raw, err := params.NewResolver(snap).Expand(params.InProvisionFrom, deref(s.ProvisionFrom))
	if err != nil {
		return store.Tile{}, err
	}
	tg, err := address.ParseTarget(raw)
	if err != nil {
		return store.Tile{}, err
	}
	ts, err := f.Stacks.GetBySlug(ctx, st.OrgID, tg.Stack)
	if errors.Is(err, errs.ErrNotFound) {
		return store.Tile{}, errs.Conflictf("slice %s: stack %s not found", s.Slug, tg.Stack)
	}
	if err != nil {
		return store.Tile{}, err
	}
	pl, err := f.Place(ctx, ts, tg.Tile)
	if err != nil {
		return store.Tile{}, err
	}
	base := ""
	if e.BaseEnvID != nil {
		b, err := f.Envs.Get(ctx, *e.BaseEnvID)
		if err != nil {
			return store.Tile{}, err
		}
		base = b.Slug
	}
	me := address.Address{
		Org:   o.Slug,
		Stack: st.Slug,
		Env:   e.Slug,
		Tile:  s.Slug,
	}
	env, err := address.Resolve(o.Slug, me, base, tg, pl)
	if err != nil {
		return store.Tile{}, errs.Conflictf("slice %s: %v", s.Slug, err)
	}
	return f.Tiles.Get(ctx, pl.Envs[env].TileID)
}

// provision is a slice tile's deploy: no container, only its database or
// bucket on the instance it resolves to.
func (f *Flow) provision(ctx context.Context, s store.Tile, log io.Writer) error {
	if f.Engines == nil {
		return fmt.Errorf("%s: no managed engines are wired", s.Slug)
	}
	e, err := f.Envs.Get(ctx, s.EnvironmentID)
	if err != nil {
		return err
	}
	st, err := f.Stacks.Get(ctx, s.StackID)
	if err != nil {
		return err
	}
	o, err := f.Orgs.Get(ctx, st.OrgID)
	if err != nil {
		return err
	}
	it, err := f.target(ctx, s, e, st, o)
	if err != nil {
		return err
	}
	p, err := f.Engines.Provision(ctx, it, s)
	if err != nil {
		return err
	}
	logf(log, "slice %s is %s on %s\n", s.Slug, p.DBName, it.Slug)
	return nil
}

// use is one slice of a consumer's env it uses, at the access it gets.
type use struct {
	slice  store.Tile
	access string
}

// uses are the slices of env e consumer t uses: the ones slice_access
// names, at that access, and the ones its env or command refs, at the
// slice's default.
func (f *Flow) uses(ctx context.Context, t store.Tile, e store.Environment) ([]use, error) {
	env, err := envMap(t.EnvJSON)
	if err != nil {
		return nil, err
	}
	tiles, err := f.Tiles.List(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	var out []use
	for _, s := range tiles {
		if s.Kind != tile.Slice {
			continue
		}
		access := deref(s.DefaultAccess)
		if access == "" {
			access = "write"
		}
		i := slices.IndexFunc(t.SliceAccess, func(a store.SliceAccess) bool {
			return a.From == s.Slug
		})
		switch {
		case i >= 0:
			access = t.SliceAccess[i].Access
		case !Refs(env, t.Command, s.Slug):
			continue
		}
		out = append(out, use{
			slice:  s,
			access: access,
		})
	}
	return out, nil
}

// bind gives consumer t its own cred on every slice it uses. Each slice's
// target is re-resolved first (target), so a consumer the instance no
// longer admits fails here and keeps running as it was.
func (f *Flow) bind(
	ctx context.Context,
	t store.Tile,
	e store.Environment,
	st store.Stack,
	o store.Org,
	log io.Writer,
) error {
	if f.Engines == nil || t.Kind == tile.Managed {
		return nil
	}
	us, err := f.uses(ctx, t, e)
	if err != nil {
		return err
	}
	for _, u := range us {
		it, err := f.target(ctx, u.slice, e, st, o)
		if err != nil {
			return err
		}
		p, err := f.Engines.Provision(ctx, it, u.slice)
		if err != nil {
			return err
		}
		b, err := f.Engines.Bind(ctx, p, t, u.access)
		if err != nil {
			return err
		}
		logf(log, "slice %s: %s as %s (%s)\n", u.slice.Slug, p.DBName, b.DBUser, b.Access)
	}
	return nil
}

// prune unbinds the slices consumer t no longer uses. Run calls it once
// the new replicas are up, so a failed rollout leaves the old ones their
// cred. A failure is logged, never fatal: the deploy already happened.
func (f *Flow) prune(ctx context.Context, t store.Tile, e store.Environment, log io.Writer) {
	if f.Engines == nil || t.Kind == tile.Managed {
		return
	}
	us, err := f.uses(ctx, t, e)
	if err != nil {
		logf(log, "warning: unbind unused slices: %v\n", err)
		return
	}
	bound, err := f.Managed.Bound(ctx, t.ID)
	if err != nil {
		logf(log, "warning: unbind unused slices: %v\n", err)
		return
	}
	for id, b := range bound {
		if slices.ContainsFunc(us, func(u use) bool {
			return u.slice.ID == id
		}) {
			continue
		}
		if err := f.Engines.Unbind(ctx, b); err != nil {
			logf(log, "warning: unbind %s: %v\n", b.DBUser, err)
		}
	}
}

// Refs reports whether a tile's env or command refs
// ${{ tile.<slugName>.<output> }}; promote asks it of the file.
func Refs(env map[string]string, command, slugName string) bool {
	vals := slices.Collect(maps.Values(env))
	vals = append(vals, command)
	for _, v := range vals {
		for _, body := range params.Refs(v) {
			if strings.HasPrefix(body, "tile."+slugName+".") {
				return true
			}
		}
	}
	return false
}
