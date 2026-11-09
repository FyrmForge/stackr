package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
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
// accept on A), both generated from the member rows. The next step, the
// replication, starts the replica container on that network with that host
// line and uses the VIP as primary_conninfo's host.

// LabelMember marks a replica member's containers.
const LabelMember = "stackr.member"

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
// tile's env's.
func (o *Orchestrator) primaryNode(ctx context.Context, t store.Tile) (string, error) {
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

// AddStandby makes the instance a replica member on node: its row (refused
// when the node already holds a member, R4), its network there and its data
// volume. The container comes with the replication.
func (o *Orchestrator) AddStandby(ctx context.Context, tileID, nodeID string) (InstanceMember, error) {
	t, inst, err := o.memberInstance(ctx, tileID)
	if err != nil {
		return InstanceMember{}, err
	}
	if _, err := o.store.Nodes.Get(ctx, nodeID); err != nil {
		return InstanceMember{}, errs.Invalidf("node", "Pick one of the cluster's nodes.")
	}
	if err := o.nodeUp(ctx, nodeID); err != nil {
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
	if err != nil {
		_ = o.dropMember(context.WithoutCancel(ctx), m)
		return InstanceMember{}, err
	}
	o.rerouteVIPsAsync(ctx) // the replica's flow
	return m, nil
}

// RemoveStandby takes a replica member away: its containers, volume and row,
// and its node's network when nothing else is on it.
func (o *Orchestrator) RemoveStandby(ctx context.Context, memberID string) error {
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
	if err := o.dropMember(ctx, m); err != nil {
		return err
	}
	o.rerouteVIPsAsync(ctx)
	return nil
}

// dropMember removes a replica's world objects then its row. A network with
// containers of others still on it stays.
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
	if err := o.docker.RemoveVolume(nctx, m.Volume); err != nil {
		return err
	}
	if err := o.envs.DropShared(nctx, operated.Network(m.InstanceID)); err != nil {
		slog.Warn("member: network stays", "member", m.ID, "err", err)
	}
	return o.members.Delete(ctx, m)
}

// FenceMember sets or clears a member's fence. The row is written first and
// nothing else changes: a fenced member is refused at every start (the
// instance's deploy, the start verb and the panel's own member start) until
// the fence is cleared by its rejoin.
func (o *Orchestrator) FenceMember(ctx context.Context, memberID string, fenced bool) error {
	m, err := o.members.Get(ctx, memberID)
	if err != nil {
		return err
	}
	_, err = o.members.SetFenced(ctx, m, fenced)
	return err
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

// startMembers starts the members the panel wants running. Postgres
// containers have restart policy "no": a crash or a node reboot leaves them
// down until this runs (the watch tick, so within a minute), and a fenced one
// is never started. A tile with a job in flight is left to it.
func (o *Orchestrator) startMembers(ctx context.Context) {
	all, err := o.members.All(ctx)
	if err != nil {
		slog.Warn("members: list", "err", err)
		return
	}
	for _, m := range all {
		if m.State != opmember.Running || opmember.MayStart(m) != nil {
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
	var failed []error
	for _, c := range cs {
		if c.State != "running" && c.State != "restarting" {
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
