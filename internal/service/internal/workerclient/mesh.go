package workerclient

import (
	"context"
	"encoding/json"

	"github.com/FyrmForge/stackr/internal/worker"
)

// ApplyMesh pushes the node's whole mesh and exits state (POST /v1/mesh). It
// returns after the node's exit ladders have run their first tick.
func (c *Client) ApplyMesh(ctx context.Context, spec worker.MeshSpec) error {
	b, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	return c.call(ctx, "POST", "/v1/mesh", "application/json", b, nil)
}

// MeshInfo is the node's public keys and ladder states (GET /v1/mesh).
func (c *Client) MeshInfo(ctx context.Context) (worker.MeshInfo, error) {
	var i worker.MeshInfo
	err := c.call(ctx, "GET", "/v1/mesh", "", nil, &i)
	return i, err
}
