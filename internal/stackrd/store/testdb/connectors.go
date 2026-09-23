package testdb

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Connectors satisfies infra/githubapp.Connectors by writing the store
// directly. The real implementation is service.ConnectorService, which
// infra/githubapp cannot name — service/ is built on it.
type Connectors struct{ Store repo.Store }

func (c Connectors) Create(ctx context.Context, cn *repo.Connector) error {
	return c.Store.CreateConnector(ctx, cn)
}

func (c Connectors) Save(ctx context.Context, cn *repo.Connector) error {
	return c.Store.UpdateConnector(ctx, cn)
}
