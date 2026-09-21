package managedtiles

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// storeRows satisfies Rows by writing the store directly.
//
// It would be better for this to be the real services, so the tests exercise
// the rules and not just the plumbing — but service/ imports this package, so
// naming it here is an import cycle even from an in-package test file. That
// cycle is the reason Rows is an interface at all.
//
// The services are covered where they live. What these tests are about is
// managedtiles, and for that a writer that writes is enough.
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

func testRows(s repo.Store) Rows { return storeRows{s: s} }
