package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type (
	ManagedInstance = store.ManagedInstance
	Provision       = store.Provision
)

// CreateManagedTile makes a managed tile (postgres | s3) and its env-scoped
// instance row. Nothing runs until Deploy.
func (o *Orchestrator) CreateManagedTile(ctx context.Context, t Tile, engine string) (Tile, error) {
	if !deploy.KnownEngine(engine) {
		return Tile{}, errs.Invalidf("engine", "%q is not an engine stackr runs.", engine)
	}
	e, err := o.envs.Get(ctx, t.EnvironmentID)
	if err != nil {
		return Tile{}, err
	}
	st, err := o.stacks.Get(ctx, e.StackID)
	if err != nil {
		return Tile{}, err
	}
	t.Kind, t.StackID = tile.Managed, st.ID
	var out Tile
	err = o.store.Tx(ctx, func(tx store.Tx) error {
		var err error
		if out, err = tile.New(tx.Tiles, o.docker, nil).Create(ctx, t); err != nil {
			return err
		}
		_, err = managed.New(tx.ManagedInstances, tx.Provisions, tx.Bindings).Create(ctx, out.ID, engine, "stackr", "")
		return err
	})
	return out, err
}

// ManagedInstances are the env's own instances. Another env or stack reaches
// one through a slice tile's provision_from, which the instance's allow list
// gates at plan time.
func (o *Orchestrator) ManagedInstances(ctx context.Context, envID string) ([]ManagedInstance, error) {
	ts, err := o.tiles.List(ctx, envID)
	if err != nil {
		return nil, err
	}
	var out []ManagedInstance
	for _, t := range ts {
		if t.Kind != tile.Managed {
			continue
		}
		m, err := o.managed.GetByTile(ctx, t.ID)
		if errors.Is(err, errs.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// instanceOf is managed tile tileID and its instance row.
func (o *Orchestrator) instanceOf(ctx context.Context, tileID string) (Tile, ManagedInstance, error) {
	t, err := o.tiles.Get(ctx, tileID)
	if err != nil {
		return t, ManagedInstance{}, err
	}
	if t.Kind != tile.Managed {
		return t, ManagedInstance{}, errs.Invalidf("tile", "%s is a %s tile, not a managed one", t.Slug, t.Kind)
	}
	m, err := o.managed.GetByTile(ctx, t.ID)
	if errors.Is(err, errs.ErrNotFound) {
		return t, m, errs.Conflictf("managed tile %s has no instance", t.Slug)
	}
	return t, m, err
}

// SetManagedAllow replaces the instance's allow list, the org:stack:env:tile
// patterns that may cut a slice from it (empty: its own env only). Nothing
// redeploys: a slice the list no longer admits fails its next deploy with
// the reason. A stack file that declares the list sets it again on promote.
func (o *Orchestrator) SetManagedAllow(ctx context.Context, tileID string, list []string) (ManagedInstance, error) {
	t, m, err := o.instanceOf(ctx, tileID)
	if err != nil {
		return m, err
	}
	st, err := o.stacks.Get(ctx, t.StackID)
	if err != nil {
		return m, err
	}
	og, err := o.orgs.Get(ctx, st.OrgID)
	if err != nil {
		return m, err
	}
	if list == nil {
		list = []string{}
	}
	return o.managed.SetAllow(ctx, m, og.Slug, list)
}

// SetManagedEnvPairs replaces the instance's env pairs: the env a slice's
// provision_from names (a consumer's env) to one of this stack's envs. A
// non-empty map is the whole rule: an env it leaves out resolves nowhere.
// Keys are any env name; values must be envs of the stack.
func (o *Orchestrator) SetManagedEnvPairs(
	ctx context.Context,
	tileID string,
	pairs map[string]string,
) (ManagedInstance, error) {
	t, m, err := o.instanceOf(ctx, tileID)
	if err != nil {
		return m, err
	}
	envs, err := o.envs.List(ctx, t.StackID)
	if err != nil {
		return m, err
	}
	for k, v := range pairs {
		if !slices.ContainsFunc(envs, func(e Environment) bool {
			return e.Slug == v
		}) {
			return m, errs.Invalidf("env_pairs", "%s: %s is not an env of this stack", k, v)
		}
	}
	if pairs == nil {
		pairs = map[string]string{}
	}
	return o.managed.SetEnvPairs(ctx, m, pairs)
}

// CreateSliceTile makes a slice tile: a database or bucket cut from the
// instance provision_from (<stack>:<env>:<tile>) names, provisioned at its
// deploy. provision_from is parsed here; whether the target exists and
// admits this env is checked by the promote plan and at deploy, never here.
// defaultAccess "" is write. A config-managed stack declares its slices in
// the stack file.
func (o *Orchestrator) CreateSliceTile(
	ctx context.Context,
	envID, slug, provisionFrom, defaultAccess string,
) (Tile, error) {
	e, err := o.envs.Get(ctx, envID)
	if err != nil {
		return Tile{}, err
	}
	st, err := o.stacks.Get(ctx, e.StackID)
	if err != nil {
		return Tile{}, err
	}
	if st.ConfigRepo != "" {
		return Tile{}, errs.Conflictf("stack %s is config-managed; declare the slice tile in its stack file", st.Slug)
	}
	t := Tile{
		StackID:       st.ID,
		EnvironmentID: e.ID,
		Name:          slug,
		Slug:          slug,
		Kind:          tile.Slice,
		ProvisionFrom: &provisionFrom,
	}
	if defaultAccess != "" {
		t.DefaultAccess = &defaultAccess
	}
	return o.tiles.Create(ctx, t)
}

// sliceTile is tile id when it is a slice tile.
func (o *Orchestrator) sliceTile(ctx context.Context, id string) (Tile, error) {
	s, err := o.tiles.Get(ctx, id)
	if err != nil {
		return s, err
	}
	if s.Kind != tile.Slice {
		return s, errs.Invalidf("tile", "%s is a %s tile, not a slice", s.Slug, s.Kind)
	}
	return s, nil
}

// SetSliceAccess sets consumer consumerTileID's access to slice sliceSlug of
// its own env: its slice_access entry is replaced or added. A consumer
// already bound is re-granted in place (its user and password stay).
// ponytail: no redeploy; an unbound consumer binds at its next deploy.
func (o *Orchestrator) SetSliceAccess(ctx context.Context, consumerTileID, sliceSlug, access string) (Tile, error) {
	c, err := o.tiles.Get(ctx, consumerTileID)
	if err != nil {
		return c, err
	}
	if c.Kind == tile.Slice || c.Kind == tile.Managed {
		return c, errs.Invalidf("tile", "%s is a %s tile; only a tile that runs holds slice access", c.Slug, c.Kind)
	}
	s, err := o.tiles.GetBySlug(ctx, c.EnvironmentID, sliceSlug)
	if errors.Is(err, errs.ErrNotFound) {
		return c, errs.Invalidf("slice", "no slice tile %s in this env", sliceSlug)
	}
	if err != nil {
		return c, err
	}
	if s.Kind != tile.Slice {
		return c, errs.Invalidf("slice", "%s is a %s tile, not a slice", s.Slug, s.Kind)
	}
	cur := c
	cur.SliceAccess = slices.Clone(c.SliceAccess)
	i := slices.IndexFunc(cur.SliceAccess, func(a store.SliceAccess) bool {
		return a.From == s.Slug
	})
	entry := store.SliceAccess{
		From:   s.Slug,
		Access: access,
	}
	if i >= 0 {
		cur.SliceAccess[i] = entry
	} else {
		cur.SliceAccess = append(cur.SliceAccess, entry)
	}
	out, _, err := o.tiles.Update(ctx, c, cur)
	if err != nil {
		return out, err
	}
	bound, err := o.managed.Bound(ctx, out.ID)
	if err != nil {
		return out, err
	}
	b, ok := bound[s.ID]
	if !ok {
		return out, nil
	}
	p, err := o.managed.GetProvision(ctx, b.ProvisionID)
	if err != nil {
		return out, err
	}
	_, err = o.engines.Bind(ctx, p, out, access)
	return out, err
}

// SetSliceOnRemove is what removing slice tile sliceTileID does to its
// database or bucket: keep | drop. An ephemeral env always drops.
func (o *Orchestrator) SetSliceOnRemove(ctx context.Context, sliceTileID, onRemove string) (Tile, error) {
	s, err := o.sliceTile(ctx, sliceTileID)
	if err != nil {
		return s, err
	}
	cur := s
	cur.OnRemove = &onRemove
	out, _, err := o.tiles.Update(ctx, s, cur)
	return out, err
}

// SetSliceDefaultAccess is the access slice tile sliceTileID grants a
// consumer that names it by a ref alone: read | write. A config-managed
// stack sets it in the stack file.
func (o *Orchestrator) SetSliceDefaultAccess(ctx context.Context, sliceTileID, access string) (Tile, error) {
	s, err := o.sliceTile(ctx, sliceTileID)
	if err != nil {
		return s, err
	}
	st, err := o.stacks.Get(ctx, s.StackID)
	if err != nil {
		return s, err
	}
	if st.ConfigRepo != "" {
		return s, errs.Conflictf("stack %s is config-managed; set default_access in its stack file", st.Slug)
	}
	cur := s
	cur.DefaultAccess = &access
	out, _, err := o.tiles.Update(ctx, s, cur)
	return out, err
}

// SliceBinding is one consumer's cred on a slice, its secret left out.
type SliceBinding struct {
	ConsumerID string    `json:"consumer_id"`
	Consumer   string    `json:"consumer"` // the consumer tile's slug
	Kind       string    `json:"kind"`
	Access     string    `json:"access"`
	User       string    `json:"user"`
	Since      time.Time `json:"since"`
}

// Bindings are the consumers holding a cred on slice tile sliceTileID;
// none until it is provisioned. Never the password or the outputs.
func (o *Orchestrator) Bindings(ctx context.Context, sliceTileID string) ([]SliceBinding, error) {
	s, err := o.sliceTile(ctx, sliceTileID)
	if err != nil {
		return nil, err
	}
	p, ok, err := o.managed.ProvisionOf(ctx, s.ID)
	if err != nil || !ok {
		return nil, err
	}
	bs, err := o.managed.Bindings(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	out := make([]SliceBinding, 0, len(bs))
	for _, b := range bs {
		c, err := o.tiles.Get(ctx, b.ConsumerTileID)
		if err != nil {
			return nil, err
		}
		out = append(out, SliceBinding{
			ConsumerID: c.ID,
			Consumer:   c.Slug,
			Kind:       c.Kind,
			Access:     b.Access,
			User:       b.DBUser,
			Since:      b.CreatedAt,
		})
	}
	return out, nil
}

// ConsumerBinding is one cred a consumer holds on a slice, its secret left
// out.
type ConsumerBinding struct {
	SliceID string    `json:"slice_id"`
	Slice   string    `json:"slice"` // the slice tile's slug
	Access  string    `json:"access"`
	User    string    `json:"user"`
	Since   time.Time `json:"since"`
}

// ConsumerBindings are the slices consumer consumerTileID holds a cred on,
// by slug: the ones its slice_access names and the ones a ref alone bound
// (DECIDE 204 (b)). Never the password or the outputs.
func (o *Orchestrator) ConsumerBindings(ctx context.Context, consumerTileID string) ([]ConsumerBinding, error) {
	bound, err := o.managed.Bound(ctx, consumerTileID)
	if err != nil {
		return nil, err
	}
	out := make([]ConsumerBinding, 0, len(bound))
	for sliceID, b := range bound {
		s, err := o.tiles.Get(ctx, sliceID)
		if err != nil {
			return nil, err
		}
		out = append(out, ConsumerBinding{
			SliceID: s.ID,
			Slice:   s.Slug,
			Access:  b.Access,
			User:    b.DBUser,
			Since:   b.CreatedAt,
		})
	}
	slices.SortFunc(out, func(a, b ConsumerBinding) int {
		return strings.Compare(a.Slice, b.Slice)
	})
	return out, nil
}

// SliceView is a slice tile as its drawer reads it.
type SliceView struct {
	TileID        string `json:"tile_id"`
	Slug          string `json:"slug"`
	ProvisionFrom string `json:"provision_from"` // as written
	Target        string `json:"target"`         // <stack>:<env>:<tile> it resolves to now
	TargetTileID  string `json:"target_tile_id"` // that instance tile's id; "" with Target
	Blocker       string `json:"blocker"`        // why it does not resolve; Target is "" then
	DefaultAccess string `json:"default_access"`
	OnRemove      string `json:"on_remove"`
	Provisioned   bool   `json:"provisioned"`
	Name          string `json:"name"`    // the database or bucket, once provisioned
	Network       string `json:"network"` // the instance's network
}

// SliceOf is slice tile sliceTileID with its target resolved by the deploy's
// own rule. A target that does not resolve (moved allow list, env pairs,
// unset param) is the view's Blocker, not an error.
func (o *Orchestrator) SliceOf(ctx context.Context, sliceTileID string) (SliceView, error) {
	s, err := o.sliceTile(ctx, sliceTileID)
	if err != nil {
		return SliceView{}, err
	}
	v := SliceView{
		TileID:        s.ID,
		Slug:          s.Slug,
		ProvisionFrom: str(s.ProvisionFrom),
		DefaultAccess: str(s.DefaultAccess),
		OnRemove:      str(s.OnRemove),
	}
	p, ok, err := o.managed.ProvisionOf(ctx, s.ID)
	if err != nil {
		return v, err
	}
	if ok {
		v.Provisioned = true
		v.Name = p.DBName
		v.Network = managed.Network(p.InstanceID)
	}
	it, err := o.deploy.Target(ctx, s)
	if blocks(err) {
		v.Blocker = err.Error()
		return v, nil
	}
	if err != nil {
		return v, err
	}
	e, err := o.envs.Get(ctx, it.EnvironmentID)
	if err != nil {
		return v, err
	}
	st, err := o.stacks.Get(ctx, it.StackID)
	if err != nil {
		return v, err
	}
	v.Target = st.Slug + ":" + e.Slug + ":" + it.Slug
	v.TargetTileID = it.ID
	if ok {
		return v, nil
	}
	m, err := o.managed.GetByTile(ctx, it.ID)
	if err != nil {
		return v, err
	}
	v.Network = managed.Network(m.ID)
	return v, nil
}

// blocks is an error a deploy would park or fail on with a reason.
func blocks(err error) bool {
	_, conflict := errs.IsConflict(err)
	_, unset := errs.IsUnset(err)
	_, invalid := errs.IsInvalid(err)
	return conflict || unset || invalid
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// InstanceSlice is one slice cut from an instance: its provision, the
// slice tile it belongs to and where that lives (any env of the org), and
// how many consumers hold a cred on it.
type InstanceSlice struct {
	Provision
	Slice    string `json:"slice"` // the slice tile's slug
	Stack    string `json:"stack"` // its stack's slug
	Env      string `json:"env"`   // its env's slug
	Bindings int    `json:"bindings"`
}

// InstanceSlices is a managed tile's instance and every slice cut from it.
func (o *Orchestrator) InstanceSlices(
	ctx context.Context,
	instanceTileID string,
) (ManagedInstance, []InstanceSlice, error) {
	m, err := o.managed.GetByTile(ctx, instanceTileID)
	if err != nil {
		return m, nil, err
	}
	ps, err := o.managed.ByInstance(ctx, m.ID)
	if err != nil {
		return m, nil, err
	}
	out := make([]InstanceSlice, 0, len(ps))
	for _, p := range ps {
		s, err := o.tiles.Get(ctx, p.TileID)
		if err != nil {
			return m, nil, err
		}
		e, err := o.envs.Get(ctx, s.EnvironmentID)
		if err != nil {
			return m, nil, err
		}
		st, err := o.stacks.Get(ctx, s.StackID)
		if err != nil {
			return m, nil, err
		}
		bs, err := o.managed.Bindings(ctx, p.ID)
		if err != nil {
			return m, nil, err
		}
		out = append(out, InstanceSlice{
			Provision: p,
			Slice:     s.Slug,
			Stack:     st.Slug,
			Env:       e.Slug,
			Bindings:  len(bs),
		})
	}
	return m, out, nil
}
