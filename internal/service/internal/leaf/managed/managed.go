// Package managed owns managed instances (engine, admin credentials,
// endpoint, allow list, env pairs), their provisions (one per slice tile:
// the database or bucket and its owner cred) and the bindings on them (one
// per consumer: its own cred at read or write, and the outputs minted for
// it). The instance container is a plain tile (leaf/tile); engines and their
// commands live in flow/managed.
package managed

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/address"
	"github.com/FyrmForge/stackr/internal/service/internal/slug"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

const (
	Keep = "keep" // the slice tile going away keeps the data (default)
	Drop = "drop" // ... destroys it
)

type Leaf struct {
	instances  store.ManagedInstanceStore
	provisions store.ProvisionStore
	bindings   store.BindingStore
}

func New(
	instances store.ManagedInstanceStore,
	provisions store.ProvisionStore,
	bindings store.BindingStore,
) *Leaf {
	return &Leaf{
		instances:  instances,
		provisions: provisions,
		bindings:   bindings,
	}
}

func secret() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Network is the instance's own Docker network: the instance container
// sits on it and every consumer of one of its slices joins it, whatever env
// or stack the consumer is in.
func Network(instanceID string) string {
	return "stackr-managed-" + instanceID
}

// Create makes the instance row for a managed tile, with a fresh admin
// password and no allow list (the tile's own env only). endpoint is where
// the S3 API is reached, "" = the tile's alias.
func (l *Leaf) Create(
	ctx context.Context,
	tileID, engine, adminUser, endpoint string,
) (store.ManagedInstance, error) {
	if engine == "" || adminUser == "" {
		return store.ManagedInstance{}, errs.Invalidf("engine", "an instance needs an engine and an admin user")
	}
	m := store.ManagedInstance{
		ID:            uuid.NewString(),
		TileID:        tileID,
		Engine:        engine,
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

func (l *Leaf) SetEndpoint(
	ctx context.Context,
	m store.ManagedInstance,
	endpoint string,
) (store.ManagedInstance, error) {
	m.Endpoint = endpoint
	return m, l.instances.Update(ctx, m)
}

// SetAllow replaces the instance's allow list. Every entry is checked
// against the instance's own org first; a bad one leaves the row as it was.
// Nothing is torn down: a slice the list no longer admits fails its
// consumers' next deploy with the reason.
// ponytail: no eager revoke; a running consumer keeps its cred until then.
func (l *Leaf) SetAllow(
	ctx context.Context,
	m store.ManagedInstance,
	orgSlug string,
	list []string,
) (store.ManagedInstance, error) {
	if _, err := address.ParseAllow(orgSlug, list); err != nil {
		return m, err
	}
	m.Allow = list
	return m, l.instances.Update(ctx, m)
}

// SetEnvPairs replaces the instance's consumer env name → own env name map.
func (l *Leaf) SetEnvPairs(
	ctx context.Context,
	m store.ManagedInstance,
	pairs map[string]string,
) (store.ManagedInstance, error) {
	for k, v := range pairs {
		if !slug.Valid(k) || !slug.Valid(v) {
			return m, errs.Invalidf("env_pairs", "%s: %s: both are environment names", k, v)
		}
	}
	m.EnvPairs = pairs
	return m, l.instances.Update(ctx, m)
}

// Teardown is step one of removing an instance: refused while slices are
// held unless force (the caller's, never a constant), else the slices the
// flow must drop before the container goes.
func (l *Leaf) Teardown(ctx context.Context, m store.ManagedInstance, force bool) ([]store.Provision, error) {
	held, err := l.provisions.ListByInstance(ctx, m.ID)
	if err != nil {
		return nil, err
	}
	if len(held) > 0 && !force {
		return nil, errs.Conflictf("%d slice(s) live on this instance; "+
			"remove their slice tiles first, or force the delete to destroy the data with it", len(held))
	}
	return held, nil
}

// Delete removes the instance row; every provision still pointing at it
// goes with it (cascade), dropped by the engine or not, so none outlives it.
func (l *Leaf) Delete(ctx context.Context, id string) error {
	return l.instances.Delete(ctx, id)
}

// Slice is a new provision as the flow built it (name uniquified against
// Names, the owner cred from Password).
type Slice struct {
	DBName     string
	DBUser     string
	DBPassword string
	Public     bool
	OnRemove   string // keep (default) | drop
}

// Password is a fresh cred password.
func Password() string {
	return secret()
}

// Names are every database, bucket and user name the instance's rows hold,
// for uniquifying a new one.
func (l *Leaf) Names(ctx context.Context, instanceID string) ([]string, error) {
	ps, err := l.provisions.ListByInstance(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, p := range ps {
		names = append(names, p.DBName, p.DBUser)
		bs, err := l.bindings.ListByProvision(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		for _, b := range bs {
			names = append(names, b.DBUser)
		}
	}
	return names, nil
}

// CreateProvision records slice tile sliceTileID's database or bucket on m
// after the engine made it. One per slice tile.
func (l *Leaf) CreateProvision(
	ctx context.Context,
	m store.ManagedInstance,
	sliceTileID string,
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
	if p, ok, err := l.ProvisionOf(ctx, sliceTileID); err != nil || ok {
		if ok {
			err = errs.Conflictf("this slice tile already has %s", p.DBName)
		}
		return store.Provision{}, err
	}
	p := store.Provision{
		ID:         uuid.NewString(),
		TileID:     sliceTileID,
		InstanceID: m.ID,
		DBName:     s.DBName,
		DBUser:     s.DBUser,
		DBPassword: s.DBPassword,
		Public:     s.Public,
		OnRemove:   s.OnRemove,
		CreatedAt:  time.Now().UTC(),
	}
	return p, l.provisions.Create(ctx, p)
}

// ProvisionOf is the slice tile's provision; false when it has none yet.
func (l *Leaf) ProvisionOf(ctx context.Context, sliceTileID string) (store.Provision, bool, error) {
	p, err := l.provisions.GetByTile(ctx, sliceTileID)
	if errors.Is(err, errs.ErrNotFound) {
		return p, false, nil
	}
	return p, err == nil, err
}

func (l *Leaf) GetProvision(ctx context.Context, id string) (store.Provision, error) {
	return l.provisions.Get(ctx, id)
}

func (l *Leaf) ByInstance(ctx context.Context, instanceID string) ([]store.Provision, error) {
	return l.provisions.ListByInstance(ctx, instanceID)
}

// SetOnRemove says what the slice tile going away does to the data.
func (l *Leaf) SetOnRemove(ctx context.Context, p store.Provision, onRemove string) (store.Provision, error) {
	if onRemove != Keep && onRemove != Drop {
		return p, errs.Invalidf("on_remove", "must be keep or drop")
	}
	p.OnRemove = onRemove
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

// DeleteProvision removes a slice's row once the engine dropped or kept it;
// its bindings go with it (cascade).
func (l *Leaf) DeleteProvision(ctx context.Context, id string) error {
	return l.provisions.Delete(ctx, id)
}

// ForConsumer are the provisions the consumer holds a binding on.
func (l *Leaf) ForConsumer(ctx context.Context, consumerTileID string) ([]store.Provision, error) {
	bs, err := l.bindings.ListByConsumer(ctx, consumerTileID)
	if err != nil {
		return nil, err
	}
	var out []store.Provision
	for _, b := range bs {
		p, err := l.provisions.Get(ctx, b.ProvisionID)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// Bound are the consumer's bindings, by slice tile id.
func (l *Leaf) Bound(ctx context.Context, consumerTileID string) (map[string]store.Binding, error) {
	bs, err := l.bindings.ListByConsumer(ctx, consumerTileID)
	if err != nil {
		return nil, err
	}
	out := map[string]store.Binding{}
	for _, b := range bs {
		p, err := l.provisions.Get(ctx, b.ProvisionID)
		if err != nil {
			return nil, err
		}
		out[p.TileID] = b
	}
	return out, nil
}

// Bindings are every consumer's binding on one slice.
func (l *Leaf) Bindings(ctx context.Context, provisionID string) ([]store.Binding, error) {
	return l.bindings.ListByProvision(ctx, provisionID)
}

// Outputs are the values minted for one binding's cred (DATABASE_URL and
// the rest), what the consumer's refs to its slice resolve to.
func Outputs(b store.Binding) (map[string]string, error) {
	out := map[string]string{}
	if b.Outputs == "" {
		return out, nil
	}
	return out, json.Unmarshal([]byte(b.Outputs), &out)
}

// Cred is a consumer's cred on a slice as the engine minted it.
type Cred struct {
	Access   string // read | write
	User     string
	Password string
	Outputs  map[string]string
}

// Bind records consumer consumerTileID's cred on p after the engine minted
// it. One per consumer per slice.
func (l *Leaf) Bind(
	ctx context.Context,
	p store.Provision,
	consumerTileID string,
	c Cred,
) (store.Binding, error) {
	if c.Access != "read" && c.Access != "write" {
		return store.Binding{}, errs.Invalidf("access", "must be read or write")
	}
	out, err := json.Marshal(c.Outputs)
	if err != nil {
		return store.Binding{}, err
	}
	b := store.Binding{
		ID:             uuid.NewString(),
		ProvisionID:    p.ID,
		ConsumerTileID: consumerTileID,
		Access:         c.Access,
		DBUser:         c.User,
		DBPassword:     c.Password,
		Outputs:        string(out),
		CreatedAt:      time.Now().UTC(),
	}
	return b, l.bindings.Create(ctx, b)
}

// SetAccess moves a binding to read or write once the engine re-granted it;
// the user and password stay.
func (l *Leaf) SetAccess(ctx context.Context, b store.Binding, access string) (store.Binding, error) {
	if access != "read" && access != "write" {
		return b, errs.Invalidf("access", "must be read or write")
	}
	b.Access = access
	return b, l.bindings.Update(ctx, b)
}

// Unbind removes a binding's row once the engine dropped its cred.
func (l *Leaf) Unbind(ctx context.Context, id string) error {
	return l.bindings.Delete(ctx, id)
}
