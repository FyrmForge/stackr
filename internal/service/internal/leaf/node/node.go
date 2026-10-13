// Package node owns the nodes table: the machines stackr runs envs on, their
// roles, tags and address ranges. "The head" in code is the controller row,
// never "this process". It also hands out the /24 blocks of a node's range
// (Reserve) so no Docker daemon picks a subnet on its own.
package node

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/service/errs"
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
// env row is not yet visible to the next reservation (the node's facts, which
// report its bridges, come in every 20 s).
const holdFor = time.Minute

// Leaf is the node leaf. One instance per process: it remembers the blocks it
// just handed out.
type Leaf struct {
	nodes store.NodeStore
	// Taken are the cluster's blocks in use beyond what a reservation's
	// caller knows of its own node: every env's network and every node's
	// reported interfaces (its shared and ingress bridges). Nil = none.
	Taken func(ctx context.Context) ([]netip.Prefix, error)

	mu   sync.Mutex
	held []hold // blocks reserved a moment ago, on any node
}

type hold struct {
	p  netip.Prefix
	at time.Time
}

func New(nodes store.NodeStore) *Leaf {
	return &Leaf{nodes: nodes}
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
		RangeCIDR: "controller",
		WGAddress: mesh.ControllerTransit.String(),
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

// Reserve is the first /24 of the cluster pool that overlaps nothing in used,
// nothing Taken reports and no block reserved a moment ago on any node: one
// pool for the whole cluster, so a network is unique across nodes and an env
// keeps its network when it moves. The caller gathers used (its node's env
// subnets, Docker's networks and the host interfaces) and writes the block
// down (envs.subnet) or creates the network with it before the hold lapses.
// ponytail: a shared or ingress bridge another node made after the panel
// restarted is seen once that node's facts are in (20 s); keep a blocks table
// if that window ever bites.
func (l *Leaf) Reserve(ctx context.Context, nodeID string, used []netip.Prefix) (netip.Prefix, error) {
	if l.Taken != nil {
		t, err := l.Taken(ctx)
		if err != nil {
			return netip.Prefix{}, err
		}
		used = append(used, t...)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	live := l.held[:0]
	for _, h := range l.held {
		if now.Sub(h.at) < holdFor {
			live = append(live, h)
			used = append(used, h.p)
		}
	}
	p, ok := docker.FirstFree(mesh.Supernet, used)
	if !ok {
		l.held = live
		return netip.Prefix{}, errs.Conflictf(
			"The cluster's network pool %s is full; remove an environment to free one.", mesh.Supernet)
	}
	l.held = append(live, hold{p, now})
	return p, nil
}
