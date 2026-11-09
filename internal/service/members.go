package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/vip"
)

// Members of an operated postgres instance.
//
// The primary is the instance tile's own container: its node is its env's, so
// its member row holds no node. A replica is the one place an instance leaves
// its env: it lives on another node, with its own data volume (a plain Docker
// volume named on the row, outside volumes: no backups, no orphan sweep).
//
// How a replica on node B reaches env A's instance. Env networks exist on one
// node only; the instance's network (operated.Network, "stackr-operated-<id>")
// is a Shared one, so node B makes a network of the same name from its own
// range (a /24 of 10.208.0.0/12, no overlap with A's). The names match, the
// networks are not joined: a replica attaches to B's, and reaches the primary
// the way a consumer on another node does. The primary's VIP (its pause
// address on A) is an ExtraHosts line "alias:vip" (memberHost), and the
// firewall opens replica network -> VIP:5432 (memberFlows: Out on B, an
// accept on A), both generated from the member rows. The replica's container
// (flow/operated StandbySpec) sits on that network with that host line, and
// the VIP is primary_conninfo's host (the primary's alias).
//
// A replica's phase is what the watch tick observes of it: starting until it
// is in recovery with its WAL receiver streaming, then streaming (in the read
// pool: consumers on its node reach it through the read alias), rebuild once
// its slot is lost.

// LabelMember marks a replica member's containers.
const LabelMember = mflow.LabelMember

// InstanceMember is a row of operated_members.
type InstanceMember = store.OperatedMember

// memberInstance is the postgres instance of operated tile tileID.
func (o *Orchestrator) memberInstance(ctx context.Context, tileID string) (store.Tile, store.OperatedInstance, error) {
	t, err := o.tiles.Get(ctx, tileID)
	if err != nil {
		return t, store.OperatedInstance{}, err
	}
	m, err := o.operated.GetByTile(ctx, t.ID)
	if err != nil {
		return t, m, err
	}
	if m.Engine != "postgres" {
		return t, m, errs.Invalidf("engine", "only postgres has standbys.")
	}
	return t, m, nil
}

// primaryNode is the node of the instance's primary member: the instance
// tile's env's, or the node a failover moved the primary to (failover.go).
func (o *Orchestrator) primaryNode(ctx context.Context, t store.Tile) (string, error) {
	if p, ok := o.relocated(ctx, t.ID); ok {
		return p.NodeID, nil
	}
	return o.envNode(ctx, t.EnvironmentID)
}

// InstanceMembers lists the instance's members, the primary first.
func (o *Orchestrator) InstanceMembers(ctx context.Context, tileID string) ([]InstanceMember, error) {
	_, m, err := o.memberInstance(ctx, tileID)
	if err != nil {
		return nil, err
	}
	ms, err := o.members.List(ctx, m.ID)
	if err != nil {
		return nil, err
	}
	p, _ := opmember.PrimaryOf(ms)
	out := []InstanceMember{}
	if p.ID != "" {
		out = append(out, p)
	}
	for _, x := range ms {
		if x.Role == opmember.Replica {
			out = append(out, x)
		}
	}
	return out, nil
}

// topoLock serialises changes to one instance's members (add, remove,
// rebuild, fence, the stack file's standbys list, a removal the tick
// finishes): each reads the rows again once it holds the lock.
func (o *Orchestrator) topoLock(instanceID string) *sync.Mutex {
	m, _ := o.repoLocks.LoadOrStore("pg topology "+instanceID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// lockInstance takes tile tileID's instance's topology lock.
func (o *Orchestrator) lockInstance(ctx context.Context, tileID string) (func(), error) {
	_, inst, err := o.memberInstance(ctx, tileID)
	if err != nil {
		return nil, err
	}
	mu := o.topoLock(inst.ID)
	mu.Lock()
	return mu.Unlock, nil
}

// AddStandby makes the instance a replica member on node (a name, slug or
// id): its row (refused when the node already holds a member, R4), its
// network there and its data volume, the primary's side of the replication
// (role, slot) and the standby's container, whose first start copies the
// primary. It is a standby once it reports streaming (the watch tick).
func (o *Orchestrator) AddStandby(ctx context.Context, tileID, nodeRef string) (InstanceMember, error) {
	unlock, err := o.lockInstance(ctx, tileID)
	if err != nil {
		return InstanceMember{}, err
	}
	defer unlock()
	return o.addStandby(ctx, tileID, nodeRef)
}

// addStandby is AddStandby with the topology lock held.
func (o *Orchestrator) addStandby(ctx context.Context, tileID, nodeRef string) (InstanceMember, error) {
	t, inst, err := o.memberInstance(ctx, tileID)
	if err != nil {
		return InstanceMember{}, err
	}
	n, err := o.nodeByRef(ctx, nodeRef)
	if err != nil {
		return InstanceMember{}, errs.Invalidf("node", "Pick one of the cluster's nodes.")
	}
	nodeID := n.ID
	if err := o.nodeUp(ctx, nodeID); err != nil {
		return InstanceMember{}, err
	}
	if err := o.engines.WALBudget(ctx, t); err != nil {
		return InstanceMember{}, err
	}
	pn, err := o.primaryNode(ctx, t)
	if err != nil {
		return InstanceMember{}, err
	}
	m, err := o.members.AddReplica(ctx, inst.ID, pn, nodeID)
	if err != nil {
		return InstanceMember{}, err
	}
	nctx := docker.WithNode(ctx, nodeID)
	net := operated.Network(inst.ID)
	commit := func(p netip.Prefix) (err error) {
		m, err = o.members.SetSubnet(ctx, m, p.String())
		return err
	}
	err = o.envs.SharedBlock(nctx, net, commit)
	if err == nil && m.Subnet == "" {
		// A network of that name was already on the node (a leftover): its
		// block is not on the row. Nothing is on it, so make it again.
		if err = o.docker.RemoveNetwork(nctx, net); err == nil {
			err = o.envs.SharedBlock(nctx, net, commit)
		}
	}
	if err == nil {
		err = o.docker.CreateVolume(nctx, m.Volume, "local", nil, map[string]string{LabelMember: m.ID})
	}
	if err == nil {
		// the primary's half: role, pg_hba lines, slot cap and this standby's slot
		err = o.engines.PrepareStandby(ctx, t, mflow.SlotName(m.ID), o.replSubnets(ctx, inst.ID, ""))
	}
	if err == nil {
		err = o.primaryRestart(ctx, t, true)
	}
	if err == nil {
		err = o.syncLease(ctx, t) // a primary with a standby runs under its worker's lease, acked
	}
	if err != nil {
		// kept as 'removing' if the drop fails: the watch tick finishes it
		bg := context.WithoutCancel(ctx)
		if x, serr := o.members.SetState(bg, m, opmember.Removing); serr == nil {
			m = x
		}
		_ = o.dropMember(bg, m)
		return InstanceMember{}, err
	}
	o.rerouteVIPsAsync(ctx) // the replica's flow
	// The container is made now (its first copy runs inside it); a failure
	// here is retried by the watch tick, which starts what is missing.
	if err := o.startMember(ctx, m); err != nil {
		slog.Warn("members: standby container", "member", m.ID, "err", err)
	}
	return m, nil
}

// RebuildStandby makes standby memberID again on its node: removed, then
// added, for one whose slot was lost. The old row has to be gone before the
// new one (R4), so a removal that cannot finish now is reported.
func (o *Orchestrator) RebuildStandby(ctx context.Context, memberID string) (InstanceMember, error) {
	m, err := o.members.Get(ctx, memberID)
	if err != nil {
		return InstanceMember{}, err
	}
	if m.Role != opmember.Replica {
		return InstanceMember{}, errs.Conflictf("The primary has no rebuild.")
	}
	inst, err := o.operated.Get(ctx, m.InstanceID)
	if err != nil {
		return InstanceMember{}, err
	}
	mu := o.topoLock(inst.ID)
	mu.Lock()
	defer mu.Unlock()
	if _, err := o.members.Get(ctx, m.ID); err != nil {
		return InstanceMember{}, err // removed while this waited
	}
	if err := o.removeStandby(ctx, m.ID); err != nil {
		return InstanceMember{}, err
	}
	if _, err := o.members.Get(ctx, m.ID); err == nil {
		return InstanceMember{}, errs.Conflictf("The old standby is still being removed. Try the rebuild again in a minute.")
	}
	return o.addStandby(ctx, inst.TileID, m.NodeID)
}

// RemoveStandby takes a replica member away: its containers, volume and row,
// and its node's network when nothing else is on it.
func (o *Orchestrator) RemoveStandby(ctx context.Context, memberID string) error {
	m, err := o.members.Get(ctx, memberID)
	if err != nil {
		return err
	}
	mu := o.topoLock(m.InstanceID)
	mu.Lock()
	defer mu.Unlock()
	return o.removeStandby(ctx, memberID)
}

// removeStandby is RemoveStandby with the topology lock held; the row is
// read again.
func (o *Orchestrator) removeStandby(ctx context.Context, memberID string) error {
	m, err := o.members.Get(ctx, memberID)
	if err != nil {
		return err
	}
	if m.Role != opmember.Replica {
		return errs.Conflictf("The primary goes with its instance.")
	}
	if err := o.nodeUp(ctx, m.NodeID); err != nil {
		return err
	}
	// The row stays, as 'removing', until the slot is dropped on the primary
	// and the network is gone; the watch tick retries a failed drop.
	if m, err = o.members.SetState(ctx, m, opmember.Removing); err != nil {
		return err
	}
	if inst, err := o.operated.Get(ctx, m.InstanceID); err == nil {
		if t, err := o.tiles.Get(ctx, inst.TileID); err == nil {
			o.syncAfterTopology(ctx, t, inst.ID, false) // never wait for the slot about to go
		}
	}
	if err := o.dropMember(ctx, m); err != nil {
		return err
	}
	o.rerouteVIPsAsync(ctx)
	return nil
}

// finishRemovals retries the drop of members left 'removing'.
func (o *Orchestrator) finishRemovals(ctx context.Context) {
	all, err := o.members.All(ctx)
	if err != nil {
		slog.Warn("members: list", "err", err)
		return
	}
	for _, m := range all {
		if m.State != opmember.Removing || o.nodeUp(ctx, m.NodeID) != nil {
			continue
		}
		mu := o.topoLock(m.InstanceID)
		if !mu.TryLock() {
			continue // an add, remove or rebuild of this instance is in flight
		}
		if cur, err := o.members.Get(ctx, m.ID); err == nil {
			if err := o.dropMember(ctx, cur); err != nil {
				slog.Warn("members: removal", "member", m.ID, "err", err)
			}
		}
		mu.Unlock()
	}
}

// dropMember removes a replica's world objects then its row; the row (and its
// subnet reservation) stays while the network does. A network with containers
// of others still on it keeps the row as 'removing' until it is empty. The last replica's going
// gives the primary back its tile's restart policy first.
func (o *Orchestrator) dropMember(ctx context.Context, m InstanceMember) error {
	nctx := docker.WithNode(ctx, m.NodeID)
	cs, err := o.docker.List(nctx, map[string]string{LabelMember: m.ID})
	if err != nil {
		return err
	}
	for _, c := range cs {
		if err := o.docker.StopRemove(nctx, c.ID); err != nil {
			return err
		}
	}
	if err := o.detachStandby(ctx, m); err != nil {
		return err
	}
	if err := o.docker.RemoveVolume(nctx, m.Volume); err != nil {
		return err
	}
	_ = o.docker.RemoveVolume(nctx, mflow.MemberArchiveVolume(m)) // kept while it was a primary with PITR on
	// Consumers of the read alias are on the network until they are
	// redeployed (below); the row stays 'removing' and the tick drops the
	// network once they have left, so it is never left behind empty.
	net := operated.Network(m.InstanceID)
	busy := false
	if on, err := o.docker.NetworkMembers(nctx, net); err == nil && len(on) > 0 {
		busy = true
		slog.Warn("member: network busy, the tick retries", "member", m.ID)
	} else if err := o.envs.DropShared(nctx, net); err != nil {
		return err
	}
	ms, err := o.members.List(ctx, m.InstanceID)
	if err != nil {
		return err
	}
	if n := len(slices.DeleteFunc(slices.Clone(ms), func(x InstanceMember) bool { return x.Role != opmember.Replica || x.ID == m.ID })); n == 0 {
		inst, err := o.operated.Get(ctx, m.InstanceID)
		if err != nil {
			return err
		}
		t, err := o.tiles.Get(ctx, inst.TileID)
		if err != nil {
			return err
		}
		if err := o.primaryRestart(ctx, t, false); err != nil {
			return err
		}
	}
	// the consumers on its node still dial it until they are redeployed; the
	// row is the record that they owe it
	if err := o.routeReaders(ctx, m); err != nil {
		return err
	}
	if busy {
		return nil
	}
	if err := o.members.Delete(ctx, m); err != nil {
		return err
	}
	if !slices.ContainsFunc(ms, func(x InstanceMember) bool { return x.ID != m.ID && x.Phase == opmember.Rebuild }) {
		o.pitrProblem(ctx, "alert."+AlertPGRebuild+"."+m.InstanceID, AlertPGRebuild, "", "", "")
	}
	return nil
}

// errPrimaryDown keeps a removal 'removing': the slot (and the standby's
// replication line) are on a primary that cannot be asked.
var errPrimaryDown = errors.New("the primary is not running: the standby's slot stays until it does")

// replSubnets are the networks of the instance's standbys that are not being
// removed, except exclude's: the replication lines the primary keeps.
func (o *Orchestrator) replSubnets(ctx context.Context, instanceID, exclude string) []string {
	ms, err := o.members.List(ctx, instanceID)
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range ms {
		if m.Role == opmember.Replica && m.State != opmember.Removing && m.Subnet != "" && m.ID != exclude {
			out = append(out, m.Subnet)
		}
	}
	slices.Sort(out)
	return out
}

// detachStandby is the primary's half of a standby's removal, once the
// standby's container is gone: its slot dropped, its replication line gone
// (the last one's going closes the role). A primary that is not running
// cannot be asked: the removal is kept, and finished when it runs.
func (o *Orchestrator) detachStandby(ctx context.Context, m InstanceMember) error {
	inst, err := o.operated.Get(ctx, m.InstanceID)
	if err != nil {
		return err
	}
	t, err := o.tiles.Get(ctx, inst.TileID)
	if err != nil {
		return err
	}
	pn, err := o.primaryNode(ctx, t)
	if err != nil {
		return err
	}
	if err := o.nodeUp(ctx, pn); err != nil {
		return fmt.Errorf("%w (%v)", errPrimaryDown, err)
	}
	cs, err := o.docker.List(docker.WithNode(ctx, pn), map[string]string{tile.LabelTile: t.ID, tile.LabelRole: "replica"})
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(cs, func(c docker.Container) bool { return c.State == "running" }) {
		return errPrimaryDown
	}
	if err := o.engines.DropSlot(ctx, t, mflow.SlotName(m.ID)); err != nil {
		return err
	}
	return o.engines.SyncReplHBA(ctx, t, o.replSubnets(ctx, m.InstanceID, m.ID))
}

// noStandbyOn refuses moving env envID's instances onto node: a standby
// there would share a node with its primary (R4). Checked when the move is
// planned and again before the env's node changes.
func (o *Orchestrator) noStandbyOn(ctx context.Context, envID, node string) error {
	ts, err := o.tiles.List(ctx, envID)
	if err != nil {
		return err
	}
	for _, t := range ts {
		inst, err := o.operated.GetByTile(ctx, t.ID)
		if errors.Is(err, errs.ErrNotFound) {
			continue
		} else if err != nil {
			return err
		}
		ms, err := o.members.List(ctx, inst.ID)
		if err != nil {
			return err
		}
		if slices.ContainsFunc(ms, func(m InstanceMember) bool { return m.Role == opmember.Replica && m.NodeID == node }) {
			return errs.Conflictf("%s has a standby on %s: remove the standby first", t.Slug, o.nodeName(ctx, node))
		}
	}
	return nil
}

// noStandbys refuses work that rewrites a primary's data while the instance
// has a standby row in any state: the standby would follow a history that no
// longer exists.
func (o *Orchestrator) noStandbys(ctx context.Context, instanceID string) error {
	ms, err := o.members.List(ctx, instanceID)
	if err != nil {
		return err
	}
	if opmember.HasReplica(ms) {
		return errs.Conflictf("This instance has a standby. Remove the standby first.")
	}
	return nil
}

// primaryRestart sets the restart policy of the instance tile's container:
// "no" while it has a standby (the panel starts it, so a rebooted primary
// never comes back beside a promoted standby), else the tile's own, so a
// single database survives a reboot with the panel down. With a standby it
// also records from the container whether the primary is wanted up (a
// primary made before members was backfilled 'stopped'). A tile not
// deployed yet has no container: its deploy sets the policy.
func (o *Orchestrator) primaryRestart(ctx context.Context, t store.Tile, hasStandby bool) error {
	pn, err := o.primaryNode(ctx, t)
	if err != nil {
		return err
	}
	if err := o.nodeUp(ctx, pn); err != nil {
		return err
	}
	nctx := docker.WithNode(ctx, pn)
	cs, err := o.docker.List(nctx, map[string]string{tile.LabelTile: t.ID, tile.LabelRole: "replica"})
	if err != nil {
		return err
	}
	policy := deploy.RestartPolicy(t.RestartPolicy)
	if hasStandby {
		policy = mflow.Engines["postgres"].Definition().Restart
		up := slices.ContainsFunc(cs, func(c docker.Container) bool { return c.State == "running" || c.State == "restarting" })
		state := opmember.Stopped
		if up {
			state = opmember.Running
		}
		o.setPrimaryState(ctx, t.ID, state)
	}
	for _, c := range cs {
		if err := o.docker.UpdateRestart(nctx, c.ID, policy); err != nil {
			return err
		}
	}
	return nil
}

// FenceMember sets or clears a member's fence. The row is written first. A
// fenced member is refused at every start (the instance's deploy, the start
// verb and the panel's own member start) until the fence is cleared by its
// rejoin. Fencing a standby also stops it and takes it out of the read pool
// (the consumers on its node are redeployed, retried by the tick if that
// fails); clearing the fence does not put it back: it is in the pool again
// once the tick has seen it streaming.
func (o *Orchestrator) FenceMember(ctx context.Context, memberID string, fenced bool) error {
	m, err := o.members.Get(ctx, memberID)
	if err != nil {
		return err
	}
	mu := o.topoLock(m.InstanceID)
	mu.Lock()
	defer mu.Unlock()
	if m, err = o.members.Get(ctx, memberID); err != nil {
		return err
	}
	if m, err = o.members.SetFenced(ctx, m, fenced); err != nil || m.Role != opmember.Replica {
		return err
	}
	if fenced {
		o.stopMember(ctx, m)
	}
	if m, err = o.members.SetPhase(ctx, m, opmember.Starting); err != nil {
		return err
	}
	o.probeRuns.Delete(m.ID)
	if err := o.routeReaders(ctx, m); err != nil {
		slog.Warn("members: readers not redeployed", "member", m.ID, "err", err)
	}
	return nil
}

// Protection is the instance's protection level for its drawer.
func (o *Orchestrator) Protection(ctx context.Context, tileID string) (string, error) {
	ms, err := o.InstanceMembers(ctx, tileID)
	if err != nil {
		return "", err
	}
	var on []string
	for _, m := range ms {
		if m.Role == opmember.Replica {
			on = append(on, o.nodeName(ctx, m.NodeID))
		}
	}
	if len(on) > 0 {
		return "Standby on " + strings.Join(on, ", "), nil
	}
	ns, err := o.nodeLeaf().List(ctx)
	if err != nil {
		return "", err
	}
	if len(ns) < 2 {
		return "No standby. Add a node to get a standby.", nil
	}
	return "No standby", nil
}

// setPrimaryState records whether the panel wants the instance tile's
// container running; no row (an instance of another engine) is nothing to do.
func (o *Orchestrator) setPrimaryState(ctx context.Context, tileID, state string) {
	m, err := o.operated.GetByTile(ctx, tileID)
	if err != nil {
		return
	}
	ms, err := o.members.List(ctx, m.ID)
	if err != nil {
		return
	}
	if p, ok := opmember.PrimaryOf(ms); ok && p.State != state {
		if _, err := o.members.SetState(ctx, p, state); err != nil {
			slog.Warn("member: state not recorded", "tile", tileID, "err", err)
		}
	}
}

// mayStartTile refuses starting the instance tile's container while its
// primary is fenced.
func (o *Orchestrator) mayStartTile(ctx context.Context, t store.Tile) error {
	m, err := o.operated.GetByTile(ctx, t.ID)
	if err != nil {
		return nil
	}
	ms, err := o.members.List(ctx, m.ID)
	if err != nil {
		return err
	}
	if p, ok := opmember.PrimaryOf(ms); ok {
		return opmember.MayStart(p)
	}
	return nil
}

// startMembers starts the members of instances that have a standby and that
// the panel wants running. Such a primary has restart policy "no": a crash or
// a node reboot leaves it down until this runs (the watch tick, so within a
// minute), and a fenced one is never started. An instance without a standby
// keeps its tile's restart policy and is not touched here, so one the user
// stopped stays stopped. A tile with a job in flight is left to it.
func (o *Orchestrator) startMembers(ctx context.Context) {
	all, err := o.members.All(ctx)
	if err != nil {
		slog.Warn("members: list", "err", err)
		return
	}
	byInst := map[string][]InstanceMember{}
	for _, m := range all {
		byInst[m.InstanceID] = append(byInst[m.InstanceID], m)
	}
	for _, m := range all {
		if m.State != opmember.Running || m.Phase == opmember.Rebuild || opmember.MayStart(m) != nil || !opmember.HasReplica(byInst[m.InstanceID]) ||
			o.failoverActive(ctx, m.InstanceID) { // the job starts what it needs
			continue
		}
		if err := o.startMember(ctx, m); err != nil {
			slog.Warn("members: start", "member", m.ID, "err", err)
		}
	}
}

func (o *Orchestrator) startMember(ctx context.Context, m InstanceMember) error {
	inst, err := o.operated.Get(ctx, m.InstanceID)
	if err != nil {
		return err
	}
	t, err := o.tiles.Get(ctx, inst.TileID)
	if err != nil {
		return err
	}
	labels := map[string]string{LabelMember: m.ID}
	node := m.NodeID
	if m.Role == opmember.Primary {
		if node, err = o.primaryNode(ctx, t); err != nil {
			return err
		}
		if j, ok, err := o.jobRows.Last(ctx, t.ID); err != nil || (ok && job.Live(j.State)) {
			return err
		}
		labels = map[string]string{tile.LabelTile: t.ID, tile.LabelRole: "replica"}
	}
	if o.nodeUp(ctx, node) != nil {
		return nil
	}
	nctx := docker.WithNode(ctx, node)
	cs, err := o.docker.List(nctx, labels)
	if err != nil {
		return err
	}
	if len(cs) == 0 && m.Role == opmember.Replica {
		return o.createStandby(ctx, t, m)
	}
	var failed []error
	for _, c := range cs {
		if c.State != "running" && c.State != "restarting" {
			if m.Role == opmember.Replica {
				// a container made with other settings is made again, so its
				// start does not put the old file back over the new one
				// (a spec that cannot be made now starts the container as it is)
				if again, err := o.refreshStandby(ctx, t, m, c, false); err != nil {
					slog.Warn("members: standby settings", "member", m.ID, "err", err)
				} else if again {
					continue
				}
			}
			if err := o.docker.Start(nctx, c.ID); err != nil {
				failed = append(failed, fmt.Errorf("%s: %w", c.Name, err))
			}
		}
	}
	return errors.Join(failed...)
}

// memberHost is the hosts line "alias:vip" for a member on node: how it
// dials the instance's primary, which is on another node (R4).
func (o *Orchestrator) memberHost(ctx context.Context, m InstanceMember, node string) (string, error) {
	inst, err := o.operated.Get(ctx, m.InstanceID)
	if err != nil {
		return "", err
	}
	return o.hostOn(ctx, node, inst)
}

// memberFlows are node n's flows for its replicas' links to their primaries:
// replica network -> primary VIP on the engine's port, Out on the replica's
// node, an accept on the primary's.
func (o *Orchestrator) memberFlows(ctx context.Context, n store.Node) []vip.Flow {
	all, err := o.members.All(ctx)
	if err != nil {
		return nil
	}
	var out []vip.Flow
	for _, m := range all {
		if m.Role != opmember.Replica || m.Subnet == "" {
			continue
		}
		src, err := netip.ParsePrefix(m.Subnet)
		if err != nil {
			continue
		}
		inst, err := o.operated.Get(ctx, m.InstanceID)
		if err != nil {
			continue
		}
		eng, ok := mflow.Engines[inst.Engine]
		if !ok {
			continue
		}
		t, err := o.tiles.Get(ctx, inst.TileID)
		if err != nil {
			continue
		}
		pn, err := o.primaryNode(ctx, t)
		if err != nil || (n.ID != pn && n.ID != m.NodeID) {
			continue
		}
		v, ok := o.tileVIP(ctx, pn, t.ID)
		if !ok {
			continue
		}
		out = append(out, vip.Flow{Src: src, VIP: v, Port: eng.Definition().Port, Out: n.ID == m.NodeID})
	}
	return out
}
