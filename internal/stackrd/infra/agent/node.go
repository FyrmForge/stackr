package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
)

// Nodes is the panel's view of the agents, and the one place that decides
// socket or agent for a call.
//
// Plan 30's "one code path, agent everywhere" is deliberately overruled here
// as bloat (docs/plans/31-node-agent-open-questions.md, which calls move): on
// a single node nothing new runs, the panel keeps its own docker socket, and
// the agent is only ever reached for a container that is on another node.
//
// This is not an interface over runtime.Runtime. Runtime has some sixty
// methods and only the handful below ever leave the manager; wrapping the
// whole thing to remote eight calls would be a much larger change for no
// more behaviour.
type Nodes struct {
	RT      *runtime.Runtime
	Key     string
	Version string
	dataDir string

	mu     sync.Mutex
	selfID string
	// addrs caches node ID to agent overlay address. Task state is a manager
	// API call per lookup otherwise, and exec and stats poll.
	addrs   map[string]string
	addrsAt time.Time
}

// New builds the panel's node dispatcher. The key is read from the swarm
// secret mounted into the panel; empty means no agent has been set up yet,
// which is every single-node install that has never added a node.
func New(rt *runtime.Runtime, version, dataDir string) *Nodes {
	return &Nodes{
		RT: rt, Key: ReadKey(dataDir), Version: version,
		dataDir: dataDir, addrs: map[string]string{},
	}
}

// key is the runtime key, re-read once if it was not there at boot. The
// first Add node writes it, and the panel that handled that request is the
// one that has to use it without being restarted first.
func (n *Nodes) key() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.Key == "" {
		n.Key = ReadKey(n.dataDir)
	}
	return n.Key
}

// ReadKey loads the shared runtime key: the mounted swarm secret in the
// agent, the copy beside the database in the panel. Absent is not an error,
// an install that has never added a node has no agent, no secret, and
// nothing to talk to.
//
// dataDir is empty in the agent, which only ever has the secret.
func ReadKey(dataDir string) string {
	paths := []string{SecretPath}
	if dataDir != "" {
		paths = append(paths, KeyFile(dataDir))
	}
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil {
			if k := strings.TrimSpace(string(b)); k != "" {
				return k
			}
		}
	}
	return ""
}

// Self is the swarm node the panel runs on.
func (n *Nodes) Self(ctx context.Context) string {
	n.mu.Lock()
	if n.selfID != "" {
		defer n.mu.Unlock()
		return n.selfID
	}
	n.mu.Unlock()
	id, _ := n.RT.SelfNodeID(ctx)
	n.mu.Lock()
	n.selfID = id
	n.mu.Unlock()
	return id
}

// IsSelf reports whether nodeID is this machine. An empty node ID means
// "wherever it is", which on a single-node install is here, that is what
// keeps every existing caller working unchanged.
func (n *Nodes) IsSelf(ctx context.Context, nodeID string) bool {
	return nodeID == "" || nodeID == n.Self(ctx)
}

// Client returns the agent client for one node, or an error when that node's
// agent task is not running. It is never called for the local node: IsSelf
// answers that first, and this is the other branch.
func (n *Nodes) Client(ctx context.Context, nodeID string) (*Client, error) {
	key := n.key()
	if key == "" {
		return nil, fmt.Errorf("no node agent on this install yet")
	}
	addr, err := n.addrOf(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	return &Client{Addr: addr, Key: key, Version: n.Version}, nil
}

// addrOf resolves a node to its agent's address on the stkr overlay, reading
// swarm task state. Cached for a few seconds: exec and stats poll, and every
// miss is a manager API round trip.
func (n *Nodes) addrOf(ctx context.Context, nodeID string) (string, error) {
	n.mu.Lock()
	if time.Since(n.addrsAt) < 5*time.Second {
		if a, ok := n.addrs[nodeID]; ok {
			n.mu.Unlock()
			return a, nil
		}
	}
	n.mu.Unlock()

	tasks, err := n.RT.ServiceTasksOnNetwork(ctx, ServiceName, runtime.NetworkName)
	if err != nil {
		return "", err
	}
	fresh := map[string]string{}
	for _, t := range tasks {
		if t.State == "running" && t.Addr != "" {
			fresh[t.NodeID] = t.Addr
		}
	}
	n.mu.Lock()
	n.addrs, n.addrsAt = fresh, time.Now()
	n.mu.Unlock()

	if a, ok := fresh[nodeID]; ok {
		return a, nil
	}
	return "", fmt.Errorf("no agent task running on node %s", nodeID)
}

// Reachable reports whether a node's agent answers. Used by the UI to decide
// whether exec, stats and volume browse are offered on an off-manager tile.
func (n *Nodes) Reachable(ctx context.Context, nodeID string) bool {
	if n.IsSelf(ctx, nodeID) {
		return true
	}
	_, err := n.addrOf(ctx, nodeID)
	return err == nil
}

// Info is the docker daemon snapshot of one node, plus the host's distro,
// which only the agent can see.
func (n *Nodes) Info(ctx context.Context, nodeID string) (InfoResp, error) {
	if n.IsSelf(ctx, nodeID) {
		h, err := n.RT.Info(ctx)
		return InfoResp{Host: h, Distro: distro()}, err
	}
	c, err := n.Client(ctx, nodeID)
	if err != nil {
		return InfoResp{}, err
	}
	return c.Info(ctx)
}
