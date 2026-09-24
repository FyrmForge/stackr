package store

import (
	"context"
	"time"
)

// ManagedInstance is a row of managed_instances. Its container is its tile.
type ManagedInstance struct {
	ID            string    `db:"id"`
	TileID        string    `db:"tile_id"`
	Engine        string    `db:"engine"`
	ScopeKind     string    `db:"scope_kind"`
	ScopeID       string    `db:"scope_id"`
	AdminUser     string    `db:"admin_user"`
	AdminPassword string    `db:"admin_password"`
	Endpoint      string    `db:"endpoint"`
	CreatedAt     time.Time `db:"created_at"`
}

type ManagedInstanceStore interface {
	Create(ctx context.Context, m ManagedInstance) error
	Get(ctx context.Context, id string) (ManagedInstance, error)
	GetByTile(ctx context.Context, tileID string) (ManagedInstance, error)
	ListByScope(ctx context.Context, scopeKind, scopeID string) ([]ManagedInstance, error)
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

func (s managedInstances) ListByScope(ctx context.Context, scopeKind, scopeID string) ([]ManagedInstance, error) {
	return s.many(ctx, "scope_kind = ? AND scope_id = ?", scopeKind, scopeID)
}

// Provision is a row of provisions: one consumer's slice of an instance.
// ConsumerTileID nil = the consumer is gone and OnRemove is pending.
type Provision struct {
	ID             string    `db:"id"`
	InstanceID     string    `db:"instance_id"`
	ConsumerTileID *string   `db:"consumer_tile_id"`
	Slug           string    `db:"slug"`
	DBName         string    `db:"db_name"`
	DBUser         string    `db:"db_user"`
	DBPassword     string    `db:"db_password"`
	Outputs        string    `db:"outputs"`
	Public         bool      `db:"public"`
	OnRemove       string    `db:"on_remove"`
	CreatedAt      time.Time `db:"created_at"`
}

type ProvisionStore interface {
	Create(ctx context.Context, p Provision) error
	Get(ctx context.Context, id string) (Provision, error)
	ListByInstance(ctx context.Context, instanceID string) ([]Provision, error)
	ListByConsumer(ctx context.Context, tileID string) ([]Provision, error)
	Update(ctx context.Context, p Provision) error
	Delete(ctx context.Context, id string) error
}

var provisionsT = newTable("provisions", func(p *Provision) []*string {
	return []*string{&p.DBPassword, &p.Outputs}
})

type provisions struct{ crud[Provision] }

func (s provisions) ListByInstance(ctx context.Context, instanceID string) ([]Provision, error) {
	return s.many(ctx, "instance_id = ?", instanceID)
}

func (s provisions) ListByConsumer(ctx context.Context, tileID string) ([]Provision, error) {
	return s.many(ctx, "consumer_tile_id = ?", tileID)
}
