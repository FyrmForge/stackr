package render

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/ui/access"
)

// ControllerWarn is the approve form's warning for a stack whose envs run
// on the controller; nil when none do or the cluster has one node.
func ControllerWarn(ctx context.Context, o *service.Orchestrator, orgSlug, stackID, stackSlug string) (*access.ControllerWarn, error) {
	node, envs, err := o.ControllerEnvs(ctx, stackID)
	if err != nil || len(envs) == 0 {
		return nil, err
	}
	w := &access.ControllerWarn{Node: node}
	for _, e := range envs {
		w.Envs = append(w.Envs, access.ControllerEnv{
			Name: e.Slug,
			Move: "/" + orgSlug + "/" + stackSlug + "/" + e.Slug + "/-/drawer?tab=placement",
		})
	}
	return w, nil
}
