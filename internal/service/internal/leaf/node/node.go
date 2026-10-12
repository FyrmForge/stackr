// Package node owns the nodes table: the machines stackr runs envs on, their
// roles, tags and address ranges. "The head" in code is the controller row,
// never "this process". It also hands out the /24 blocks of a node's range
// (Reserve) so no Docker daemon picks a subnet on its own.
package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/cluster"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Roles. The controller runs the panel; every other node is a worker running
// `stackrd worker` (flow/jobs says "worker" for job runners; not this).
const (
	Controller = "controller"
	Worker     = "worker"
)

// Capability tags. The controller is tagged with all three at bootstrap.
const (
	TagIngress = "ingress"
	TagEgress  = "egress"
	TagBuilder = "builder"
)

// Status of a node row.
const (
	StatusOnline  = "online"
	StatusOffline = "offline" // joined once, not answering
)

// holdFor is how long a reserved block stays claimed while its network or
// env row is not yet visible to the next reservation.
const holdFor = time.Minute

// Leaf is the node leaf. One instance per process: it remembers the blocks it
// just handed out.
type Leaf struct {
	nodes store.NodeStore

	mu   sync.Mutex
	held map[string][]hold // node id -> blocks reserved a moment ago
}

type hold struct {
	p  netip.Prefix
	at time.Time
}

func New(nodes store.NodeStore) *Leaf {
	return &Leaf{nodes: nodes, held: map[string][]hold{}}
}

func (l *Leaf) Get(ctx context.Context, id string) (store.Node, error) {
	return l.nodes.Get(ctx, id)
}

func (l *Leaf) GetBySlug(ctx context.Context, slug string) (store.Node, error) {
	return l.nodes.GetBySlug(ctx, slug)
}

func (l *Leaf) List(ctx context.Context) ([]store.Node, error) {
	return l.nodes.List(ctx)
}

// Bootstrap makes the controller row when it is missing. created reports
// whether this call made it, so the caller writes the one node.bootstrap
// security event. A race for the row is settled by the unique index: the
// loser reads the winner's row and reports created false.
func (l *Leaf) Bootstrap(ctx context.Context) (n store.Node, created bool, err error) {
	n, err = l.nodes.GetByRole(ctx, Controller)
	if err == nil || !errors.Is(err, errs.ErrNotFound) {
		return n, false, err
	}
	tags, _ := json.Marshal([]string{TagIngress, TagEgress, TagBuilder})
	n = store.Node{
		ID:        uuid.NewString(),
		Name:      "controller",
		Slug:      "controller",
		Role:      Controller,
		Tags:      string(tags),
		RangeCIDR: cluster.ControllerRange,
		Status:    StatusOnline,
		CreatedAt: time.Now().UTC(),
	}
	if err = l.nodes.Create(ctx, n); err == nil {
		return n, true, nil
	}
	if n, gerr := l.nodes.GetByRole(ctx, Controller); gerr == nil {
		return n, false, nil
	}
	return store.Node{}, false, err
}

// Controller is the controller row, made first when missing (a database the
// boot path has not seen, as in a test).
func (l *Leaf) Controller(ctx context.Context) (store.Node, error) {
	n, _, err := l.Bootstrap(ctx)
	return n, err
}

// Tags are the node's capability tags.
func Tags(n store.Node) []string {
	var t []string
	_ = json.Unmarshal([]byte(n.Tags), &t)
	return t
}

// Reserve is the first /24 of the node's range that overlaps nothing in used
// nor a block reserved a moment ago. The caller gathers used (the node's env
// subnets, Docker's networks and the host interfaces) and writes the block
// down (envs.subnet) or creates the network with it before the hold lapses.
func (l *Leaf) Reserve(ctx context.Context, nodeID string, used []netip.Prefix) (netip.Prefix, error) {
	n, err := l.nodes.Get(ctx, nodeID)
	if err != nil {
		return netip.Prefix{}, err
	}
	rng, err := netip.ParsePrefix(n.RangeCIDR)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("node %s: range %q: %w", n.Name, n.RangeCIDR, err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	live := l.held[nodeID][:0]
	for _, h := range l.held[nodeID] {
		if now.Sub(h.at) < holdFor {
			live = append(live, h)
			used = append(used, h.p)
		}
	}
	p, ok := docker.FirstFree(rng, used)
	if !ok {
		l.held[nodeID] = live
		return netip.Prefix{}, errs.Conflictf(
			"No free network space in %s on node %s for a new network; remove an environment, or move the host off that range.",
			rng, n.Name)
	}
	l.held[nodeID] = append(live, hold{p, now})
	return p, nil
}
