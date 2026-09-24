// Package managed owns managed instances (engine, admin credentials,
// endpoint, scope) and their provisions: one slice per consumer, the row
// that is also the binding. The instance container is a plain tile
// (leaf/tile); engines and their commands live in flow/managed.
package managed

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

const (
	Env   = "env"
	Stack = "stack"
	Org   = "org"

	Keep = "keep" // the consumer going away orphans the slice (default)
	Drop = "drop" // ... destroys it
)

type Leaf struct {
	instances  store.ManagedInstanceStore
	provisions store.ProvisionStore
}

func New(instances store.ManagedInstanceStore, provisions store.ProvisionStore) *Leaf {
	return &Leaf{instances: instances, provisions: provisions}
}

// Home is where the instance's own tile lives; the scope id is derived from
// it, never supplied, so no request can point an instance at another tenant.
type Home struct{ EnvID, StackID, OrgID string }

// scope maps a scope name to the column pair. Unknown names are refused,
// not coerced to env (a typo used to narrow a shared instance silently).
func scope(kind string, h Home) (string, string, error) {
	switch kind {
	case "", Env:
		return Env, h.EnvID, nil
	case Stack:
		return Stack, h.StackID, nil
	case Org:
		return Org, h.OrgID, nil
	}
	return "", "", errs.Invalidf("scope", "must be env, stack, or org")
}

func secret() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Create makes the instance row for a managed tile, with a fresh admin
// password. endpoint is where slices reach it (the flow knows the alias).
func (l *Leaf) Create(
	ctx context.Context,
	tileID, engine, scopeKind string,
	h Home,
	adminUser, endpoint string,
) (store.ManagedInstance, error) {
	kind, id, err := scope(scopeKind, h)
	if err != nil {
		return store.ManagedInstance{}, err
	}
	if engine == "" || adminUser == "" {
		return store.ManagedInstance{}, errs.Invalidf("engine", "an instance needs an engine and an admin user")
	}
	m := store.ManagedInstance{
		ID:            uuid.NewString(),
		TileID:        tileID,
		Engine:        engine,
		ScopeKind:     kind,
		ScopeID:       id,
		AdminUser:     adminUser,
		AdminPassword: secret(),
		Endpoint:      endpoint,
		CreatedAt:     time.Now().UTC(),
	}
	return m, l.instances.Create(ctx, m)
}

func (l *Leaf) Get(ctx context.Context, id string) (store.ManagedInstance, error) {
	return l.instances.Get(ctx, id)
}

func (l *Leaf) GetByTile(ctx context.Context, tileID string) (store.ManagedInstance, error) {
	return l.instances.GetByTile(ctx, tileID)
}

// Network is a stack- or org-scoped instance's shared network: the instance
// and each consumer outside its env join it. An env-scoped instance is
// reached on its env's own network.
func Network(instanceID string) string { return "stackr-shared-" + instanceID }

// Visible is every instance a tile at h may provision from: its env's,
// its stack's and its org's.
func (l *Leaf) Visible(ctx context.Context, h Home) ([]store.ManagedInstance, error) {
	var out []store.ManagedInstance
	for _, s := range [][2]string{
		{Env, h.EnvID},
		{Stack, h.StackID},
		{Org, h.OrgID},
	} {
		ms, err := l.instances.ListByScope(ctx, s[0], s[1])
		if err != nil {
			return nil, err
		}
		out = append(out, ms...)
	}
	return out, nil
}

// SetScope widens or narrows who may provision. It never touches the
// container: the running engine knows nothing about scope.
func (l *Leaf) SetScope(
	ctx context.Context,
	m store.ManagedInstance,
	scopeKind string,
	h Home,
) (store.ManagedInstance, error) {
	kind, id, err := scope(scopeKind, h)
	if err != nil {
		return m, err
	}
	m.ScopeKind, m.ScopeID = kind, id
	return m, l.instances.Update(ctx, m)
}

func (l *Leaf) SetEndpoint(
	ctx context.Context,
	m store.ManagedInstance,
	endpoint string,
) (store.ManagedInstance, error) {
	m.Endpoint = endpoint
	return m, l.instances.Update(ctx, m)
}

// Teardown is step one of removing an instance: refused while slices are
// held unless force (the caller's, never a constant), else the distinct
// slices the flow must drop before the container goes. Several consumer
// rows can share one slice, so they are deduped by database name.
func (l *Leaf) Teardown(ctx context.Context, m store.ManagedInstance, force bool) ([]store.Provision, error) {
	held, err := l.provisions.ListByInstance(ctx, m.ID)
	if err != nil {
		return nil, err
	}
	if len(held) > 0 && !force {
		return nil, errs.Conflictf("%d consumer(s) hold slices on this instance; "+
			"detach them first, or force the delete to destroy the data with it", len(held))
	}
	seen := map[string]bool{}
	var drop []store.Provision
	for _, p := range held {
		if !seen[p.DBName] {
			seen[p.DBName] = true
			drop = append(drop, p)
		}
	}
	return drop, nil
}

// Delete removes the instance row; every provision still pointing at it
// goes with it (cascade), dropped by the engine or not, so none outlives it.
func (l *Leaf) Delete(ctx context.Context, id string) error { return l.instances.Delete(ctx, id) }

// Slice is a new provision as the flow built it (name uniquified against
// SliceNames, credentials generated by the engine or by Password).
type Slice struct {
	Slug, DBName, DBUser, DBPassword string
	Public                           bool
	OnRemove                         string // keep (default) | drop
}

// Password is a fresh slice password.
func Password() string { return secret() }

// SliceNames are the instance's taken slice names, for uniquifying.
func (l *Leaf) SliceNames(ctx context.Context, instanceID string) ([]string, error) {
	ps, err := l.provisions.ListByInstance(ctx, instanceID)
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = p.DBName
	}
	return names, err
}

// Provision records a consumer's slice after the engine made it. One slice
// per consumer per instance.
func (l *Leaf) Provision(
	ctx context.Context,
	m store.ManagedInstance,
	consumerTileID string,
	s Slice,
) (store.Provision, error) {
	if s.OnRemove == "" {
		s.OnRemove = Keep
	}
	if s.OnRemove != Keep && s.OnRemove != Drop {
		return store.Provision{}, errs.Invalidf("on_remove", "must be keep or drop")
	}
	if s.DBName == "" || s.DBUser == "" {
		return store.Provision{}, errs.Invalidf("slice", "a slice needs a name and a user")
	}
	if p, ok, err := l.find(ctx, m.ID, consumerTileID); err != nil || ok {
		if ok {
			err = errs.Conflictf("this tile already has slice %s on the instance", p.DBName)
		}
		return store.Provision{}, err
	}
	p := store.Provision{
		ID:             uuid.NewString(),
		InstanceID:     m.ID,
		ConsumerTileID: &consumerTileID,
		Slug:           s.Slug,
		DBName:         s.DBName,
		DBUser:         s.DBUser,
		DBPassword:     s.DBPassword,
		Outputs:        "{}",
		Public:         s.Public,
		OnRemove:       s.OnRemove,
		CreatedAt:      time.Now().UTC(),
	}
	return p, l.provisions.Create(ctx, p)
}

// Share attaches a consumer to an existing slice. Same env only (fact):
// the slice's url secret is env-scoped.
func (l *Leaf) Share(
	ctx context.Context,
	of store.Provision,
	consumerTileID string,
	sameEnv bool,
) (store.Provision, error) {
	if !sameEnv {
		return store.Provision{}, errs.Refusedf("an existing slice can only be shared inside its environment")
	}
	if _, ok, err := l.find(ctx, of.InstanceID, consumerTileID); err != nil || ok {
		if ok {
			err = errs.Conflictf("this tile already has a slice on the instance")
		}
		return store.Provision{}, err
	}
	p := of
	p.ID, p.ConsumerTileID, p.CreatedAt = uuid.NewString(), &consumerTileID, time.Now().UTC()
	return p, l.provisions.Create(ctx, p)
}

func (l *Leaf) find(ctx context.Context, instanceID, tileID string) (store.Provision, bool, error) {
	ps, err := l.provisions.ListByConsumer(ctx, tileID)
	i := slices.IndexFunc(ps, func(p store.Provision) bool { return p.InstanceID == instanceID })
	if i < 0 {
		return store.Provision{}, false, err
	}
	return ps[i], true, err
}

func (l *Leaf) GetProvision(ctx context.Context, id string) (store.Provision, error) {
	return l.provisions.Get(ctx, id)
}

func (l *Leaf) ForConsumer(ctx context.Context, tileID string) ([]store.Provision, error) {
	return l.provisions.ListByConsumer(ctx, tileID)
}

func (l *Leaf) ByInstance(ctx context.Context, instanceID string) ([]store.Provision, error) {
	return l.provisions.ListByInstance(ctx, instanceID)
}

// Outputs parses a slice's published values.
func Outputs(p store.Provision) map[string]string {
	out := map[string]string{}
	_ = json.Unmarshal([]byte(p.Outputs), &out)
	return out
}

// SetOutputs writes the slice's bindings once the engine's commands ran.
func (l *Leaf) SetOutputs(ctx context.Context, p store.Provision, out map[string]string) (store.Provision, error) {
	b, _ := json.Marshal(out)
	p.Outputs = string(b)
	return p, l.provisions.Update(ctx, p)
}

// SetPublic flips public read. A public slice needs the instance to have a
// public base URL (fact), or browsers could not reach it anyway.
func (l *Leaf) SetPublic(
	ctx context.Context,
	p store.Provision,
	public bool,
	publicBase string,
) (store.Provision, error) {
	if public && publicBase == "" {
		return p, errs.Refusedf("give the instance a public domain before making a slice public")
	}
	p.Public = public
	return p, l.provisions.Update(ctx, p)
}

// Release is a consumer letting go of its slice (removed from the file, or
// the tile going). The binding goes, so its refs stop resolving; the slice
// stays orphaned unless on_remove is drop or the env is ephemeral (fact),
// in which case drop says the flow must drop it and then call DropRow.
func (l *Leaf) Release(ctx context.Context, p store.Provision, ephemeral bool) (drop bool, err error) {
	if p.OnRemove == Drop || ephemeral {
		return true, nil
	}
	p.ConsumerTileID, p.Outputs = nil, "{}"
	return false, l.provisions.Update(ctx, p)
}

// DropRow removes a slice row after the engine dropped it.
func (l *Leaf) DropRow(ctx context.Context, id string) error { return l.provisions.Delete(ctx, id) }
