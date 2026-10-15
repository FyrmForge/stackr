package service

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
)

// WithNode names the node a Docker call is for. The DockerRouter sends the
// call to that node's client; a call without one fails with docker.ErrNoNode,
// so every caller has to decide which node it means:
//
//   - env-bound work (deploy, logs, exec, terminal, forward, shares, volumes,
//     limits) runs on the env's node (nodeOfEnv, nodeOfTile);
//   - controller-only work (proxy inspect, ingress reopen, panel backup) runs
//     on the controller (withController);
//   - multi-env jobs (push, org and server config, image watch) decide per env;
//   - per-node work (VIP rebuild, prune) loops over the nodes (eachNode).
func WithNode(ctx context.Context, nodeID string) context.Context {
	return docker.WithNode(ctx, nodeID)
}

// DockerRouter is the one Docker the service tree holds: it picks the node's
// client from the node id in ctx. A single-node install has one client.
type DockerRouter struct {
	mu      sync.RWMutex
	clients map[string]Docker
	// OwnRegistry answers stackr's registry host (Pull); nil = none.
	OwnRegistry func(ctx context.Context) string
	// ToolReady makes sure a node's tool image can be pulled (EnsureTool);
	// nil = nothing to do.
	ToolReady func(ctx context.Context, nodeID string) error
	// Gate is asked on every call with whether the node has a client; its
	// error refuses the call (offline, too old). nil = no gate.
	Gate func(ctx context.Context, nodeID string, connected bool) error
}

// NewRouter routes to the given clients, by node id.
func NewRouter(clients map[string]Docker) *DockerRouter {
	r := &DockerRouter{clients: map[string]Docker{}}
	for id, c := range clients {
		r.clients[id] = c
	}
	return r
}

// Set adds or replaces a node's client (a worker coming up, step 2).
func (r *DockerRouter) Set(nodeID string, c Docker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clients[nodeID] = c
}

// Remove drops a node's client.
func (r *DockerRouter) Remove(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.clients, nodeID)
}

// pick is the client for the node in ctx. No node is a bug in the caller;
// a node with no client is a node that is not connected.
func (r *DockerRouter) pick(ctx context.Context) (Docker, error) {
	id, ok := docker.NodeFrom(ctx)
	if !ok {
		return nil, docker.ErrNoNode
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.clients[id]
	if r.Gate != nil {
		if err := r.Gate(ctx, id, ok); err != nil {
			return nil, err
		}
	}
	if !ok {
		return nil, fmt.Errorf("%w: no connection to node %s", errs.ErrBusy, id)
	}
	return c, nil
}

// via runs f on the client for ctx's node.
func via[T any](r *DockerRouter, ctx context.Context, f func(Docker) (T, error)) (T, error) {
	c, err := r.pick(ctx)
	if err != nil {
		var zero T
		return zero, err
	}
	return f(c)
}

// do is via for calls that answer only an error.
func (r *DockerRouter) do(ctx context.Context, f func(Docker) error) error {
	c, err := r.pick(ctx)
	if err != nil {
		return err
	}
	return f(c)
}

func (r *DockerRouter) Run(ctx context.Context, spec docker.ContainerSpec) (string, error) {
	spec.Image = r.pinnedLocal(ctx, spec.Image)
	return via(r, ctx, func(c Docker) (string, error) { return c.Run(ctx, spec) })
}

func (r *DockerRouter) Create(ctx context.Context, spec docker.ContainerSpec) (string, error) {
	spec.Image = r.pinnedLocal(ctx, spec.Image)
	return via(r, ctx, func(c Docker) (string, error) { return c.Create(ctx, spec) })
}

func (r *DockerRouter) HostInfo(ctx context.Context) (docker.HostInfo, error) {
	return via(r, ctx, func(c Docker) (docker.HostInfo, error) { return c.HostInfo(ctx) })
}

func (r *DockerRouter) Start(ctx context.Context, id string) error {
	return r.do(ctx, func(c Docker) error { return c.Start(ctx, id) })
}

func (r *DockerRouter) Stop(ctx context.Context, id string) error {
	return r.do(ctx, func(c Docker) error { return c.Stop(ctx, id) })
}

func (r *DockerRouter) Restart(ctx context.Context, id string) error {
	return r.do(ctx, func(c Docker) error { return c.Restart(ctx, id) })
}

func (r *DockerRouter) StopRemove(ctx context.Context, id string) error {
	return r.do(ctx, func(c Docker) error { return c.StopRemove(ctx, id) })
}

func (r *DockerRouter) Rename(ctx context.Context, id, name string) error {
	return r.do(ctx, func(c Docker) error { return c.Rename(ctx, id, name) })
}

func (r *DockerRouter) Pause(ctx context.Context, id string) error {
	return r.do(ctx, func(c Docker) error { return c.Pause(ctx, id) })
}

func (r *DockerRouter) Unpause(ctx context.Context, id string) error {
	return r.do(ctx, func(c Docker) error { return c.Unpause(ctx, id) })
}

func (r *DockerRouter) List(ctx context.Context, labels map[string]string) ([]docker.Container, error) {
	return via(r, ctx, func(c Docker) ([]docker.Container, error) { return c.List(ctx, labels) })
}

func (r *DockerRouter) Inspect(ctx context.Context, id string) (docker.Detail, error) {
	return via(r, ctx, func(c Docker) (docker.Detail, error) { return c.Inspect(ctx, id) })
}

func (r *DockerRouter) Wait(ctx context.Context, id string) (int, error) {
	return via(r, ctx, func(c Docker) (int, error) { return c.Wait(ctx, id) })
}

func (r *DockerRouter) EnsureNetwork(ctx context.Context, name string, subnet netip.Prefix, labels map[string]string) error {
	return r.do(ctx, func(c Docker) error { return c.EnsureNetwork(ctx, name, subnet, labels) })
}

func (r *DockerRouter) UsedSubnets(ctx context.Context) ([]netip.Prefix, error) {
	return via(r, ctx, func(c Docker) ([]netip.Prefix, error) { return c.UsedSubnets(ctx) })
}

func (r *DockerRouter) RemoveNetwork(ctx context.Context, name string) error {
	return r.do(ctx, func(c Docker) error { return c.RemoveNetwork(ctx, name) })
}

func (r *DockerRouter) ListNetworks(ctx context.Context, labels map[string]string) ([]string, error) {
	return via(r, ctx, func(c Docker) ([]string, error) { return c.ListNetworks(ctx, labels) })
}

func (r *DockerRouter) Gateways(ctx context.Context, labels map[string]string) ([]string, error) {
	return via(r, ctx, func(c Docker) ([]string, error) { return c.Gateways(ctx, labels) })
}

func (r *DockerRouter) Connect(ctx context.Context, network, containerID string, aliases []string) error {
	return r.do(ctx, func(c Docker) error { return c.Connect(ctx, network, containerID, aliases) })
}

func (r *DockerRouter) Disconnect(ctx context.Context, network, containerID string) error {
	return r.do(ctx, func(c Docker) error { return c.Disconnect(ctx, network, containerID) })
}

func (r *DockerRouter) NetworkMembers(ctx context.Context, network string) ([]string, error) {
	return via(r, ctx, func(c Docker) ([]string, error) { return c.NetworkMembers(ctx, network) })
}

func (r *DockerRouter) MemberAddr(ctx context.Context, network, containerID string) (ip, cidr string, err error) {
	c, err := r.pick(ctx)
	if err != nil {
		return "", "", err
	}
	return c.MemberAddr(ctx, network, containerID)
}

func (r *DockerRouter) CreateVolume(ctx context.Context, name, driver string, opts, labels map[string]string) error {
	return r.do(ctx, func(c Docker) error { return c.CreateVolume(ctx, name, driver, opts, labels) })
}

func (r *DockerRouter) RemoveVolume(ctx context.Context, name string) error {
	return r.do(ctx, func(c Docker) error { return c.RemoveVolume(ctx, name) })
}

func (r *DockerRouter) InspectVolume(ctx context.Context, name string) (docker.VolumeInfo, error) {
	return via(r, ctx, func(c Docker) (docker.VolumeInfo, error) { return c.InspectVolume(ctx, name) })
}

func (r *DockerRouter) ListVolumes(ctx context.Context, labels map[string]string) ([]docker.VolumeInfo, error) {
	return via(r, ctx, func(c Docker) ([]docker.VolumeInfo, error) { return c.ListVolumes(ctx, labels) })
}

func (r *DockerRouter) EnsureTool(ctx context.Context) error {
	if id, ok := docker.NodeFrom(ctx); ok && r.ToolReady != nil {
		if err := r.ToolReady(ctx, id); err != nil {
			return fmt.Errorf("tool image: %w", err)
		}
	}
	return r.do(ctx, func(c Docker) error { return c.EnsureTool(ctx) })
}

func (r *DockerRouter) TarVolume(ctx context.Context, name string, w io.Writer, live bool) error {
	return r.do(ctx, func(c Docker) error { return c.TarVolume(ctx, name, w, live) })
}

func (r *DockerRouter) UntarVolume(ctx context.Context, name string, rd io.Reader) error {
	return r.do(ctx, func(c Docker) error { return c.UntarVolume(ctx, name, rd) })
}

// Pull fetches ref on ctx's node. An image of stackr's own registry
// (registry.<panel domain>/...) is loaded under its localhost:5000 name from
// the registry on the controller (docker.Client.Pull). A tag ref is then
// tagged with the ref, so containers name it as given; Docker refuses to tag
// a digest ref, so that one is used under its localhost name everywhere
// instead (pinnedLocal).
func (r *DockerRouter) Pull(ctx context.Context, ref, auth string, log io.Writer) error {
	return r.do(ctx, func(c Docker) error {
		if local, ok := r.ownLocal(ctx, ref); ok {
			if err := c.Pull(ctx, local, "", log); err != nil {
				return err
			}
			if strings.Contains(ref, "@") {
				return nil
			}
			return c.Tag(ctx, local, ref)
		}
		return c.Pull(ctx, ref, auth, log)
	})
}

// ownLocal maps an image of stackr's own registry to its localhost:5000 name.
func (r *DockerRouter) ownLocal(ctx context.Context, ref string) (string, bool) {
	own := r.ownRegistry(ctx)
	if own == "" {
		return "", false
	}
	path, ok := strings.CutPrefix(ref, own+"/")
	if !ok {
		return "", false
	}
	return "localhost:" + installspec.RegistryPort + "/" + path, true
}

// pinnedLocal is the name a digest ref of the own registry has on a node.
func (r *DockerRouter) pinnedLocal(ctx context.Context, ref string) string {
	if local, ok := r.ownLocal(ctx, ref); ok && strings.Contains(ref, "@") {
		return local
	}
	return ref
}

// ownRegistry is RegistryHost, "" until the orchestrator sets it.
func (r *DockerRouter) ownRegistry(ctx context.Context) string {
	if r.OwnRegistry == nil {
		return ""
	}
	return r.OwnRegistry(ctx)
}

// LocalDigest asks for an image of stackr's registry by its localhost name:
// the loaded copy is checked against the registry's.
func (r *DockerRouter) LocalDigest(ctx context.Context, ref string) (string, error) {
	if local, ok := r.ownLocal(ctx, ref); ok {
		ref = local
	}
	return via(r, ctx, func(c Docker) (string, error) { return c.LocalDigest(ctx, ref) })
}

func (r *DockerRouter) Tag(ctx context.Context, src, dst string) error {
	return r.do(ctx, func(c Docker) error { return c.Tag(ctx, src, dst) })
}

func (r *DockerRouter) RemoveImage(ctx context.Context, ref string) error {
	return r.do(ctx, func(c Docker) error { return c.RemoveImage(ctx, ref) })
}

func (r *DockerRouter) ListImages(ctx context.Context, labels map[string]string) ([]docker.Image, error) {
	return via(r, ctx, func(c Docker) ([]docker.Image, error) { return c.ListImages(ctx, labels) })
}

func (r *DockerRouter) PruneImages(ctx context.Context, labels map[string]string, keep []string) (removed []string, bytes int64, err error) {
	c, err := r.pick(ctx)
	if err != nil {
		return nil, 0, err
	}
	return c.PruneImages(ctx, labels, keep)
}

func (r *DockerRouter) PruneDangling(ctx context.Context, labels map[string]string) (n int, bytes int64, err error) {
	c, err := r.pick(ctx)
	if err != nil {
		return 0, 0, err
	}
	return c.PruneDangling(ctx, labels)
}

func (r *DockerRouter) BuildCachePrune(ctx context.Context, builder string, olderThan time.Duration) (n int, total string, err error) {
	c, err := r.pick(ctx)
	if err != nil {
		return 0, "", err
	}
	return c.BuildCachePrune(ctx, builder, olderThan)
}

func (r *DockerRouter) EnsureBuilder(ctx context.Context, name string, memMB int) error {
	return r.do(ctx, func(c Docker) error { return c.EnsureBuilder(ctx, name, memMB) })
}

func (r *DockerRouter) Build(
	ctx context.Context,
	builder, dir, dockerfile, tag string,
	buildArgs, labels map[string]string,
	log io.Writer,
) (imageID string, err error) {
	return via(r, ctx, func(c Docker) (string, error) {
		return c.Build(ctx, builder, dir, dockerfile, tag, buildArgs, labels, log)
	})
}

func (r *DockerRouter) Save(ctx context.Context, ref string, w io.Writer) error {
	return r.do(ctx, func(c Docker) error { return c.Save(ctx, ref, w) })
}

func (r *DockerRouter) Logs(ctx context.Context, id string, tail int) (string, error) {
	return via(r, ctx, func(c Docker) (string, error) { return c.Logs(ctx, id, tail) })
}

func (r *DockerRouter) StreamLogs(ctx context.Context, id string, tail int) (lines <-chan string, stop func(), err error) {
	c, err := r.pick(ctx)
	if err != nil {
		return nil, nil, err
	}
	return c.StreamLogs(ctx, id, tail)
}

func (r *DockerRouter) Exec(ctx context.Context, id string, cmd []string) (string, error) {
	return via(r, ctx, func(c Docker) (string, error) { return c.Exec(ctx, id, cmd) })
}

func (r *DockerRouter) ExecStream(
	ctx context.Context,
	id string,
	cmd []string,
	stdin io.Reader,
) (out io.Reader, wait func() error, err error) {
	c, err := r.pick(ctx)
	if err != nil {
		return nil, nil, err
	}
	return c.ExecStream(ctx, id, cmd, stdin)
}

func (r *DockerRouter) ExecTTY(ctx context.Context, id string, cmd []string) (
	conn io.ReadWriteCloser, resize func(cols, rows uint) error, exit func() (int, error), err error) {
	c, err := r.pick(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	return c.ExecTTY(ctx, id, cmd)
}
