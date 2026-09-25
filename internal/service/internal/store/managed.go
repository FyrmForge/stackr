package store

import (
	"context"
	"time"
)

// ManagedInstance is a row of managed_instances. Its container is its tile.
// Allow holds org:stack:env:tile patterns (empty = the tile's own env);
// EnvPairs maps a consumer's env name to one of this stack's envs.
type ManagedInstance struct {
	ID            string     `db:"id" json:"id"`
	TileID        string     `db:"tile_id" json:"tile_id"`
	Engine        string     `db:"engine" json:"engine"`
	Allow         StringList `db:"allow" json:"allow"`
	EnvPairs      StringMap  `db:"env_pairs" json:"env_pairs"`
	AdminUser     string     `db:"admin_user" json:"admin_user"`
	AdminPassword string     `db:"admin_password" json:"-"`
	Endpoint      string     `db:"endpoint" json:"endpoint"`
	CreatedAt     time.Time  `db:"created_at" json:"created_at"`
}

type ManagedInstanceStore interface {
	Create(ctx context.Context, m ManagedInstance) error
	Get(ctx context.Context, id string) (ManagedInstance, error)
	GetByTile(ctx context.Context, tileID string) (ManagedInstance, error)
	Update(ctx context.Context, m ManagedInstance) error
	Delete(ctx context.Context, id string) error
}

var managedInstancesT = newTable("managed_instances", func(m *ManagedInstance) []*string {
	return []*string{&m.AdminPassword}
})

type managedInstances struct{ crud[ManagedInstance] }

func (s managedInstances) GetByTile(ctx context.Context, tileID string) (ManagedInstance, error) {
	return s.one(ctx, "tile_id = ?", tileID)
}

// Provision is a row of provisions: a slice tile's database or bucket on an
// instance, held by its owner cred. One per slice tile.
type Provision struct {
	ID         string    `db:"id" json:"id"`
	TileID     string    `db:"tile_id" json:"tile_id"`
	InstanceID string    `db:"instance_id" json:"instance_id"`
	DBName     string    `db:"db_name" json:"db_name"`
	DBUser     string    `db:"db_user" json:"db_user"`
	DBPassword string    `db:"db_password" json:"-"`
	Public     bool      `db:"public" json:"public"`
	OnRemove   string    `db:"on_remove" json:"on_remove"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
}

type ProvisionStore interface {
	Create(ctx context.Context, p Provision) error
	Get(ctx context.Context, id string) (Provision, error)
	GetByTile(ctx context.Context, sliceTileID string) (Provision, error)
	ListByInstance(ctx context.Context, instanceID string) ([]Provision, error)
	Update(ctx context.Context, p Provision) error
	Delete(ctx context.Context, id string) error
}

var provisionsT = newTable("provisions", func(p *Provision) []*string {
	return []*string{&p.DBPassword}
})

type provisions struct{ crud[Provision] }

func (s provisions) GetByTile(ctx context.Context, sliceTileID string) (Provision, error) {
	return s.one(ctx, "tile_id = ?", sliceTileID)
}

func (s provisions) ListByInstance(ctx context.Context, instanceID string) ([]Provision, error) {
	return s.many(ctx, "instance_id = ?", instanceID)
}

// Binding is a row of bindings: one consumer's own cred on a slice, at read
// or write. Outputs is the JSON of the values minted for that cred.
type Binding struct {
	ID             string    `db:"id" json:"id"`
	ProvisionID    string    `db:"provision_id" json:"provision_id"`
	ConsumerTileID string    `db:"consumer_tile_id" json:"consumer_tile_id"`
	Access         string    `db:"access" json:"access"`
	DBUser         string    `db:"db_user" json:"db_user"`
	DBPassword     string    `db:"db_password" json:"-"`
	Outputs        string    `db:"outputs" json:"-"` // holds the password too
	CreatedAt      time.Time `db:"created_at" json:"created_at"`
}

type BindingStore interface {
	Create(ctx context.Context, b Binding) error
	Get(ctx context.Context, id string) (Binding, error)
	ListByProvision(ctx context.Context, provisionID string) ([]Binding, error)
	ListByConsumer(ctx context.Context, consumerTileID string) ([]Binding, error)
	Update(ctx context.Context, b Binding) error
	Delete(ctx context.Context, id string) error
}

var bindingsT = newTable("bindings", func(b *Binding) []*string {
	return []*string{&b.DBPassword, &b.Outputs}
})

type bindings struct{ crud[Binding] }

func (s bindings) ListByProvision(ctx context.Context, provisionID string) ([]Binding, error) {
	return s.many(ctx, "provision_id = ?", provisionID)
}

func (s bindings) ListByConsumer(ctx context.Context, consumerTileID string) ([]Binding, error) {
	return s.many(ctx, "consumer_tile_id = ?", consumerTileID)
}
