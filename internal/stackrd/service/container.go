package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
)

// ContainerService owns what may be done to a raw container on a node.
//
// One guard, four verbs. The panel had the guard on stop, half of it on
// remove, and none at all on start or the terminal — so the panel container
// could not be stopped, but a terminal could be opened inside it, which is a
// root shell holding the docker socket, and a previous generation of the node
// agent could be started back up underneath the running one.
type ContainerService struct{ clus *cluster.Cluster }

// NewContainerService creates a new container service.
func NewContainerService(clus *cluster.Cluster) *ContainerService {
	return &ContainerService{clus: clus}
}

// systemNames is what the refusals call the containers this covers. The
// cluster decides which ones they are; this is only the wording.
const systemNames = "the panel, proxy and node agent containers"

// refuseSystem is the guard. cluster.ContainerIsSystem fails closed — a node
// it cannot reach answers true — which is the right way round: refusing an
// action on an unreachable node costs a retry, allowing one costs the panel.
func (s *ContainerService) refuseSystem(ctx context.Context, node, id, verb string) error {
	if s.clus.ContainerIsSystem(ctx, node, id) {
		return svcerr.Conflictf("%s cannot be %s from here", systemNames, verb)
	}
	return nil
}

// Start runs a stopped container again.
func (s *ContainerService) Start(ctx context.Context, node, id string) error {
	if err := s.refuseSystem(ctx, node, id, "started"); err != nil {
		return err
	}
	return s.clus.StartContainer(ctx, node, id)
}

// Stop stops a running container.
func (s *ContainerService) Stop(ctx context.Context, node, id string) error {
	if err := s.refuseSystem(ctx, node, id, "stopped"); err != nil {
		return err
	}
	return s.clus.StopContainer(ctx, node, id)
}

// Remove stops and removes a container.
//
// The one place the guard bends: a system container that has already exited is
// a previous generation left behind by an upgrade. It cuts nothing off, and
// refusing it is what left every node accumulating agent corpses no operator
// could clear. The guard is about the running one, which is the one the panel
// depends on.
func (s *ContainerService) Remove(ctx context.Context, node, id string) error {
	if s.clus.ContainerIsSystem(ctx, node, id) {
		d, err := s.clus.InspectContainer(ctx, node, id)
		if err != nil || d.State == "running" {
			return svcerr.Conflictf("%s cannot be removed while they are running", systemNames)
		}
	}
	return s.clus.StopRemove(ctx, node, id)
}

// EnsureExecAllowed is the terminal's gate. A shell inside the panel container
// holds the docker socket and the data dir, which is every stack on the box.
func (s *ContainerService) EnsureExecAllowed(ctx context.Context, node, id string) error {
	return s.refuseSystem(ctx, node, id, "opened a terminal into")
}
