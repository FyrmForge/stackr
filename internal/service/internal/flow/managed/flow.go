package managed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Flow sequences leaf/managed (rows), leaf/tile (exec), leaf/volume (the
// instance's data) and the engines.
type Flow struct {
	Tiles     *tile.Leaf
	Instances *managed.Leaf
	Volumes   *volume.Leaf
	Envs      *environment.Leaf
	// S3 is the bucket client for an instance's API (s3.Admin in
	// production); nil = no s3 engine.
	S3 func(endpoint, access, secret string) S3Admin
	// ReadyWait bounds the readiness poll; 0 = 60s. ReadyPoll is its tick; 0 = 2s.
	ReadyWait, ReadyPoll time.Duration
}

// Container is the instance tile's container as flow/deploy runs it.
type Container struct {
	Image    string
	Cmd      []string
	Env      []string
	Binds    []string           // resolved "volume:/path"
	Networks []docker.NetAttach // the shared network, for a stack or org instance
}

// Container gives flow/deploy the instance's container: the engine's
// definition, its credentials as first-boot env, its volumes (following the
// instance's scope) and, when shared, its shared network.
func (f *Flow) Container(ctx context.Context, t store.Tile) (Container, error) {
	m, e, err := f.instance(ctx, t.ID)
	if err != nil {
		return Container{}, err
	}
	def := e.Definition()
	c := Container{Image: def.Image, Cmd: def.Command, Env: def.Config(f.facts(t, m, def))}
	if t.ImageRef != "" {
		c.Image = t.ImageRef // the instance's own pin
	}
	for i, path := range def.Volumes {
		sl := t.Slug + "-data"
		if i > 0 {
			sl += "-" + strconv.Itoa(i+1)
		}
		v, _, err := f.Volumes.Declare(ctx, volume.Scope{Kind: m.ScopeKind, ID: m.ScopeID}, sl, 0, &m.ID)
		if err != nil {
			return c, err
		}
		name, err := f.Volumes.Ensure(ctx, v)
		if err != nil {
			return c, err
		}
		c.Binds = append(c.Binds, name+":"+path)
	}
	if m.ScopeKind != managed.Env {
		n := managed.Network(m.ID)
		if err := f.Envs.Shared(ctx, n); err != nil {
			return c, err
		}
		c.Networks = []docker.NetAttach{{Name: n, Aliases: []string{t.Slug}}}
	}
	return c, nil
}

// Ready polls the engine's own probe until it answers or the wait runs out.
// It does not pre-check for a container: a fresh instance has none running
// for its first seconds.
func (f *Flow) Ready(ctx context.Context, t store.Tile) error {
	m, e, err := f.instance(ctx, t.ID)
	if err != nil {
		return err
	}
	wait, poll := f.ReadyWait, f.ReadyPoll
	if wait == 0 {
		wait = time.Minute
	}
	if poll == 0 {
		poll = 2 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		err = e.Ready(ctx, f.facts(t, m, e.Definition()), f.tools(t, m))
		if err == nil || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// Attach cuts a slice for consumer on the instance tile it, named after the
// consumer unless name is given, and records its outputs. The instance must
// be visible from the consumer's env.
func (f *Flow) Attach(
	ctx context.Context,
	consumer store.Tile,
	it store.Tile,
	home managed.Home,
	name string,
	public bool,
	onRemove string,
) (store.Provision, error) {
	m, e, err := f.instance(ctx, it.ID)
	if err != nil {
		return store.Provision{}, err
	}
	vis, err := f.Instances.Visible(ctx, home)
	if err != nil {
		return store.Provision{}, err
	}
	if !visible(vis, m.ID) {
		return store.Provision{}, errs.Refusedf("%s is not shared with this environment", it.Slug)
	}
	def := e.Definition()
	if public && !def.PublicSlices {
		return store.Provision{}, errs.Refusedf("a %s cannot be public", def.SliceNoun)
	}
	if name == "" {
		name = consumer.Slug
	}
	taken, err := f.Instances.SliceNames(ctx, m.ID)
	if err != nil {
		return store.Provision{}, err
	}
	s := Slice{
		Name:     uniqueSliceName(def.SliceName(name), taken, def.SliceSep),
		Password: managed.Password(),
		Public:   public,
	}
	s.User = s.Name
	if def.PublicSlices { // ponytail: shared root keys, see s3.go
		s.User, s.Password = m.AdminUser, m.AdminPassword
	}
	inst := f.facts(it, m, def)
	if err := e.Provision(ctx, inst, s, f.tools(it, m)); err != nil {
		return store.Provision{}, fmt.Errorf("provision %s on %s: %w", s.Name, it.Slug, err)
	}
	p, err := f.Instances.Provision(ctx, m, consumer.ID, managed.Slice{
		Slug:       name,
		DBName:     s.Name,
		DBUser:     s.User,
		DBPassword: s.Password,
		Public:     public,
		OnRemove:   onRemove,
	})
	if err != nil {
		return p, err
	}
	return f.Instances.SetOutputs(ctx, p, outputs(e.Bindings(inst, s)))
}

// Reconcile re-provisions every slice the consumer holds (recreating one
// dropped behind stackr's back) and republishes its outputs, before the
// consumer's values resolve.
func (f *Flow) Reconcile(ctx context.Context, consumer store.Tile, log io.Writer) error {
	ps, err := f.Instances.ForConsumer(ctx, consumer.ID)
	if err != nil {
		return err
	}
	for _, p := range ps {
		m, err := f.Instances.Get(ctx, p.InstanceID)
		if err != nil {
			return err
		}
		it, err := f.Tiles.Get(ctx, m.TileID)
		if err != nil {
			return err
		}
		e, err := engine(m.Engine)
		if err != nil {
			return err
		}
		inst, s := f.facts(it, m, e.Definition()), slice(p)
		if err := e.Provision(ctx, inst, s, f.tools(it, m)); err != nil {
			return fmt.Errorf("re-provision %s on %s: %w", p.DBName, it.Slug, err)
		}
		if _, err := f.Instances.SetOutputs(ctx, p, outputs(e.Bindings(inst, s))); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(log, "slice %s on %s is in place\n", p.DBName, it.Slug)
	}
	return nil
}

// Detach is a consumer letting go of its slice: the binding goes; the slice
// is dropped only when on_remove says drop or the env is ephemeral.
func (f *Flow) Detach(ctx context.Context, p store.Provision, ephemeral bool) error {
	drop, err := f.Instances.Release(ctx, p, ephemeral)
	if err != nil || !drop {
		return err
	}
	m, err := f.Instances.Get(ctx, p.InstanceID)
	if err != nil {
		return err
	}
	it, err := f.Tiles.Get(ctx, m.TileID)
	if err != nil {
		return err
	}
	e, err := engine(m.Engine)
	if err != nil {
		return err
	}
	// The rows share one slice: drop in the engine once, when the last goes.
	others, err := f.Instances.ByInstance(ctx, m.ID)
	if err != nil {
		return err
	}
	shared := 0
	for _, o := range others {
		if o.DBName == p.DBName && o.ID != p.ID {
			shared++
		}
	}
	if shared == 0 {
		if err := e.Drop(ctx, f.facts(it, m, e.Definition()), slice(p), f.tools(it, m)); err != nil {
			return err
		}
	}
	return f.Instances.DropRow(ctx, p.ID)
}

// Teardown removes the instance's slices and row: refused while slices are
// held unless force. Slices go before the container (the caller removes the
// tile after), so the engine can still reach it. A failed drop is logged,
// not fatal: the rows go either way.
func (f *Flow) Teardown(ctx context.Context, it store.Tile, force bool, log io.Writer) error {
	m, e, err := f.instance(ctx, it.ID)
	if errors.Is(err, errs.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	drop, err := f.Instances.Teardown(ctx, m, force)
	if err != nil {
		return err
	}
	inst := f.facts(it, m, e.Definition())
	for _, p := range drop {
		if err := e.Drop(ctx, inst, slice(p), f.tools(it, m)); err != nil {
			_, _ = fmt.Fprintf(log, "drop %s: %v (the row goes anyway)\n", p.DBName, err)
		}
	}
	return f.Instances.Delete(ctx, m.ID)
}

// Backup and Restore expose the engine's argv for flow/backup.
func (f *Flow) Backup(ctx context.Context, it store.Tile, method string) ([]string, error) {
	m, e, err := f.instance(ctx, it.ID)
	if err != nil {
		return nil, err
	}
	return e.Backup(method, f.facts(it, m, e.Definition())), nil
}

func (f *Flow) Restore(ctx context.Context, it store.Tile, method, target string) ([]string, error) {
	m, e, err := f.instance(ctx, it.ID)
	if err != nil {
		return nil, err
	}
	def := e.Definition()
	if target == "" {
		target = def.AdminDB
	}
	return e.Restore(method, target, f.facts(it, m, def)), nil
}

// Methods are the backup methods the tile's engine offers.
func (f *Flow) Methods(ctx context.Context, it store.Tile) ([]string, error) {
	_, e, err := f.instance(ctx, it.ID)
	if err != nil {
		return nil, err
	}
	return e.Definition().Backups, nil
}

func (f *Flow) instance(ctx context.Context, tileID string) (store.ManagedInstance, ManagedTile, error) {
	m, err := f.Instances.GetByTile(ctx, tileID)
	if err != nil {
		return m, nil, err
	}
	e, err := engine(m.Engine)
	return m, e, err
}

// facts is the instance as an engine sees it. Consumers dial the tile's
// alias; its port is the engine's.
// ponytail: PublicBase is "" until instance domains feed it.
func (f *Flow) facts(t store.Tile, m store.ManagedInstance, def Definition) Instance {
	return Instance{
		Slug:          t.Slug,
		Engine:        m.Engine,
		AdminUser:     m.AdminUser,
		AdminPassword: m.AdminPassword,
		AdminDB:       def.AdminDB,
		Host:          t.Slug,
		Port:          def.Port,
	}
}

// tools reach the instance: exec in its first running replica, and the S3
// API at its endpoint (the row's, else the alias).
// ponytail: stackrd must be able to route to that endpoint (DECIDE 28).
func (f *Flow) tools(t store.Tile, m store.ManagedInstance) Tools {
	x := Tools{
		Exec: func(ctx context.Context, cmd []string) (string, error) {
			cs, err := f.Tiles.Replicas(ctx, t)
			if err != nil {
				return "", err
			}
			for _, c := range cs {
				if c.State == "running" {
					return f.Tiles.Exec(ctx, t.ID, c.ID, cmd)
				}
			}
			return "", fmt.Errorf("%s is not running", t.Slug)
		},
	}
	if f.S3 != nil {
		ep := m.Endpoint
		if ep == "" {
			ep = fmt.Sprintf("http://%s:%d", t.Slug, Engines[m.Engine].Definition().Port)
		}
		x.S3 = f.S3(ep, m.AdminUser, m.AdminPassword)
	}
	return x
}

func slice(p store.Provision) Slice {
	return Slice{
		Name:     p.DBName,
		User:     p.DBUser,
		Password: p.DBPassword,
		Public:   p.Public,
	}
}

func outputs(bs []Binding) map[string]string {
	out := make(map[string]string, len(bs))
	for _, b := range bs {
		out[b.Name] = b.Value
	}
	return out
}

func visible(ms []store.ManagedInstance, id string) bool {
	return slices.ContainsFunc(ms, func(m store.ManagedInstance) bool { return m.ID == id })
}
