package store

import (
	"context"
	"time"
)

// OperatedInstance is a row of operated_instances. Its container is its tile.
// Allow holds org:stack:env:tile patterns (empty = the tile's own env);
// EnvPairs maps a consumer's env name to one of this stack's envs.
type OperatedInstance struct {
	ID            string     `db:"id" json:"id"`
	TileID        string     `db:"tile_id" json:"tile_id"`
	Engine        string     `db:"engine" json:"engine"`
	Allow         StringList `db:"allow" json:"allow"`
	EnvPairs      StringMap  `db:"env_pairs" json:"env_pairs"`
	AdminUser     string     `db:"admin_user" json:"admin_user"`
	AdminPassword string     `db:"admin_password" json:"-"`
	Endpoint      string     `db:"endpoint" json:"endpoint"`
	// PITRDays is the point-in-time recovery window in days; 0 = off.
	PITRDays int `db:"pitr_days" json:"pitr_days"`
	// Settings are a postgres instance's own overrides of the computed
	// defaults, name to value.
	Settings StringMap `db:"settings" json:"settings"`
	// ArchiveEpoch numbers the point-in-time archive: 0 is the first, and a
	// major change or its rollback starts the next, with its own local
	// archive directory and object prefix.
	ArchiveEpoch int `db:"archive_epoch" json:"-"`
	// Major is the postgres major the instance was made with; 0 = not postgres.
	Major int `db:"major" json:"major"`
	// PoolerPassword is the stackr_pooler role's password, minted when the
	// instance's pooler is first made.
	PoolerPassword string `db:"pooler_password" json:"-"`
	// ReplPassword is the stackr_repl role's password, minted with the
	// first standby.
	ReplPassword string `db:"repl_password" json:"-"`
	// PreferSync turns synchronous replication on; SyncLimitMS is the round
	// trip an auto standby may have and still be waited for; SyncRequired
	// makes writes wait even when no standby answers.
	PreferSync   bool      `db:"prefer_sync" json:"prefer_sync"`
	SyncLimitMS  int       `db:"sync_limit_ms" json:"sync_limit_ms"`
	SyncRequired bool      `db:"sync_required" json:"sync_required"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
}

type OperatedInstanceStore interface {
	Create(ctx context.Context, m OperatedInstance) error
	Get(ctx context.Context, id string) (OperatedInstance, error)
	GetByTile(ctx context.Context, tileID string) (OperatedInstance, error)
	// ListPITR lists the instances with point-in-time recovery on.
	ListPITR(ctx context.Context) ([]OperatedInstance, error)
	ListEngine(ctx context.Context, engine string) ([]OperatedInstance, error)
	Update(ctx context.Context, m OperatedInstance) error
	Delete(ctx context.Context, id string) error
}

var operatedInstancesT = newTable("operated_instances", func(m *OperatedInstance) []*string {
	return []*string{&m.AdminPassword, &m.PoolerPassword, &m.ReplPassword}
})

type operatedInstances struct{ crud[OperatedInstance] }

func (s operatedInstances) GetByTile(ctx context.Context, tileID string) (OperatedInstance, error) {
	return s.one(ctx, "tile_id = ?", tileID)
}

func (s operatedInstances) ListEngine(ctx context.Context, engine string) ([]OperatedInstance, error) {
	return s.many(ctx, "engine = ?", engine)
}

func (s operatedInstances) ListPITR(ctx context.Context) ([]OperatedInstance, error) {
	return s.many(ctx, "pitr_days > 0")
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
