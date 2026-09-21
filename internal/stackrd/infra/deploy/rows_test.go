package deploy

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// storeRows satisfies managedtiles.Rows by writing the store directly.
//
// The real implementation is service.Rows, which this package cannot name:
// service/ is built on top of deploy/, so importing it here is a cycle — the
// cycle Rows exists as an interface to avoid.
//
// The services are covered where they live.
type storeRows struct{ s repo.Store }

func (r storeRows) SetNetwork(ctx context.Context, envID, network string) error {
	return r.s.SetEnvironmentNetwork(ctx, envID, network)
}

func (r storeRows) SetSharedNet(ctx context.Context, tileID, name string) error {
	return r.s.SetTileSharedNet(ctx, tileID, name)
}

func (r storeRows) SetProxy(ctx context.Context, envID, ip, cidr string) error {
	return r.s.SetEnvironmentProxy(ctx, envID, ip, cidr)
}

func (r storeRows) SetTileStatus(ctx context.Context, tileID, status string) error {
	return r.s.UpdateTileStatus(ctx, tileID, status)
}

// SetStatus is the same write under the name deploy.Tiles asks for: that
// interface is the tile service's own method set, Rows is the bundle.
func (r storeRows) SetStatus(ctx context.Context, tileID, status string) error {
	return r.s.UpdateTileStatus(ctx, tileID, status)
}

func (r storeRows) RecordDeployment(ctx context.Context, d *repo.Deployment) error {
	return r.s.CreateDeployment(ctx, d)
}

func (r storeRows) DeploymentProgress(ctx context.Context, d *repo.Deployment) error {
	return r.s.UpdateDeployment(ctx, d)
}
