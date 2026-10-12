package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
)

// ingressNodes are the nodes tagged ingress, the controller included.
func (o *Orchestrator) ingressNodes(ctx context.Context) []string {
	ns, err := o.store.Nodes.List(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, n := range ns {
		if n.Status != node.StatusPending && slices.Contains(node.Tags(n), node.TagIngress) {
			out = append(out, n.ID)
		}
	}
	return out
}

// ensureNodeProxy brings a worker's proxy container to its tags: tagged
// ingress = installspec.NodeProxy from the controller's own proxy image,
// recreated when its spec stamp differs; untagged = none. The DNS-01 token
// never leaves the controller: workers get certificates by HTTP-01. The
// controller's proxy is the installer's, never touched here.
func (o *Orchestrator) ensureNodeProxy(ctx context.Context, nodeID string) error {
	ctl, err := o.controllerID(ctx)
	if err != nil || nodeID == ctl {
		return err
	}
	n, err := o.store.Nodes.Get(ctx, nodeID)
	if err != nil {
		return err
	}
	want := n.Status != node.StatusPending && slices.Contains(node.Tags(n), node.TagIngress)
	nctx := WithNode(ctx, nodeID)
	var spec installspec.Container
	if want {
		d, err := o.docker.Inspect(WithNode(ctx, ctl), installspec.ProxyName)
		if err != nil {
			return fmt.Errorf("controller proxy: %w", err)
		}
		spec = installspec.NodeProxy(d.Image)
	}
	cs, err := o.docker.List(nctx, map[string]string{installspec.LabelRole: "proxy"})
	if err != nil {
		return err
	}
	for _, c := range cs {
		if want && c.Labels[installspec.LabelSpec] == spec.Labels[installspec.LabelSpec] {
			return nil
		}
		if err := o.docker.StopRemove(nctx, c.ID); err != nil {
			return fmt.Errorf("remove old proxy on %s: %w", n.Name, err)
		}
	}
	if !want {
		return nil
	}
	if _, err := o.docker.LocalDigest(nctx, spec.Image); err != nil {
		if err := o.docker.Pull(nctx, spec.Image, "", io.Discard); err != nil {
			return fmt.Errorf("proxy image on %s: %w", n.Name, err)
		}
	}
	if _, err := o.docker.Run(nctx, docker.ContainerSpec{
		Name: spec.Name, Image: spec.Image, Cmd: spec.Cmd, Env: spec.Env, Labels: spec.Labels,
		Volumes: spec.Volumes, HostNetwork: true, Restart: spec.Restart,
	}); err != nil {
		return fmt.Errorf("start proxy on %s: %w", n.Name, err)
	}
	// The first push must find the admin socket the new Caddy makes.
	w := o.caddyFor(nodeID)
	for i := 0; w != nil && i < 30; i++ {
		if resp, err := w.Caddy(ctx, http.MethodGet, "/config/", nil); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil
}

// pushNodeProxy is the Syncer's PushNode: a worker's proxy is made (or
// brought up to date) before its config goes to it.
func (o *Orchestrator) pushNodeProxy(push func(context.Context, string, json.RawMessage) error) func(context.Context, string, json.RawMessage) error {
	return func(ctx context.Context, nodeID string, cfg json.RawMessage) error {
		if err := o.ensureNodeProxy(ctx, nodeID); err != nil {
			return err
		}
		return push(ctx, nodeID, cfg)
	}
}

// ingressTagged runs after a node's tags change: its proxy follows the
// ingress tag, then every ingress node gets its config again.
func (o *Orchestrator) ingressTagged(nodeID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := o.ensureNodeProxy(ctx, nodeID); err != nil {
		slog.Warn("ingress: node proxy", "node", nodeID, "err", err)
	}
	if err := o.SyncProxy(ctx); err != nil {
		slog.Warn("ingress: proxy sync", "err", err)
	}
}
