package metrics

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/config/runpolicy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// reconcileStatus decides what a tile's status should be given the real state
// of its container, or ok=false to leave the row alone.
//
// Only running<->stopped is arbitrated. building/paused/done/idle are
// app-level states a container check must not fight: a tile mid-deploy has no
// container yet, a paused cron is a schedule decision, and a finished cron run
// is history. Tiles that own no container of their own (volumes, crons,
// resources) are never touched, same ownership rule as app/handler.go's
// containerID.
//
// state string, not a struct, docker's ManagedContainer.State is
// all the signal there is, and "did it exit non-zero" would cost an inspect
// per container per tick to distinguish error from stopped.
func reconcileStatus(t *repo.Tile, state, health string, hasContainer bool) (string, bool) {
	// Run-to-completion kinds (cron, function) own no long-lived container.
	if pol, ok := runpolicy.For(t.Kind); ok && !pol.KeepAlive {
		return "", false
	}
	if t.IsVolume() {
		return "", false
	}
	running := hasContainer && state == "running"
	// Docker's native HEALTHCHECK verdict rides the same list pass: a running
	// container that reports unhealthy is its own live state, distinct from
	// stopped, amber on the canvas, not red.
	switch t.Status {
	case "running":
		if !running {
			return "stopped", true
		}
		if health == "unhealthy" {
			return "unhealthy", true
		}
	case "unhealthy":
		if !running {
			return "stopped", true
		}
		if health != "unhealthy" {
			return "running", true
		}
	case "stopped":
		if running {
			return "running", true
		}
	// A failed deploy writes "error" (infra/deploy/engine.go), and nothing
	// clears it: a bad image tag leaves Swarm serving the previous task while
	// the canvas reads Crashed for ever. The container is the truth here, same
	// as every other case; the failed deployment row keeps the cause.
	case "error":
		if running {
			if health == "unhealthy" {
				return "unhealthy", true
			}
			return "running", true
		}
	}
	return "", false
}

// reconcile walks every tile against the containers docker actually has and
// writes back the ones that drifted, a manual `docker stop`, an OOM kill, a
// crash, or the container admin page, none of which touch the tiles table.
func (s *Sampler) reconcile(ctx context.Context, cs []runtime.ManagedContainer) {
	state := map[string]string{}  // tile id -> container state
	health := map[string]string{} // tile id -> docker health word
	for _, c := range cs {
		id := c.Labels[runtime.LabelApp]
		if id == "" {
			id = c.Labels[runtime.LabelDB]
		}
		if id == "" {
			continue
		}
		// A tile can carry two containers under one label: a deploy runs the
		// new one alongside the old until retirement, and a failed Remove
		// leaves the exited twin behind for good. A running container wins,
		// or a stale corpse would mark a live tile stopped every 30s.
		if state[id] != "running" {
			state[id] = c.State
			health[id] = c.Health
		}
	}
	if remote, err := s.clus.RunningTileTasks(ctx); err == nil {
		mergeRemote(state, health, remote)
	}
	tiles, err := s.store.ListTiles(ctx)
	if err != nil {
		return
	}
	changed := map[string]bool{} // stack ids whose canvases need a nudge
	for _, t := range tiles {
		st, has := state[t.ID]
		next, ok := reconcileStatus(&t, st, health[t.ID], has)
		if !ok {
			continue
		}
		if err := s.rows.SetTileStatus(ctx, t.ID, next); err == nil {
			changed[t.StackID] = true
		}
	}
	for stackID := range changed {
		s.notifier.Project(stackID)
	}
}

// mergeRemote folds the swarm task list into the state read from this node's
// containers. Swarm's list covers every node; the container list covers only
// this one, so without it a tile running on a worker has no local container,
// reads as gone, and is marked stopped on the canvas while it serves traffic.
//
// A running task beats any non-running local state, not just an absent one: a
// task rescheduled off this node leaves its container behind in `created` or
// `exited`, and honouring that corpse marks a live tile stopped until someone
// prunes by hand. Local state still wins while it says running, which is where
// the finer-grained answer (health, a stale twin) lives.
func mergeRemote(state, health map[string]string, remote map[string]runtime.NodeTask) {
	for id := range remote {
		if state[id] != "running" {
			state[id] = "running"
			health[id] = ""
		}
	}
}
