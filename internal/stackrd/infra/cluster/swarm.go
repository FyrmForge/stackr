package cluster

import (
	"context"
	"io"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
)

// Swarm calls. A service, an overlay or the node list is cluster-wide by
// nature and always answered by the manager, so none of these takes a node
// and passing one does not compile. That is what keeps the two kinds of call
// from being confused: a container call without a node is an error, a swarm
// call with one is a type error.
//
// Only what callers actually use is forwarded. Runtime has some sixty
// methods and most of them are the manager's own business (proxy, netpool,
// the registry) and stay on runtime directly.

func (c *Cluster) ListNodes(ctx context.Context) ([]runtime.Node, error) {
	return c.rt.ListNodes(ctx)
}

func (c *Cluster) RunningTasks(ctx context.Context, service string) ([]runtime.NodeTask, error) {
	return c.rt.RunningTasks(ctx, service)
}

func (c *Cluster) RunningTileTasks(ctx context.Context) (map[string]runtime.NodeTask, error) {
	return c.rt.RunningTileTasks(ctx)
}

func (c *Cluster) EnsureService(ctx context.Context, s runtime.ServiceSpec) (created bool, err error) {
	return c.rt.EnsureService(ctx, s)
}

func (c *Cluster) RemoveService(ctx context.Context, name string) error {
	return c.rt.RemoveService(ctx, name)
}

func (c *Cluster) StopService(ctx context.Context, name string) error {
	return c.rt.StopService(ctx, name)
}

func (c *Cluster) ScaleService(ctx context.Context, name string, n int) error {
	return c.rt.ScaleService(ctx, name, n)
}

func (c *Cluster) RestartService(ctx context.Context, name string, want int) error {
	return c.rt.RestartService(ctx, name, want)
}

func (c *Cluster) ServiceStatus(ctx context.Context, name string) (runtime.ServiceState, error) {
	return c.rt.ServiceStatus(ctx, name)
}

func (c *Cluster) StreamServiceLogsMarked(ctx context.Context, name string, tail int) (<-chan string, func(), error) {
	return c.rt.StreamServiceLogsMarked(ctx, name, tail)
}

func (c *Cluster) RunJob(ctx context.Context, s runtime.ServiceSpec) (string, error) {
	return c.rt.RunJob(ctx, s)
}

func (c *Cluster) AttachServiceNetwork(ctx context.Context, name string, n runtime.NetAttach) (bool, error) {
	return c.rt.AttachServiceNetwork(ctx, name, n)
}

func (c *Cluster) AttachServiceNetworks(ctx context.Context, name string, ns []runtime.NetAttach) (bool, error) {
	return c.rt.AttachServiceNetworks(ctx, name, ns)
}

func (c *Cluster) DetachServiceNetwork(ctx context.Context, name, netName string) (bool, error) {
	return c.rt.DetachServiceNetwork(ctx, name, netName)
}

func (c *Cluster) WaitConverged(ctx context.Context, name string, timeout time.Duration) error {
	return c.rt.WaitConverged(ctx, name, timeout)
}

func (c *Cluster) WaitRolled(ctx context.Context, name string, since time.Time, timeout time.Duration) error {
	return c.rt.WaitRolled(ctx, name, since, timeout)
}

func (c *Cluster) EnsureOverlay(ctx context.Context, name string) error {
	return c.rt.EnsureOverlay(ctx, name)
}

// Manager-only for now. Each of these is really a node call, a container or
// an image or a volume lives on one machine, but the agent has no endpoint
// for it yet, so it runs on the manager whatever node was meant. They are
// here rather than left on runtime so callers still hold one thing and the
// gap is written in one place. Every one is a "node string" argument away
// from being routed once its agent endpoint exists (docs/notes.md, misc).

// PullImage pulls on the manager. Swarm pulls on the target node itself
// when the service is scheduled, so this is a warm-up, not the real pull.
func (c *Cluster) PullImage(ctx context.Context, ref string, logW io.Writer) error {
	return c.rt.PullImage(ctx, ref, logW)
}

// LocalDigest reads the manager's copy of the image, which can differ from
// what a worker runs.
func (c *Cluster) LocalDigest(ctx context.Context, ref string) (string, error) {
	return c.rt.LocalDigest(ctx, ref)
}

// ConnectContainer attaches a container on the manager to a network. The
// only caller is the panel joining itself to an instance's overlay, which is
// itself on the list to be removed altogether.
func (c *Cluster) ConnectContainer(ctx context.Context, netName, containerID string, aliases []string) error {
	return c.rt.ConnectContainer(ctx, netName, containerID, aliases)
}

func (c *Cluster) NetworkMemberAddr(ctx context.Context, netName, containerID string) (ip, cidr string, err error) {
	return c.rt.NetworkMemberAddr(ctx, netName, containerID)
}

func (c *Cluster) InspectVolume(ctx context.Context, name string) (runtime.VolumeInfo, error) {
	return c.rt.InspectVolume(ctx, name)
}

func (c *Cluster) ListByLabel(ctx context.Context, label, value string) ([]runtime.ManagedContainer, error) {
	return c.rt.ListByLabel(ctx, label, value)
}

// VerifyTar and EnsureVolumeTool act on the manager's own disk and image
// cache, which is where infra/backup stages archives. Manager-only by
// design, not by gap.
func (c *Cluster) VerifyTar(ctx context.Context, path string) error {
	return c.rt.VerifyTar(ctx, path)
}

func (c *Cluster) EnsureVolumeTool(ctx context.Context) error {
	return c.rt.EnsureVolumeTool(ctx)
}

// Logs is the last tail lines of one container, one shot. Manager-only: the
// agent streams logs but has no one-shot endpoint, and the only caller is the
// pre-swarm container fallback below, which lists the manager's own
// containers by label. Anything following a live tile's logs goes through the
// service, which swarm collects from every node.
func (c *Cluster) Logs(ctx context.Context, id string, tail int) (string, error) {
	return c.rt.Logs(ctx, id, tail)
}

func (c *Cluster) ServiceLogs(ctx context.Context, name string, tail int) (string, error) {
	return c.rt.ServiceLogs(ctx, name, tail)
}

// RemoveImage and PushImage act on the manager's own image store, which is
// where the build engine builds and where the registry push reads from.
func (c *Cluster) RemoveImage(ctx context.Context, ref string) error {
	return c.rt.RemoveImage(ctx, ref)
}

func (c *Cluster) PushImage(ctx context.Context, ref string, logW io.Writer) error {
	return c.rt.PushImage(ctx, ref, logW)
}

// PullImageAuth and PushImageAuth move an image on the manager with a
// credential supplied per call, so one org's registry token never lands in the
// node's shared daemon config. See runtime.PullImageAuth.
func (c *Cluster) PullImageAuth(ctx context.Context, ref, auth string, logW io.Writer) error {
	return c.rt.PullImageAuth(ctx, ref, auth, logW)
}

func (c *Cluster) PushImageAuth(ctx context.Context, ref, auth string, logW io.Writer) error {
	return c.rt.PushImageAuth(ctx, ref, auth, logW)
}
