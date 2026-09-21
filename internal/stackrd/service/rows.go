package service

import "context"

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
