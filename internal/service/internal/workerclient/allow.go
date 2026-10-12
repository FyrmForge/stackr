package workerclient

import (
	"context"
	"encoding/json"

	"github.com/FyrmForge/stackr/internal/worker"
)

// AllowTCP opens or closes the node to tcp ports lo..hi from src (POST
// /v1/allow-tcp); repeating a call changes nothing.
func (c *Client) AllowTCP(ctx context.Context, src string, lo, hi int, on bool) error {
	b, err := json.Marshal(worker.AllowTCP{Src: src, Lo: lo, Hi: hi, On: on})
	if err != nil {
		return err
	}
	return c.call(ctx, "POST", "/v1/allow-tcp", "application/json", b, nil)
}
