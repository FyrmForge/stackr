package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envutil"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/deploy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/service/notify"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// SliceService owns the slices cut out of a managed instance: a logical
// database for postgres, a bucket for s3. A slice belongs to the instance
// that provides it, not to the tile that consumes it, which is why dropping
// one and detaching from one are different operations with different blast
// radii.
//
// The engine hooks below the line (infra/managedtiles) already do the
// cutting. What lived in the handlers, in four incompatible versions, was
// everything around it: who may provision from what, which environment a
// slice lands in, whether the instance is up yet, and what gets wired into
// the consumer afterwards.
type SliceService struct {
	store    repo.Store
	dbs      *managedtiles.Service
	engine   *deploy.Engine
	notifier *notify.Notifier
}

func NewSliceService(store repo.Store, dbs *managedtiles.Service, engine *deploy.Engine,
	n *notify.Notifier) *SliceService {
	return &SliceService{store: store, dbs: dbs, engine: engine, notifier: n}
}

func (s *SliceService) ready() error {
	if s.dbs == nil {
		return fmt.Errorf("database engine: %w", svcerr.ErrUnavailable)
	}
	return nil
}

// consumable refuses the tiles that cannot hold a slice. A managed instance
// consuming another instance's slice would be a database inside a database;
// a volume has no env to inject into.
func consumable(t *repo.Tile) error {
	if t == nil {
		return svcerr.ErrNotFound
	}
	if t.IsManaged() || t.IsVolume() {
		return invalid("", "only services and crons can consume provisions")
	}
	return nil
}

// Provision cuts a new slice out of instance for consumer.
//
// Eligible is the scope check — env-scoped instances serve their own
// environment, stack-scoped their stack, org-scoped their org — and it is the
// reason a provision cannot reach across tenants.
func (s *SliceService) Provision(ctx context.Context, instance, consumer *repo.Tile,
	name string, public bool) (*repo.Provision, error) {
	if err := consumable(consumer); err != nil {
		return nil, err
	}
	if err := s.ready(); err != nil {
		return nil, err
	}
	if instance == nil || !managedtiles.Eligible(ctx, s.store, instance, consumer) {
		return nil, invalid("instance", "invalid shared instance")
	}
	p, err := s.dbs.Provision(ctx, instance, consumer, name, public)
	if err != nil {
		return nil, svcerr.Invalidf("", "%v", err)
	}
	return p, nil
}

// Attach points a second consumer at an existing slice, so two tiles can
// share one database. Same environment only: the slice records the env it
// belongs to and its credentials are env-scoped.
func (s *SliceService) Attach(ctx context.Context, src *repo.Provision, consumer *repo.Tile) (*repo.Provision, *repo.Tile, error) {
	if err := consumable(consumer); err != nil {
		return nil, nil, err
	}
	if err := s.ready(); err != nil {
		return nil, nil, err
	}
	if src == nil || src.EnvID != consumer.EnvironmentID {
		return nil, nil, invalid("provision_id", "invalid database")
	}
	instance, err := s.store.GetTile(ctx, src.InstanceTileID)
	if err != nil || instance == nil {
		return nil, nil, invalid("provision_id", "instance not found")
	}
	p, err := s.dbs.AttachExisting(ctx, instance, src, consumer)
	if err != nil {
		return nil, nil, svcerr.Invalidf("", "%v", err)
	}
	return p, instance, nil
}

// Wire injects a slice's connection details into the consumer's env and
// redeploys it, so it joins the instance's shared network. It reports the
// variable name it actually used, which is not always the one asked for
// (Inject refuses to clobber a different existing value).
//
// Two behaviours are unioned here. Engines whose slices need more than one
// string — s3 needs an endpoint, a bucket and a key pair — inject their whole
// output set and ignore envVar; the panel injected only S3_ENDPOINT, which is
// not enough to reach a bucket with, so every s3 consumer added through the
// panel needed three variables written by hand afterwards. Everything else
// injects the single reference under envVar.
//
// A blank envVar is publish-only: the resource exists and the user writes the
// reference themselves. On a config-managed stack it is always publish-only,
// because the file owns consumer.Env and an injection here is stripped by the
// next apply.
func (s *SliceService) Wire(ctx context.Context, instance, consumer *repo.Tile,
	p *repo.Provision, envVar string) (used string, err error) {
	st, err := s.store.GetStack(ctx, consumer.StackID)
	if err != nil || st == nil {
		return "", svcerr.ErrNotFound
	}
	if st.ConfigManaged() {
		return "", nil
	}
	if managedtiles.Engines[instance.Engine].AutoInjectAll {
		for k, v := range s.dbs.AutoInjectVars(ctx, instance, p) {
			consumer.Env, _ = envutil.Inject(consumer.Env, k, v)
		}
		return "", s.saveAndRedeploy(ctx, consumer)
	}
	envVar = EnvVarName(envVar)
	if envVar == "" {
		return "", nil
	}
	ref := managedtiles.Ref(instance, p, managedtiles.DefaultOutput(instance.Engine))
	consumer.Env, used = envutil.Inject(consumer.Env, envVar, ref)
	return used, s.saveAndRedeploy(ctx, consumer)
}

func (s *SliceService) saveAndRedeploy(ctx context.Context, consumer *repo.Tile) error {
	if err := s.store.UpdateTile(ctx, consumer); err != nil {
		return err
	}
	if s.engine == nil {
		return nil
	}
	_, err := s.engine.Enqueue(ctx, consumer, "provision")
	return err
}

// EnvVarName folds a user-supplied variable name into a usable shell
// identifier ("" if nothing usable remains). The API took the name raw, so a
// name with a space or a dash in it produced a container env line the shell
// could not read.
func EnvVarName(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for i, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r == '_':
			b.WriteRune(r)
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 32) // upper-case, env-var convention
		case r >= '0' && r <= '9' && i > 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Detach unhooks one consumer from a slice and keeps the data (orphaned).
// Ownership is checked here rather than by the caller: a provision id from
// another tile must read as missing, not as someone else's row.
func (s *SliceService) Detach(ctx context.Context, consumer *repo.Tile, provisionID string) error {
	p, err := s.store.GetProvision(ctx, provisionID)
	if err != nil {
		return err
	}
	// Checked before the engine is looked at, so the answer to "is this mine"
	// never depends on how this process happens to be configured.
	if p == nil || consumer == nil || p.ConsumerTileID != consumer.ID {
		return svcerr.ErrNotFound
	}
	if err := s.ready(); err != nil {
		return err
	}
	return s.dbs.Detach(ctx, p)
}

// Cut creates a consumer-less slice on an instance, the shape a config file
// declares and the CLI's `infra provision` asks for.
//
// The four checks are the union of what the two callers each had half of.
// ServesEnv (the API's) because a slice records the env it belongs to and
// Attach refuses across environments, so a slice cut into the wrong one is a
// database nothing can ever use. Eligible, the readiness wait and
// adopt-or-create (the config engine's) because one apply can create an
// instance and its slices together, and because the file names the slice it
// means rather than a uniquified second copy of it.
//
// clone asks for a fresh uniquified copy instead of adopting: an ephemeral
// (PR) environment must never touch another env's data.
func (s *SliceService) Cut(ctx context.Context, instance *repo.Tile, env *repo.Environment,
	slug, name string, public bool, clone bool) (*repo.Provision, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if instance == nil || env == nil {
		return nil, svcerr.ErrNotFound
	}
	if !managedtiles.Eligible(ctx, s.store, instance,
		&repo.Tile{StackID: env.StackID, EnvironmentID: env.ID}) {
		return nil, svcerr.Invalidf("", "%s is not shared into %s", instance.Slug, env.Slug)
	}
	// Cutting a slice execs into the instance's container, so it has to be
	// accepting connections first.
	if err := s.dbs.WaitReady(ctx, instance, 90*time.Second); err != nil {
		return nil, svcerr.Invalidf("", "%s: %v", instance.Slug, err)
	}
	cut := s.dbs.ProvisionSlice
	if clone {
		cut = s.dbs.CloneSlice
	}
	p, err := cut(ctx, instance, env.ID, slug, name, public)
	if err != nil {
		return nil, svcerr.Invalidf("", "%v", err)
	}
	return p, nil
}

// Drop destroys a slice and every consumer's row pointing at it. That breadth
// is not incidental: the slice is one object those rows share.
func (s *SliceService) Drop(ctx context.Context, instance *repo.Tile, dbName string) error {
	if err := s.ready(); err != nil {
		return err
	}
	if err := s.dbs.DropDB(ctx, instance, dbName); err != nil {
		return svcerr.Invalidf("", "%v", err)
	}
	s.changed(instance)
	return nil
}

// Fork copies a live slice into a fresh one on the same instance, so a
// migration can be rehearsed against real data.
func (s *SliceService) Fork(ctx context.Context, instance *repo.Tile, src *repo.Provision,
	slug, name string) (*repo.Provision, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if src == nil || instance == nil || src.InstanceTileID != instance.ID {
		return nil, svcerr.ErrNotFound
	}
	p, err := s.dbs.ForkSlice(ctx, instance, src, slug, name)
	if err != nil {
		return nil, svcerr.Invalidf("", "%v", err)
	}
	return p, nil
}

// SetPublic flips a slice's public-read policy.
//
// Every row sharing the slice name moves together: the policy is a property
// of the bucket, so one consumer public and its neighbour private is not a
// state the bucket can actually be in. A config apply flipped the one
// representative row it had found, which left every other consumer's row
// claiming the old visibility for ever.
func (s *SliceService) SetPublic(ctx context.Context, instance *repo.Tile,
	p *repo.Provision, public bool) error {
	if err := s.ready(); err != nil {
		return err
	}
	if instance == nil || p == nil {
		return svcerr.ErrNotFound
	}
	rows, err := s.store.ListProvisionsByInstance(ctx, instance.ID)
	if err != nil {
		return err
	}
	for i := range rows {
		if rows[i].DBName != p.DBName {
			continue
		}
		if err := s.dbs.SetBucketPublic(ctx, instance, &rows[i], public); err != nil {
			return svcerr.Invalidf("", "%v", err)
		}
	}
	p.Public = public
	s.changed(instance)
	return nil
}

// Find locates a slice on an instance by its row id, so a form or a path
// parameter cannot steer a provision from another instance into an operation
// scoped to this one.
func (s *SliceService) Find(ctx context.Context, instance *repo.Tile, provisionID string) (*repo.Provision, error) {
	rows, err := s.store.ListProvisionsByInstance(ctx, instance.ID)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].ID == provisionID {
			return &rows[i], nil
		}
	}
	return nil, svcerr.ErrNotFound
}

// Rows returns every provision row sharing one slice on an instance, the set
// Drop removes together.
func (s *SliceService) Rows(ctx context.Context, instanceID, dbName string) []repo.Provision {
	ps, err := s.store.ListProvisionsByInstance(ctx, instanceID)
	if err != nil {
		slog.Error("provisions not listed", "instance", instanceID, "error", err)
		return nil
	}
	var out []repo.Provision
	for i := range ps {
		if ps[i].DBName == dbName {
			out = append(out, ps[i])
		}
	}
	return out
}

func (s *SliceService) changed(instance *repo.Tile) {
	if s.notifier != nil {
		s.notifier.Project(instance.StackID)
	}
}
