package service

import "context"

// ControllerEnv is an env that runs on the controller.
type ControllerEnv struct {
	ID, Slug string
}

// ControllerEnvs is the stack's envs that run on the controller, and the
// controller's name, once a second node exists; before that every env is
// there and a warning would be noise. The approve screen uses it: a host
// grant on the controller reaches every node.
func (o *Orchestrator) ControllerEnvs(ctx context.Context, stackID string) (string, []ControllerEnv, error) {
	ns, err := o.store.Nodes.List(ctx)
	if err != nil || len(ns) < 2 {
		return "", nil, err
	}
	ctl, err := o.nodeLeaf().Controller(ctx)
	if err != nil {
		return "", nil, err
	}
	es, err := o.store.Environments.ListByStack(ctx, stackID)
	if err != nil {
		return "", nil, err
	}
	var out []ControllerEnv
	for _, e := range es {
		if e.NodeID == ctl.ID {
			out = append(out, ControllerEnv{ID: e.ID, Slug: e.Slug})
		}
	}
	return ctl.Name, out, nil
}

// NodeNames is every node's name by id, for pages that show a node by what
// an audit row or a pin names.
func (o *Orchestrator) NodeNames(ctx context.Context) (map[string]string, error) {
	ns, err := o.store.Nodes.List(ctx)
	out := make(map[string]string, len(ns))
	for _, n := range ns {
		out[n.ID] = n.Name
	}
	return out, err
}
