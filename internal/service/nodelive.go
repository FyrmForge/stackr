package service

import (
	"context"
	"runtime"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// controllerFacts is what the controller node reports about itself. On a
// multi-node install the mesh already reads it through localMesh, the same
// code a worker runs; a single node or a fake host has no mesh, so the
// facts are built from the interfaces and the daemon it talks to.
func (o *Orchestrator) controllerFacts(ctx context.Context, n store.Node) *worker.Facts {
	if o.mesh != nil {
		if f, ok := o.mesh.factsOf(n.ID); ok {
			return &f
		}
	}
	f := worker.Facts{Interfaces: worker.Interfaces(), Arch: runtime.GOARCH, Version: o.cfg.Version}
	if h, err := o.docker.HostInfo(docker.WithNode(ctx, n.ID)); err == nil {
		f.CPUs, f.MemBytes, f.Docker = h.CPUs, uint64(h.MemBytes), h.Version
	}
	return &f
}
