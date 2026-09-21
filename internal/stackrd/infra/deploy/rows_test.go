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
