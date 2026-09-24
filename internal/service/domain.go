package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type (
	Domain     = store.Domain
	DomainSpec = domain.Spec // RawCaddy is admin-only: the handler gates it
)

func (o *Orchestrator) Domains(ctx context.Context, tileID string) ([]Domain, error) {
	return o.domains.ListByTile(ctx, tileID)
}

// AttachDomain adds a host to a tile and pushes the proxy config. Port 0 =
// the tile's container port.
func (o *Orchestrator) AttachDomain(ctx context.Context, tileID string, s DomainSpec) (Domain, error) {
	t, err := o.tiles.Get(ctx, tileID)
	if err != nil {
		return Domain{}, err
	}
	if s.Port == 0 {
		s.Port = t.ContainerPort
	}
	cs, err := o.tiles.Replicas(ctx, t)
	if err != nil {
		return Domain{}, err
	}
	ids := make([]string, len(cs))
	for i, c := range cs {
		ids[i] = c.ID
	}
	d, err := o.domains.Attach(ctx, tileID, s, o.dns01(ctx), ids)
	if err != nil {
		return d, err
	}
	return d, o.sync.Sync(ctx)
}

func (o *Orchestrator) UpdateDomain(ctx context.Context, id string, s DomainSpec) (Domain, error) {
	d, err := o.domains.Get(ctx, id)
	if err != nil {
		return d, err
	}
	if s.Port == 0 {
		s.Port = d.ContainerPort
	}
	if d, err = o.domains.Update(ctx, d, s, o.dns01(ctx)); err != nil {
		return d, err
	}
	return d, o.sync.Sync(ctx)
}

func (o *Orchestrator) DetachDomain(ctx context.Context, id string) error {
	d, err := o.domains.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := o.domains.Detach(ctx, d); err != nil {
		return err
	}
	return o.sync.Sync(ctx)
}

// SyncProxy rebuilds and pushes the whole proxy config now.
func (o *Orchestrator) SyncProxy(ctx context.Context) error { return o.sync.Sync(ctx) }

func (o *Orchestrator) dns01(ctx context.Context) bool {
	p, _ := o.settings.Get(ctx, "dns_provider")
	return p != ""
}
