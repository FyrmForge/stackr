package service

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/worker"
)

// The worker lease (worker/lease.go), the panel's half. A postgres primary
// that has a standby is bound to the panel's contact: its worker stops it if
// the panel is silent for leaseSeconds. The panel tells each worker which
// containers are bound every leaseEvery (a call that is also the contact), and
// before it promotes a standby of a primary on a node it cannot reach it waits
// until the worker's lease on that node has certainly run out (leaseWait).
// Instances without a standby are never bound. The controller's own node needs
// none: the panel runs there.
// ponytail: one length for every instance; a setting if a deployment needs
// another.
const (
	leaseSeconds = 30
	leaseEvery   = 10 * time.Second
)

// leaseState is what the panel remembers of its leases.
type leaseBook struct {
	mu     sync.Mutex
	leased map[string]bool      // nodes the last push bound containers on
	ackAt  map[string]time.Time // when each node last took a push
}

func (b *leaseBook) acked(node string, at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ackAt == nil {
		b.ackAt = map[string]time.Time{}
	}
	b.ackAt[node] = at
}

// leaseContainers are the containers each worker's lease binds: the running
// primaries, of instances that have a standby row, on that node.
func (o *Orchestrator) leaseContainers(ctx context.Context) map[string][]string {
	out := map[string][]string{}
	all, err := o.members.All(ctx)
	if err != nil {
		return out
	}
	seen := map[string]bool{}
	for _, m := range all {
		if m.Role != opmember.Replica || m.State == opmember.Removing || seen[m.InstanceID] {
			continue
		}
		seen[m.InstanceID] = true
		inst, err := o.operated.Get(ctx, m.InstanceID)
		if err != nil {
			continue
		}
		t, err := o.tiles.Get(ctx, inst.TileID)
		if err != nil {
			continue
		}
		pn, err := o.primaryNode(ctx, t)
		if err != nil {
			continue
		}
		if n, err := o.store.Nodes.Get(ctx, pn); err != nil || n.Role != node.Worker || n.Status != node.StatusOnline {
			continue
		}
		cs, err := o.docker.List(docker.WithNode(ctx, pn), map[string]string{tile.LabelTile: t.ID, tile.LabelRole: "replica"})
		if err != nil {
			continue
		}
		for _, c := range cs {
			if c.State == "running" || c.State == "restarting" {
				out[pn] = append(out[pn], c.ID)
			}
		}
	}
	return out
}

// pushLeases is the tick: every worker with bound primaries gets the list; one
// that had some and has none now gets an empty one, which clears its lease.
func (o *Orchestrator) pushLeases(ctx context.Context) {
	want := o.leaseContainers(ctx)
	o.lease.mu.Lock()
	var nodes []string
	for n := range o.lease.leased {
		nodes = append(nodes, n)
	}
	o.lease.mu.Unlock()
	for n := range want {
		if !slices.Contains(nodes, n) {
			nodes = append(nodes, n)
		}
	}
	for _, id := range nodes {
		ids := want[id]
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := o.pushLease(pctx, id, worker.Lease{Seconds: leaseSeconds, Containers: ids})
		cancel()
		if err != nil {
			slog.Debug("lease: not pushed", "node", id, "err", err)
			continue
		}
		o.lease.acked(id, time.Now())
		o.lease.mu.Lock()
		if o.lease.leased == nil {
			o.lease.leased = map[string]bool{}
		}
		if len(ids) > 0 {
			o.lease.leased[id] = true
		} else {
			delete(o.lease.leased, id)
		}
		o.lease.mu.Unlock()
	}
}

// pushLease is one PUT to a worker; tests swap leasePush.
func (o *Orchestrator) pushLease(ctx context.Context, nodeID string, l worker.Lease) error {
	if o.leasePush != nil {
		return o.leasePush(ctx, nodeID, l)
	}
	c, err := o.nodeExtras(nodeID)
	if err != nil {
		return err
	}
	return c.PutLease(ctx, l)
}

// leaseWait is how long until the lease of the worker on node has run out
// for certain, counted from the last time the panel reached it (a push it
// took, or what the node monitor last saw), plus a margin; 0 = now. A node the
// panel never reached has nothing to wait for.
func (o *Orchestrator) leaseWait(nodeID string) time.Duration {
	o.lease.mu.Lock()
	last := o.lease.ackAt[nodeID]
	o.lease.mu.Unlock()
	if seen := o.lastSeen(nodeID); seen.After(last) {
		last = seen
	}
	if last.IsZero() {
		return 0
	}
	return max(time.Until(last.Add(leaseSeconds*time.Second+leaseMargin)), 0)
}
