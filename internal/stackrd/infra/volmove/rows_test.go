package volmove

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// storeRows satisfies Rows by writing and reading the store directly.
//
// The real implementation is service.Rows. service/ is built on top of this
// package, so an in-package test cannot name it — same shape as the doubles
// in infra/deploy and store/testdb, and the same caveat: none of these three
// carries a rule today, and if one grows one these tests should fail rather
// than quietly keep passing.
type storeRows struct{ s repo.Store }

func (r storeRows) SetHomeNode(ctx context.Context, tileID, nodeID string) error {
	return r.s.SetTileHomeNode(ctx, tileID, nodeID)
}

func (r storeRows) SetTileStatus(ctx context.Context, tileID, status string) error {
	return r.s.UpdateTileStatus(ctx, tileID, status)
}

func (r storeRows) Row(ctx context.Context, id string) (*repo.Deployment, error) {
	return r.s.GetDeployment(ctx, id)
}
