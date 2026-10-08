package service

import (
	"context"
	"errors"
	"net"
	"strconv"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
)

// DialTile opens a TCP connection to a port inside one replica ("" = a
// running one) over the env network, and returns it with the port used:
// port, else the tile's declared one, else a managed engine's own. The
// panel shares the host's network, which reaches every env bridge. A
// redeploy changes the IP, so each connection resolves again.
func (o *Orchestrator) DialTile(ctx context.Context, tileID, container string, port int) (net.Conn, int, error) {
	t, err := o.tiles.Get(ctx, tileID)
	if err != nil {
		return nil, 0, err
	}
	if port < 0 || port > 65535 {
		return nil, 0, errs.Invalidf("port", "invalid port")
	}
	if port == 0 {
		port = t.ContainerPort
	}
	if port == 0 && t.Kind == tile.Managed {
		if m, err := o.managed.GetByTile(ctx, t.ID); err == nil {
			if e, ok := managed.Engines[m.Engine]; ok {
				port = e.Definition().Port
			}
		}
	}
	if port == 0 {
		return nil, 0, errs.Invalidf("port", "tile declares no port; pass --port")
	}
	env, err := o.envs.Get(ctx, t.EnvironmentID)
	if err != nil {
		return nil, 0, err
	}
	id, err := o.replica(ctx, tileID, container)
	if errors.Is(err, errNoContainer) {
		return nil, 0, errs.Conflictf("tile is not running")
	}
	if err != nil {
		return nil, 0, err
	}
	ip, err := o.tiles.IP(ctx, tileID, id, env.Network)
	if err != nil {
		return nil, 0, err
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		return nil, 0, errs.Conflictf("cannot reach %s:%d (%v)", t.Slug, port, err)
	}
	return conn, port, nil
}
