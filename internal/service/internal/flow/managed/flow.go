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
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/stack"
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
	Stacks    *stack.Leaf
	// S3 is the bucket client for an instance's API (s3.Admin in
	// production); nil = no s3 engine.
	S3 func(endpoint, access, secret string) S3Admin
	// PublicBase is the instance tile's public scheme+host, "" when it has
	// none (the orchestrator's, through leaf/domainres AutoHost); nil = none.
	PublicBase func(ctx context.Context, t store.Tile) string
	// ReadyWait bounds the readiness poll; 0 = 60s. ReadyPoll is its tick; 0 = 2s.
	ReadyWait, ReadyPoll time.Duration
}

// Container is the instance tile's container as flow/deploy runs it.
type Container struct {
	Image    string
	Cmd      []string
	Env      []string
	Binds    []string           // resolved "volume:/path"
	Networks []docker.NetAttach // the instance network, with its alias there
}

// Container gives flow/deploy the instance's container: the engine's
// definition, its credentials as first-boot env, its volumes (env-scoped,
// DECIDE 194) and the instance network every consumer of its slices joins.
func (f *Flow) Container(ctx context.Context, t store.Tile) (Container, error) {
	m, e, err := f.instance(ctx, t.ID)
	if err != nil {
		return Container{}, err
	}
	def := e.Definition()
	c := Container{
		Image: def.Image,
		Cmd:   def.Command,
		Env:   def.Config(f.facts(ctx, t, m, def)),
	}
	if t.ImageRef != "" {
		c.Image = t.ImageRef // the instance's own pin
	}
	for i, path := range def.Volumes {
		sl := t.Slug + "-data"
		if i > 0 {
			sl += "-" + strconv.Itoa(i+1)
		}
		v, _, err := f.Volumes.Declare(ctx, volume.Scope{Kind: "env", ID: t.EnvironmentID}, sl, 0, &m.ID)
		if err != nil {
			return c, err
		}
		name, err := f.Volumes.Ensure(ctx, v)
		if err != nil {
			return c, err
		}
		c.Binds = append(c.Binds, name+":"+path)
	}
	net := managed.Network(m.ID)
	if err := f.Envs.Shared(ctx, net); err != nil {
		return c, err
	}
	c.Networks = []docker.NetAttach{
		{
			Name:    net,
			Aliases: []string{alias(t, m)},
		},
	}
	return c, nil
}

// alias is the instance's name on its network. Not the bare slug: a
// consumer's own env may hold a tile of that slug, and the consumer sits on
// both networks.
func alias(t store.Tile, m store.ManagedInstance) string {
	return t.Slug + "-" + m.ID[:min(8, len(m.ID))]
}

// Ready polls the engine's own probe until it answers or the wait runs out.
// It does not pre-check for a container: a fresh instance has none running
// for its first seconds.
func (f *Flow) Ready(ctx context.Context, t store.Tile) error {
	m, e, err := f.instance(ctx, t.ID)
	if err != nil {
		return err
	}
	wait := f.ReadyWait
	if wait == 0 {
		wait = time.Minute
	}
	poll := f.ReadyPoll
	if poll == 0 {
		poll = 2 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		err = e.Ready(ctx, f.facts(ctx, t, m, e.Definition()), f.tools(t, m))
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

// Provision is slice tile s's deploy: its database or bucket on instance
// tile it, made once with an owner cred. The name is the slice's own
// address, <stack>_<env>_<slice> (DECIDE 197; the engine's SliceName makes
// it an identifier or a bucket name), so a kept slice added back in the
// same env finds its data. With the row present it re-syncs through the
// engine's idempotent Provision (a database dropped behind stackr's back
// comes back, empty).
func (f *Flow) Provision(ctx context.Context, it store.Tile, s store.Tile) (store.Provision, error) {
	m, e, err := f.instance(ctx, it.ID)
	if err != nil {
		return store.Provision{}, err
	}
	def := e.Definition()
	inst := f.facts(ctx, it, m, def)
	p, ok, err := f.Instances.ProvisionOf(ctx, s.ID)
	if err != nil {
		return p, err
	}
	if ok {
		if p.InstanceID != m.ID {
			return p, errs.Conflictf(
				"slice %s lives on another instance than %s; moving a slice is not supported, "+
					"remove it in one release and add it back in the next",
				s.Slug,
				it.Slug,
			)
		}
		return p, e.Provision(ctx, inst, slice(p), f.tools(it, m))
	}
	name, err := f.sliceName(ctx, def, s)
	if err != nil {
		return p, err
	}
	taken, err := f.Instances.Names(ctx, m.ID)
	if err != nil {
		return p, err
	}
	if slices.Contains(taken, name) {
		return p, errs.Conflictf(
			"slice %s: %s on %s is held by another slice or binding; rename the slice tile",
			s.Slug,
			name,
			it.Slug,
		)
	}
	sl := Slice{
		Name:     name,
		Password: managed.Password(),
	}
	sl.User = sl.Name
	if def.RootCreds {
		sl.User = m.AdminUser
		sl.Password = m.AdminPassword
	}
	if err := e.Provision(ctx, inst, sl, f.tools(it, m)); err != nil {
		return p, fmt.Errorf("provision %s on %s: %w", sl.Name, it.Slug, err)
	}
	return f.Instances.CreateProvision(ctx, m, s.ID, managed.Slice{
		DBName:     sl.Name,
		DBUser:     sl.User,
		DBPassword: sl.Password,
	})
}

// sliceName is slice tile s's name on its instance: its stack, env and
// slug joined by the engine's separator, spelled as the engine spells names.
// ponytail: hyphenated slugs can meet (stack my-shop env dev, stack my env
// shop-dev) and a name past maxName is cut; Provision refuses the clash, a
// hash suffix when one bites.
func (f *Flow) sliceName(ctx context.Context, def Definition, s store.Tile) (string, error) {
	e, err := f.Envs.Get(ctx, s.EnvironmentID)
	if err != nil {
		return "", err
	}
	st, err := f.Stacks.Get(ctx, s.StackID)
	if err != nil {
		return "", err
	}
	return def.SliceName(st.Slug + def.SliceSep + e.Slug + def.SliceSep + s.Slug), nil
}

// Bind gives consumer c its own cred on slice p at access (read | write)
// and stores the outputs minted for it. A consumer holding one at another
// access is re-granted in place (user and password stay); at the same
// access nothing happens.
func (f *Flow) Bind(ctx context.Context, p store.Provision, c store.Tile, access string) (store.Binding, error) {
	bound, err := f.Instances.Bindings(ctx, p.ID)
	if err != nil {
		return store.Binding{}, err
	}
	i := slices.IndexFunc(bound, func(b store.Binding) bool {
		return b.ConsumerTileID == c.ID
	})
	if i >= 0 && bound[i].Access == access {
		return bound[i], nil
	}
	m, it, e, err := f.of(ctx, p)
	if err != nil {
		return store.Binding{}, err
	}
	def := e.Definition()
	inst := f.facts(ctx, it, m, def)
	var others []Grant
	for j, b := range bound {
		if j != i {
			others = append(others, grant(b))
		}
	}
	if i >= 0 {
		b := bound[i]
		g := grant(b)
		g.Access = access
		if err := e.Bind(ctx, inst, slice(p), g, b.Access, others, f.tools(it, m)); err != nil {
			return b, fmt.Errorf("re-grant %s on %s: %w", b.DBUser, p.DBName, err)
		}
		return f.Instances.SetAccess(ctx, b, access)
	}
	g := Grant{
		User:     m.AdminUser,
		Password: m.AdminPassword,
		Access:   access,
	}
	if !def.RootCreds {
		taken, err := f.Instances.Names(ctx, m.ID)
		if err != nil {
			return store.Binding{}, err
		}
		g.User = uniqueSliceName(def.SliceName(p.DBName+"_"+c.Slug), taken, def.SliceSep)
		g.Password = managed.Password()
	}
	if err := e.Bind(ctx, inst, slice(p), g, "", others, f.tools(it, m)); err != nil {
		return store.Binding{}, fmt.Errorf("bind %s on %s: %w", g.User, p.DBName, err)
	}
	as := slice(p)
	as.User = g.User
	as.Password = g.Password
	return f.Instances.Bind(ctx, p, c.ID, managed.Cred{
		Access:   access,
		User:     g.User,
		Password: g.Password,
		Outputs:  outputs(e.Bindings(inst, as)),
	})
}

// Unbind drops the consumer's cred on its slice and the binding row; what
// the cred made in the database passes to the slice's owner.
func (f *Flow) Unbind(ctx context.Context, b store.Binding) error {
	p, err := f.Instances.GetProvision(ctx, b.ProvisionID)
	if err != nil {
		return err
	}
	m, it, e, err := f.of(ctx, p)
	if err != nil {
		return err
	}
	inst := f.facts(ctx, it, m, e.Definition())
	if err := e.Unbind(ctx, inst, slice(p), grant(b), f.tools(it, m)); err != nil {
		return fmt.Errorf("unbind %s from %s: %w", b.DBUser, p.DBName, err)
	}
	return f.Instances.Unbind(ctx, b.ID)
}

// Drop is slice p's tile going: every binding goes, then the data when drop
// (the caller's: the tile's on_remove, or an ephemeral env, which nothing
// else would ever reclaim), then the row. keep leaves the database or
// bucket on the instance, held by no row; the same slice added back in the
// same env finds it (its name is its address, DECIDE 197).
func (f *Flow) Drop(ctx context.Context, p store.Provision, drop bool, log io.Writer) error {
	bs, err := f.Instances.Bindings(ctx, p.ID)
	if err != nil {
		return err
	}
	for _, b := range bs {
		if err := f.Unbind(ctx, b); err != nil {
			return err
		}
	}
	if drop {
		m, it, e, err := f.of(ctx, p)
		if err != nil {
			return err
		}
		if err := e.Drop(ctx, f.facts(ctx, it, m, e.Definition()), slice(p), f.tools(it, m)); err != nil {
			return fmt.Errorf("drop %s: %w", p.DBName, err)
		}
		logf(log, "dropped %s\n", p.DBName)
	} else {
		logf(log, "kept %s on the instance (on_remove: keep)\n", p.DBName)
	}
	return f.Instances.DeleteProvision(ctx, p.ID)
}

// Teardown removes the instance's row: refused while slices live on it
// unless force, which drops them all. It runs before the container goes,
// so the engine can still reach it and a refusal leaves the instance
// running; the caller removes the container, then the instance network
// (Docker refuses that while anything sits on it). A failed drop is logged,
// not fatal: the rows go either way.
func (f *Flow) Teardown(ctx context.Context, it store.Tile, force bool, log io.Writer) error {
	m, _, err := f.instance(ctx, it.ID)
	if errors.Is(err, errs.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	held, err := f.Instances.Teardown(ctx, m, force)
	if err != nil {
		return err
	}
	for _, p := range held {
		// forced: the data goes with its instance
		if err := f.Drop(ctx, p, true, log); err != nil {
			logf(log, "drop %s: %v (the row goes anyway)\n", p.DBName, err)
		}
	}
	return f.Instances.Delete(ctx, m.ID)
}

// SliceWord is a slice tile's status word: its instance's (leaf/tile State)
// once provisioned, "none" before.
func (f *Flow) SliceWord(ctx context.Context, s store.Tile) (string, error) {
	p, ok, err := f.Instances.ProvisionOf(ctx, s.ID)
	if err != nil || !ok {
		return "none", err
	}
	m, err := f.Instances.Get(ctx, p.InstanceID)
	if err != nil {
		return "", err
	}
	it, err := f.Tiles.Get(ctx, m.TileID)
	if err != nil {
		return "", err
	}
	st, err := f.Tiles.State(ctx, it)
	return st.Word, err
}

// Backup and Restore expose the engine's argv for flow/backup.
func (f *Flow) Backup(ctx context.Context, it store.Tile, method string) ([]string, error) {
	m, e, err := f.instance(ctx, it.ID)
	if err != nil {
		return nil, err
	}
	return e.Backup(method, f.facts(ctx, it, m, e.Definition())), nil
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
	return e.Restore(method, target, f.facts(ctx, it, m, def)), nil
}

// Methods are the backup methods the tile's engine offers.
func (f *Flow) Methods(ctx context.Context, it store.Tile) ([]string, error) {
	_, e, err := f.instance(ctx, it.ID)
	if err != nil {
		return nil, err
	}
	return e.Definition().Backups, nil
}

// of is the instance a provision lives on: its row, its tile and engine.
func (f *Flow) of(ctx context.Context, p store.Provision) (store.ManagedInstance, store.Tile, ManagedTile, error) {
	m, err := f.Instances.Get(ctx, p.InstanceID)
	if err != nil {
		return m, store.Tile{}, nil, err
	}
	it, err := f.Tiles.Get(ctx, m.TileID)
	if err != nil {
		return m, it, nil, err
	}
	e, err := engine(m.Engine)
	return m, it, e, err
}

func (f *Flow) instance(ctx context.Context, tileID string) (store.ManagedInstance, ManagedTile, error) {
	m, err := f.Instances.GetByTile(ctx, tileID)
	if err != nil {
		return m, nil, err
	}
	e, err := engine(m.Engine)
	return m, e, err
}

// facts is the instance as an engine sees it. Consumers dial its alias on
// the instance network; its port is the engine's.
func (f *Flow) facts(ctx context.Context, t store.Tile, m store.ManagedInstance, def Definition) Instance {
	i := Instance{
		Slug:          t.Slug,
		Engine:        m.Engine,
		AdminUser:     m.AdminUser,
		AdminPassword: m.AdminPassword,
		AdminDB:       def.AdminDB,
		Host:          alias(t, m),
		Port:          def.Port,
	}
	if f.PublicBase != nil {
		i.PublicBase = f.PublicBase(ctx, t)
	}
	return i
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

func grant(b store.Binding) Grant {
	return Grant{
		User:     b.DBUser,
		Password: b.DBPassword,
		Access:   b.Access,
	}
}

func outputs(bs []Binding) map[string]string {
	out := make(map[string]string, len(bs))
	for _, b := range bs {
		out[b.Name] = b.Value
	}
	return out
}

// logf writes a line to the job log; a log write failing never fails the work.
func logf(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}
