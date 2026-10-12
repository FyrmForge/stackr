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
// has no range yet; its range_cidr holds a unique placeholder until confirm.
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

// Confirm gives a pending node the lowest free /16 of the supernet, avoiding
// every range on a node row and every interface prefix in ifaces, and its
// transit address. ErrNotFound when the node is not pending.
// ponytail: ranges of forgotten nodes are not kept, so a deleted node's range
// can come back; keep a ranges-ever-issued list when forget node lands.
func (l *Leaf) Confirm(ctx context.Context, id string, ifaces []netip.Prefix) (store.Node, error) {
	n, err := l.nodes.Get(ctx, id)
	if err != nil {
		return n, err
	}
	if n.Status != StatusPending {
		return n, errs.ErrNotFound
	}
	all, err := l.nodes.List(ctx)
	if err != nil {
		return n, err
	}
	var taken []netip.Prefix
	for _, o := range all {
		if p, err := netip.ParsePrefix(o.RangeCIDR); err == nil {
			taken = append(taken, p)
		}
	}
	rng, err := mesh.AllocateRange(taken, ifaces)
	if err != nil {
		return n, errs.Conflictf("The cluster has no free network range left for a new node.")
	}
	addr, err := mesh.TransitAddr(rng)
	if err != nil {
		return n, err
	}
	n.RangeCIDR, n.WGAddress, n.Status = rng.String(), addr.String(), StatusJoined
	return n, l.nodes.Update(ctx, n)
}

// Reject deletes a pending node. ErrNotFound when the node is not pending.
func (l *Leaf) Reject(ctx context.Context, id string) (store.Node, error) {
	n, err := l.nodes.Get(ctx, id)
	if err != nil {
		return n, err
	}
	if n.Status != StatusPending {
		return n, errs.ErrNotFound
	}
	return n, l.nodes.Delete(ctx, id)
}
