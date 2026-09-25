package service

import (
	"context"
	"errors"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type (
	ManagedInstance = store.ManagedInstance
	Provision       = store.Provision
)

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
		_, err = managed.New(tx.ManagedInstances, tx.Provisions, tx.Bindings).Create(ctx, out.ID, engine, "stackr", "")
		return err
	})
	return out, err
}

// ManagedInstances are the env's own instances. Another env or stack reaches
// one through a slice tile's provision_from, which the instance's allow list
// gates at plan time.
func (o *Orchestrator) ManagedInstances(ctx context.Context, envID string) ([]ManagedInstance, error) {
	ts, err := o.tiles.List(ctx, envID)
	if err != nil {
		return nil, err
	}
	var out []ManagedInstance
	for _, t := range ts {
		if t.Kind != tile.Managed {
			continue
		}
		m, err := o.managed.GetByTile(ctx, t.ID)
		if errors.Is(err, errs.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// SetInstanceScope is gone with scope (DECIDE 194): an instance is env-scoped
// and its allow list says who may cut slices. env is accepted as a no-op.
// step 7b task 6 replaces this: the verb, route and command go.
func (o *Orchestrator) SetInstanceScope(ctx context.Context, instanceTileID, scope string) (ManagedInstance, error) {
	m, err := o.managed.GetByTile(ctx, instanceTileID)
	if err != nil {
		return m, err
	}
	if scope != "" && scope != "env" {
		return m, errs.Invalidf("scope", "instances are env-scoped; share one with its allow list (DECIDE 194)")
	}
	return m, nil
}

// Slices are the provisions the consumer holds a binding on.
func (o *Orchestrator) Slices(ctx context.Context, consumerID string) ([]Provision, error) {
	return o.managed.ForConsumer(ctx, consumerID)
}

// AttachSlice is gone with per-consumer slices (DECIDE 194): a slice tile
// and the consumer's slice_access (or a ref) bind it at deploy.
// step 7b task 6 replaces this: the verb, route and command go.
func (o *Orchestrator) AttachSlice(
	ctx context.Context,
	consumerID, instanceTileID, name string,
	public bool,
	onRemove string,
) (Job, error) {
	return Job{}, errs.Invalidf("slice", "declare a slice tile and ref it; attach is gone (DECIDE 194)")
}

// DetachSlice is gone the same way: removing the consumer, or its ref and
// slice_access entry, unbinds it.
// step 7b task 6 replaces this: the verb, route and command go.
func (o *Orchestrator) DetachSlice(ctx context.Context, provisionID string) (Job, error) {
	return Job{}, errs.Invalidf("slice", "remove the slice tile or the consumer's ref; detach is gone (DECIDE 194)")
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
