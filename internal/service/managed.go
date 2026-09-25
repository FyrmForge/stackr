package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type (
	ManagedInstance = store.ManagedInstance
	Provision       = store.Provision
)

type attachJob struct {
	ConsumerID string `json:"consumer_id"`
	InstanceID string `json:"instance_tile_id"`
	Name       string `json:"name,omitempty"`
	Public     bool   `json:"public,omitempty"`
	OnRemove   string `json:"on_remove,omitempty"`
}

type detachJob struct {
	ProvisionID string `json:"provision_id"`
}

// CreateManagedTile makes a managed tile (postgres | s3) and its env-scoped
// instance row. Nothing runs until Deploy.
func (o *Orchestrator) CreateManagedTile(ctx context.Context, t Tile, engine string) (Tile, error) {
	if !deploy.KnownEngine(engine) {
		return Tile{}, errs.Invalidf("engine", "%q is not an engine stackr runs.", engine)
	}
	e, err := o.envs.Get(ctx, t.EnvironmentID)
	if err != nil {
		return Tile{}, err
	}
	st, err := o.stacks.Get(ctx, e.StackID)
	if err != nil {
		return Tile{}, err
	}
	t.Kind, t.StackID = tile.Managed, st.ID
	var out Tile
	err = o.store.Tx(ctx, func(tx store.Tx) error {
		var err error
		if out, err = tile.New(tx.Tiles, o.docker, nil).Create(ctx, t); err != nil {
			return err
		}
		_, err = managed.New(tx.ManagedInstances, tx.Provisions).Create(ctx, out.ID, engine, managed.Env,
			managed.Home{EnvID: e.ID, StackID: st.ID, OrgID: st.OrgID}, "stackr", "")
		return err
	})
	return out, err
}

// ManagedInstances are the instances visible from an env.
func (o *Orchestrator) ManagedInstances(ctx context.Context, envID string) ([]ManagedInstance, error) {
	h, err := o.home(ctx, envID)
	if err != nil {
		return nil, err
	}
	return o.managed.Visible(ctx, h)
}

// SetInstanceScope widens or narrows who may cut slices: env | stack | org.
func (o *Orchestrator) SetInstanceScope(ctx context.Context, instanceTileID, scope string) (ManagedInstance, error) {
	m, err := o.managed.GetByTile(ctx, instanceTileID)
	if err != nil {
		return m, err
	}
	t, err := o.tiles.Get(ctx, instanceTileID)
	if err != nil {
		return m, err
	}
	h, err := o.home(ctx, t.EnvironmentID)
	if err != nil {
		return m, err
	}
	return o.managed.SetScope(ctx, m, scope, h)
}

// Slices are the consumer's provisions (its bindings come from them).
func (o *Orchestrator) Slices(ctx context.Context, consumerID string) ([]Provision, error) {
	return o.managed.ForConsumer(ctx, consumerID)
}

// AttachSlice queues cutting a slice of an instance for a consumer, then
// its redeploy with the bindings.
func (o *Orchestrator) AttachSlice(
	ctx context.Context,
	consumerID, instanceTileID, name string,
	public bool,
	onRemove string,
) (Job, error) {
	return o.enqueue(ctx, kindAttach, attachJob{
		ConsumerID: consumerID,
		InstanceID: instanceTileID,
		Name:       name,
		Public:     public,
		OnRemove:   onRemove,
	}, consumerID, instanceTileID)
}

// DetachSlice queues letting go of a slice and the consumer's redeploy.
func (o *Orchestrator) DetachSlice(ctx context.Context, provisionID string) (Job, error) {
	p, err := o.managed.GetProvision(ctx, provisionID)
	if err != nil {
		return Job{}, err
	}
	if p.ConsumerTileID == nil {
		return Job{}, errs.Conflictf("this slice has no consumer left")
	}
	return o.enqueue(ctx, kindDetach, detachJob{ProvisionID: p.ID}, *p.ConsumerTileID)
}

func (o *Orchestrator) runAttach(ctx context.Context, r *jobs.Run, p attachJob) error {
	c, err := o.tiles.Get(ctx, p.ConsumerID)
	if err != nil {
		return err
	}
	it, err := o.tiles.Get(ctx, p.InstanceID)
	if err != nil {
		return err
	}
	h, err := o.home(ctx, c.EnvironmentID)
	if err != nil {
		return err
	}
	if _, err := o.engines.Attach(ctx, c, it, h, p.Name, p.Public, p.OnRemove); err != nil {
		return err
	}
	return o.deploy.Redeploy(ctx, c.ID, r.Log, r.Swap)
}

func (o *Orchestrator) runDetach(ctx context.Context, r *jobs.Run, p detachJob) error {
	pr, err := o.managed.GetProvision(ctx, p.ProvisionID)
	if err != nil {
		return err
	}
	if pr.ConsumerTileID == nil {
		return errs.Conflictf("this slice has no consumer left")
	}
	c, err := o.tiles.Get(ctx, *pr.ConsumerTileID)
	if err != nil {
		return err
	}
	e, err := o.envs.Get(ctx, c.EnvironmentID)
	if err != nil {
		return err
	}
	if err := o.engines.Detach(ctx, pr, e.Type == environment.Ephemeral); err != nil {
		return err
	}
	return o.deploy.Redeploy(ctx, c.ID, r.Log, r.Swap)
}

func (o *Orchestrator) home(ctx context.Context, envID string) (managed.Home, error) {
	e, err := o.envs.Get(ctx, envID)
	if err != nil {
		return managed.Home{}, err
	}
	st, err := o.stacks.Get(ctx, e.StackID)
	return managed.Home{EnvID: e.ID, StackID: st.ID, OrgID: st.OrgID}, err
}

// InstanceSlices is a managed tile's instance and every slice cut from it.
func (o *Orchestrator) InstanceSlices(
	ctx context.Context,
	instanceTileID string,
) (ManagedInstance, []Provision, error) {
	m, err := o.managed.GetByTile(ctx, instanceTileID)
	if err != nil {
		return m, nil, err
	}
	ps, err := o.managed.ByInstance(ctx, m.ID)
	return m, ps, err
}

// Provision is one slice by id.
func (o *Orchestrator) Provision(ctx context.Context, id string) (Provision, error) {
	return o.managed.GetProvision(ctx, id)
}
