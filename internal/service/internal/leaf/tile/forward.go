package tile

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

// IP is the container's address on network, the guard as Exec: a system
// container or another tile's is refused.
func (l *Leaf) IP(ctx context.Context, tileID, id, network string) (string, error) {
	if err := l.guard(ctx, tileID, id, "tunnelled into"); err != nil {
		return "", err
	}
	d, err := l.docker.Inspect(ctx, id)
	if err != nil {
		return "", err
	}
	if d.Running && d.HostNetwork { // the panel shares the host's netns
		return "127.0.0.1", nil
	}
	ip := d.Networks[network]
	if !d.Running || ip == "" {
		return "", errs.Conflictf("tile is not running")
	}
	return ip, nil
}
