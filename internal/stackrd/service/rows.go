package service

import (
	"context"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Rows is the row-owning services, bundled for the layer below.
//
// Most of infra/ writes rows that belong to a service: an environment's
// overlay, a shared managed instance's overlay, where traefik answered. Those
// packages cannot import this one — service/ is built on top of them — so
// each declares the narrow interface it needs (netpool.EnvOwner,
// netpool.TileOwner, envnet.Envs, managedtiles.Rows) and this satisfies all
// of them.
//
// It is a forwarder, and deliberately the only one. The alternative was a
// parameter per service threaded through four call layers, which is how a
// caller ends up passing the store "just for now".
//
// Nothing here has a rule in it. The rules are on the services; what this
// fixes is *who writes the row*, which is what stops a second implementation
// appearing next to the first.
type Rows struct {
	Envs  *EnvironmentService
	Tiles *TileService
	// Deploys is filled after the deploy engine exists, because the service
	// is built on the engine and the engine holds this. Only the engine
	// reaches these two methods, and only while running a deploy — long
	// after the assignment.
	Deploys *DeployService
	// Slices, Instances and Vars are filled at the same point and for the
	// same reason: all three are built on infra packages that hold this.
	Slices    *SliceService
	Instances *ManagedInstanceService
	Vars      *VariableService
	Telemetry *TileTelemetryService
	Nodes     *NodeService
}

// The servers and join_keys tables, for infra/nodes.
func (r Rows) Adopt(ctx context.Context, sv *repo.Server) error {
	return r.Nodes.Adopt(ctx, sv)
}

func (r Rows) SaveServer(ctx context.Context, sv *repo.Server) error {
	return r.Nodes.Save(ctx, sv)
}

func (r Rows) IssueKey(ctx context.Context, k *repo.JoinKey) error {
	return r.Nodes.IssueKey(ctx, k)
}

func (r Rows) BurnKey(ctx context.Context, key string) (bool, error) {
	return r.Nodes.BurnKey(ctx, key)
}

func (r Rows) BurnKeysFor(ctx context.Context, serverID string) (int, error) {
	return r.Nodes.BurnKeysFor(ctx, serverID)
}

// RecordSample and PruneSamples are the metrics table, for infra/metrics.
func (r Rows) RecordSample(ctx context.Context, m *repo.Metric) error {
	return r.Telemetry.RecordSample(ctx, m)
}

func (r Rows) PruneSamples(ctx context.Context, before time.Time) error {
	return r.Telemetry.Prune(ctx, before)
}

func (r Rows) SetHomeNode(ctx context.Context, tileID, nodeID string) error {
	return r.Tiles.SetHomeNode(ctx, tileID, nodeID)
}

func (r Rows) SetTileImageDigest(ctx context.Context, tileID, digest string) error {
	return r.Tiles.SetImageDigest(ctx, tileID, digest)
}

func (r Rows) SaveTile(ctx context.Context, t *repo.Tile) error {
	return r.Tiles.Save(ctx, t)
}

func (r Rows) RecordProvision(ctx context.Context, p *repo.Provision) error {
	return r.Slices.Record(ctx, p)
}

func (r Rows) UpdateProvision(ctx context.Context, p *repo.Provision) error {
	return r.Slices.Update(ctx, p)
}

func (r Rows) RemoveProvision(ctx context.Context, id string) error {
	return r.Slices.Remove(ctx, id)
}

func (r Rows) SaveResource(ctx context.Context, res *repo.ManagedResource, create bool) error {
	return r.Instances.SaveResource(ctx, res, create)
}

func (r Rows) RemoveResource(ctx context.Context, id string) error {
	return r.Instances.RemoveResource(ctx, id)
}

func (r Rows) SaveOutput(ctx context.Context, o *repo.ResourceOutput) error {
	return r.Instances.SaveOutput(ctx, o)
}

func (r Rows) Bind(ctx context.Context, b *repo.ResourceBinding) error {
	return r.Instances.Bind(ctx, b)
}

func (r Rows) Unbind(ctx context.Context, resourceID, consumerTileID string) error {
	return r.Instances.Unbind(ctx, resourceID, consumerTileID)
}

func (r Rows) UpsertVariable(ctx context.Context, v *repo.Variable) error {
	return r.Vars.Upsert(ctx, v)
}

func (r Rows) RemoveVariable(ctx context.Context, ownerKind, ownerID, name string) error {
	return r.Vars.Remove(ctx, VarOwner{Kind: ownerKind, ID: ownerID}, name)
}

func (r Rows) SetTileStatus(ctx context.Context, tileID, status string) error {
	return r.Tiles.SetStatus(ctx, tileID, status)
}

func (r Rows) RecordDeployment(ctx context.Context, d *repo.Deployment) error {
	return r.Deploys.Record(ctx, d)
}

func (r Rows) DeploymentProgress(ctx context.Context, d *repo.Deployment) error {
	return r.Deploys.Progress(ctx, d)
}

// The read half of the deployments table, for the engine below.

func (r Rows) Row(ctx context.Context, id string) (*repo.Deployment, error) {
	return r.Deploys.Row(ctx, id)
}

func (r Rows) ForTile(ctx context.Context, tileID string, limit int) ([]repo.Deployment, error) {
	return r.Deploys.ForTile(ctx, tileID, limit)
}

func (r Rows) CurrentImage(ctx context.Context, tileID string) (string, error) {
	return r.Deploys.CurrentImage(ctx, tileID)
}

func (r Rows) Waiting(ctx context.Context) ([]repo.Deployment, error) {
	return r.Deploys.Waiting(ctx)
}

func (r Rows) Latest(ctx context.Context, tileID string) (*repo.Deployment, error) {
	return r.Deploys.Latest(ctx, tileID)
}

func (r Rows) SetNetwork(ctx context.Context, envID, network string) error {
	return r.Envs.SetNetwork(ctx, envID, network)
}

func (r Rows) SetSharedNet(ctx context.Context, tileID, name string) error {
	return r.Tiles.SetSharedNet(ctx, tileID, name)
}

func (r Rows) SetProxy(ctx context.Context, envID, ip, cidr string) error {
	return r.Envs.SetProxy(ctx, envID, ip, cidr)
}
