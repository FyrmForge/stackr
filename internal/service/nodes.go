package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// This file is how a verb says which node a Docker call is for (the
// DockerRouter fails a call that does not). Four kinds of caller:
//
//   - env-bound work runs on the env's node: withEnvNode, withEnvIDNode,
//     withTileNode;
//   - controller-only work (proxy, panel backup, builds) runs on the
//     controller: withController;
//   - multi-env jobs decide per env, inside the loop;
//   - per-node work (VIP rebuild, prune) loops over the nodes: eachNode.
//
// "The controller" is the controller row, never "this process".

// nodeLeaf is a handle on the nodes table. It holds no state worth sharing:
// subnet reservations go through the one leaf service.New wires into the
// allocators.
func (o *Orchestrator) nodeLeaf() *node.Leaf { return node.New(o.store.Nodes) }

// controllerID is the controller row's id, made first when the boot path has
// not (a database nobody bootstrapped, as in a test).
func (o *Orchestrator) controllerID(ctx context.Context) (string, error) {
	n, err := o.nodeLeaf().Controller(ctx)
	return n.ID, err
}

// withController is ctx for controller-only work.
func (o *Orchestrator) withController(ctx context.Context) (context.Context, error) {
	id, err := o.controllerID(ctx)
	if err != nil {
		return ctx, err
	}
	return docker.WithNode(ctx, id), nil
}

// unplaced is the node of an env row that has none: a row from before nodes
// or a bare fixture. Routed, that is an error, a call without a node fails
// loudly instead of guessing; until the DockerRouter is wired nothing routes,
// and such an env runs on the controller, where every env ran before nodes.
func (o *Orchestrator) unplaced(ctx context.Context, e store.Environment) (string, error) {
	if _, routed := o.docker.(*DockerRouter); routed {
		return "", fmt.Errorf("environment %s has no node: %w", e.Slug, docker.ErrNoNode)
	}
	return o.controllerID(ctx)
}

// withEnvNode is ctx for work on one env: its node.
func (o *Orchestrator) withEnvNode(ctx context.Context, e store.Environment) (context.Context, error) {
	id := e.NodeID
	if id == "" {
		var err error
		if id, err = o.unplaced(ctx, e); err != nil {
			return ctx, err
		}
	}
	return docker.WithNode(ctx, id), nil
}

// withEnvIDNode is withEnvNode for an env id.
func (o *Orchestrator) withEnvIDNode(ctx context.Context, envID string) (context.Context, error) {
	e, err := o.store.Environments.Get(ctx, envID)
	if err != nil {
		return ctx, err
	}
	return o.withEnvNode(ctx, e)
}

// withTileNode is ctx for work on one tile: its env's node.
func (o *Orchestrator) withTileNode(ctx context.Context, tileID string) (context.Context, error) {
	t, err := o.store.Tiles.Get(ctx, tileID)
	if err != nil {
		return ctx, err
	}
	return o.withEnvIDNode(ctx, t.EnvironmentID)
}

// envNode is the node id an env runs on ("" = the controller's): the leaves
// that hold containers (tile, volume) are given it to send each Docker call
// to the right node without their callers naming one.
func (o *Orchestrator) envNode(ctx context.Context, envID string) (string, error) {
	if envID != "" {
		e, err := o.store.Environments.Get(ctx, envID)
		if err != nil {
			return "", err
		}
		if e.NodeID == "" {
			return o.unplaced(ctx, e)
		}
		return e.NodeID, nil
	}
	return o.controllerID(ctx)
}

// scopeNode is the node a volume scope lives on: an env's own; a stack's, an
// org's or a share's ("" kind) is the controller's.
func (o *Orchestrator) scopeNode(ctx context.Context, kind, id string) (string, error) {
	if kind == "env" {
		return o.envNode(ctx, id)
	}
	return o.controllerID(ctx)
}

// nodeOfEnvID is the node row an env runs on; an env with none runs on the
// controller (see withEnvNode).
func (o *Orchestrator) nodeOfEnvID(ctx context.Context, envID string) (store.Node, error) {
	e, err := o.store.Environments.Get(ctx, envID)
	if err != nil {
		return store.Node{}, err
	}
	if e.NodeID == "" {
		id, err := o.unplaced(ctx, e)
		if err != nil {
			return store.Node{}, err
		}
		return o.nodeLeaf().Get(ctx, id)
	}
	return o.nodeLeaf().Get(ctx, e.NodeID)
}

// eachNode runs f once per node with that node in ctx and joins the failures,
// so one node being down does not stop the others.
func (o *Orchestrator) eachNode(ctx context.Context, f func(ctx context.Context, n store.Node) error) error {
	if _, err := o.controllerID(ctx); err != nil { // the boot path's row, if nobody made it
		return err
	}
	ns, err := o.nodeLeaf().List(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, n := range ns {
		if n.Status == node.StatusPending || n.Status == node.StatusJoined { // never reachable yet
			continue
		}
		if err := f(docker.WithNode(ctx, n.ID), n); err != nil {
			errs = append(errs, fmt.Errorf("node %s: %w", n.Name, err))
		}
	}
	return errors.Join(errs...)
}

// envPins is environment.Leaf's WithPlacement: the pins of a new env's stack
// and org, and the controller.
func (o *Orchestrator) envPins(ctx context.Context, stackID string) (environment.Pins, error) {
	var p environment.Pins
	st, err := o.store.Stacks.Get(ctx, stackID)
	if err != nil {
		return p, err
	}
	org, err := o.store.Orgs.Get(ctx, st.OrgID)
	if err != nil {
		return p, err
	}
	p.Stack, p.Org = st.NodeID, org.NodeID
	p.Controller, err = o.controllerID(ctx)
	return p, err
}
