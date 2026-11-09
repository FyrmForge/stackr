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
	"slices"
	"sync"
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
	// Removing is a replica being taken away: its row stays until its
	// network is gone. It may still have a container, so it is not stopped.
	Removing = "removing"

	// A replica's observed phase (State is what the panel wants): Starting
	// until it reports in recovery and streaming, Streaming after, Rebuild
	// once its slot was lost and it has to be made again.
	Starting  = "starting"
	Streaming = "streaming"
	Rebuild   = "rebuild"
	// Rewind is the old primary after a failover: fenced, its data waiting to
	// be rewound onto the new primary (or copied afresh) before it follows it.
	Rewind = "rewind"

	// MaxReplicas is how many replicas an instance may have.
	MaxReplicas = 2
)

type Leaf struct {
	rows store.OperatedMemberStore
	// addMu makes AddReplica's count-then-insert atomic.
	// ponytail: one lock for every instance; per-instance if adds ever queue.
	addMu sync.Mutex
}

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
	l.addMu.Lock()
	defer l.addMu.Unlock()
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
		State: Running, Phase: Starting, SyncMode: "auto", CreatedAt: now, UpdatedAt: now,
	}
	return m, l.rows.Create(ctx, m)
}

// SetPhase records what the panel observes of a replica.
func (l *Leaf) SetPhase(ctx context.Context, m store.OperatedMember, phase string) (store.OperatedMember, error) {
	if m.Phase == phase {
		return m, nil
	}
	m.Phase = phase
	return l.save(ctx, m)
}

// SetRouted records the read pool membership the consumers were deployed with.
func (l *Leaf) SetRouted(ctx context.Context, m store.OperatedMember, routed bool) (store.OperatedMember, error) {
	if m.Routed == routed {
		return m, nil
	}
	m.Routed = routed
	return l.save(ctx, m)
}

// SetSync records a replica's sync mode and whether the primary waits for it.
func (l *Leaf) SetSync(ctx context.Context, m store.OperatedMember, mode string, in bool) (store.OperatedMember, error) {
	if m.SyncMode == mode && m.SyncIn == in {
		return m, nil
	}
	m.SyncMode, m.SyncIn = mode, in
	return l.save(ctx, m)
}

// InReadPool says whether reads may go to replica m: it wants to run, is not
// fenced, and is streaming. A standby that is stopped, past its slot cap
// (rebuild) or not yet caught up is out; a lagging one stays in.
func InReadPool(m store.OperatedMember) bool {
	return m.Role == Replica && m.State == Running && !m.Fenced && m.Phase == Streaming
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
	if state != Running && state != Stopped && state != Removing {
		return m, errs.Invalidf("state", "a member runs, is stopped or is being removed")
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

// HasReplica reports whether any of ms is a replica, whatever its state.
func HasReplica(ms []store.OperatedMember) bool {
	return slices.ContainsFunc(ms, func(m store.OperatedMember) bool { return m.Role == Replica })
}

// LiveReplica is a replica that may be running: anything but positively
// stopped. Fenced says nothing (a fence is only the panel refusing to start
// it; the container may be up), and its node cannot be asked (it may be
// down), so the rows are the answer.
func (l *Leaf) LiveReplica(ctx context.Context, instanceID string) (store.OperatedMember, bool, error) {
	ms, err := l.rows.ListByInstance(ctx, instanceID)
	if err != nil {
		return store.OperatedMember{}, false, err
	}
	for _, m := range ms {
		if m.Role == Replica && m.State != Stopped {
			return m, true, nil
		}
	}
	return store.OperatedMember{}, false, nil
}
