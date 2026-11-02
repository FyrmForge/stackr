package service

import (
	"context"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
)

const dockerAlertKey = "alert.docker"

// dockerAlert pings the controller's Docker daemon on the 30s tick and keeps
// a banner problem open while it does not answer. Deploys and the canvas
// fail one by one without it; this says why.
func (o *Orchestrator) dockerAlert(ctx context.Context) {
	n, err := o.nodeLeaf().Controller(ctx)
	if err != nil {
		return
	}
	cctx, cancel := context.WithTimeout(docker.WithNode(ctx, n.ID), 5*time.Second)
	defer cancel()
	if _, err := o.docker.HostInfo(cctx); err != nil {
		o.openProblem(ctx, dockerAlertKey, SevCritical, "Docker does not answer on the controller. Check the daemon.")
		return
	}
	o.clearProblem(ctx, dockerAlertKey)
}

// DockerDown reports whether the last tick found Docker not answering.
func (o *Orchestrator) DockerDown(ctx context.Context) bool {
	return o.hasProblem(ctx, dockerAlertKey)
}
