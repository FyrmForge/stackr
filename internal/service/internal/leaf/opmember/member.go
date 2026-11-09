// Package opmember owns the members of an operated instance (operated_members):
// the primary, which is the instance tile's own container, and up to
// MaxReplicas replicas, each with its own container, data volume and node.
// Only operated instances get this exception to "the env is the unit". The
// leaf holds the rows and the rules (R4: a replica never shares a node with
// another member); the world objects (network, volume, container) are
// sequenced by the orchestrator.
package opmember

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

const (
	Primary = "primary"
	Replica = "replica"

	Running = "running"
	Stopped = "stopped"

	// MaxReplicas is how many replicas an instance may have.
	MaxReplicas = 2
)

type Leaf struct{ rows store.OperatedMemberStore }

func New(rows store.OperatedMemberStore) *Leaf { return &Leaf{rows: rows} }

func (l *Leaf) List(ctx context.Context, instanceID string) ([]store.OperatedMember, error) {
	return l.rows.ListByInstance(ctx, instanceID)
}

// All is every member of every instance.
func (l *Leaf) All(ctx context.Context) ([]store.OperatedMember, error) { return l.rows.List(ctx) }

func (l *Leaf) Get(ctx context.Context, id string) (store.OperatedMember, error) {
	return l.rows.Get(ctx, id)
}

// CreatePrimary is the row the instance's own container stands for. Its node,
// container and volume follow the instance tile, so they stay empty.
func (l *Leaf) CreatePrimary(ctx context.Context, instanceID string) (store.OperatedMember, error) {
	now := time.Now().UTC()
	m := store.OperatedMember{ID: uuid.NewString(), InstanceID: instanceID, Role: Primary, State: Running, CreatedAt: now, UpdatedAt: now}
	return m, l.rows.Create(ctx, m)
}

// PrimaryOf is the primary among members; ok is false for an instance made
// before members existed and never backfilled.
func PrimaryOf(ms []store.OperatedMember) (store.OperatedMember, bool) {
	for _, m := range ms {
		if m.Role == Primary {
			return m, true
		}
	}
	return store.OperatedMember{}, false
}

// Check is R4: node, a node other than primaryNode and every other member's,
// may take a replica; an instance holds at most MaxReplicas of them.
func Check(primaryNode, node string, ms []store.OperatedMember) error {
	replicas := 0
	switch {
	case node == "":
		return errs.Invalidf("node", "Pick a node for the standby.")
	case node == primaryNode:
		return errs.Conflictf("A standby must run on a different node than the primary.")
	}
	for _, m := range ms {
		if m.Role == Replica {
			replicas++
		}
		if m.NodeID == node {
			return errs.Conflictf("This node already holds a member of the instance.")
		}
	}
	if replicas >= MaxReplicas {
		return errs.Conflictf("An instance has at most %d standbys.", MaxReplicas)
	}
	return nil
}

// AddReplica is the row of a replica on node, refused by Check. It names the
// container and the data volume, which the caller makes.
func (l *Leaf) AddReplica(ctx context.Context, instanceID, primaryNode, node string) (store.OperatedMember, error) {
	ms, err := l.rows.ListByInstance(ctx, instanceID)
	if err != nil {
		return store.OperatedMember{}, err
	}
	if err := Check(primaryNode, node, ms); err != nil {
		return store.OperatedMember{}, err
	}
	id := uuid.NewString()
	now := time.Now().UTC()
	m := store.OperatedMember{
		ID: id, InstanceID: instanceID, Role: Replica, NodeID: node,
		Container: "stackr-member-" + id[:8], Volume: "stackr-member-" + id + "-data",
		State: Running, CreatedAt: now, UpdatedAt: now,
	}
	return m, l.rows.Create(ctx, m)
}

func (l *Leaf) save(ctx context.Context, m store.OperatedMember) (store.OperatedMember, error) {
	m.UpdatedAt = time.Now().UTC()
	return m, l.rows.Update(ctx, m)
}

func (l *Leaf) SetSubnet(ctx context.Context, m store.OperatedMember, subnet string) (store.OperatedMember, error) {
	m.Subnet = subnet
	return l.save(ctx, m)
}

// SetState records what the panel wants of the member: Running or Stopped.
func (l *Leaf) SetState(ctx context.Context, m store.OperatedMember, state string) (store.OperatedMember, error) {
	if state != Running && state != Stopped {
		return m, errs.Invalidf("state", "a member runs or is stopped")
	}
	m.State = state
	return l.save(ctx, m)
}

// SetFenced persists the fence. It is written before anything acts on it.
func (l *Leaf) SetFenced(ctx context.Context, m store.OperatedMember, fenced bool) (store.OperatedMember, error) {
	m.Fenced = fenced
	return l.save(ctx, m)
}

func (l *Leaf) Delete(ctx context.Context, m store.OperatedMember) error {
	if m.Role == Primary {
		return errs.Conflictf("The primary goes with its instance.")
	}
	return l.rows.Delete(ctx, m.ID)
}

// MayStart refuses starting a fenced member: it stays down until it rejoins.
func MayStart(m store.OperatedMember) error {
	if m.Fenced {
		return errs.Conflictf("This member is fenced: it does not start until it rejoins.")
	}
	return nil
}

// FencedPrimary reports whether the instance's primary is fenced.
func (l *Leaf) FencedPrimary(ctx context.Context, instanceID string) (bool, error) {
	ms, err := l.rows.ListByInstance(ctx, instanceID)
	if err != nil {
		return false, err
	}
	p, ok := PrimaryOf(ms)
	return ok && p.Fenced, nil
}

// LiveReplica is a replica that may be running: not fenced and wanted up.
// Its node cannot be asked (it may be down), so the rows are the answer.
func (l *Leaf) LiveReplica(ctx context.Context, instanceID string) (store.OperatedMember, bool, error) {
	ms, err := l.rows.ListByInstance(ctx, instanceID)
	if err != nil {
		return store.OperatedMember{}, false, err
	}
	for _, m := range ms {
		if m.Role == Replica && !m.Fenced && m.State == Running {
			return m, true, nil
		}
	}
	return store.OperatedMember{}, false, nil
}
