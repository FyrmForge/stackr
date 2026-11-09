package service

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// AlertPGRebuild is the problem of an instance with a standby that needs
// rebuilding (its slot was lost). It is not one of pgAlertKinds: the sampler
// does not judge it, observeStandbys does.
const AlertPGRebuild = "pgmon.rebuild"

// createStandby makes standby m's container on its node and starts it: its
// first run copies the primary into the empty data volume (flow/operated
// StandbySpec). The image is pulled there first.
func (o *Orchestrator) createStandby(ctx context.Context, t store.Tile, m InstanceMember) error {
	host, err := o.memberHost(ctx, m, m.NodeID)
	if err != nil {
		return err
	}
	spec, err := o.engines.StandbySpec(ctx, t, m, host)
	if err != nil {
		return err
	}
	nctx := docker.WithNode(ctx, m.NodeID)
	if _, err := o.images.Ensure(nctx, spec.Image, "", io.Discard); err != nil {
		return err
	}
	_, err = o.docker.Run(nctx, spec)
	return err
}

// observeStandbys is the watch tick's look at every instance's standbys,
// after startMembers: a standby whose slot was lost (wal_status lost, or
// gone) is marked rebuild, stopped and alerted; one else is streaming or
// starting by its own report (in recovery, WAL receiver streaming). A change
// in who may serve reads redeploys the consumers on that standby's node,
// whose read alias resolves differently now.
// ponytail: a flapping standby redeploys its node's consumers each time; a
// hold-down (two ticks) when it shows.
func (o *Orchestrator) observeStandbys(ctx context.Context) {
	all, err := o.members.All(ctx)
	if err != nil {
		return
	}
	byInst := map[string][]InstanceMember{}
	for _, m := range all {
		if m.Role == opmember.Replica && m.State == opmember.Running {
			byInst[m.InstanceID] = append(byInst[m.InstanceID], m)
		}
	}
	for id, reps := range byInst {
		o.observeInstance(ctx, id, reps)
	}
}

func (o *Orchestrator) observeInstance(ctx context.Context, instanceID string, reps []InstanceMember) {
	inst, err := o.operated.Get(ctx, instanceID)
	if err != nil {
		return
	}
	t, err := o.tiles.Get(ctx, inst.TileID)
	if err != nil {
		return
	}
	// no verdict on slots while the primary cannot be asked
	slots, slotsErr := o.engines.SlotStates(ctx, t)
	probe, err := o.engines.StandbyProbeArgv(ctx, t)
	if err != nil {
		return
	}
	needs := false
	for _, m := range reps {
		phase := m.Phase
		if st, ok := slots[mflow.SlotName(m.ID)]; slotsErr == nil && (!ok || st == "lost") {
			phase = opmember.Rebuild
			o.stopMember(ctx, m)
		} else if phase != opmember.Rebuild {
			if p, known := o.probeStandby(ctx, m, probe); known {
				phase = p
			}
		}
		needs = needs || phase == opmember.Rebuild
		if phase == m.Phase {
			continue
		}
		upd, err := o.members.SetPhase(ctx, m, phase)
		if err != nil {
			slog.Warn("members: phase not recorded", "member", m.ID, "err", err)
			continue
		}
		if opmember.InReadPool(m) != opmember.InReadPool(upd) {
			o.redeployReaders(ctx, inst.ID, m.NodeID)
		}
	}
	sev, msg := "", ""
	if needs {
		sev, msg = SevWarning, t.Slug+": a standby fell too far behind and has to be rebuilt."
	}
	o.pitrProblem(ctx, "alert."+AlertPGRebuild+"."+inst.ID, AlertPGRebuild, sev, t.Slug, msg)
}

// probeStandby is a standby's phase from its own report: streaming when its
// container runs and the WAL receiver streams, else starting. known is false
// when its node cannot be asked.
func (o *Orchestrator) probeStandby(ctx context.Context, m InstanceMember, probe []string) (string, bool) {
	if o.nodeUp(ctx, m.NodeID) != nil {
		return "", false
	}
	nctx := docker.WithNode(ctx, m.NodeID)
	cs, err := o.docker.List(nctx, map[string]string{LabelMember: m.ID})
	if err != nil {
		return "", false
	}
	i := slices.IndexFunc(cs, func(c docker.Container) bool { return c.State == "running" })
	if i < 0 {
		return opmember.Starting, true
	}
	cctx, cancel := context.WithTimeout(nctx, 10*time.Second)
	defer cancel()
	out, err := o.docker.Exec(cctx, cs[i].ID, probe)
	if err == nil && mflow.StandbyStreaming(out) {
		return opmember.Streaming, true
	}
	return opmember.Starting, true
}

// stopMember stops standby m's containers; it stays down (restart "no", and
// startMembers skips a rebuild).
func (o *Orchestrator) stopMember(ctx context.Context, m InstanceMember) {
	nctx := docker.WithNode(ctx, m.NodeID)
	cs, err := o.docker.List(nctx, map[string]string{LabelMember: m.ID})
	if err != nil {
		return
	}
	for _, c := range cs {
		if c.State == "running" || c.State == "restarting" {
			if err := o.docker.Stop(nctx, c.ID); err != nil {
				slog.Warn("members: stop", "member", m.ID, "err", err)
			}
		}
	}
}

// redeployReaders redeploys the consumers of the instance's slices that run
// on node, so their read alias resolves against the standby's new standing.
// ponytail: every consumer there, not only those that ref a _read output
// (a ref may come from a params block); narrow it when it hurts.
func (o *Orchestrator) redeployReaders(ctx context.Context, instanceID, node string) {
	ps, err := o.operated.ByInstance(ctx, instanceID)
	if err != nil {
		return
	}
	var readers []Tile
	for _, p := range ps {
		bs, err := o.operated.Bindings(ctx, p.ID)
		if err != nil {
			continue
		}
		for _, b := range bs {
			c, err := o.tiles.Get(ctx, b.ConsumerTileID)
			if err != nil {
				continue
			}
			if n, err := o.envNode(ctx, c.EnvironmentID); err == nil && n == node {
				readers = append(readers, c)
			}
		}
	}
	if err := o.redeployUnion(ctx, readers); err != nil {
		slog.Warn("members: readers not redeployed", "instance", instanceID, "err", err)
	}
}

// StandbyView is one member of an instance as its drawer and `stackr db
// standby list` show it. State: running (primary), streaming, starting,
// needs rebuild, stopped or removing. The lag and the slot figures come from
// the monitor's last sample; the slot's cap is max_slot_wal_keep_size.
type StandbyView struct {
	ID           string `json:"id"`
	Role         string `json:"role"`
	Node         string `json:"node"`
	State        string `json:"state"`
	Fenced       bool   `json:"fenced"`
	ReadPool     bool   `json:"read_pool"`
	LagBytes     int64  `json:"lag_bytes"`
	LagSeconds   int64  `json:"lag_seconds"`
	SlotBytes    int64  `json:"slot_bytes"`
	SlotCapBytes int64  `json:"slot_cap_bytes"`
}

// Standbys lists the instance's members, the primary first.
func (o *Orchestrator) Standbys(ctx context.Context, tileID string) ([]StandbyView, error) {
	t, _, err := o.memberInstance(ctx, tileID)
	if err != nil {
		return nil, err
	}
	ms, err := o.InstanceMembers(ctx, tileID)
	if err != nil {
		return nil, err
	}
	o.metrics.mu.Lock()
	snap, sampled := o.metrics.pg[tileID]
	o.metrics.mu.Unlock()
	out := []StandbyView{}
	for _, m := range ms {
		v := StandbyView{ID: m.ID, Role: m.Role, Fenced: m.Fenced, ReadPool: opmember.InReadPool(m)}
		node := m.NodeID
		if m.Role == opmember.Primary {
			node, _ = o.primaryNode(ctx, t)
			v.State = m.State
		} else {
			v.State = standbyWord(m)
			if sampled {
				slot := mflow.SlotName(m.ID)
				for _, r := range snap.last.Replicas {
					if r.Name == slot {
						v.LagBytes, v.LagSeconds = r.Bytes, r.Seconds
					}
				}
				for _, s := range snap.last.Slots {
					if s.Name == slot {
						v.SlotBytes = s.Bytes
					}
				}
				v.SlotCapBytes = snap.last.SlotKeepMB << 20
			}
		}
		v.Node = o.nodeName(ctx, node)
		out = append(out, v)
	}
	return out, nil
}

func standbyWord(m InstanceMember) string {
	switch {
	case m.State == opmember.Removing:
		return "removing"
	case m.State == opmember.Stopped:
		return "stopped"
	case m.Phase == opmember.Rebuild:
		return "needs rebuild"
	case m.Phase == opmember.Streaming:
		return "streaming"
	}
	return "starting"
}

// StandbyNodes are the nodes of the instance's standbys, by node name, for a
// stack file's standbys list.
func (o *Orchestrator) StandbyNodes(ctx context.Context, tileID string) ([]string, error) {
	ms, err := o.InstanceMembers(ctx, tileID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range ms {
		if m.Role == opmember.Replica && m.State != opmember.Removing {
			out = append(out, o.nodeName(ctx, m.NodeID))
		}
	}
	slices.Sort(out)
	return out, nil
}

// standbyPlan is promote.Flow.StandbyPlan: the file's standby nodes resolved
// to node names, refused when one is no node or is the primary's own (R4).
func (o *Orchestrator) standbyPlan(ctx context.Context, t store.Tile, want []string) (have, canon []string, err error) {
	if have, err = o.StandbyNodes(ctx, t.ID); err != nil {
		return nil, nil, err
	}
	pn, err := o.primaryNode(ctx, t)
	if err != nil {
		return nil, nil, err
	}
	for _, ref := range want {
		n, err := o.nodeByRef(ctx, ref)
		if err != nil {
			return nil, nil, errs.Invalidf("standbys", "%q is not a node of this cluster", ref)
		}
		if n.ID == pn {
			return nil, nil, errs.Conflictf("%s is the primary's node: a standby runs on another", ref)
		}
		canon = append(canon, o.nodeName(ctx, n.ID))
	}
	slices.Sort(canon)
	return have, canon, nil
}

// setStandbys makes instance tile t's standbys exactly nodes (names): the
// ones off the list are removed first, then the missing ones added.
func (o *Orchestrator) setStandbys(ctx context.Context, t store.Tile, nodes []string) error {
	ms, err := o.InstanceMembers(ctx, t.ID)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, m := range ms {
		if m.Role != opmember.Replica || m.State == opmember.Removing {
			continue
		}
		name := o.nodeName(ctx, m.NodeID)
		have[name] = true
		if !slices.Contains(nodes, name) {
			if err := o.RemoveStandby(ctx, m.ID); err != nil {
				return err
			}
		}
	}
	for _, n := range nodes {
		if !have[n] {
			if _, err := o.AddStandby(ctx, t.ID, n); err != nil {
				return err
			}
		}
	}
	return nil
}

// standbyOf is member id of the instance of tile tileID, a replica.
func (o *Orchestrator) standbyOf(ctx context.Context, tileID, id string) (InstanceMember, error) {
	_, inst, err := o.memberInstance(ctx, tileID)
	if err != nil {
		return InstanceMember{}, err
	}
	m, err := o.members.Get(ctx, id)
	if err != nil || m.InstanceID != inst.ID || m.Role != opmember.Replica {
		return InstanceMember{}, errs.ErrNotFound
	}
	return m, nil
}

// view is member m as the lists show it.
func (o *Orchestrator) viewOf(ctx context.Context, tileID, id string) (StandbyView, error) {
	vs, err := o.Standbys(ctx, tileID)
	if err != nil {
		return StandbyView{}, err
	}
	for _, v := range vs {
		if v.ID == id {
			return v, nil
		}
	}
	return StandbyView{}, errs.ErrNotFound
}

// AddStandbyView is AddStandby for the API and the CLI.
func (o *Orchestrator) AddStandbyView(ctx context.Context, tileID, nodeRef string) (StandbyView, error) {
	m, err := o.AddStandby(ctx, tileID, nodeRef)
	if err != nil {
		return StandbyView{}, err
	}
	return o.viewOf(ctx, tileID, m.ID)
}

// RemoveStandbyOf removes standby id of tile tileID's instance.
func (o *Orchestrator) RemoveStandbyOf(ctx context.Context, tileID, id string) error {
	m, err := o.standbyOf(ctx, tileID, id)
	if err != nil {
		return err
	}
	return o.RemoveStandby(ctx, m.ID)
}

// RebuildStandbyOf rebuilds standby id of tile tileID's instance.
func (o *Orchestrator) RebuildStandbyOf(ctx context.Context, tileID, id string) (StandbyView, error) {
	m, err := o.standbyOf(ctx, tileID, id)
	if err != nil {
		return StandbyView{}, err
	}
	n, err := o.RebuildStandby(ctx, m.ID)
	if err != nil {
		return StandbyView{}, err
	}
	return o.viewOf(ctx, tileID, n.ID)
}
