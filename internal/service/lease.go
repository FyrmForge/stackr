package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
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

// leaseRec is the panel's proof of a binding: the worker on Node took a push
// that bound Containers, the primary of instance Inst, at Ack. It is kept per
// instance and node ("pglease.<inst>.<node>") and outlives the node's row: a
// forgotten node's last binding is its fence (the deadline is Ack + the lease
// + a margin), and a promote needs it to know the old primary's exact
// container was under a lease. Dropped only once the worker took an empty
// list for that instance, which is also how a restarted panel knows which
// workers to clear.
// ponytail: written at every push (a few small rows every leaseEvery); a
// write on change plus a safety margin would do if that ever shows.
type leaseRec struct {
	Containers []string  `json:"containers"`
	Ack        time.Time `json:"ack"`
}

const leasePrefix = "pglease."

func leaseKey(inst, node string) string { return leasePrefix + inst + "." + node }

func (o *Orchestrator) leaseRecs(ctx context.Context) (map[string]leaseRec, error) {
	rows, err := o.store.Settings.ListPrefix(ctx, leasePrefix)
	if err != nil {
		return nil, err
	}
	out := map[string]leaseRec{}
	for k, v := range rows {
		var r leaseRec
		if json.Unmarshal([]byte(v), &r) == nil {
			out[strings.TrimPrefix(k, leasePrefix)] = r
		}
	}
	return out, nil
}

// leaseRecOf is the proof for the instance's primary on node, if any.
func (o *Orchestrator) leaseRecOf(ctx context.Context, inst, node string) (leaseRec, bool) {
	v, ok, err := o.store.Settings.Get(ctx, leaseKey(inst, node))
	if err != nil || !ok {
		return leaseRec{}, false
	}
	var r leaseRec
	return r, json.Unmarshal([]byte(v), &r) == nil && len(r.Containers) > 0
}

// leaseWanted is what every worker's lease should bind.
type leaseWanted struct {
	byNode map[string][]string            // node -> containers, all instances
	byInst map[string]map[string][]string // node -> instance -> containers
	hold   map[string]bool                // nodes not to touch: a primary there that cannot be listed
	err    error                          // something could not be read: push nothing
}

// leaseContainers are the containers each worker's lease binds: the running
// primaries, of instances that have a standby row, on that node.
func (o *Orchestrator) leaseContainers(ctx context.Context) map[string][]string {
	return o.leaseWanted(ctx).byNode
}

func (o *Orchestrator) leaseWanted(ctx context.Context) leaseWanted {
	w := leaseWanted{byNode: map[string][]string{}, byInst: map[string]map[string][]string{}, hold: map[string]bool{}}
	all, err := o.members.All(ctx)
	if err != nil {
		w.err = err
		return w
	}
	seen := map[string]bool{}
	for _, m := range all {
		if m.Role != opmember.Replica || m.State == opmember.Removing || seen[m.InstanceID] {
			continue
		}
		seen[m.InstanceID] = true
		inst, err := o.operated.Get(ctx, m.InstanceID)
		if err != nil {
			w.err = err
			continue
		}
		t, err := o.tiles.Get(ctx, inst.TileID)
		if err != nil {
			w.err = err
			continue
		}
		pn, err := o.primaryNode(ctx, t)
		if err != nil {
			w.err = err
			continue
		}
		n, err := o.store.Nodes.Get(ctx, pn)
		if err != nil || n.Role != node.Worker {
			continue // the controller's own node needs none; a forgotten one cannot be pushed to
		}
		if n.Status != node.StatusOnline {
			w.hold[pn] = true // never push a shorter list to a node we cannot read
			continue
		}
		cs, err := o.docker.List(docker.WithNode(ctx, pn), map[string]string{tile.LabelTile: t.ID, tile.LabelRole: "replica"})
		if err != nil {
			w.err = err
			continue
		}
		for _, c := range cs {
			if c.State == "running" || c.State == "restarting" {
				w.byNode[pn] = append(w.byNode[pn], c.ID)
				if w.byInst[pn] == nil {
					w.byInst[pn] = map[string][]string{}
				}
				w.byInst[pn][inst.ID] = append(w.byInst[pn][inst.ID], c.ID)
			}
		}
	}
	return w
}

// pushLeases is the tick: every worker with bound primaries gets the list; a
// worker that had some (the persisted records say so, a restart of the panel
// does not forget) and has none now gets an explicit empty one. A collection
// that failed pushes nothing: the workers keep what they were last given.
func (o *Orchestrator) pushLeases(ctx context.Context) {
	w := o.leaseWanted(ctx)
	if w.err != nil {
		slog.Warn("lease: not pushed, the primaries could not all be read", "err", w.err)
		return
	}
	recs, err := o.leaseRecs(ctx)
	if err != nil {
		return
	}
	var nodes []string
	for k := range recs {
		if _, n, ok := strings.Cut(k, "."); ok && !slices.Contains(nodes, n) {
			nodes = append(nodes, n)
		}
	}
	for n := range w.byNode {
		if !slices.Contains(nodes, n) {
			nodes = append(nodes, n)
		}
	}
	for _, id := range nodes {
		if w.hold[id] {
			continue
		}
		if err := o.pushNode(ctx, id, w); err != nil {
			slog.Debug("lease: not pushed", "node", id, "err", err)
		}
	}
}

// pushNode puts node's list and, once the worker took it, records what it
// bound. The records of instances no longer bound there go.
func (o *Orchestrator) pushNode(ctx context.Context, nodeID string, w leaseWanted) error {
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := o.pushLease(pctx, nodeID, worker.Lease{Seconds: leaseSeconds, Containers: w.byNode[nodeID]}); err != nil {
		return err
	}
	now := time.Now()
	for inst, ids := range w.byInst[nodeID] {
		b, _ := json.Marshal(leaseRec{Containers: ids, Ack: now})
		if err := o.store.Settings.Set(ctx, leaseKey(inst, nodeID), string(b)); err != nil {
			return err
		}
	}
	recs, err := o.leaseRecs(ctx)
	if err != nil {
		return err
	}
	for k := range recs {
		if inst, n, ok := strings.Cut(k, "."); ok && n == nodeID {
			if _, bound := w.byInst[nodeID][inst]; !bound {
				if err := o.store.Settings.Delete(ctx, leasePrefix+k); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// syncLease pushes the lease of the instance's primary node and requires the
// worker's ack: the primary must never run writable without its lease. Used
// where a primary gains a standby, is replaced, or a failover completes. The
// controller's own node needs none.
func (o *Orchestrator) syncLease(ctx context.Context, t store.Tile) error {
	pn, err := o.primaryNode(ctx, t)
	if err != nil {
		return err
	}
	if n, err := o.store.Nodes.Get(ctx, pn); err != nil || n.Role != node.Worker {
		return nil
	}
	w := o.leaseWanted(ctx)
	if w.err != nil {
		return fmt.Errorf("lease: %w", w.err)
	}
	if w.hold[pn] {
		return fmt.Errorf("lease: the primary's node is not online")
	}
	if err := o.pushNode(ctx, pn, w); err != nil {
		return fmt.Errorf("the worker did not take the primary's lease: %w", err)
	}
	return nil
}

// leaseOnPrimaryUp is the Early hook's half: a container of an instance tile
// that has standbys is up. Its lease is pushed and acked now; failing that it
// is stopped, never left writable without one.
func (o *Orchestrator) leaseOnPrimaryUp(ctx context.Context, t store.Tile, id string) {
	if t.Kind != tile.Operated {
		return
	}
	inst, err := o.operated.GetByTile(ctx, t.ID)
	if err != nil {
		return
	}
	ms, err := o.members.List(ctx, inst.ID)
	if err != nil || !opmember.HasReplica(ms) {
		return
	}
	if err := o.syncLease(context.WithoutCancel(ctx), t); err != nil {
		slog.Error("lease: the primary has no lease, stopping it", "tile", t.ID, "err", err)
		if pn, perr := o.primaryNode(ctx, t); perr == nil {
			_ = o.docker.Stop(docker.WithNode(ctx, pn), id)
		}
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
// took, recorded for any instance and surviving the node's row, or what the
// node monitor last saw), plus a margin; 0 = now.
func (o *Orchestrator) leaseWait(ctx context.Context, nodeID string) time.Duration {
	last := o.lastSeen(nodeID)
	recs, _ := o.leaseRecs(ctx)
	for k, r := range recs {
		if _, n, ok := strings.Cut(k, "."); ok && n == nodeID && r.Ack.After(last) {
			last = r.Ack
		}
	}
	if last.IsZero() {
		return 0
	}
	return max(time.Until(last.Add(leaseSeconds*time.Second+leaseMargin)), 0)
}
