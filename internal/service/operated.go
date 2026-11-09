package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/s3"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type (
	OperatedInstance = store.OperatedInstance
	Provision        = store.Provision
	PoolerStatus     = mflow.PoolerStatus
)

// CreateOperatedTile makes an operated tile (postgres | s3) and its env-scoped
// instance row. Nothing runs until Deploy. major picks a postgres major, 0 =
// the newest one still getting security fixes.
func (o *Orchestrator) CreateOperatedTile(ctx context.Context, t Tile, engine string, major int) (Tile, error) {
	out, _, err := o.createOperated(ctx, t, engine, "", major)
	return out, err
}

// createOperated is CreateOperatedTile, plus slice tile slice cut from the
// instance in the same transaction when slice is set ("-" = <tile>-db). major
// -1 keeps the row's image as it is, whatever its major's support (a restore
// copy of an instance that already runs it).
func (o *Orchestrator) createOperated(ctx context.Context, t Tile, engine, slice string, major int) (Tile, Tile, error) {
	if !deploy.KnownEngine(engine) {
		return Tile{}, Tile{}, errs.Invalidf("engine", "%q is not an engine stackr runs.", engine)
	}
	e, err := o.envs.Get(ctx, t.EnvironmentID)
	if err != nil {
		return Tile{}, Tile{}, err
	}
	st, err := o.stacks.Get(ctx, e.StackID)
	if err != nil {
		return Tile{}, Tile{}, err
	}
	if slice != "" && st.ConfigRepo != "" {
		return Tile{}, Tile{}, errs.Conflictf("stack %s is config-managed; declare the slice tile in its stack file", st.Slug)
	}
	t.Kind, t.StackID = tile.Operated, st.ID
	days := 0
	if mflow.Engines[engine].Definition().ArchiveDir != "" {
		if _, ok := o.visibleOffBox(ctx, st.OrgID); ok {
			days = DefaultPITRDays // the daily dump's rule: only with a copy off this box
		}
	}
	switch {
	case engine != "postgres" && major > 0:
		return Tile{}, Tile{}, errs.Invalidf("major", "only postgres has a major version.")
	case engine != "postgres":
	case major < 0:
		// the layout and flags follow the major: an image that does not name
		// one is refused, never guessed at
		if major = mflow.ImageMajor(t.ImageRef); major == 0 {
			return Tile{}, Tile{}, errs.Invalidf("image_ref", "%s names no postgres major; pin a numeric tag such as postgres:17.", t.ImageRef)
		}
	default:
		if t.ImageRef, major, err = mflow.PGPick(major, t.ImageRef, time.Now()); err != nil {
			return Tile{}, Tile{}, err
		}
	}
	var out, sl Tile
	err = o.store.Tx(ctx, func(tx store.Tx) error {
		tl := tile.New(tx.Tiles, o.docker, nil)
		var err error
		if out, err = tl.Create(ctx, t); err != nil {
			return err
		}
		ml := operated.New(tx.OperatedInstances, tx.Provisions, tx.Bindings)
		m, err := ml.Create(ctx, out.ID, engine, "stackr", "")
		if err != nil {
			return err
		}
		if _, err = opmember.New(tx.OperatedMembers).CreatePrimary(ctx, m.ID); err != nil {
			return err
		}
		if major > 0 {
			if m, err = ml.SetMajor(ctx, m, major); err != nil {
				return err
			}
		}
		if days > 0 {
			if _, err = ml.SetPITR(ctx, m, days); err != nil {
				return err
			}
		}
		if slice == "" {
			return nil
		}
		if slice == "-" {
			slice = out.Slug + "-db"
		}
		from := st.Slug + ":" + e.Slug + ":" + out.Slug
		sl, err = tl.Create(ctx, Tile{
			StackID:       st.ID,
			EnvironmentID: e.ID,
			Name:          slice,
			Slug:          slice,
			Kind:          tile.Slice,
			ProvisionFrom: &from,
		})
		return err
	})
	return out, sl, err
}

// OperatedInstances are the env's own instances. Another env or stack reaches
// one through a slice tile's provision_from, which the instance's allow list
// gates at plan time.
func (o *Orchestrator) OperatedInstances(ctx context.Context, envID string) ([]OperatedInstance, error) {
	ts, err := o.tiles.List(ctx, envID)
	if err != nil {
		return nil, err
	}
	var out []OperatedInstance
	for _, t := range ts {
		if t.Kind != tile.Operated {
			continue
		}
		m, err := o.operated.GetByTile(ctx, t.ID)
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

// instanceOf is operated tile tileID and its instance row.
func (o *Orchestrator) instanceOf(ctx context.Context, tileID string) (Tile, OperatedInstance, error) {
	t, err := o.tiles.Get(ctx, tileID)
	if err != nil {
		return t, OperatedInstance{}, err
	}
	if t.Kind != tile.Operated {
		return t, OperatedInstance{}, errs.Invalidf("tile", "%s is kind %s, not operated", t.Slug, t.Kind)
	}
	m, err := o.operated.GetByTile(ctx, t.ID)
	if errors.Is(err, errs.ErrNotFound) {
		return t, m, errs.Conflictf("operated tile %s has no instance", t.Slug)
	}
	return t, m, err
}

// SetOperatedAllow replaces the instance's allow list, the org:stack:env:tile
// patterns that may cut a slice from it beyond its own env, which always may. Nothing
// redeploys: a slice the list no longer admits fails its next deploy with
// the reason. A stack file that declares the list sets it again on promote.
func (o *Orchestrator) SetOperatedAllow(ctx context.Context, tileID string, list []string) (OperatedInstance, error) {
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
	return o.operated.SetAllow(ctx, m, og.Slug, list)
}

// SetOperatedEnvPairs replaces the instance's env pairs: the env a slice's
// provision_from names (a consumer's env) to one of this stack's envs. A
// non-empty map is the whole rule: an env it leaves out resolves nowhere.
// Keys are any env name; values must be envs of the stack.
func (o *Orchestrator) SetOperatedEnvPairs(
	ctx context.Context,
	tileID string,
	pairs map[string]string,
) (OperatedInstance, error) {
	t, m, err := o.instanceOf(ctx, tileID)
	if err != nil {
		return m, err
	}
	envs, err := o.envs.List(ctx, t.StackID)
	if err != nil {
		return m, err
	}
	for _, v := range pairs {
		if !slices.ContainsFunc(envs, func(e Environment) bool {
			return e.Slug == v
		}) {
			return m, errs.Invalidf("env_pairs", "%s is not an environment of this stack.", v)
		}
	}
	if pairs == nil {
		pairs = map[string]string{}
	}
	// One map per tile slug (DECIDE 212): every instance of this slug in the
	// stack takes it, so a deploy reads the same map whichever env it finds
	// first.
	for _, e := range envs {
		it, err := o.tiles.GetBySlug(ctx, e.ID, t.Slug)
		if errors.Is(err, errs.ErrNotFound) {
			continue
		}
		if err != nil {
			return m, err
		}
		im, err := o.operated.GetByTile(ctx, it.ID)
		if errors.Is(err, errs.ErrNotFound) {
			continue
		}
		if err != nil {
			return m, err
		}
		if im.ID == m.ID {
			continue
		}
		if _, err := o.operated.SetEnvPairs(ctx, im, pairs); err != nil {
			return m, err
		}
	}
	return o.operated.SetEnvPairs(ctx, m, pairs)
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
		return s, errs.Invalidf("tile", "%s is kind %s, not a slice", s.Slug, s.Kind)
	}
	return s, nil
}

// sliceAccessDefault is the access SetSliceAccess takes to drop a
// consumer's slice_access entry: it then gets the slice's default access.
const sliceAccessDefault = "default"

// SetSliceAccess sets consumer consumerTileID's access to slice sliceSlug of
// its own env: read | write replaces or adds its slice_access entry,
// "default" removes it (no entry: a no-op). A consumer already
// bound is re-granted in place (its user and password stay), at the slice's
// default once its entry is gone.
// ponytail: no redeploy; an unbound consumer binds at its next deploy.
func (o *Orchestrator) SetSliceAccess(ctx context.Context, consumerTileID, sliceSlug, access string) (Tile, error) {
	if access != tile.Read && access != tile.Write && access != sliceAccessDefault {
		return Tile{}, errs.Invalidf("access", "access must be read, write or default")
	}
	c, err := o.tiles.Get(ctx, consumerTileID)
	if err != nil {
		return c, err
	}
	if c.Kind == tile.Slice || c.Kind == tile.Operated {
		return c, errs.Invalidf("tile", "%s is a %s tile; only a tile that runs holds slice access", c.Slug, c.Kind)
	}
	s, err := o.tiles.GetBySlug(ctx, c.EnvironmentID, sliceSlug)
	if errors.Is(err, errs.ErrNotFound) && access == sliceAccessDefault {
		// The slice is gone and the entry is stale: drop it, nothing to re-grant.
		cur := c
		cur.SliceAccess = slices.DeleteFunc(slices.Clone(c.SliceAccess), func(a store.SliceAccess) bool {
			return a.From == sliceSlug
		})
		out, _, err := o.tiles.Update(ctx, c, cur)
		return out, err
	}
	if errors.Is(err, errs.ErrNotFound) {
		return c, errs.Invalidf("slice", "no slice tile %s in this env", sliceSlug)
	}
	if err != nil {
		return c, err
	}
	if s.Kind != tile.Slice {
		return c, errs.Invalidf("slice", "%s is kind %s, not a slice", s.Slug, s.Kind)
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
	switch {
	case access == sliceAccessDefault && i < 0:
		return c, nil
	case access == sliceAccessDefault:
		cur.SliceAccess = slices.Delete(cur.SliceAccess, i, i+1)
		access = str(s.DefaultAccess)
		if access == "" {
			access = tile.Write
		}
	case i >= 0:
		cur.SliceAccess[i] = entry
	default:
		cur.SliceAccess = append(cur.SliceAccess, entry)
	}
	out, _, err := o.tiles.Update(ctx, c, cur)
	if err != nil {
		return out, err
	}
	bound, err := o.operated.Bound(ctx, out.ID)
	if err != nil {
		return out, err
	}
	b, ok := bound[s.ID]
	if !ok {
		return out, nil
	}
	p, err := o.operated.GetProvision(ctx, b.ProvisionID)
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

// SetSliceURL is what a postgres slice's url, host and port point at: direct
// (the instance) or pooled (its pooler). The stored outputs follow at once and
// the consumers redeploy to read them. A config-managed stack sets it in the
// stack file.
func (o *Orchestrator) SetSliceURL(ctx context.Context, sliceTileID, url string) (Tile, error) {
	s, err := o.sliceTile(ctx, sliceTileID)
	if err != nil {
		return s, err
	}
	st, err := o.stacks.Get(ctx, s.StackID)
	if err != nil {
		return s, err
	}
	if st.ConfigRepo != "" {
		return s, errs.Conflictf("stack %s is config-managed; set url in its stack file", st.Slug)
	}
	var m OperatedInstance
	p, ok, err := o.operated.ProvisionOf(ctx, s.ID)
	if err != nil {
		return s, err
	}
	if ok {
		if m, err = o.operated.Get(ctx, p.InstanceID); err != nil {
			return s, err
		}
		if url != tile.Direct && m.Engine != "postgres" {
			return s, errs.Invalidf("url", "url %s applies to postgres slices only", url)
		}
	}
	cur := s
	cur.SliceURL = &url
	out, _, err := o.tiles.Update(ctx, s, cur)
	if err != nil || !ok {
		return out, err // not provisioned: its first bind reads the setting
	}
	it, err := o.tiles.Get(ctx, m.TileID)
	if err != nil {
		return out, err
	}
	ids, err := o.engines.Republish(ctx, it)
	if err != nil {
		return out, err
	}
	var readers []Tile
	for _, id := range ids {
		c, err := o.tiles.Get(ctx, id)
		if err != nil {
			return out, err
		}
		readers = append(readers, c)
	}
	return out, o.redeployUnion(ctx, readers)
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
	Consumer   string    `json:"consumer"`      // the consumer tile's slug
	Name       string    `json:"consumer_name"` // and its name, as people read it
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
	p, ok, err := o.operated.ProvisionOf(ctx, s.ID)
	if err != nil || !ok {
		return nil, err
	}
	bs, err := o.operated.Bindings(ctx, p.ID)
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
			Name:       c.Name,
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
	bound, err := o.operated.Bound(ctx, consumerTileID)
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
	URL           string `json:"url"` // direct | pooled: what url, host and port point at
	Provisioned   bool   `json:"provisioned"`
	Engine        string `json:"engine"`  // the instance's, once provisioned
	Name          string `json:"name"`    // the database or bucket, once provisioned
	Network       string `json:"network"` // the instance's network
	// A bucket's protection as it is applied: days old versions are kept
	// (0 = versioning off), the size cap in GB (0 = none), CORS origins.
	VersionDays int      `json:"version_days"`
	QuotaGB     int      `json:"quota_gb"`
	CORSOrigins []string `json:"cors_origins"`
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
		URL:           tile.URLOf(s),
		VersionDays:   s3.DefaultVersionDays,
		CORSOrigins:   append([]string{}, s.CORSOrigins...),
	}
	if s.VersionDays != nil {
		v.VersionDays = *s.VersionDays
	}
	if s.QuotaGB != nil {
		v.QuotaGB = *s.QuotaGB
	}
	p, ok, err := o.operated.ProvisionOf(ctx, s.ID)
	if err != nil {
		return v, err
	}
	if ok {
		v.Provisioned = true
		v.Name = p.DBName
		v.Network = operated.Network(p.InstanceID)
		if m, err := o.operated.Get(ctx, p.InstanceID); err == nil {
			v.Engine = m.Engine
		}
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
	m, err := o.operated.GetByTile(ctx, it.ID)
	if err != nil {
		return v, err
	}
	v.Network = operated.Network(m.ID)
	v.Engine = m.Engine
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

// InstanceSlices is an operated tile's instance and every slice cut from it.
func (o *Orchestrator) InstanceSlices(
	ctx context.Context,
	instanceTileID string,
) (OperatedInstance, []InstanceSlice, error) {
	m, err := o.operated.GetByTile(ctx, instanceTileID)
	if err != nil {
		return m, nil, err
	}
	ps, err := o.operated.ByInstance(ctx, m.ID)
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
		bs, err := o.operated.Bindings(ctx, p.ID)
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

// Pooler is the instance's pooler as the drawer shows it.
func (o *Orchestrator) Pooler(ctx context.Context, tileID string) (PoolerStatus, error) {
	t, _, err := o.instanceOf(ctx, tileID)
	if err != nil {
		return PoolerStatus{}, err
	}
	return o.engines.Pooler(ctx, t)
}
