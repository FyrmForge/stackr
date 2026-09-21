package testdb

import (
	"context"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// NodeRows satisfies infra/nodes.Rows by writing the store directly.
//
// The real implementation is service.NodeService. infra/nodes is a package
// service/ is built on, so its in-package tests cannot name it. Same shape,
// and same caveat, as WorkItems above: the methods it doubles carry no rule
// today, and if one of them grows one these tests should fail rather than
// quietly keep passing.
type NodeRows struct{ Store repo.Store }

func (n NodeRows) Adopt(ctx context.Context, sv *repo.Server) error {
	return n.Store.CreateServer(ctx, sv)
}

func (n NodeRows) SaveServer(ctx context.Context, sv *repo.Server) error {
	return n.Store.UpdateServer(ctx, sv)
}

func (n NodeRows) IssueKey(ctx context.Context, k *repo.JoinKey) error {
	return n.Store.CreateJoinKey(ctx, k)
}

func (n NodeRows) BurnKey(ctx context.Context, key string) (bool, error) {
	return n.Store.BurnJoinKey(ctx, key)
}

func (n NodeRows) BurnKeysFor(ctx context.Context, serverID string) (int, error) {
	return n.Store.BurnServerJoinKeys(ctx, serverID)
}

func (n NodeRows) RecordSample(ctx context.Context, m *repo.Metric) error {
	return n.Store.InsertMetric(ctx, m)
}

// The rest of infra/metrics.Rows, which infra/nodes.Rows embeds: a node ping
// is a metric point, and a worker's host sample goes through the same door.
func (n NodeRows) PruneSamples(ctx context.Context, before time.Time) error {
	return n.Store.PruneMetrics(ctx, before)
}

func (n NodeRows) SetTileStatus(ctx context.Context, tileID, status string) error {
	return n.Store.UpdateTileStatus(ctx, tileID, status)
}
