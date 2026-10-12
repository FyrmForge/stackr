package main

import (
	"context"

	"github.com/FyrmForge/stackr/internal/worker"
)

// runWorker is `stackrd worker`: the node program. The caller builds the
// mTLS config; the real command runner is filled in when none is given.
func runWorker(ctx context.Context, cfg worker.Config) error {
	if cfg.Runner == nil {
		cfg.Runner = worker.ExecRunner{}
	}
	return worker.Run(ctx, cfg)
}
