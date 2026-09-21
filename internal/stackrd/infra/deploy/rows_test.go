package deploy

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// storeRows satisfies Rows by writing the store directly.
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

func (r storeRows) SetTileImageDigest(ctx context.Context, tileID, digest string) error {
	return r.s.SetTileImageDigest(ctx, tileID, digest)
}

func (r storeRows) SaveTile(ctx context.Context, t *repo.Tile) error {
	return r.s.UpdateTile(ctx, t.ID, t.TileConfig)
}

func (r storeRows) RecordProvision(ctx context.Context, p *repo.Provision) error {
	return r.s.CreateProvision(ctx, p)
}

func (r storeRows) UpdateProvision(ctx context.Context, p *repo.Provision) error {
	return r.s.UpdateProvision(ctx, p)
}

func (r storeRows) RemoveProvision(ctx context.Context, id string) error {
	return r.s.DeleteProvision(ctx, id)
}

func (r storeRows) SaveResource(ctx context.Context, res *repo.ManagedResource, create bool) error {
	if create {
		return r.s.CreateResource(ctx, res)
	}
	return r.s.UpdateResource(ctx, res)
}

func (r storeRows) RemoveResource(ctx context.Context, id string) error {
	return r.s.DeleteResource(ctx, id)
}

func (r storeRows) SaveOutput(ctx context.Context, o *repo.ResourceOutput) error {
	return r.s.UpsertOutput(ctx, o)
}

func (r storeRows) Bind(ctx context.Context, b *repo.ResourceBinding) error {
	return r.s.CreateBinding(ctx, b)
}

func (r storeRows) Unbind(ctx context.Context, resourceID, consumerTileID string) error {
	return r.s.DeleteBinding(ctx, resourceID, consumerTileID)
}

func (r storeRows) UpsertVariable(ctx context.Context, v *repo.Variable) error {
	return r.s.UpsertVariable(ctx, v)
}

func (r storeRows) RemoveVariable(ctx context.Context, ownerKind, ownerID, name string) error {
	return r.s.DeleteVariable(ctx, ownerKind, ownerID, name)
}

func (r storeRows) RecordDeployment(ctx context.Context, d *repo.Deployment) error {
	return r.s.CreateDeployment(ctx, d)
}

func (r storeRows) DeploymentProgress(ctx context.Context, d *repo.Deployment) error {
	return r.s.UpdateDeployment(ctx, d)
}
