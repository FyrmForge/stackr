package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/FyrmForge/hamr/pkg/config"
	"github.com/FyrmForge/hamr/pkg/logging"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/agent"
	stackruntime "github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
)

// runAgent is `stackrd agent`: the node agent, one task per swarm node.
//
// Same binary and same image as the panel, a different entrypoint. That is
// what makes the upgrade path "update the agent service, then the panel",
// the two can never be built from different sources
// (docs/plans/30-docker-swarm.md, step 7).
//
// It opens no database, runs no scheduler and serves no UI. It is a narrow
// proxy to its own node's docker socket, plus the /proc sampler and the two
// sides of a volume move.
func runAgent() {
	log := logging.New(!config.GetEnvOrDefaultBool("DEV_MODE", false))
	slog.SetDefault(log)

	rt, err := stackruntime.New()
	if err != nil {
		log.Error("agent: docker runtime init failed", "error", err)
		os.Exit(1)
	}

	// Both sides of a volume move run this image, and so does every volume
	// op the agent serves. The panel sets MoveImage from the agent service
	// spec for its own process; the agent has to set its own, or a send falls
	// back to the package default (an image with no rsync in it) and a
	// volume browse needs an alpine the node has no reason to have.
	if img := stackruntime.SelfImage(context.Background(), rt); img != "" {
		stackruntime.MoveImage = img
		stackruntime.VolumeToolImage = img
	}

	// No data dir in the agent: its key is the mounted swarm secret.
	key := agent.ReadKey("")
	if key == "" {
		// Without the secret the agent would answer nobody and look healthy
		// doing it. Failing here makes swarm restart the task, which is the
		// visible version of the same problem.
		log.Error("agent: no runtime key at " + agent.SecretPath + ", is the swarm secret mounted?")
		os.Exit(1)
	}
	nodeID, err := rt.SelfNodeID(context.Background())
	if err != nil {
		log.Error("agent: cannot read this node's swarm id", "error", err)
		os.Exit(1)
	}

	// A move receiver left behind by an agent that died mid-move holds a
	// volume open and squats the alias the next move wants.
	rt.SweepMoveReceivers(context.Background())

	// Not deferred: the failure path below calls os.Exit, which skips defers.
	// stop is called on both paths out of here instead.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	s := &agent.Server{
		RT:       rt,
		Key:      key,
		Version:  version,
		NodeID:   nodeID,
		PanelURL: config.GetEnvOrDefault("STACKR_PANEL_URL", "http://"+stackruntime.PanelAlias+":8080"),
	}
	err = s.Run(ctx)
	stop()
	if err != nil {
		log.Error("agent: stopped", "error", err)
		os.Exit(1)
	}
}
