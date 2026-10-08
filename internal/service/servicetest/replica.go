package servicetest

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
)

// Replica adds a running replica "c-<tileID>" of the tile whose address on
// its env network is ip (127.0.0.1 lets a local listener stand in).
func (e *Env) Replica(tileID, ip string) {
	ctx := context.Background()
	t, err := e.Store.Tiles.Get(ctx, tileID)
	if err != nil {
		panic(err)
	}
	en, err := e.Store.Environments.Get(ctx, t.EnvironmentID)
	if err != nil {
		panic(err)
	}
	id := "c-" + tileID
	e.Docker.Containers = append(e.Docker.Containers, docker.Container{
		ID: id, Name: id, State: "running",
		Labels: map[string]string{tile.LabelTile: tileID, tile.LabelRole: "replica"},
	})
	if e.Docker.Details == nil {
		e.Docker.Details = map[string]docker.Detail{}
	}
	e.Docker.Details[id] = docker.Detail{Running: true, Networks: map[string]string{en.Network: ip}}
}
