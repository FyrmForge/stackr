package runtime

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
)

// The swarm cluster itself: nodes, their labels, and where a service's tasks
// actually landed. Everything the servers screen reads comes from here
// (docs/plans/32-multi-node-ui.md).

// NodeGroupLabel is the docker node label a node's group is stored in. The
// tile half is the node_group config key, turned into a placement constraint
// against this label (docs/plans/31-node-agent-open-questions.md, node groups).
const NodeGroupLabel = "stackr.group"

// Node is one swarm node as the panel needs it.
type Node struct {
	ID       string
	Hostname string
	Addr     string
	Role     string // manager | worker
	// State is docker's own node state: ready, down, disconnected, unknown.
	State string
	// Availability is active, pause or drain. Draining is what the Drain
	// button sets; it is separate from State, and a draining node is still
	// ready.
	Availability  string
	EngineVersion string
	Arch          string
	OS            string
	NCPU          int
	MemBytes      int64
	Group         string
	Labels        map[string]string
	UpdatedAt     time.Time
	// Self marks the node this panel runs on.
	Self bool
}

// Status folds docker's state and availability into the one word the servers
// list shows (docs/plans/32-multi-node-ui.md). "pending" is not here: that is
// a row whose node has not appeared in the swarm at all, which is a fact
// about the table, not about a node.
func (n Node) Status() string {
	if n.State != "ready" {
		return "down"
	}
	if n.Availability == "drain" || n.Availability == "pause" {
		return "draining"
	}
	return "ready"
}

// IsManager reports whether the node runs the control plane. The manager page
// has no Drain and no Remove: traefik and the registry are pinned there, so
// draining it kills 80, 443 and the panel.
func (n Node) IsManager() bool { return n.Role == "manager" }

// SelfNodeID is the swarm node this daemon is. Empty when the daemon is not
// in a swarm at all.
func (r *Runtime) SelfNodeID(ctx context.Context) (string, error) {
	info, err := r.cli.Info(ctx)
	if err != nil {
		return "", err
	}
	return info.Swarm.NodeID, nil
}

// IsManagerNode reports whether this daemon can run swarm control-plane
// calls. The Add node button is hidden when it cannot.
func (r *Runtime) IsManagerNode(ctx context.Context) bool {
	info, err := r.cli.Info(ctx)
	return err == nil && info.Swarm.ControlAvailable
}

// ListNodes returns every node in the swarm.
func (r *Runtime) ListNodes(ctx context.Context) ([]Node, error) {
	self, _ := r.SelfNodeID(ctx)
	ns, err := r.cli.NodeList(ctx, swarm.NodeListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(ns))
	for _, n := range ns {
		out = append(out, toNode(n, self))
	}
	return out, nil
}

// GetNode returns one node by ID.
func (r *Runtime) GetNode(ctx context.Context, id string) (Node, error) {
	self, _ := r.SelfNodeID(ctx)
	n, _, err := r.cli.NodeInspectWithRaw(ctx, id)
	if err != nil {
		return Node{}, err
	}
	return toNode(n, self), nil
}

func toNode(n swarm.Node, selfID string) Node {
	out := Node{
		ID:            n.ID,
		Hostname:      n.Description.Hostname,
		Addr:          n.Status.Addr,
		Role:          string(n.Spec.Role),
		State:         string(n.Status.State),
		Availability:  string(n.Spec.Availability),
		EngineVersion: n.Description.Engine.EngineVersion,
		Arch:          n.Description.Platform.Architecture,
		OS:            n.Description.Platform.OS,
		NCPU:          int(n.Description.Resources.NanoCPUs / 1e9),
		MemBytes:      n.Description.Resources.MemoryBytes,
		Labels:        n.Spec.Labels,
		UpdatedAt:     n.UpdatedAt,
		Self:          n.ID == selfID,
	}
	out.Group = n.Spec.Labels[NodeGroupLabel]
	return out
}

// SetNodeGroup writes (or clears, with "") the node's group label. Every
// other docker node label is left alone: free-form labels are a separate,
// parked job (docs/notes.md, misc).
func (r *Runtime) SetNodeGroup(ctx context.Context, nodeID, group string) error {
	return r.updateNode(ctx, nodeID, func(spec *swarm.NodeSpec) {
		if spec.Labels == nil {
			spec.Labels = map[string]string{}
		}
		if group == "" {
			delete(spec.Labels, NodeGroupLabel)
			return
		}
		spec.Labels[NodeGroupLabel] = group
	})
}

// SetNodeAvailability drains, pauses or reactivates a node. Draining is how
// swarm moves stateless tasks off; pinned tiles do not move on their own and
// are the Move modal's job (docs/plans/32-multi-node-ui.md, Drain).
func (r *Runtime) SetNodeAvailability(ctx context.Context, nodeID, availability string) error {
	return r.updateNode(ctx, nodeID, func(spec *swarm.NodeSpec) {
		spec.Availability = swarm.NodeAvailability(availability)
	})
}

func (r *Runtime) updateNode(ctx context.Context, nodeID string, mutate func(*swarm.NodeSpec)) error {
	n, _, err := r.cli.NodeInspectWithRaw(ctx, nodeID)
	if err != nil {
		return err
	}
	spec := n.Spec
	mutate(&spec)
	return r.cli.NodeUpdate(ctx, n.ID, n.Version, spec)
}

// RemoveNode takes a node out of the swarm. Force, because a node that has
// already gone dark never leaves cleanly and leaving it listed is worse than
// removing it, the volumes on its disk are unreachable either way, which is
// what the Remove modal warns about.
func (r *Runtime) RemoveNode(ctx context.Context, nodeID string) error {
	return r.cli.NodeRemove(ctx, nodeID, swarm.NodeRemoveOptions{Force: true})
}

// JoinToken is the swarm's worker join token, which the join script needs
// alongside a manager address.
func (r *Runtime) JoinToken(ctx context.Context) (token, managerAddr string, err error) {
	sw, err := r.cli.SwarmInspect(ctx)
	if err != nil {
		return "", "", err
	}
	info, err := r.cli.Info(ctx)
	if err != nil {
		return "", "", err
	}
	addr := info.Swarm.NodeAddr
	for _, m := range info.Swarm.RemoteManagers {
		if m.Addr != "" {
			addr = strings.TrimSuffix(m.Addr, ":2377")
			break
		}
	}
	return sw.JoinTokens.Worker, addr, nil
}

// NodeTask is one task of one service, with the node it landed on and the
// address it answers to on a network.
type NodeTask struct {
	ID           string
	NodeID       string
	Slot         int
	State        string
	DesiredState string
	ContainerID  string
	// Addr is the task's IP on the network asked for, without the mask.
	Addr string
	Err  string
}

// ServiceTasksOnNetwork lists a service's running tasks with each task's
// address on netName. This is how the panel finds an agent: the agent service
// is global, so one task per node, and the task's stkr address is where that
// node's agent listens. Nothing is configured and nothing is registered.
func (r *Runtime) ServiceTasksOnNetwork(ctx context.Context, service, netName string) ([]NodeTask, error) {
	netID := ""
	if n, err := r.cli.NetworkInspect(ctx, netName, network.InspectOptions{}); err == nil {
		netID = n.ID
	}
	ts, err := r.cli.TaskList(ctx, swarm.TaskListOptions{
		Filters: filters.NewArgs(filters.Arg("service", service)),
	})
	if err != nil {
		return nil, err
	}
	var out []NodeTask
	for _, t := range ts {
		nt := NodeTask{
			ID: t.ID, NodeID: t.NodeID, Slot: t.Slot,
			State: string(t.Status.State), DesiredState: string(t.DesiredState),
			Err: t.Status.Err,
		}
		if t.Status.ContainerStatus != nil {
			nt.ContainerID = t.Status.ContainerStatus.ContainerID
		}
		for _, na := range t.NetworksAttachments {
			if na.Network.ID != netID && na.Network.Spec.Name != netName {
				continue
			}
			for _, a := range na.Addresses {
				if ip, _, ok := strings.Cut(a, "/"); ok {
					nt.Addr = ip
				} else {
					nt.Addr = a
				}
				break
			}
		}
		out = append(out, nt)
	}
	return out, nil
}

// RunningTasks lists a service's running tasks: the node each is on and the
// container id there.
//
// This is the multi-node answer to "where is this tile's container". Looking
// it up by label on the local socket only ever finds the ones on the manager,
// so a tile running on a worker came back with nothing, and the callers read
// that as "not running" rather than "not here", which is how a backup came to
// tar an empty volume and report success.
func (r *Runtime) RunningTasks(ctx context.Context, service string) ([]NodeTask, error) {
	if service == "" {
		return nil, nil
	}
	ts, err := r.cli.TaskList(ctx, swarm.TaskListOptions{
		Filters: filters.NewArgs(
			filters.Arg("service", service),
			filters.Arg("desired-state", "running"),
		),
	})
	if err != nil {
		return nil, err
	}
	var out []NodeTask
	for _, t := range ts {
		if t.Status.State != swarm.TaskStateRunning || t.Status.ContainerStatus == nil {
			continue
		}
		out = append(out, NodeTask{
			ID: t.ID, NodeID: t.NodeID, Slot: t.Slot,
			State:        string(t.Status.State),
			DesiredState: string(t.DesiredState),
			ContainerID:  t.Status.ContainerStatus.ContainerID,
			Err:          t.Status.Err,
		})
	}
	return out, nil
}

// RunningTileTasks maps a tile id to the node and container of its running
// task, for every tile in the swarm.
//
// The manager's own socket lists only the manager's containers, so a tile on
// a worker looked exactly like a tile whose container had gone: the status
// reconciler marked it stopped and the canvas showed it red while it was
// serving traffic. Swarm's task list is the one view that covers every node.
func (r *Runtime) RunningTileTasks(ctx context.Context) (map[string]NodeTask, error) {
	ts, err := r.cli.TaskList(ctx, swarm.TaskListOptions{
		Filters: filters.NewArgs(filters.Arg("desired-state", "running")),
	})
	if err != nil {
		return nil, err
	}
	out := map[string]NodeTask{}
	for _, t := range ts {
		if t.Status.State != swarm.TaskStateRunning || t.Spec.ContainerSpec == nil {
			continue
		}
		id := t.Spec.ContainerSpec.Labels[LabelApp]
		if id == "" {
			id = t.Spec.ContainerSpec.Labels[LabelDB]
		}
		if id == "" {
			continue
		}
		nt := NodeTask{
			ID: t.ID, NodeID: t.NodeID, Slot: t.Slot,
			State: string(t.Status.State), DesiredState: string(t.DesiredState),
		}
		if t.Status.ContainerStatus != nil {
			nt.ContainerID = t.Status.ContainerStatus.ContainerID
		}
		out[id] = nt
	}
	return out, nil
}

// TasksByNode counts a node's running tasks, the servers list's "containers"
// column, since one task is one container.
func (r *Runtime) TasksByNode(ctx context.Context) (map[string]int, error) {
	ts, err := r.cli.TaskList(ctx, swarm.TaskListOptions{
		Filters: filters.NewArgs(filters.Arg("desired-state", "running")),
	})
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, t := range ts {
		if t.Status.State != swarm.TaskStateRunning {
			continue
		}
		out[t.NodeID]++
	}
	return out, nil
}

// NodeTasks lists the running tasks on one node, service name included, for
// the server page's "Running here" list.
func (r *Runtime) NodeTasks(ctx context.Context, nodeID string) ([]NodeTaskInfo, error) {
	ts, err := r.cli.TaskList(ctx, swarm.TaskListOptions{
		Filters: filters.NewArgs(filters.Arg("node", nodeID), filters.Arg("desired-state", "running")),
	})
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	if svcs, err := r.cli.ServiceList(ctx, swarm.ServiceListOptions{}); err == nil {
		for _, s := range svcs {
			names[s.ID] = s.Spec.Name
		}
	}
	var out []NodeTaskInfo
	for _, t := range ts {
		// A network-attachment task has no ContainerSpec at all, so reading
		// its labels panics and takes the whole server page with it. Swarm
		// creates one per node per attachable overlay, which is every node on
		// this install.
		tileID := ""
		if t.Spec.ContainerSpec != nil {
			tileID = t.Spec.ContainerSpec.Labels[LabelApp]
		}
		out = append(out, NodeTaskInfo{
			Service: names[t.ServiceID],
			Slot:    t.Slot,
			State:   string(t.Status.State),
			TileID:  tileID,
		})
	}
	return out, nil
}

// NodeTaskInfo is one row of a node's "Running here" list.
type NodeTaskInfo struct {
	Service string
	Slot    int
	State   string
	TileID  string
}

// --- secrets --------------------------------------------------------------

// EnsureSecret creates a swarm secret if it is missing and returns whether it
// already existed. The agent's shared runtime key lives in one of these:
// mounted into the agent and the panel services, readable by neither tiles
// nor users (docs/plans/31-node-agent-open-questions.md, where the key lives).
func (r *Runtime) EnsureSecret(ctx context.Context, name string, value []byte) (existed bool, err error) {
	ss, err := r.cli.SecretList(ctx, swarm.SecretListOptions{
		Filters: filters.NewArgs(filters.Arg("name", name)),
	})
	if err != nil {
		return false, err
	}
	for _, s := range ss {
		if s.Spec.Name == name {
			return true, nil
		}
	}
	_, err = r.cli.SecretCreate(ctx, swarm.SecretSpec{
		Annotations: swarm.Annotations{Name: name},
		Data:        value,
	})
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return true, nil
	}
	return false, err
}

// SecretID resolves a secret's name to its ID, which is what a service spec
// references.
func (r *Runtime) SecretID(ctx context.Context, name string) (string, error) {
	ss, err := r.cli.SecretList(ctx, swarm.SecretListOptions{
		Filters: filters.NewArgs(filters.Arg("name", name)),
	})
	if err != nil {
		return "", err
	}
	for _, s := range ss {
		if s.Spec.Name == name {
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("secret %s not found", name)
}

// SelfServiceName is the swarm service this process's own task belongs to, or
// "" when the panel is still a plain container.
//
// Read from the task's own labels rather than guessed from a constant. Note
// that "" is two answers in one: a plain-container install, and a lookup that
// failed. The one caller, agent.Ensure, only mounts a key either way, so it
// does not have to tell them apart; anything destructive would.
func (r *Runtime) SelfServiceName(ctx context.Context) string {
	id := SelfContainerID(ctx, r)
	if id == "" {
		return ""
	}
	info, err := r.cli.ContainerInspect(ctx, id)
	if err != nil || info.Config == nil {
		return ""
	}
	return info.Config.Labels["com.docker.swarm.service.name"]
}

// SelfImage is the image this process's own container runs. Empty outside a
// container.
func SelfImage(ctx context.Context, r *Runtime) string {
	id := SelfContainerID(ctx, r)
	if id == "" {
		return ""
	}
	info, err := r.cli.ContainerInspect(ctx, id)
	if err != nil {
		return ""
	}
	return info.Config.Image
}

// containerIDRe matches the container id in a bind docker sets up for every
// container: /var/lib/docker/containers/<id>/hostname and friends.
var containerIDRe = regexp.MustCompile(`/containers/([0-9a-f]{64})/`)

// SelfContainerID is the id of the container this process runs in, or "" when
// it is not in one.
//
// The hostname is the usual answer and is wrong here: both the panel and the
// installer set --hostname stkr-panel so the overlay alias resolves, and a
// lookup by that name finds nothing. That failure is silent and expensive,
// it is what left the runtime key unmounted on the panel, so every call to a
// remote agent answered "no node agent on this install yet", and what would
// have let the encryption flip tear down the panel's own service instead of
// stepping over it. So: try the hostname, then read the id out of the binds
// docker gives every container.
func SelfContainerID(ctx context.Context, r *Runtime) string {
	if h, err := os.Hostname(); err == nil && h != "" {
		if _, err := r.cli.ContainerInspect(ctx, h); err == nil {
			return h
		}
	}
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	if m := containerIDRe.FindSubmatch(b); m != nil {
		return string(m[1])
	}
	return ""
}
