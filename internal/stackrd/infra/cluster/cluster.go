// Package cluster is the one door for every docker call the panel makes.
//
// A call is either about the swarm (a service, an overlay, the node list),
// which always goes to the manager's socket, or about one container or
// volume, which lives on exactly one node and has to be run there. Callers
// used to decide which by hand, three packages wrote their own copy of the
// decision, and the postgres provisioner never made it at all: it ran every
// exec against the local socket, which was right for as long as every
// managed instance happened to live on the manager (docs/plans/35-cluster.md).
//
// Here the node id is the first argument of every container and volume call
// and it is required. There is no "empty means here": that convenience is how
// a forgotten node argument turned into a call against the wrong machine with
// no error. A caller that means this machine says so with Self.
//
// runtime stays the socket layer and agent stays the client and server. This
// package is a front over both, not a rewrite of either.
package cluster

import (
	"context"
	"fmt"
	"io"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/agent"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/placement"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Cluster routes calls to the docker socket or to a node's agent.
type Cluster struct {
	rt    *runtime.Runtime
	nodes *agent.Nodes
	store repo.Store
}

// New takes everything a Cluster needs to reach any node, in the signature.
// There is no optional wiring step: of the thirteen places that used to
// build the managed-tiles service, one plugged the agent in and twelve ran
// against the local socket without anyone noticing.
func New(rt *runtime.Runtime, nodes *agent.Nodes, store repo.Store) *Cluster {
	return &Cluster{rt: rt, nodes: nodes, store: store}
}

// Runtime is the manager's own docker socket, for the swarm plumbing
// packages that take one: envnet, placement, netpool and proxy read and
// shape swarm state and are the manager's business by definition. It is not
// a way to reach a container: those calls take a node and live on Cluster,
// and the grep gate in docs/plans/35-cluster.md step 6 is what keeps them
// there.
func (c *Cluster) Runtime() *runtime.Runtime { return c.rt }

// Self is the swarm node the panel runs on. Callers that mean "this machine"
// pass this, never "".
func (c *Cluster) Self(ctx context.Context) string { return c.nodes.Self(ctx) }

// Reachable reports whether a node's agent answers, or true for this node.
func (c *Cluster) Reachable(ctx context.Context, node string) bool {
	return c.nodes.Reachable(ctx, node)
}

// Client is the agent on one node, for the one job that is agent-to-agent
// by nature: a volume move streams rsync between two workers' agents, and
// neither end of that is a container call the panel makes. Anything that is
// a container or volume call has a method on Cluster and must use it.
func (c *Cluster) Client(ctx context.Context, node string) (*agent.Client, error) {
	if node == "" {
		return nil, fmt.Errorf("cluster: agent client: no node given")
	}
	return c.nodes.Client(ctx, node)
}

// NodeOf is the node a tile's data lives on. It answers through placement,
// never t.HomeNode directly, because a volume tile has no home node of its
// own. A tile with no resolvable node is an error here
// rather than "" one call later, so the message can name the tile.
func (c *Cluster) NodeOf(ctx context.Context, t *repo.Tile) (string, error) {
	if n := placement.NodeOf(ctx, c.store, t); n != "" {
		return n, nil
	}
	if placement.IsPinned(ctx, c.store, t) {
		return "", fmt.Errorf("%s holds a volume but has no home node yet; it gets one on its first deploy", t.Slug)
	}
	return "", fmt.Errorf("%s is not pinned to a node; use the node of its running task", t.Slug)
}

// route decides where one container or volume call goes. local means this
// machine's socket; otherwise cl is the agent on that node. An empty node is
// refused, see the package comment.
func (c *Cluster) route(ctx context.Context, node, what string) (local bool, cl *agent.Client, err error) {
	if node == "" {
		return false, nil, fmt.Errorf("cluster: %s: no node given", what)
	}
	if node == c.nodes.Self(ctx) {
		return true, nil, nil
	}
	cl, err = c.nodes.Client(ctx, node)
	return false, cl, err
}

// Container and volume calls. Each is the runtime method of the same name
// with a node in front, run on that node.

func (c *Cluster) InspectContainer(ctx context.Context, node, id string) (*runtime.ContainerDetail, error) {
	local, cl, err := c.route(ctx, node, "inspect container")
	if err != nil {
		return nil, err
	}
	if local {
		return c.rt.InspectContainer(ctx, id)
	}
	return cl.InspectContainer(ctx, id)
}

func (c *Cluster) Stats(ctx context.Context, node, id string) (runtime.ContainerStats, error) {
	local, cl, err := c.route(ctx, node, "container stats")
	if err != nil {
		return runtime.ContainerStats{}, err
	}
	if local {
		return c.rt.Stats(ctx, id)
	}
	return cl.Stats(ctx, id)
}

func (c *Cluster) HealthStatus(ctx context.Context, node, id string) (string, error) {
	local, cl, err := c.route(ctx, node, "health status")
	if err != nil {
		return "", err
	}
	if local {
		return c.rt.HealthStatus(ctx, id)
	}
	return cl.HealthStatus(ctx, id)
}

func (c *Cluster) PauseContainer(ctx context.Context, node, id string) error {
	local, cl, err := c.route(ctx, node, "pause container")
	if err != nil {
		return err
	}
	if local {
		return c.rt.PauseContainer(ctx, id)
	}
	return cl.PauseContainer(ctx, id)
}

func (c *Cluster) UnpauseContainer(ctx context.Context, node, id string) error {
	local, cl, err := c.route(ctx, node, "unpause container")
	if err != nil {
		return err
	}
	if local {
		return c.rt.UnpauseContainer(ctx, id)
	}
	return cl.UnpauseContainer(ctx, id)
}

func (c *Cluster) StartContainer(ctx context.Context, node, id string) error {
	local, cl, err := c.route(ctx, node, "start container")
	if err != nil {
		return err
	}
	if local {
		return c.rt.StartContainer(ctx, id)
	}
	return cl.StartContainer(ctx, id)
}

func (c *Cluster) StopContainer(ctx context.Context, node, id string) error {
	local, cl, err := c.route(ctx, node, "stop container")
	if err != nil {
		return err
	}
	if local {
		return c.rt.StopContainer(ctx, id)
	}
	return cl.StopContainer(ctx, id)
}

func (c *Cluster) StopRemove(ctx context.Context, node, id string) error {
	local, cl, err := c.route(ctx, node, "stop and remove container")
	if err != nil {
		return err
	}
	if local {
		return c.rt.StopRemove(ctx, id)
	}
	return cl.StopRemove(ctx, id)
}

// ContainerIsSystem is the guard in front of stop and remove. When it cannot
// tell, it says yes: refusing a stop is recoverable, stopping the panel or
// the proxy on a node is not.
func (c *Cluster) ContainerIsSystem(ctx context.Context, node, id string) bool {
	local, cl, err := c.route(ctx, node, "container is system")
	if err != nil {
		return true
	}
	if local {
		return c.rt.ContainerIsSystem(ctx, id)
	}
	return cl.ContainerIsSystem(ctx, id)
}

// ListAll lists every container on one node.
func (c *Cluster) ListAll(ctx context.Context, node string) ([]runtime.ManagedContainer, error) {
	local, cl, err := c.route(ctx, node, "list containers")
	if err != nil {
		return nil, err
	}
	if local {
		return c.rt.ListAll(ctx)
	}
	return cl.ListAll(ctx)
}

// Exec runs a command in a container on node and returns stdout and stderr
// interleaved, the way `docker exec` printed them. The convenience over
// ExecStream for the short psql-style calls that just want the text.
func (c *Cluster) Exec(ctx context.Context, node, id string, cmd []string) (string, error) {
	rd, wait, err := c.ExecStream(ctx, node, id, cmd, nil)
	if err != nil {
		return "", err
	}
	out, rerr := io.ReadAll(rd)
	if werr := wait(); werr != nil {
		return string(out), werr
	}
	return string(out), rerr
}

// ExecStream runs a command in a container on node and streams its output.
func (c *Cluster) ExecStream(ctx context.Context, node, id string, cmd []string, stdin io.Reader) (io.Reader, func() error, error) {
	local, cl, err := c.route(ctx, node, "exec")
	if err != nil {
		return nil, nil, err
	}
	if local {
		return c.rt.ExecStream(ctx, id, cmd, stdin)
	}
	return cl.ExecStream(ctx, id, cmd, stdin)
}

// ExecShell runs one shell command in a container on node and waits for it.
func (c *Cluster) ExecShell(ctx context.Context, node, id, command string) (string, error) {
	local, cl, err := c.route(ctx, node, "exec shell")
	if err != nil {
		return "", err
	}
	if local {
		return c.rt.ExecShell(ctx, id, command)
	}
	return cl.ExecShell(ctx, id, command)
}

// ExecTTY opens an interactive shell in a container wherever it runs. The
// return shape is the local one, so handlers/web/wsterm bridges a browser to
// it without knowing which node answered.
func (c *Cluster) ExecTTY(ctx context.Context, node, id string, cmd []string) (io.ReadWriter, func(w, h uint) error, func(), error) {
	local, cl, err := c.route(ctx, node, "exec tty")
	if err != nil {
		return nil, nil, nil, err
	}
	if local {
		return c.rt.ExecTTY(ctx, id, cmd)
	}
	return cl.ExecTTY(ctx, id, cmd)
}

// StreamLogsMarked follows one container's logs on its own node. Most log
// viewing goes through the service, which the manager collects for every
// replica; this is the per-container case.
func (c *Cluster) StreamLogsMarked(ctx context.Context, node, id string, tail int) (<-chan string, func(), error) {
	local, cl, err := c.route(ctx, node, "stream logs")
	if err != nil {
		return nil, nil, err
	}
	if local {
		return c.rt.StreamLogsMarked(ctx, id, tail)
	}
	return cl.StreamLogsMarked(ctx, id, tail)
}

// Prune runs the image and container prune on one node. Every node keeps its
// own images, so this is per node and not swarm-wide.
func (c *Cluster) Prune(ctx context.Context, node string) (string, error) {
	local, cl, err := c.route(ctx, node, "prune")
	if err != nil {
		return "", err
	}
	if local {
		return c.rt.Prune(ctx)
	}
	return cl.Prune(ctx)
}

// Info is the docker daemon snapshot of one node plus the host's distro.
func (c *Cluster) Info(ctx context.Context, node string) (agent.InfoResp, error) {
	if node == "" {
		return agent.InfoResp{}, fmt.Errorf("cluster: node info: no node given")
	}
	return c.nodes.Info(ctx, node)
}

// Volumes are a directory on one host's disk, so "which node" is never
// optional for any of these. Creating a volume on the manager when the
// operator asked for a worker leaves the worker without the volume it is
// about to mount, and swarm reports the resulting task healthy.

func (c *Cluster) ListVolumes(ctx context.Context, node string) ([]runtime.VolumeInfo, error) {
	local, cl, err := c.route(ctx, node, "list volumes")
	if err != nil {
		return nil, err
	}
	if local {
		return c.rt.ListVolumes(ctx)
	}
	return cl.ListVolumes(ctx)
}

func (c *Cluster) CreateVolume(ctx context.Context, node, name string) error {
	local, cl, err := c.route(ctx, node, "create volume")
	if err != nil {
		return err
	}
	if local {
		return c.rt.CreateVolume(ctx, name)
	}
	return cl.CreateVolume(ctx, name)
}

// RemoveVolume destroys a named volume on one node. Purges rather than
// plain-removes: this is the node page's Delete button, the operator has
// already said the volume goes, and the stopped containers holding it would
// otherwise make that impossible.
func (c *Cluster) RemoveVolume(ctx context.Context, node, name string) error {
	local, cl, err := c.route(ctx, node, "remove volume")
	if err != nil {
		return err
	}
	if local {
		return c.rt.PurgeVolume(ctx, name)
	}
	return cl.RemoveVolume(ctx, name)
}

func (c *Cluster) VolumeSize(ctx context.Context, node, vol string) (int64, error) {
	local, cl, err := c.route(ctx, node, "volume size")
	if err != nil {
		return 0, err
	}
	if local {
		return c.rt.VolumeSize(ctx, vol)
	}
	return cl.VolumeSize(ctx, vol)
}

func (c *Cluster) ListVolumeFiles(ctx context.Context, node, vol, dir string) ([]runtime.VolumeEntry, error) {
	local, cl, err := c.route(ctx, node, "list volume files")
	if err != nil {
		return nil, err
	}
	if local {
		return c.rt.ListVolumeFiles(ctx, vol, dir)
	}
	return cl.ListVolumeFiles(ctx, vol, dir)
}

func (c *Cluster) ReadVolumeFile(ctx context.Context, node, vol, file string) (io.ReadCloser, func() error, error) {
	local, cl, err := c.route(ctx, node, "read volume file")
	if err != nil {
		return nil, nil, err
	}
	if local {
		return c.rt.ReadVolumeFile(ctx, vol, file)
	}
	return cl.ReadVolumeFile(ctx, vol, file)
}

func (c *Cluster) WriteVolumeFile(ctx context.Context, node, vol, file string, src io.Reader) error {
	local, cl, err := c.route(ctx, node, "write volume file")
	if err != nil {
		return err
	}
	if local {
		return c.rt.WriteVolumeFile(ctx, vol, file, src)
	}
	return cl.WriteVolumeFile(ctx, vol, file, src)
}

func (c *Cluster) DeleteVolumeFile(ctx context.Context, node, vol, file string) error {
	local, cl, err := c.route(ctx, node, "delete volume file")
	if err != nil {
		return err
	}
	if local {
		return c.rt.DeleteVolumeFile(ctx, vol, file)
	}
	return cl.DeleteVolumeFile(ctx, vol, file)
}

// TarVolume streams a backup of one node's volume, so a pinned tile on a
// worker backs up the same way one on the manager does.
func (c *Cluster) TarVolume(ctx context.Context, node, vol string, w io.Writer, live bool) error {
	local, cl, err := c.route(ctx, node, "tar volume")
	if err != nil {
		return err
	}
	if local {
		return c.rt.TarVolume(ctx, vol, w, live)
	}
	return cl.TarVolume(ctx, vol, w, live)
}

// UntarVolume wipes a volume and extracts an archive into it. The wipe is the
// destructive half of a restore and there is no way back from it, so callers
// verify the archive before calling, locally and remotely alike.
func (c *Cluster) UntarVolume(ctx context.Context, node, vol string, src io.Reader) error {
	local, cl, err := c.route(ctx, node, "untar volume")
	if err != nil {
		return err
	}
	if local {
		return c.rt.UntarVolume(ctx, vol, src)
	}
	return cl.UntarVolume(ctx, vol, src)
}

// NodeOfStorage is the node a storage tile's volumes live on. A storage row
// is not a tile: it names a server, and the server row carries the swarm node
// id. Until this existed every storage volume was created and destroyed on
// the manager whatever server the operator picked, which is the postgres
// provisioner's bug with a different noun (docs/plans/36-cluster-only.md).
func (c *Cluster) NodeOfStorage(ctx context.Context, s *repo.Storage) (string, error) {
	if s == nil {
		return "", fmt.Errorf("cluster: storage node: no storage given")
	}
	sv, err := c.store.GetServer(ctx, s.ServerID)
	if err != nil {
		return "", err
	}
	if sv == nil || sv.NodeID == "" {
		// "local" is the manager's own row, seeded by the first migration and
		// given its node id by the node reconcile at boot. Before that has run
		// once it is still this machine, so say so rather than refuse.
		if s.ServerID == "local" {
			return c.Self(ctx), nil
		}
		return "", fmt.Errorf("storage %s sits on a server that has not joined the swarm yet", s.Slug)
	}
	return sv.NodeID, nil
}

// CreateVolumeOpts creates a volume with an explicit driver and driver opts,
// how a storage sub-path becomes an nfs, cifs or bind mount. Same node rule
// as CreateVolume: the mount is a directory on one host.
func (c *Cluster) CreateVolumeOpts(ctx context.Context, node, name, driver string, opts map[string]string) error {
	local, cl, err := c.route(ctx, node, "create volume with opts")
	if err != nil {
		return err
	}
	if local {
		return c.rt.CreateVolumeOpts(ctx, name, driver, opts)
	}
	return cl.CreateVolumeOpts(ctx, name, driver, opts)
}

// VolumeExists reports whether a node already has a named volume. The agent
// has no inspect endpoint and this is only ever an existence check, so it
// reads the node's volume list rather than earning one. Callers run it at
// attach and edit time, not in a loop.
func (c *Cluster) VolumeExists(ctx context.Context, node, name string) bool {
	vols, err := c.ListVolumes(ctx, node)
	if err != nil {
		return false
	}
	for _, v := range vols {
		if v.Name == name {
			return true
		}
	}
	return false
}
