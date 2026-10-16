package node

import (
	"context"
	"encoding/json"
	"net/netip"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/mesh"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Statuses of a node that is joining. A pending node has asked to join and
// has no transit address yet. range_cidr is a leftover of the /16-per-node
// plan: it holds a unique placeholder, nothing reads it as a range.
// Anything that walks the nodes (router, mesh) must skip StatusPending.
const (
	StatusPending = "pending"
	StatusJoined  = "joined" // confirmed, cert issued, not yet seen online
)

// CreatePending makes the row for a worker that asked to join.
func (l *Leaf) CreatePending(ctx context.Context, name string, tags []string, wgKey string) (store.Node, error) {
	t, _ := json.Marshal(tags)
	id := uuid.NewString()
	n := store.Node{
		ID: id, Name: name, Slug: name, Role: Worker, Tags: string(t),
		RangeCIDR: "pending:" + id, WGPublicKey: wgKey, Status: StatusPending,
		CreatedAt: time.Now().UTC(),
	}
	if err := l.nodes.Create(ctx, n); err != nil {
		if _, ok := errs.IsConflict(err); ok {
			return n, errs.Conflictf("A node named %q already exists.", name)
		}
		return n, err
	}
	return n, nil
}

// CreatePendingOrRejoin is CreatePending, except a worker already in the
// table in state rejoin (a re-issued join command) goes back to pending with
// the new WireGuard key and keeps its id, range and address. fresh is
// true for a new row.
func (l *Leaf) CreatePendingOrRejoin(ctx context.Context, name string, tags []string, wgKey, rejoin string) (n store.Node, fresh bool, err error) {
	if n, err := l.nodes.GetBySlug(ctx, name); err == nil && n.Status == rejoin {
		n.Status, n.WGPublicKey = StatusPending, wgKey
		return n, false, l.nodes.Update(ctx, n)
	}
	n, err = l.CreatePending(ctx, name, tags, wgKey)
	return n, true, err
}

// Confirm gives a pending node the lowest free transit address; a rejoin
// keeps the one it had. ifaces are the node's reported interface prefixes:
// one on the cluster's reserved ranges is refused, its networks would clash
// with the cluster's.
func (l *Leaf) Confirm(ctx context.Context, id string, ifaces []netip.Prefix) (store.Node, error) {
	n, err := l.nodes.Get(ctx, id)
	if err != nil {
		return n, err
	}
	if n.Status != StatusPending {
		return n, errs.ErrNotFound
	}
	for _, p := range ifaces {
		if p.Overlaps(mesh.Supernet) || p.Overlaps(mesh.Transit) {
			return n, errs.Conflictf("Node %s has a network (%s) on the cluster's reserved range %s or %s; move it off first.",
				n.Name, p, mesh.Supernet, mesh.Transit)
		}
	}
	n.RangeCIDR, n.Status = n.ID, StatusJoined
	if a, err := netip.ParseAddr(n.WGAddress); err == nil && mesh.Transit.Contains(a) {
		return n, l.nodes.Update(ctx, n)
	}
	all, err := l.nodes.List(ctx)
	if err != nil {
		return n, err
	}
	var taken []netip.Addr
	for _, o := range all {
		if a, err := netip.ParseAddr(o.WGAddress); err == nil {
			taken = append(taken, a)
		}
	}
	addr, err := mesh.AllocTransit(taken)
	if err != nil {
		return n, errs.Conflictf("The cluster has no free transit address left for a new node.")
	}
	n.WGAddress = addr.String()
	return n, l.nodes.Update(ctx, n)
}

// Reject deletes a pending node. A pending rejoin (it has its transit
// address from before) goes back to state rejoin instead, keeping its name,
// address and envs for the next join command. ErrNotFound when the node is
// not pending.
func (l *Leaf) Reject(ctx context.Context, id, rejoin string) (store.Node, error) {
	n, err := l.nodes.Get(ctx, id)
	if err != nil {
		return n, err
	}
	if n.Status != StatusPending {
		return n, errs.ErrNotFound
	}
	if n.WGAddress != "" {
		n.Status = rejoin
		return n, l.nodes.Update(ctx, n)
	}
	return n, l.nodes.Delete(ctx, id)
}
