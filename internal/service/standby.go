package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// AlertPGRebuild is the problem of an instance with a standby that needs
// rebuilding (its slot was lost). It is not one of pgAlertKinds: the sampler
// does not judge it, observeStandbys does.
const AlertPGRebuild = "pgmon.rebuild"

// createStandby makes standby m's container on its node and starts it: its
// first run copies the primary into the empty data volume (flow/operated
// StandbySpec). The image is pulled there first. The replication passfile is
// copied into the created container before it starts: the password is in no
// env, argv or image layer.
func (o *Orchestrator) createStandby(ctx context.Context, t store.Tile, m InstanceMember) error {
	host, err := o.memberHost(ctx, m, m.NodeID)
	if err != nil {
		return err
	}
	spec, err := o.engines.StandbySpec(ctx, t, m, host)
	if err != nil {
		return err
	}
	pass, err := o.engines.StandbyPassfile(ctx, t)
	if err != nil {
		return err
	}
	nctx := docker.WithNode(ctx, m.NodeID)
	if _, err := o.images.Ensure(nctx, spec.Image, "", io.Discard); err != nil {
		return err
	}
	id, err := o.docker.Create(nctx, spec)
	if err != nil {
		return err
	}
	dir, name := path.Split(mflow.ReplPassfile)
	err = o.docker.CopyTo(nctx, id, strings.TrimSuffix(dir, "/"), name, 0o600, pass)
	if err == nil {
		err = o.docker.Start(nctx, id)
	}
	if err != nil {
		// not left lying around under a name the retry would collide with
		_ = o.docker.StopRemove(context.WithoutCancel(nctx), id)
	}
	return err
}

// observeStandbys is the watch tick's look at every instance's standbys,
// after startMembers: a standby whose slot was lost (wal_status lost, or
// gone) is marked rebuild, stopped and alerted; one else is streaming or
// starting by its own report (in recovery, WAL receiver streaming). Who may
// serve reads changes by hysteresis: a standby joins the read pool after two
// probes in a row that find it streaming and leaves after two that do not,
// or at once when its container is not running, it is fenced, or it needs a
// rebuild; so a flapping standby does not redeploy its node's consumers on
// every flap. A change is routed to those consumers (routeReaders), which
// the tick retries until it has.
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
		if o.failoverActive(ctx, id) {
			continue // the slots and the roles are changing: no verdict
		}
		o.observeInstance(ctx, id, reps)
	}
}

// probeHold is how many probes in a row move a standby into or out of the
// read pool.
const probeHold = 2

// bumpProbe counts consecutive probes of member id: positive for streaming,
// negative for not. It returns the run.
func (o *Orchestrator) bumpProbe(id string, streaming bool) int {
	n := 0
	if v, ok := o.probeRuns.Load(id); ok {
		n = v.(int)
	}
	switch {
	case streaming:
		n = max(n, 0) + 1
	default:
		n = min(n, 0) - 1
	}
	o.probeRuns.Store(id, n)
	return n
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
		if phase == opmember.Rewind {
			continue // the old primary, waiting for its node: the rejoin moves it on
		}
		if st, ok := slots[mflow.SlotName(m.ID)]; slotsErr == nil && (!ok || st == "lost") {
			phase = opmember.Rebuild
			if err := o.stopMember(ctx, m); err != nil {
				slog.Warn("members: stop", "member", m.ID, "err", err)
			}
			o.probeRuns.Delete(m.ID)
		} else if phase != opmember.Rebuild {
			switch o.probeStandby(ctx, m, probe) {
			case probeDown:
				phase = opmember.Starting
				o.probeRuns.Delete(m.ID)
				o.behindSince.Delete(m.ID)
			case probeStreaming:
				o.behindSince.Delete(m.ID)
				if n := o.bumpProbe(m.ID, true); phase != opmember.Streaming && n >= probeHold {
					phase = opmember.Streaming
				}
			case probeBehind:
				if o.walGone(ctx, m) {
					phase = opmember.Rebuild
					if err := o.stopMember(ctx, m); err != nil {
						slog.Warn("members: stop", "member", m.ID, "err", err)
					}
					o.probeRuns.Delete(m.ID)
					break
				}
				if n := o.bumpProbe(m.ID, false); phase == opmember.Streaming && n <= -probeHold {
					phase = opmember.Starting
				}
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
		if err := o.routeReaders(ctx, upd); err != nil {
			slog.Warn("members: readers not redeployed (the tick retries)", "member", m.ID, "err", err)
		}
	}
	sev, msg := "", ""
	if needs {
		sev, msg = SevWarning, t.Slug+": a standby fell too far behind and has to be rebuilt."
	}
	o.pitrProblem(ctx, "alert."+AlertPGRebuild+"."+inst.ID, AlertPGRebuild, sev, t.Slug, msg)
}

// What a probe of a standby found.
type probeResult int

const (
	probeUnknown   probeResult = iota // its node cannot be asked
	probeDown                         // its container is not running
	probeStreaming                    // in recovery, WAL receiver streaming
	probeBehind                       // running, not streaming
)

// probeStandby asks standby m: streaming when its container runs and the WAL
// receiver streams.
func (o *Orchestrator) probeStandby(ctx context.Context, m InstanceMember, probe []string) probeResult {
	if o.nodeUp(ctx, m.NodeID) != nil {
		return probeUnknown
	}
	nctx := docker.WithNode(ctx, m.NodeID)
	cs, err := o.docker.List(nctx, map[string]string{LabelMember: m.ID})
	if err != nil {
		return probeUnknown
	}
	i := slices.IndexFunc(cs, func(c docker.Container) bool { return c.State == "running" })
	if i < 0 {
		return probeDown
	}
	cctx, cancel := context.WithTimeout(nctx, 10*time.Second)
	defer cancel()
	out, err := o.docker.Exec(cctx, cs[i].ID, probe)
	if err == nil && mflow.StandbyStreaming(out) {
		return probeStreaming
	}
	return probeBehind
}

// standbyStuckAfter is how long a standby may sit not streaming before its
// log is read for a WAL segment the primary no longer has.
const standbyStuckAfter = 5 * time.Minute

// walGone: standby m has not streamed for standbyStuckAfter and its receiver
// asks for a segment the primary already removed (it lagged the promote
// point, so its slot never went "lost"). Only a rebuild helps.
func (o *Orchestrator) walGone(ctx context.Context, m InstanceMember) bool {
	v, _ := o.behindSince.LoadOrStore(m.ID, time.Now())
	if time.Since(v.(time.Time)) < standbyStuckAfter {
		return false
	}
	nctx := docker.WithNode(ctx, m.NodeID)
	cs, err := o.docker.List(nctx, map[string]string{LabelMember: m.ID})
	if err != nil {
		return false
	}
	for _, c := range cs {
		if c.State == "running" {
			out, err := o.docker.Logs(nctx, c.ID, 50)
			return err == nil && strings.Contains(out, "has already been removed")
		}
	}
	return false
}

// stopMember stops standby m's containers, the postgres and its pooler; it
// stays down (restart "no", and startMembers skips a rebuild). Both are
// tried, and every failure is returned.
func (o *Orchestrator) stopMember(ctx context.Context, m InstanceMember) error {
	nctx := docker.WithNode(ctx, m.NodeID)
	cs, err := o.docker.List(nctx, map[string]string{LabelMember: m.ID})
	var errList []error
	if err != nil {
		errList = append(errList, err)
	}
	ps, err := o.poolerContainers(nctx, m.ID)
	if err != nil {
		errList = append(errList, err)
	}
	for _, c := range append(cs, ps...) {
		if c.State == "running" || c.State == "restarting" {
			if err := o.docker.Stop(nctx, c.ID); err != nil {
				errList = append(errList, fmt.Errorf("stop %s: %w", c.Name, err))
			}
		}
	}
	return errors.Join(errList...)
}

// routeReaders makes the consumers on standby m's node match its standing in
// the read pool: when it differs from what they were last deployed with
// (m.Routed), those that ref a *_read output are redeployed, so the read
// alias resolves against it. Routed is recorded only once every one of them
// was queued; until then it differs and the tick calls this again.
func (o *Orchestrator) routeReaders(ctx context.Context, m InstanceMember) error {
	want := opmember.InReadPool(m)
	if m.Role != opmember.Replica || m.Routed == want {
		return nil
	}
	if err := o.redeployReaders(ctx, m.InstanceID, m.NodeID); err != nil {
		return err
	}
	_, err := o.members.SetRouted(ctx, m, want)
	return err
}

// routePending retries the routing changes still owed.
func (o *Orchestrator) routePending(ctx context.Context) {
	all, err := o.members.All(ctx)
	if err != nil {
		return
	}
	for _, m := range all {
		if m.Role == opmember.Replica && m.State != opmember.Removing && m.Routed != opmember.InReadPool(m) {
			if err := o.routeReaders(ctx, m); err != nil {
				slog.Warn("members: readers not redeployed", "member", m.ID, "err", err)
			}
		}
	}
}

// usesRead says whether tile t refs a *_read output of a slice.
func usesRead(t Tile) bool {
	for _, b := range params.Refs(t.EnvJSON + "\n" + t.Volumes + "\n" + t.Command) {
		if r, err := params.Parse(b); err == nil && r.Kind == params.KindTile && strings.Contains(r.Name, "_read") {
			return true
		}
	}
	return false
}

// redeployReaders redeploys the consumers of the instance's slices that run
// on node and ref a *_read output. Every one is tried; the errors are joined.
func (o *Orchestrator) redeployReaders(ctx context.Context, instanceID, node string) error {
	ps, err := o.operated.ByInstance(ctx, instanceID)
	if err != nil {
		return err
	}
	var readers []Tile
	for _, p := range ps {
		bs, err := o.operated.Bindings(ctx, p.ID)
		if err != nil {
			return err
		}
		for _, b := range bs {
			c, err := o.tiles.Get(ctx, b.ConsumerTileID)
			if err != nil {
				return err
			}
			if n, err := o.envNode(ctx, c.EnvironmentID); err == nil && n == node && usesRead(c) {
				readers = append(readers, c)
			}
		}
	}
	var failed []error
	for _, c := range readers {
		if _, err := o.redeployIfRunning(ctx, c); err != nil {
			failed = append(failed, err)
		}
	}
	return errors.Join(failed...)
}

// StandbyView is one member of an instance as its drawer and `stackr operated
// standby ls` show it. State: running (primary), streaming, starting,
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
	SyncMode     string `json:"sync_mode,omitempty"` // auto, always or never
	SyncIn       bool   `json:"sync_in"`             // the primary waits for it
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
			v.State = standbyWord(m, o.probeRun(m.ID) > 0)
			v.SyncMode, v.SyncIn = cmpMode(m.SyncMode), m.SyncIn
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

// probeRun is the current run of probes for member id: positive streaming.
func (o *Orchestrator) probeRun(id string) int {
	n, _ := o.probeRuns.Load(id)
	i, _ := n.(int)
	return i
}

// standbyWord is the state shown. Phase moves to streaming after probeHold
// probes (the read pool's hysteresis); the display does not wait for it:
// one streaming probe in a row already says streaming.
func standbyWord(m InstanceMember, probedStreaming bool) string {
	switch {
	case m.State == opmember.Removing:
		return "removing"
	case m.State == opmember.Stopped:
		return "stopped"
	case m.Phase == opmember.Rewind:
		return "fenced, needs rewind"
	case m.Phase == opmember.Rebuild:
		return "needs rebuild"
	case m.Fenced:
		return "fenced"
	case m.Phase == opmember.Streaming, probedStreaming && m.Phase == opmember.Starting:
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

// clusterNodes is instance tile t's member nodes by node id: the primary's
// (primaryNode) and each standby's not being removed, the standby's member
// alongside. A tile with no ID yet (a new instance) has none.
func (o *Orchestrator) clusterNodes(ctx context.Context, t store.Tile) (primary string, standbys map[string]InstanceMember, err error) {
	standbys = map[string]InstanceMember{}
	if t.ID == "" {
		primary, err = o.envNode(ctx, t.EnvironmentID)
		return primary, standbys, err
	}
	ms, err := o.InstanceMembers(ctx, t.ID)
	if err != nil {
		return "", nil, err
	}
	if primary, err = o.primaryNode(ctx, t); err != nil {
		return "", nil, err
	}
	for _, m := range ms {
		if m.Role == opmember.Replica && m.State != opmember.Removing {
			standbys[m.NodeID] = m
		}
	}
	return primary, standbys, nil
}

// standbyExport is promote.Flow.StandbyNames: the node of every member but the
// current primary's, by name, a standby's mode when not auto. An existing
// instance's cluster is its listed standbys plus wherever its primary is.
func (o *Orchestrator) standbyExport(ctx context.Context, t store.Tile) ([]promote.Standby, error) {
	primary, sbs, err := o.clusterNodes(ctx, t)
	if err != nil {
		return nil, err
	}
	var out []promote.Standby
	for id, m := range sbs {
		if id != primary {
			s := promote.Standby{Node: o.nodeName(ctx, id)}
			if md := cmpMode(m.SyncMode); md != SyncAuto {
				s.Sync = md
			}
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b promote.Standby) int { return strings.Compare(a.Node, b.Node) })
	return out, nil
}

// standbyPlan is promote.Flow.StandbyPlan. The cluster the file names is its
// standbys plus the primary's node (the placement node on a new instance); the
// members' node set is compared with it, so a switchover changes nothing. A node to add, one to remove (never the primary's) and a standby
// whose sync mode moves are the diff. A name that is no node is refused.
func (o *Orchestrator) standbyPlan(ctx context.Context, t store.Tile, want []promote.Standby) (d promote.StandbyDiff, err error) {
	primary, have, err := o.clusterNodes(ctx, t)
	if err != nil {
		return d, err
	}
	home, err := o.envNode(ctx, t.EnvironmentID)
	if err != nil {
		return d, err
	}
	// A new instance's primary is created on home; an existing one's is where it is.
	fresh := t.ID == ""
	wantIDs := map[string]string{} // node id to mode
	var order []string
	if fresh {
		wantIDs[home], order = SyncAuto, []string{home}
	}
	for _, s := range want {
		n, err := o.nodeByRef(ctx, s.Node)
		if err != nil {
			return d, errs.Invalidf("standbys", "%q is not a node of this cluster", s.Node)
		}
		mode := cmpMode(strings.ToLower(s.Sync))
		if !SyncModeOK(mode) {
			return d, errs.Invalidf("standbys", "%s: sync is auto, always or never", s.Node)
		}
		if fresh && n.ID == home {
			continue // the primary is created there; a standby on the same node is dropped
		}
		if _, dup := wantIDs[n.ID]; dup {
			return d, errs.Invalidf("standbys", "%q is listed twice", s.Node)
		}
		wantIDs[n.ID] = mode
		if !slices.Contains(order, n.ID) {
			order = append(order, n.ID)
		}
		if _, has := have[n.ID]; !has && n.ID != primary {
			if admin, _ := ctx.Value(placeAdminKey{}).(bool); !admin {
				if err := o.nodeOpenToTile(ctx, false, t, n.ID); err != nil {
					return d, errs.Invalidf("standbys", "%q is not a node of this cluster", s.Node)
				}
			}
		}
	}
	var text []string
	for _, id := range order {
		m, has := have[id]
		switch {
		case id == primary:
		case !has:
			d.Add = append(d.Add, promote.Standby{Node: o.nodeName(ctx, id), Sync: wantIDs[id]})
			text = append(text, "+"+o.nodeName(ctx, id))
		case cmpMode(m.SyncMode) != wantIDs[id]:
			d.Mode = append(d.Mode, promote.Standby{Node: o.nodeName(ctx, id), Sync: wantIDs[id]})
			text = append(text, o.nodeName(ctx, id)+" sync "+wantIDs[id])
		}
	}
	for id := range have {
		if _, keep := wantIDs[id]; !keep {
			d.Remove = append(d.Remove, o.nodeName(ctx, id))
			text = append(text, "−"+o.nodeName(ctx, id))
		}
	}
	slices.Sort(d.Remove)
	slices.Sort(text)
	d.Text = strings.Join(text, " ")
	if ok, _ := ctx.Value(mayPlaceKey{}).(bool); !ok && len(d.Add)+len(d.Remove) > 0 {
		return d, errs.Conflictf("placing a standby on a node is for an org owner (placement.write), so an owner must promote this")
	}
	return d, nil
}

// setStandbys carries a standbys diff out on instance tile t: the standbys
// off the file go first, then the missing ones are added under the instance's
// topology lock, and last each listed mode is set (its own lock).
func (o *Orchestrator) setStandbys(ctx context.Context, t store.Tile, d promote.StandbyDiff) error {
	if err := o.changeStandbys(ctx, t, d); err != nil {
		return err
	}
	modes := slices.Concat(d.Add, d.Mode)
	if len(modes) == 0 {
		return nil
	}
	ms, err := o.InstanceMembers(ctx, t.ID)
	if err != nil {
		return err
	}
	for _, s := range modes {
		for _, m := range ms {
			if m.Role == opmember.Replica && o.nodeName(ctx, m.NodeID) == s.Node && cmpMode(m.SyncMode) != s.Sync {
				if _, err := o.SetStandbySync(ctx, t.ID, m.ID, s.Sync); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (o *Orchestrator) changeStandbys(ctx context.Context, t store.Tile, d promote.StandbyDiff) error {
	unlock, err := o.lockInstance(ctx, t.ID)
	if err != nil {
		return err
	}
	defer unlock()
	ms, err := o.InstanceMembers(ctx, t.ID)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if m.Role == opmember.Replica && m.State != opmember.Removing && slices.Contains(d.Remove, o.nodeName(ctx, m.NodeID)) {
			if err := o.removeStandby(ctx, m.ID); err != nil {
				return err
			}
		}
	}
	for _, s := range d.Add {
		if _, err := o.addStandby(ctx, t.ID, s.Node); err != nil {
			return err
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
func (o *Orchestrator) AddStandbyView(ctx context.Context, by *Principal, tileID, nodeRef string) (StandbyView, error) {
	ctx = context.WithoutCancel(ctx) // runs to the end: the request's timeout must not cut it
	m, err := o.AddStandby(ctx, by, tileID, nodeRef)
	if err != nil {
		return StandbyView{}, err
	}
	return o.viewOf(ctx, tileID, m.ID)
}

// RemoveStandbyOf removes standby id of tile tileID's instance.
func (o *Orchestrator) RemoveStandbyOf(ctx context.Context, tileID, id string) error {
	ctx = context.WithoutCancel(ctx) // runs to the end: the request's timeout must not cut it
	m, err := o.standbyOf(ctx, tileID, id)
	if err != nil {
		return err
	}
	return o.RemoveStandby(ctx, m.ID)
}

// RebuildStandbyOf rebuilds standby id of tile tileID's instance.
func (o *Orchestrator) RebuildStandbyOf(ctx context.Context, tileID, id string) (StandbyView, error) {
	ctx = context.WithoutCancel(ctx) // runs to the end: the request's timeout must not cut it
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

// FenceStandbyOf fences or unfences standby id of tile tileID's instance by
// hand: a fenced standby is stopped, out of the read pool and never started
// until it is unfenced. The primary is never fenced here. A standby that
// needs a rewind or a rebuild is left to that, and nothing changes while a
// failover is in flight.
func (o *Orchestrator) FenceStandbyOf(ctx context.Context, tileID, id string, fenced bool) (StandbyView, error) {
	ctx = context.WithoutCancel(ctx) // runs to the end: the request's timeout must not cut it
	unlock, err := o.lockInstance(ctx, tileID)
	if err != nil {
		return StandbyView{}, err
	}
	defer unlock()
	m, err := o.standbyOf(ctx, tileID, id) // a replica of this instance, read under the lock
	if err != nil {
		return StandbyView{}, err
	}
	if o.failoverActive(ctx, m.InstanceID) {
		return StandbyView{}, errs.Conflictf("A failover is in progress. Wait for it.")
	}
	if m.Phase == opmember.Rewind || m.Phase == opmember.Rebuild {
		return StandbyView{}, errs.Conflictf("This standby needs a %s. It rejoins by itself, or rebuild it.", m.Phase)
	}
	// fencing again retries the stop a failed one left undone
	if m.Fenced != fenced || fenced {
		if err := o.fenceLocked(ctx, m.ID, fenced); err != nil {
			return StandbyView{}, err
		}
	}
	return o.viewOf(ctx, tileID, m.ID)
}
