package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Failover of an operated postgres instance: a planned switchover to a
// standby, and the manual promote of one when the primary is down. There is
// no automatic failover: the panel is the only decider, and a person asks.
//
// Where the primary lives. Until a failover the primary is the instance
// tile's own container on its env's node, and its member row holds no node.
// A failover makes the standby's member the primary: its row becomes the
// primary row WITH a node (NodeID, Container, Volume), and the old primary's
// row becomes a replica row on the old node, fenced, until it is rewound. A
// primary row with a node is "relocated". The one place that knows is
// primaryNode; everything that asked "where is the primary" follows it:
//   - tile.Leaf.Reroute sends Replicas, Stream, Exec and the container verbs
//     of the instance tile to the relocated primary (it carries the tile's
//     labels, PromotedSpec), so settings, slots, slice provisioning,
//     backups and PITR base backups reach it unchanged;
//   - tileVIP answers the relocated primary's own pinned address on its
//     node's instance network, which cross-node consumers dial as before;
//   - the instance tile's deploy is refused (flow/operated Container): it
//     would make a second primary on the env's node. It is the way back: when
//     the env moves to the primary's node (adoptPrimary) the primary is the
//     tile's own again.
//
// The job. A failover is a cursor in the settings (failover, "pgfail.<id>"):
// kind, target, old primary and the step done last. The fence (cursor first,
// then the old primary's row) is persisted before anything else; every step
// re-reads the world and is safe to run again, so a panel that dies anywhere
// resumes from the cursor (resumeFailovers, the watch tick). While a cursor
// exists the tick leaves the instance alone (startMembers, observeStandbys).
//
//	switchover: fence, drain, stop, check, promote, recreate, swap, slots, route
//	promote:    fence, stop, promote, recreate, swap, slots, route
//
// then the old primary rejoins as a standby when its node answers (rejoin):
// pg_rewind on its data volume (wal_log_hints is on), or a fresh copy when the
// rewind fails.

const (
	stepFence    = "fence"
	stepDrain    = "drain"
	stepStop     = "stop"
	stepCheck    = "check"
	stepPromote  = "promote"
	stepRecreate = "recreate"
	stepSwap     = "swap"
	stepSlots    = "slots"
	stepRoute    = "route"

	foSwitchover = "switchover"
	foPromoted   = "promote"

	// AlertPGPrimaryDown is the problem of an instance whose primary is down
	// while a standby could take over.
	AlertPGPrimaryDown = "pgmon.primarydown"
	// AlertPGFailoverStuck is the problem of a failover whose cursor holds an
	// error or has been open too long; the primary-down alert is muted meanwhile.
	AlertPGFailoverStuck = "pgmon.failoverstuck"
)

var failoverSteps = map[string][]string{
	foSwitchover: {stepFence, stepDrain, stepStop, stepCheck, stepPromote, stepRecreate, stepSwap, stepSlots, stepRoute},
	foPromoted:   {stepFence, stepStop, stepPromote, stepRecreate, stepSwap, stepSlots, stepRoute},
}

// Timings; tests shorten them.
var (
	failPoll  = time.Second
	failWait  = 2 * time.Minute
	downAfter = 2 * time.Minute // how long a primary is down before the alert
	// stuckAfter is how long a failover may stay open before it alerts.
	stuckAfter = 10 * time.Minute
	// leaseMargin is added to the worker's lease before a promote may count
	// on the old primary being stopped on an unreachable node.
	leaseMargin = 10 * time.Second
	// unreachableAfter is how long a node still marked online may take to
	// answer a promote's first Docker call before it counts as unreachable.
	unreachableAfter = 5 * time.Second
)

// failover is the cursor.
type failover struct {
	Kind       string    `json:"kind"`
	Target     string    `json:"target"`      // member promoted
	Old        string    `json:"old"`         // member that was the primary
	OldNode    string    `json:"old_node"`    // where the old primary runs
	NodeGone   bool      `json:"node_gone"`   // the admin confirmed its node is gone
	PoweredOff bool      `json:"powered_off"` // the admin typed PoweredOffPhrase instead of a lease proof
	Step       string    `json:"step"`        // the next step to run
	Checkpoint string    `json:"checkpoint"`  // the old primary's last checkpoint (switchover)
	Started    time.Time `json:"started"`     //
	Error      string    `json:"error"`       // why the last run stopped
	Slots      string    `json:"slots"`       // the primary's slots before it was stopped; logged once on a failed step
}

func failoverKey(instanceID string) string { return "pgfail." + instanceID }

func (o *Orchestrator) failoverOf(ctx context.Context, instanceID string) (failover, bool) {
	v, ok, err := o.store.Settings.Get(ctx, failoverKey(instanceID))
	if err != nil || !ok {
		return failover{}, false
	}
	var f failover
	return f, json.Unmarshal([]byte(v), &f) == nil
}

func (o *Orchestrator) saveFailover(ctx context.Context, instanceID string, f failover) error {
	b, _ := json.Marshal(f)
	return o.store.Settings.Set(ctx, failoverKey(instanceID), string(b))
}

// failoverActive says whether a failover of the instance is in flight.
func (o *Orchestrator) failoverActive(ctx context.Context, instanceID string) bool {
	_, ok := o.failoverOf(ctx, instanceID)
	return ok
}

// relocated is the instance's primary row when the primary lives off its
// env's node (a failover made it so).
func (o *Orchestrator) relocated(ctx context.Context, tileID string) (InstanceMember, bool) {
	inst, err := o.operated.GetByTile(ctx, tileID)
	if err != nil {
		return InstanceMember{}, false
	}
	ms, err := o.members.List(ctx, inst.ID)
	if err != nil {
		return InstanceMember{}, false
	}
	p, ok := opmember.PrimaryOf(ms)
	return p, ok && p.NodeID != ""
}

// primaryReroute is tile.Leaf.Reroute.
func (o *Orchestrator) primaryReroute(ctx context.Context, t store.Tile) (string, bool) {
	if t.Kind != tile.Operated {
		return "", false
	}
	p, ok := o.relocated(ctx, t.ID)
	return p.NodeID, ok
}

// relocatedAddr is the pinned address of the relocated primary p on its
// node's instance network: what stands in for the tile's VIP.
func (o *Orchestrator) relocatedAddr(ctx context.Context, p InstanceMember) (netip.Addr, bool) {
	cs, err := o.docker.List(docker.WithNode(ctx, p.NodeID), map[string]string{LabelMember: p.ID})
	if err != nil {
		return netip.Addr{}, false
	}
	net := operated.Network(p.InstanceID)
	for _, c := range cs {
		if c.State != "running" {
			continue
		}
		if a, err := netip.ParseAddr(c.Nets[net]); err == nil {
			return a, true
		}
	}
	return netip.Addr{}, false
}

// ---- starting one ----

// Switchover moves the primary to a standby while both run: the archive is
// drained, the primary shut down cleanly, the standby confirmed to have
// replayed the shutdown checkpoint, and only then promoted. to names the
// standby's node; "" picks the streaming standby that replayed the most.
func (o *Orchestrator) Switchover(ctx context.Context, by *Principal, tileID, to string) (Job, error) {
	return o.startFailover(ctx, by, tileID, to, foSwitchover, false, false, false)
}

// AbortSwitchover gives up a switchover that is stuck before the promote
// (the standby never replays the shutdown checkpoint, say): the old primary
// is unfenced and started again, and the cursor goes. Once the promote step
// is reached, or the standby is out of recovery, it is refused: the standby
// may be the primary already and the switchover must finish.
func (o *Orchestrator) AbortSwitchover(ctx context.Context, tileID string) error {
	ctx = context.WithoutCancel(ctx)
	unlock, err := o.lockInstance(ctx, tileID)
	if err != nil {
		return err
	}
	defer unlock()
	t, inst, err := o.memberInstance(ctx, tileID)
	if err != nil {
		return err
	}
	f, ok := o.failoverOf(ctx, inst.ID)
	if !ok {
		return errs.Conflictf("No failover of %s is in progress.", t.Slug)
	}
	if f.Kind != foSwitchover {
		return errs.Conflictf("Only a switchover can be aborted. A promote is not undone.")
	}
	steps := failoverSteps[foSwitchover]
	if slices.Index(steps, f.Step) >= slices.Index(steps, stepPromote) {
		return errs.Conflictf("The standby is being or was promoted (%s). The switchover has to finish.", f.Step)
	}
	// fail closed: the standby must be shown to be in recovery
	target, err := o.members.Get(ctx, f.Target)
	if err != nil {
		return errs.Conflictf("The standby cannot be read, so it cannot be shown unpromoted: %v", err)
	}
	if rec, err := o.inRecovery(ctx, t, target); err != nil {
		return errs.Conflictf("The standby cannot be asked, so it cannot be shown unpromoted: %v", err)
	} else if !rec {
		return errs.Conflictf("The standby is out of recovery: it was promoted. The switchover has to finish.")
	}
	old, err := o.members.Get(ctx, f.Old)
	if err != nil {
		return err
	}
	if old.Role == opmember.Primary && old.Fenced {
		if old, err = o.members.SetFenced(ctx, old, false); err != nil {
			return err
		}
	}
	if err := o.store.Settings.Delete(ctx, failoverKey(inst.ID)); err != nil {
		return err
	}
	slog.Warn("failover: switchover aborted", "instance", inst.ID, "step", f.Step)
	if old.State == opmember.Running {
		if err := o.startMember(ctx, old); err != nil {
			slog.Warn("failover: old primary not started, the tick retries", "instance", inst.ID, "err", err)
		}
	}
	return nil
}

// inRecovery says whether standby m's postgres is in recovery.
func (o *Orchestrator) inRecovery(ctx context.Context, t store.Tile, m InstanceMember) (bool, error) {
	if err := o.nodeUp(ctx, m.NodeID); err != nil {
		return false, err
	}
	nctx := docker.WithNode(ctx, m.NodeID)
	cs, err := o.docker.List(nctx, map[string]string{LabelMember: m.ID})
	if err != nil {
		return false, err
	}
	argv, err := o.engines.PsqlArgv(ctx, t, mflow.SQLInRecovery)
	if err != nil {
		return false, err
	}
	for _, c := range cs {
		if c.State == "running" {
			out, err := o.docker.Exec(nctx, c.ID, argv)
			if err != nil {
				return false, err
			}
			switch strings.TrimSpace(out) {
			case "1":
				return true, nil
			case "0":
				return false, nil
			}
			return false, fmt.Errorf("unexpected answer %q", strings.TrimSpace(out))
		}
	}
	return false, errors.New("the standby is not running")
}

// PoweredOffPhrase is what an admin types to promote past a missing lease
// proof: the old primary's node has no recorded binding (or its lease is not
// waited out), and they state they powered it off. It is recorded in the
// cursor and the log.
const PoweredOffPhrase = "I powered the node off"

// PromoteStandby is the manual failover for a primary that is down: the
// standby on node to becomes the primary. The old primary must be known to be
// stopped: its node answers and the container is not running, or the node is
// forgotten, or the admin confirmed (nodeGone) that an unreachable node is
// really gone and the worker's lease on it has run out, and the panel holds
// the proof that the primary's container was bound to that lease (see
// mayPromote); poweredOff (PoweredOffPhrase) replaces that proof and the wait.
// A target the primary did not wait for, while one it did is usable, is
// refused unless allowAsync: it may lack commits the primary confirmed.
func (o *Orchestrator) PromoteStandby(ctx context.Context, by *Principal, tileID, to string, nodeGone bool, poweredOff string, allowAsync bool) (Job, error) {
	if poweredOff != "" && poweredOff != PoweredOffPhrase {
		return Job{}, errs.Invalidf("powered_off", "Type exactly: %s", PoweredOffPhrase)
	}
	if strings.TrimSpace(to) == "" {
		return Job{}, errs.Invalidf("to", "Pick the standby to promote.")
	}
	return o.startFailover(ctx, by, tileID, to, foPromoted, nodeGone, poweredOff != "", allowAsync)
}

// kindFailover is the job that runs a failover's cursor: a switchover or a
// promote. The job log carries one line per step.
const kindFailover jobs.Kind = "pg-failover"

// failoverJob is the job's payload: the request itself, checked again when
// the job runs. The cursor is not written before then, so a job that waits
// or is cancelled leaves nothing for the tick to resume.
type failoverJob struct {
	TileID     string `json:"tile_id"`
	Kind       string `json:"kind,omitempty"`
	To         string `json:"to,omitempty"`
	NodeGone   bool   `json:"node_gone,omitempty"`
	PoweredOff bool   `json:"powered_off,omitempty"`
	AllowAsync bool   `json:"allow_async,omitempty"`
	Admin      bool   `json:"admin,omitempty"`
}

// startFailover checks the failover and queues the job that runs it. Everything
// that can be refused is refused here, before any change, and returned as the
// error. The cursor is the job's to write when it starts.
func (o *Orchestrator) startFailover(ctx context.Context, by *Principal, tileID, to, kind string, nodeGone, poweredOff, allowAsync bool) (Job, error) {
	ctx = context.WithoutCancel(ctx) // the request's own timeout ends well before the checks do
	unlock, err := o.lockInstance(ctx, tileID)
	if err != nil {
		return Job{}, err
	}
	t, inst, _, err := o.beginFailover(ctx, by, "", tileID, to, kind, nodeGone, poweredOff, allowAsync)
	if err == nil && o.failoverJobLive(ctx, inst.ID) {
		err = errs.Conflictf("A failover of %s is already queued.", t.Slug)
	}
	unlock()
	if err != nil {
		return Job{}, err
	}
	// a promote's primary is down: it never waits behind the tile's deploys
	// (a deploy locks every tile of its env), only behind another failover.
	lock := []string{failoverKey(inst.ID)}
	if kind == foSwitchover {
		lock = append(lock, t.ID)
	}
	return o.enqueue(ctx, kindFailover, failoverJob{TileID: t.ID, Kind: kind, To: to, NodeGone: nodeGone, PoweredOff: poweredOff, AllowAsync: allowAsync, Admin: by != nil && by.Access.Admin}, lock...)
}

// runFailoverJob is the job: it takes the topology lock, checks the request
// again (the state may have changed while it waited), writes the cursor and
// runs it to its end. A step that fails keeps the cursor; the tick resumes it.
func (o *Orchestrator) runFailoverJob(ctx context.Context, r *jobs.Run, p failoverJob) error {
	unlock, err := o.lockInstance(ctx, p.TileID)
	if err != nil {
		return err
	}
	defer unlock()
	t, inst, err := o.memberInstance(ctx, p.TileID)
	if err != nil {
		return err
	}
	f, ok := o.failoverOf(ctx, inst.ID)
	if !ok {
		if p.Kind == "" {
			_, _ = fmt.Fprintln(r.Log, "nothing to run: the failover was aborted or already finished")
			return nil
		}
		by := &Principal{Access: authz.User{Admin: p.Admin}}
		_, _, f, err = o.beginFailover(ctx, by, r.Job.ID, p.TileID, p.To, p.Kind, p.NodeGone, p.PoweredOff, p.AllowAsync)
		if err != nil {
			return err
		}
		if err := o.saveFailover(ctx, inst.ID, f); err != nil {
			return err
		}
	}
	return o.runFailover(ctx, t, inst, f, r.Log)
}

// failoverJobLive says whether a failover job of the instance is queued or running.
func (o *Orchestrator) failoverJobLive(ctx context.Context, instanceID string) bool {
	live, err := o.store.Jobs.ListByState(ctx, "queued", "waiting", "running")
	return err == nil && slices.ContainsFunc(live, func(j store.Job) bool {
		return j.Kind == string(kindFailover) && slices.Contains(j.LockSet, failoverKey(instanceID))
	})
}

// beginFailover is every check of a failover and the cursor it would start
// from, which the caller saves. The caller holds the topology lock. Nothing is
// written; the fence and the steps are the job's.
func (o *Orchestrator) beginFailover(ctx context.Context, by *Principal, ownJob, tileID, to, kind string, nodeGone, poweredOff, allowAsync bool) (store.Tile, store.OperatedInstance, failover, error) {
	t, inst, err := o.memberInstance(ctx, tileID)
	if err != nil {
		return t, inst, failover{}, err
	}
	if f, ok := o.failoverOf(ctx, inst.ID); ok {
		return t, inst, failover{}, errs.Conflictf("A %s of %s is in progress (%s). It resumes by itself.", f.Kind, t.Slug, f.Step)
	}
	ms, err := o.members.List(ctx, inst.ID)
	if err != nil {
		return t, inst, failover{}, err
	}
	old, ok := opmember.PrimaryOf(ms)
	if !ok {
		return t, inst, failover{}, errs.Conflictf("%s has no primary row.", t.Slug)
	}
	oldNode, err := o.primaryNode(ctx, t)
	if err != nil {
		return t, inst, failover{}, err
	}
	target, err := o.failoverTarget(ctx, by, t, ms, to, kind)
	if err != nil {
		return t, inst, failover{}, err
	}
	if kind == foPromoted && !allowAsync {
		if err := syncTargetOK(t, ms, target); err != nil {
			return t, inst, failover{}, err
		}
	}
	var slots string
	if kind == foSwitchover {
		if err = o.mayHandOver(ctx, t, old, oldNode, ownJob); err == nil {
			slots, err = o.switchoverReady(ctx, t, target)
		}
	} else {
		err = o.mayPromote(ctx, t, inst, oldNode, nodeGone, poweredOff)
	}
	if err != nil {
		return t, inst, failover{}, err
	}
	if kind == foPromoted && !allowAsync {
		if err := o.targetBehind(ctx, t, ms, target); err != nil {
			return t, inst, failover{}, err
		}
	}
	// the cursor first, the old primary's fence (the job's first step) second.
	f := failover{Kind: kind, Target: target.ID, Old: old.ID, OldNode: oldNode, NodeGone: nodeGone, PoweredOff: poweredOff, Step: stepFence, Started: time.Now().UTC(), Slots: slots}
	return t, inst, f, nil
}

// failoverTarget picks the standby that is promoted. A named node must hold a
// usable standby; none named, the streaming standby with the highest replay
// LSN (a switchover), or the only usable one (a promote asks for a node).
func (o *Orchestrator) failoverTarget(ctx context.Context, by *Principal, t store.Tile, ms []InstanceMember, to, kind string) (InstanceMember, error) {
	var cands []InstanceMember
	notStreaming := map[string]bool{} // standbys skipped for not streaming yet, by node
	for _, m := range ms {
		if m.Role != opmember.Replica || m.State == opmember.Removing || m.Fenced || m.Phase == opmember.Rebuild || m.Phase == opmember.Rewind {
			continue
		}
		// a planned move goes to a standby that is caught up: the stored phase
		// says so, or a live probe does (the phase lags a rejoin by minutes)
		if kind == foSwitchover && m.Phase != opmember.Streaming && !o.liveStreaming(ctx, t, m) {
			notStreaming[m.NodeID] = true
			continue
		}
		cands = append(cands, m)
	}
	if to != "" {
		n, err := o.nodeByRef(ctx, to)
		if err != nil {
			return InstanceMember{}, errs.Invalidf("to", "Pick one of the cluster's nodes.")
		}
		if err := o.nodeOpenToTile(ctx, by != nil && by.Access.Admin, t, n.ID); err != nil {
			return InstanceMember{}, errs.Invalidf("to", "Pick one of the cluster's nodes.")
		}
		for _, m := range cands {
			if m.NodeID == n.ID {
				return m, o.nodeUp(ctx, n.ID)
			}
		}
		if notStreaming[n.ID] {
			return InstanceMember{}, errs.Conflictf("The standby on %s is not streaming yet. Nothing was stopped; wait until it streams.", n.Name)
		}
		return InstanceMember{}, errs.Conflictf("%s holds no standby that can take over.", n.Name)
	}
	return o.mostAdvanced(ctx, t, cands)
}

// liveStreaming asks standby m itself (not the stored phase) whether it streams.
func (o *Orchestrator) liveStreaming(ctx context.Context, t store.Tile, m InstanceMember) bool {
	probe, err := o.engines.StandbyProbeArgv(ctx, t)
	return err == nil && o.probeStandby(ctx, m, probe) == probeStreaming
}

// switchoverReady runs before anything is stopped, on live state: the target
// streams, and the primary holds a slot for it that is in use and keeps WAL
// (a slot that does not hold the shutdown checkpoint loses the standby). It
// returns the primary's slots, kept for the log should a later step fail.
func (o *Orchestrator) switchoverReady(ctx context.Context, t store.Tile, target InstanceMember) (string, error) {
	if !o.liveStreaming(ctx, t, target) {
		return "", errs.Conflictf("The standby on %s is not streaming from the primary right now. Nothing was stopped; wait until it streams.", o.nodeName(ctx, target.NodeID))
	}
	out, err := o.primarySQL(ctx, t, mflow.SlotHeldSQL(mflow.SlotName(target.ID)))
	if err != nil {
		return "", errs.Conflictf("The primary's replication slots could not be read (%v). Nothing was stopped.", err)
	}
	if err := mflow.SlotHeld(out); err != nil {
		return "", errs.Conflictf("The standby on %s cannot take over safely: %v. Nothing was stopped.", o.nodeName(ctx, target.NodeID), err)
	}
	dump, _ := o.primarySQL(ctx, t, mflow.SlotDumpSQL)
	return strings.TrimSpace(dump), nil
}

// logSlots is the evidence of a switchover step that failed after the primary
// stopped: the slots as they are (when something answers) and as they were
// before the stop. Once per failover.
func (o *Orchestrator) logSlots(ctx context.Context, t store.Tile, instID string, f *failover) {
	live, err := o.primarySQL(ctx, t, mflow.SlotDumpSQL)
	if err != nil {
		live = "unreadable: " + err.Error()
	}
	slog.Warn("failover: replication slots (slot|active|restart_lsn|wal_status)", "instance", instID, "step", f.Step, "now", strings.TrimSpace(live), "before_stop", f.Slots)
	f.Slots = ""
}

// usableStandby says whether m may take over.
func usableStandby(m InstanceMember) bool {
	return m.Role == opmember.Replica && m.State != opmember.Removing && !m.Fenced && m.Phase != opmember.Rebuild && m.Phase != opmember.Rewind
}

// competitor says whether m counts against a promote target: a usable standby
// that runs. Its phase is not asked: with the primary dead every standby
// drops from streaming to starting, and a lagging one must still be compared.
// A stopped one cannot be read and does not count.
func competitor(m InstanceMember) bool {
	return usableStandby(m) && m.State == opmember.Running
}

// targetBehind refuses a promote target that has replayed less WAL than
// another usable standby: the one behind may miss commits the primary
// confirmed. A position that cannot be read refuses too (fail closed);
// allow_async_target bypasses it.
func (o *Orchestrator) targetBehind(ctx context.Context, t store.Tile, ms []InstanceMember, target InstanceMember) error {
	if !slices.ContainsFunc(ms, func(m InstanceMember) bool { return m.ID != target.ID && competitor(m) }) {
		return nil // the only standby: nothing to be behind
	}
	tl, err := o.replayLSN(ctx, t, target)
	if err != nil {
		return errs.Conflictf("The replay position of the standby you picked cannot be read (%v), so it cannot be compared with the others. Allow a target that may lack the last writes to override.", err)
	}
	for _, m := range ms {
		if m.ID == target.ID || !competitor(m) {
			continue
		}
		l, err := o.replayLSN(ctx, t, m)
		if err != nil {
			return errs.Conflictf("The replay position of the standby on %s cannot be read (%v), so it is not known whether the one you picked is behind it. Allow a target that may lack the last writes to override.", o.nodeName(ctx, m.NodeID), err)
		}
		if l > tl {
			return errs.Conflictf("The standby on %s is more advanced than the one you picked, which may miss commits the primary confirmed. Promote that one, or allow a target that may lack the last writes to override.", o.nodeName(ctx, m.NodeID))
		}
	}
	return nil
}

// mostAdvanced is the candidate with the highest replay LSN, among the ones
// the primary waited for if any can be read: they hold every commit it
// confirmed (the highest of them holds all, WAL being in order).
func (o *Orchestrator) mostAdvanced(ctx context.Context, t store.Tile, cands []InstanceMember) (InstanceMember, error) {
	if waited, _ := syncFirst(cands); len(waited) > 0 && len(waited) < len(cands) {
		if m, err := o.mostAdvanced(ctx, t, waited); err == nil {
			return m, nil
		}
	}
	var best InstanceMember
	var bestLSN uint64
	found := false
	for _, m := range cands {
		lsn, err := o.replayLSN(ctx, t, m)
		if err != nil {
			continue
		}
		if !found || lsn > bestLSN {
			best, bestLSN, found = m, lsn, true
		}
	}
	if !found {
		return InstanceMember{}, errs.Conflictf("No standby of %s can take over: none is running.", t.Slug)
	}
	return best, nil
}

// mayHandOver is the switchover's precondition: the primary runs on a node
// that answers, and no job of the tile is in flight.
func (o *Orchestrator) mayHandOver(ctx context.Context, t store.Tile, old InstanceMember, oldNode, ownJob string) error {
	if err := o.nodeUp(ctx, oldNode); err != nil {
		return errs.Conflictf("The primary's node is not reachable. If the primary is down, promote a standby instead.")
	}
	if old.Fenced {
		return errs.Conflictf("The primary is fenced.")
	}
	if cs, err := o.tiles.Replicas(ctx, t); err != nil || !slices.ContainsFunc(cs, func(c docker.Container) bool { return c.State == "running" }) {
		return errs.Conflictf("The primary is not running. If it is down, promote a standby instead.")
	}
	if j, ok, err := o.jobRows.Last(ctx, t.ID); err != nil || (ok && j.ID != ownJob && job.Live(j.State)) {
		return errs.Conflictf("%s has a job running. Wait for it, then switch over.", t.Slug)
	}
	return nil
}

// mayPromote is R3 (a) and (b): the old primary is known stopped. Its node
// answers and the container is not running (or is stopped here). Otherwise
// the node does not answer or is forgotten, and then the old primary is
// stopped only by its worker's lease: the panel must hold the record that the
// primary's container was bound on that node (leaseRec, kept past the node's
// row) and the lease on it, counted from the last ack, must have run out, a
// forgotten node included. An unreachable node also needs the admin's word
// that it is gone. poweredOff (the typed phrase) stands in for the record and
// the wait; it is logged.
func (o *Orchestrator) mayPromote(ctx context.Context, t store.Tile, inst store.OperatedInstance, oldNode string, nodeGone, poweredOff bool) error {
	n, err := o.store.Nodes.Get(ctx, oldNode)
	forgotten := errors.Is(err, errs.ErrNotFound)
	if err != nil && !forgotten {
		return err
	}
	name := "The old primary's node"
	if !forgotten {
		name = n.Name
		if o.nodeUp(ctx, oldNode) == nil {
			// a node that went silent is marked offline minutes later: until
			// then it counts as unreachable here, not as a request that hangs
			lctx, cancel := context.WithTimeout(ctx, unreachableAfter)
			cs, err := o.docker.List(docker.WithNode(lctx, oldNode), map[string]string{tile.LabelTile: t.ID, tile.LabelRole: "replica"})
			cancel()
			if err == nil {
				if slices.ContainsFunc(cs, func(c docker.Container) bool { return c.State == "running" || c.State == "restarting" }) {
					return errs.Conflictf("The primary is running on %s. Switch over instead of promoting.", n.Name)
				}
				return nil // its worker says it is stopped
			}
		}
		if !nodeGone {
			return errs.Conflictf("%s is not reachable. If it is really gone, confirm that to promote.", n.Name)
		}
	}
	if poweredOff {
		slog.Warn("failover: promote on the admin's word that the old node is powered off", "instance", inst.ID, "node", oldNode)
		return nil
	}
	if _, ok := o.leaseRecOf(ctx, inst.ID, oldNode); !ok {
		return errs.Conflictf("%s has no recorded lease on the primary, so it cannot be shown to stop by itself. If you powered it off, type: %s", name, PoweredOffPhrase)
	}
	if wait := o.leaseWait(ctx, oldNode); wait > 0 {
		return errs.Conflictf("%s may still be running the primary: its lease runs out in %d s. Try again then.", name, int(wait.Seconds())+1)
	}
	return nil
}

// ---- running one ----

// runFailover runs the cursor's remaining steps with the topology lock held.
// A step that fails keeps the cursor (and its error) for the tick to resume.
func (o *Orchestrator) runFailover(ctx context.Context, t store.Tile, inst store.OperatedInstance, f failover, log io.Writer) error {
	ctx = context.WithoutCancel(ctx) // a cancelled request must not leave it half done
	steps := failoverSteps[f.Kind]
	from := slices.Index(steps, f.Step)
	if from < 0 {
		return fmt.Errorf("failover: unknown step %q", f.Step)
	}
	run := map[string]func(context.Context, store.Tile, store.OperatedInstance, *failover) error{
		stepFence: o.foFence, stepDrain: o.foDrain, stepStop: o.foStop, stepCheck: o.foCheck,
		stepPromote: o.foPromote, stepRecreate: o.foRecreate, stepSwap: o.foSwap, stepSlots: o.foSlots, stepRoute: o.foRoute,
	}
	for _, step := range steps[from:] {
		f.Step = step
		_, _ = fmt.Fprintln(log, stepLine[step])
		if err := run[step](ctx, t, inst, &f); err != nil {
			f.Error = fmt.Sprintf("%s: %v", step, err)
			_, _ = fmt.Fprintf(log, "failed at %s: %v\nIt carries on by itself; before the promote a switchover can be aborted.\n", step, err)
			if f.Kind == foSwitchover && f.Slots != "" && slices.Index(steps, step) >= slices.Index(steps, stepStop) {
				o.logSlots(ctx, t, inst.ID, &f)
			}
			_ = o.saveFailover(ctx, inst.ID, f)
			slog.Warn("failover: step failed, the tick resumes it", "instance", inst.ID, "step", step, "err", err)
			if f.Kind == foSwitchover { // a bare error reads as "internal error"; say what stopped and what happens next
				return errs.Conflictf("The switchover stopped at %s: %v. It carries on by itself; before the promote it can be given up with abort.", step, err)
			}
			return err
		}
		if o.failCrashAfter == step { // tests: the panel dies here
			return errors.New("crash")
		}
		if next := slices.Index(steps, step) + 1; next < len(steps) {
			f.Step, f.Error = steps[next], ""
			if err := o.saveFailover(ctx, inst.ID, f); err != nil {
				return err
			}
		}
	}
	if err := o.store.Settings.Delete(ctx, failoverKey(inst.ID)); err != nil {
		return err
	}
	if o.nodeForgotten(ctx, f.OldNode) {
		_ = o.store.Settings.Delete(ctx, leaseKey(inst.ID, f.OldNode)) // the fence did its job; no push will ever clear it
	}
	o.pitrProblem(ctx, "alert."+AlertPGPrimaryDown+"."+inst.ID, AlertPGPrimaryDown, "", "", "")
	o.pitrProblem(ctx, "alert."+AlertPGFailoverStuck+"."+inst.ID, AlertPGFailoverStuck, "", "", "")
	_, _ = fmt.Fprintln(log, "the primary moved; rejoining the old primary as a standby")
	if m, err := o.members.Get(ctx, f.Old); err == nil {
		o.tryRejoin(ctx, m) // best effort now; the tick retries when its node returns
		if m, err = o.members.Get(ctx, f.Old); err == nil && m.Fenced {
			_, _ = fmt.Fprintln(log, "the old primary is not back yet; it rejoins when its node answers")
		}
	}
	return nil
}

// stepLine is what a step writes to the job log as it starts.
var stepLine = map[string]string{
	stepFence:    "fencing the old primary",
	stepDrain:    "draining the WAL archive",
	stepStop:     "stopping the old primary",
	stepCheck:    "waiting for the standby to catch up with the shutdown checkpoint",
	stepPromote:  "promoting the standby",
	stepRecreate: "making the standby's container the primary's",
	stepSwap:     "switching the primary role",
	stepSlots:    "pointing the other standbys at the new primary",
	stepRoute:    "rerouting the pooler, firewall and consumers",
}

// resumeFailovers is the watch tick's half: it runs the cursors that stopped
// (a panel that died, a step that failed) and rejoins old primaries whose
// node is back. An instance whose lock is held is being worked on.
func (o *Orchestrator) resumeFailovers(ctx context.Context) {
	rows, err := o.store.Settings.ListPrefix(ctx, "pgfail.")
	if err != nil {
		return
	}
	for k := range rows {
		id := strings.TrimPrefix(k, "pgfail.")
		mu := o.topoLock(id)
		if !mu.TryLock() {
			continue
		}
		o.resumeOne(ctx, id)
		mu.Unlock()
	}
	all, err := o.members.All(ctx)
	if err != nil {
		return
	}
	for _, m := range all {
		if m.Role != opmember.Replica || !m.Fenced || m.Phase != opmember.Rewind || o.failoverActive(ctx, m.InstanceID) {
			continue
		}
		mu := o.topoLock(m.InstanceID)
		if !mu.TryLock() {
			continue
		}
		if cur, err := o.members.Get(ctx, m.ID); err == nil {
			o.tryRejoin(ctx, cur)
		}
		mu.Unlock()
	}
}

func (o *Orchestrator) resumeOne(ctx context.Context, instanceID string) {
	f, ok := o.failoverOf(ctx, instanceID)
	if !ok {
		return
	}
	inst, err := o.operated.Get(ctx, instanceID)
	if err != nil {
		_ = o.store.Settings.Delete(ctx, failoverKey(instanceID)) // the instance is gone
		return
	}
	t, err := o.tiles.Get(ctx, inst.TileID)
	if err != nil || o.failoverJobLive(ctx, instanceID) {
		return // a job is about to run it, with its log
	}
	_ = o.runFailover(ctx, t, inst, f, io.Discard) // logs and records its own failure
}

// ---- the steps ----

func (o *Orchestrator) foFence(ctx context.Context, _ store.Tile, _ store.OperatedInstance, f *failover) error {
	old, err := o.members.Get(ctx, f.Old)
	if err != nil {
		return err
	}
	if old.Role != opmember.Primary {
		return nil // the swap already ran: the old row is a fenced replica
	}
	_, err = o.members.SetFenced(ctx, old, true)
	return err
}

// foDrain makes the archive current before the primary goes: the segment in
// progress is switched out, the archiver waited for, and what waits on the
// primary's box is shipped. Without PITR there is no archive to drain.
func (o *Orchestrator) foDrain(ctx context.Context, t store.Tile, inst store.OperatedInstance, _ *failover) error {
	if inst.PITRDays <= 0 {
		return nil
	}
	seg, err := o.primarySQL(ctx, t, mflow.SQLCloseWAL)
	if err != nil {
		return err
	}
	seg = strings.TrimSpace(seg)
	if err := o.poll(ctx, func() (bool, error) {
		out, err := o.primarySQL(ctx, t, mflow.SQLArchivedPast(seg))
		return strings.TrimSpace(out) == "1", err
	}); err != nil {
		return fmt.Errorf("the archiver did not catch up: %w", err)
	}
	return o.poll(ctx, func() (bool, error) {
		o.pitrShip(ctx, inst)
		st, _ := o.pitrState(ctx, inst.ID)
		return st.Waiting == 0 && !st.Failing, nil
	})
}

// foStop stops the old primary. A switchover asks postgres for a fast
// shutdown (SIGINT; docker's SIGTERM is a smart one that waits for every
// client and is killed after ten seconds with no shutdown checkpoint). A node
// that does not answer is left to the lease and the admin's confirmation,
// checked when the failover was asked for.
func (o *Orchestrator) foStop(ctx context.Context, t store.Tile, _ store.OperatedInstance, f *failover) error {
	if o.nodeUp(ctx, f.OldNode) != nil {
		if f.NodeGone || o.nodeForgotten(ctx, f.OldNode) {
			return nil
		}
		return errors.New("the old primary's node is not reachable")
	}
	if o.nodeForgotten(ctx, f.OldNode) {
		return nil
	}
	nctx := docker.WithNode(ctx, f.OldNode)
	lctx, cancel := context.WithTimeout(nctx, unreachableAfter)
	cs, err := o.oldPrimaryContainers(lctx, t, f.Old)
	cancel()
	if err != nil {
		if f.NodeGone { // confirmed gone and past its lease (mayPromote); not yet marked offline
			return nil
		}
		return err
	}
	for _, c := range cs {
		if c.State != "running" && c.State != "restarting" {
			continue
		}
		o.fastStop(nctx, c.ID)
		if err := o.docker.Stop(nctx, c.ID); err != nil { // a no-op once it exited
			return err
		}
	}
	cs, err = o.oldPrimaryContainers(nctx, t, f.Old)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(cs, func(c docker.Container) bool { return c.State == "running" || c.State == "restarting" }) {
		return errors.New("the old primary is still running")
	}
	return nil
}

// oldPrimaryContainers are the containers of the member that was the primary:
// the tile's own, or the member's own once it was relocated.
func (o *Orchestrator) oldPrimaryContainers(nctx context.Context, t store.Tile, oldID string) ([]docker.Container, error) {
	cs, err := o.docker.List(nctx, map[string]string{tile.LabelTile: t.ID, tile.LabelRole: "replica"})
	if err != nil {
		return nil, err
	}
	more, err := o.docker.List(nctx, map[string]string{LabelMember: oldID})
	if err != nil {
		return nil, err
	}
	for _, c := range more {
		if !slices.ContainsFunc(cs, func(x docker.Container) bool { return x.ID == c.ID }) {
			cs = append(cs, c)
		}
	}
	return cs, nil
}

// fastStop asks the postgres in container id for a fast shutdown and waits
// for the container to exit. Best effort: the caller's docker stop follows.
func (o *Orchestrator) fastStop(nctx context.Context, id string) {
	if _, err := o.docker.Exec(nctx, id, []string{"sh", "-c", "kill -INT 1"}); err != nil {
		slog.Warn("failover: fast shutdown", "container", id, "err", err)
		return
	}
	wctx, cancel := context.WithTimeout(nctx, failWait)
	defer cancel()
	_, _ = o.docker.Wait(wctx, id)
}

// foCheck is the switchover's proof that nothing is lost: the stopped
// cluster's control file says "shut down" and names its last checkpoint, and
// the standby has replayed to at least that point.
func (o *Orchestrator) foCheck(ctx context.Context, t store.Tile, inst store.OperatedInstance, f *failover) error {
	if f.Checkpoint == "" {
		vol, err := o.memberVolume(ctx, t, f.Old)
		if err != nil {
			return err
		}
		spec, err := o.engines.ControlSpec(ctx, t, "stackr-ctl-"+f.Old[:8], vol)
		if err != nil {
			return err
		}
		out, code, err := o.oneShot(ctx, f.OldNode, spec, nil)
		if err != nil {
			return err
		}
		if code != 0 {
			return errors.New("the old primary's control file could not be read")
		}
		cd := mflow.ParseControlData(out)
		if cd.State != "shut down" {
			return fmt.Errorf("the old primary did not shut down cleanly (%q)", cd.State)
		}
		if _, err := mflow.ParseLSN(cd.Checkpoint); err != nil {
			return err
		}
		f.Checkpoint = cd.Checkpoint
		if err := o.saveFailover(ctx, inst.ID, *f); err != nil {
			return err
		}
	}
	want, _ := mflow.ParseLSN(f.Checkpoint)
	target, err := o.members.Get(ctx, f.Target)
	if err != nil {
		return err
	}
	if err := o.poll(ctx, func() (bool, error) {
		have, err := o.replayLSN(ctx, t, target)
		return err == nil && have >= want, nil
	}); err != nil {
		return fmt.Errorf("the standby has not replayed the shutdown checkpoint %s: %w", f.Checkpoint, err)
	}
	return nil
}

// foPromote promotes the standby (pg_promote); done when it is out of recovery.
func (o *Orchestrator) foPromote(ctx context.Context, t store.Tile, inst store.OperatedInstance, f *failover) error {
	target, err := o.members.Get(ctx, f.Target)
	if err != nil {
		return err
	}
	nctx := docker.WithNode(ctx, target.NodeID)
	if err := o.removePoolers(nctx, target); err != nil { // before the promote: nothing pools a standby that is about to be a primary
		return err
	}
	cs, err := o.docker.List(nctx, map[string]string{LabelMember: target.ID})
	if err != nil {
		return err
	}
	if len(cs) == 0 {
		return errors.New("the standby has no container")
	}
	c := cs[0]
	if c.State != "running" {
		if err := o.docker.Start(nctx, c.ID); err != nil {
			return err
		}
	}
	if c.Labels[tile.LabelTile] != "" {
		return nil // already recreated as the primary
	}
	exec := func(sql string) (string, error) {
		argv, err := o.engines.PsqlArgv(ctx, t, sql)
		if err != nil {
			return "", err
		}
		return o.docker.Exec(nctx, c.ID, argv)
	}
	if err := o.poll(ctx, func() (bool, error) { _, err := exec(mflow.SQLInRecovery); return err == nil, nil }); err != nil {
		return fmt.Errorf("the standby does not answer: %w", err)
	}
	if out, err := exec(mflow.SQLInRecovery); err != nil || strings.TrimSpace(out) == "0" {
		return err // out of recovery already: promoted
	}
	if inst.PreferSync && inst.SyncRequired {
		// the new primary's writes freeze until a standby is in (the sync
		// pass names them after the slots are made); preferred mode starts
		// asynchronous and is reported unprotected until that pass
		run := func(_ context.Context, cmd []string) (string, error) { return o.docker.Exec(nctx, c.ID, cmd) }
		if err := o.engines.PrimeSync(ctx, t, run, mflow.SyncStandbyNames([]string{mflow.SyncNoneSlot})); err != nil {
			return err
		}
	}
	if _, err := exec(mflow.SQLPromote); err != nil {
		return err
	}
	return o.poll(ctx, func() (bool, error) {
		out, err := exec(mflow.SQLInRecovery)
		return err == nil && strings.TrimSpace(out) == "0", nil
	})
}

// foRecreate replaces the promoted standby's container with the primary's:
// stopped cleanly, removed, made again on the same volume with the primary's
// command, the tile's labels and both aliases at a pinned address. A standby
// container started again by itself would empty the volume (standbyPrep), so
// this runs before anything can start it.
func (o *Orchestrator) foRecreate(ctx context.Context, t store.Tile, _ store.OperatedInstance, f *failover) error {
	target, err := o.members.Get(ctx, f.Target)
	if err != nil {
		return err
	}
	nctx := docker.WithNode(ctx, target.NodeID)
	if err := o.removePoolers(nctx, target); err != nil { // the standby's pooler goes with the standby
		return err
	}
	cs, err := o.docker.List(nctx, map[string]string{LabelMember: target.ID})
	if err != nil {
		return err
	}
	for _, c := range cs {
		if c.Labels[tile.LabelTile] != "" { // the primary's container already
			if c.State != "running" {
				return o.docker.Start(nctx, c.ID)
			}
			return nil
		}
		if c.State == "running" || c.State == "restarting" {
			o.fastStop(nctx, c.ID)
		}
		if err := o.docker.StopRemove(nctx, c.ID); err != nil {
			return err
		}
	}
	ip, err := o.pinMemberAddr(ctx, target)
	if err != nil {
		return err
	}
	spec, err := o.engines.PromotedSpec(ctx, t, target, ip)
	if err != nil {
		return err
	}
	if _, err := o.images.Ensure(nctx, spec.Image, "", io.Discard); err != nil {
		return err
	}
	for _, b := range spec.Volumes[1:] { // the archive volume, on the primary's own node
		name, _, _ := strings.Cut(b, ":")
		if err := o.docker.CreateVolume(nctx, name, "local", nil, map[string]string{LabelMember: target.ID}); err != nil {
			return err
		}
	}
	id, err := o.docker.Create(nctx, spec)
	if err != nil {
		return err
	}
	if err := o.docker.Start(nctx, id); err != nil {
		return err
	}
	return o.poll(ctx, func() (bool, error) {
		argv, err := o.engines.PsqlArgv(ctx, t, "SELECT 1")
		if err != nil {
			return false, err
		}
		_, err = o.docker.Exec(nctx, id, argv)
		return err == nil, nil
	})
}

// pinMemberAddr is a stable free address in the lower half of member m's
// network on its node, "" when there is no block to pick from.
func (o *Orchestrator) pinMemberAddr(ctx context.Context, m InstanceMember) (string, error) {
	sub, err := netip.ParsePrefix(m.Subnet)
	if err != nil {
		return "", nil
	}
	net := operated.Network(m.InstanceID)
	rows, err := o.docker.List(docker.WithNode(ctx, m.NodeID), map[string]string{docker.LabelManaged: "true"})
	if err != nil {
		return "", err
	}
	used := map[netip.Addr]bool{}
	for _, c := range rows {
		if a, err := netip.ParseAddr(c.Nets[net]); err == nil {
			used[a] = true
		}
	}
	return docker.StaticAddr(sub, m.InstanceID, func(a netip.Addr) bool { return used[a] }), nil
}

// foSwap exchanges the two rows in one transaction, so there is never no
// primary (the guards read PrimaryOf) nor two. The old primary becomes a
// fenced replica on its node holding its data volume, waiting for its rewind.
func (o *Orchestrator) foSwap(ctx context.Context, t store.Tile, inst store.OperatedInstance, f *failover) error {
	target, err := o.members.Get(ctx, f.Target)
	if err != nil {
		return err
	}
	if target.Role == opmember.Primary {
		return nil
	}
	old, err := o.members.Get(ctx, f.Old)
	if err != nil {
		return err
	}
	vol, err := o.memberVolume(ctx, t, old.ID)
	if err != nil {
		return err
	}
	sub := old.Subnet
	if sub == "" {
		if _, cidr, err := o.docker.MemberAddr(docker.WithNode(ctx, f.OldNode), operated.Network(inst.ID), ""); err == nil {
			sub = cidr
		}
	}
	now := time.Now().UTC()
	old.Role, old.NodeID, old.Container, old.Volume, old.Subnet = opmember.Replica, f.OldNode, "stackr-member-"+old.ID[:8], vol, sub
	old.Fenced, old.Phase, old.State, old.Routed, old.UpdatedAt = true, opmember.Rewind, opmember.Running, false, now
	target.Role, target.Fenced, target.Phase, target.Routed, target.UpdatedAt = opmember.Primary, false, opmember.Starting, false, now
	// The data volume row follows the primary, so its backup schedules, runs
	// and archive root (all keyed by the row) now back up the live data on
	// its node; the old primary's volume is no longer named by any row.
	dv, err := o.dataVolume(ctx, t)
	if err != nil {
		return err
	}
	en, err := o.envNode(ctx, t.EnvironmentID)
	if err != nil {
		return err
	}
	if target.Volume != "" {
		dv.Name = target.Volume
	}
	dv.NodeID = ""
	if target.NodeID != en {
		dv.NodeID = target.NodeID
	}
	// A backup or restore running now holds the volume by its old name; the
	// tick retries the step once it is done.
	if err := o.volumeIdle(ctx, dv); err != nil {
		return err
	}
	// The archive is held in the same transaction that moves the primary: no
	// moment where the new timeline is current and the old epoch still open.
	// Unarmed until foRoute names the segment to wait for.
	mu := o.pitrLock(inst.ID)
	mu.Lock()
	defer mu.Unlock()
	var held pitrState
	if inst.PITRDays > 0 {
		prev, _, err := o.readPITRState(ctx, inst.ID)
		if err != nil {
			return err // never swap over a window that could not be read
		}
		held = prev.held(now, "")
	}
	return o.store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.OperatedMembers.Update(ctx, old); err != nil {
			return err
		}
		if err := tx.OperatedMembers.Update(ctx, target); err != nil {
			return err
		}
		if inst.PITRDays > 0 {
			b, _ := json.Marshal(held)
			if err := tx.Settings.Set(ctx, pitrKey(inst.ID), string(b)); err != nil {
				return err
			}
		}
		return tx.Volumes.Update(ctx, dv)
	})
}

// volumeIdle refuses while a job holds the volume's lock (a backup or restore
// reading or writing the data the row names).
// ponytail: a job the runner starts between this check and the row update
// reads the row either side; closing that needs the runner's own lock.
func (o *Orchestrator) volumeIdle(ctx context.Context, v store.Volume, except ...string) error {
	rs, err := o.jobRows.List(ctx, job.Running)
	if err != nil {
		return err
	}
	for _, j := range rs {
		if slices.Contains(j.LockSet, "volume:"+v.ID) && !slices.Contains(except, string(j.Kind)) {
			return fmt.Errorf("a %s job holds the data volume; waiting for it", j.Kind)
		}
	}
	return nil
}

// foSlots is the new primary's half of the standbys that are left: their
// slots, replication lines and role, and a container made again for each
// that runs, so its hosts line names the new primary.
func (o *Orchestrator) foSlots(ctx context.Context, t store.Tile, inst store.OperatedInstance, _ *failover) error {
	ms, err := o.members.List(ctx, inst.ID)
	if err != nil {
		return err
	}
	subnets := o.replSubnets(ctx, inst.ID, "")
	var failed []error
	for _, m := range ms {
		if m.Role != opmember.Replica || m.State == opmember.Removing {
			continue
		}
		if err := o.engines.PrepareStandby(ctx, t, mflow.SlotName(m.ID), subnets); err != nil {
			return err
		}
		if m.Fenced || o.nodeUp(ctx, m.NodeID) != nil {
			continue // the rejoin, or the start after the node returns, makes it
		}
		nctx := docker.WithNode(ctx, m.NodeID)
		cs, err := o.docker.List(nctx, map[string]string{LabelMember: m.ID})
		if err != nil {
			return err
		}
		if len(cs) == 0 {
			continue
		}
		for _, c := range cs {
			if err := o.docker.StopRemove(nctx, c.ID); err != nil {
				failed = append(failed, err)
			}
		}
		if err := o.createStandby(ctx, t, m); err != nil {
			failed = append(failed, fmt.Errorf("standby on %s: %w", o.nodeName(ctx, m.NodeID), err))
		}
	}
	if len(failed) == 0 {
		o.syncAfterTopology(ctx, t, inst.ID, true) // the slots differ now: the new primary waits for its own standbys
	}
	return errors.Join(failed...)
}

// foRoute points everything that dialled the old primary at the new one: the
// firewall flows follow the address, the consumers are deployed again (their
// hosts line or network join changed), and the archive is paused until the new
// primary is seen archiving (pitrPause): a new timeline cannot continue the
// old one's base backups.
func (o *Orchestrator) foRoute(ctx context.Context, t store.Tile, inst store.OperatedInstance, _ *failover) error {
	// the primary's pooler follows the primary: made on the new primary's node
	// (the tile's containers are looked for there now); the old node's goes at
	// its rejoin. Consumers on another node dial the instance direct.
	if err := o.engines.SyncPooler(ctx, t); err != nil {
		return fmt.Errorf("the pooler on the new primary: %w", err)
	}
	o.rerouteVIPs(ctx)
	if err := o.redeployConsumers(ctx, inst.ID); err != nil {
		return err
	}
	cur, err := o.operated.Get(ctx, inst.ID)
	if err != nil {
		return err
	}
	if cur.PITRDays > 0 {
		if err := o.pitrPause(ctx, t, cur); err != nil {
			return err
		}
	} else if err := o.pitrReset(ctx, cur); err != nil {
		return err
	}
	// the new primary runs under its worker's lease, acked, before the
	// failover counts as complete
	if err := o.syncLease(ctx, t); err != nil {
		return err
	}
	o.setPrimaryState(ctx, t.ID, opmember.Running)
	return nil
}

// redeployConsumers deploys again every running consumer of the instance's
// slices, whatever env or node: their link to the primary changed.
func (o *Orchestrator) redeployConsumers(ctx context.Context, instanceID string) error {
	ps, err := o.operated.ByInstance(ctx, instanceID)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var failed []error
	for _, p := range ps {
		bs, err := o.operated.Bindings(ctx, p.ID)
		if err != nil {
			return err
		}
		for _, b := range bs {
			if seen[b.ConsumerTileID] {
				continue
			}
			seen[b.ConsumerTileID] = true
			c, err := o.tiles.Get(ctx, b.ConsumerTileID)
			if err != nil {
				failed = append(failed, err)
				continue
			}
			if _, err := o.redeployIfRunning(ctx, c); err != nil {
				failed = append(failed, err)
			}
		}
	}
	return errors.Join(failed...)
}

// ---- the old primary rejoins ----

// tryRejoin makes fenced replica m (the old primary, waiting for its rewind)
// a standby again, when its node answers: its own stopped container goes, the
// data volume is rewound onto the new primary (pg_rewind; a failed rewind
// leaves the volume for the standby's own copy), the fence is lifted and the
// standby container made. A node that was forgotten has nothing to rejoin:
// the row goes. The topology lock is held.
func (o *Orchestrator) tryRejoin(ctx context.Context, m InstanceMember) {
	if err := o.rejoin(ctx, m); err != nil {
		slog.Info("failover: old primary not rejoined yet", "member", m.ID, "err", err)
	}
}

// rejoinRewindWait bounds the rewind step: its one-shot container waits up to
// ten minutes itself. The other steps get tickAttempt each, so a node that
// stops answering cannot hold the topology lock past it.
const rejoinRewindWait = 12 * time.Minute

func (o *Orchestrator) rejoin(ctx context.Context, m InstanceMember) error {
	if m.Role != opmember.Replica || !m.Fenced || m.Phase != opmember.Rewind {
		return nil
	}
	if o.nodeForgotten(ctx, m.NodeID) {
		return o.members.Delete(ctx, m)
	}
	step := func(d time.Duration, what string, fn func(context.Context) error) error {
		sctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		err := fn(sctx)
		if err != nil && sctx.Err() != nil && ctx.Err() == nil {
			slog.Warn("failover: rejoin step timed out", "member", m.ID, "node", m.NodeID, "step", what, "after", d)
		}
		return err
	}
	var t store.Tile
	if err := step(o.tickAttempt(), "remove old primary", func(c context.Context) error {
		if err := o.nodeUp(c, m.NodeID); err != nil {
			return err
		}
		inst, err := o.operated.Get(c, m.InstanceID)
		if err != nil {
			return err
		}
		if t, err = o.tiles.Get(c, inst.TileID); err != nil {
			return err
		}
		if err := o.removeOldPrimary(docker.WithNode(c, m.NodeID), t, m); err != nil {
			return err
		}
		m, err = o.fillSubnet(c, t, m)
		return err
	}); err != nil {
		return err
	}
	if err := step(rejoinRewindWait, "rewind", func(c context.Context) error { return o.rewind(c, t, m) }); err != nil {
		return err
	}
	return step(o.tickAttempt(), "start", func(c context.Context) (err error) {
		if m, err = o.members.SetFenced(c, m, false); err != nil {
			return err
		}
		if m, err = o.members.SetPhase(c, m, opmember.Starting); err != nil {
			return err
		}
		o.probeRuns.Delete(m.ID)
		o.rerouteVIPsAsync(c) // its flow to the new primary
		return o.startMember(c, m)
	})
}

// fillSubnet gives a member that has no network block on its row (its node was
// down when the failover swapped the rows) the one its node's instance network
// holds, then makes the block count: the new primary lets it replicate in and
// the replica flow to the new primary is built, both before the rewind. The
// making runs on every attempt (it is repeatable), so one that failed or was
// cut off is redone by the retry although the subnet is already saved.
func (o *Orchestrator) fillSubnet(ctx context.Context, t store.Tile, m InstanceMember) (InstanceMember, error) {
	if m.Subnet == "" {
		_, cidr, err := o.docker.MemberAddr(docker.WithNode(ctx, m.NodeID), operated.Network(m.InstanceID), "")
		if err != nil && !errors.Is(err, docker.ErrNotFound) {
			return m, err
		}
		if cidr == "" {
			return m, nil // the node has no such network yet; the standby's own creation makes it
		}
		if m, err = o.members.SetSubnet(ctx, m, cidr); err != nil {
			return m, err
		}
	}
	if err := o.engines.PrepareStandby(ctx, t, mflow.SlotName(m.ID), o.replSubnets(ctx, m.InstanceID, "")); err != nil {
		return m, err
	}
	o.rerouteVIPs(ctx)
	return m, nil
}

// removeOldPrimary removes what a fenced old primary left on its node: the
// tile's own container (never again beside the new primary), which holds the
// member's data volume until it goes, and the primary's pooler.
func (o *Orchestrator) removeOldPrimary(nctx context.Context, t store.Tile, m InstanceMember) error {
	old, err := o.oldPrimaryContainers(nctx, t, m.ID)
	if err != nil {
		return err
	}
	for _, c := range old {
		if c.State == "running" || c.State == "restarting" {
			o.fastStop(nctx, c.ID)
		}
		if err := o.docker.StopRemove(nctx, c.ID); err != nil {
			return err
		}
	}
	ps, err := o.docker.List(nctx, map[string]string{tile.LabelTile: t.ID, tile.LabelRole: mflow.PoolerRole})
	if err != nil {
		return err
	}
	for _, c := range ps { // the standby's own pooler comes with the tick
		if err := o.docker.StopRemove(nctx, c.ID); err != nil {
			return err
		}
	}
	return nil
}

// rewind runs pg_rewind in a one-shot container on the member's volume. A
// rewind that fails (the old primary was not shut down cleanly, the
// timelines cannot be joined) is not an error here: the volume is left
// without its completion marker and the standby container copies afresh.
func (o *Orchestrator) rewind(ctx context.Context, t store.Tile, m InstanceMember) error {
	host, err := o.memberHost(ctx, m, m.NodeID)
	if err != nil {
		return err
	}
	spec, err := o.engines.RewindSpec(ctx, t, m, "stackr-rewind-"+m.ID[:8], host)
	if err != nil {
		return err
	}
	pass, err := o.engines.RewindPassfile(ctx, t)
	if err != nil {
		return err
	}
	_, code, err := o.oneShot(ctx, m.NodeID, spec, pass)
	if err != nil {
		return err
	}
	if code != 0 {
		slog.Warn("failover: rewind failed, the standby is copied afresh", "member", m.ID, "exit", code)
	}
	return nil
}

// oneShot runs spec to completion on node and returns its output and exit
// code. passfile, when set, is copied in before the start.
func (o *Orchestrator) oneShot(ctx context.Context, node string, spec docker.ContainerSpec, passfile []byte) (string, int, error) {
	return o.oneShotFor(ctx, node, spec, passfile, 10*time.Minute, 50)
}

// ensureHelperImage is images.Ensure for a helper image, as the primary pooler's
// pull: a copy already on the node (a rig build, a registry outage) is good enough.
func (o *Orchestrator) ensureHelperImage(nctx context.Context, ref string) error {
	_, err := o.images.Ensure(nctx, ref, "", io.Discard)
	if _, lerr := o.docker.LocalDigest(nctx, ref); err != nil && lerr == nil {
		return nil
	}
	return err
}

// oneShotFor is oneShot with the wait and the lines of output it keeps.
func (o *Orchestrator) oneShotFor(ctx context.Context, node string, spec docker.ContainerSpec, passfile []byte, wait time.Duration, tail int) (string, int, error) {
	nctx := docker.WithNode(ctx, node)
	if err := o.ensureHelperImage(nctx, spec.Image); err != nil {
		return "", 0, err
	}
	if old, err := o.docker.List(nctx, spec.Labels); err == nil {
		for _, c := range old { // a leftover of a run that was cut short
			if c.Name == spec.Name {
				_ = o.docker.StopRemove(nctx, c.ID)
			}
		}
	}
	id, err := o.docker.Create(nctx, spec)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = o.docker.StopRemove(context.WithoutCancel(nctx), id) }()
	if passfile != nil {
		dir, name := path.Split(mflow.ReplPassfile)
		if err := o.docker.CopyTo(nctx, id, strings.TrimSuffix(dir, "/"), name, 0o600, passfile); err != nil {
			return "", 0, err
		}
	}
	if err := o.docker.Start(nctx, id); err != nil {
		return "", 0, err
	}
	wctx, cancel := context.WithTimeout(nctx, wait)
	defer cancel()
	code, err := o.docker.Wait(wctx, id)
	if err != nil {
		return "", 0, err
	}
	out, _ := o.docker.Logs(nctx, id, tail)
	return out, code, nil
}

// ---- helpers ----

// memberVolume is the Docker volume of member id's data: its own when it
// has one, else the instance tile's data volume.
func (o *Orchestrator) memberVolume(ctx context.Context, t store.Tile, id string) (string, error) {
	m, err := o.members.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if m.Volume != "" {
		return m.Volume, nil
	}
	v, err := o.dataVolume(ctx, t)
	if err != nil {
		return "", err
	}
	return v.Name, nil
}

// primarySQL runs sql in the instance's primary (wherever it runs).
func (o *Orchestrator) primarySQL(ctx context.Context, t store.Tile, sql string) (string, error) {
	cs, err := o.tiles.Replicas(ctx, t)
	if err != nil {
		return "", err
	}
	argv, err := o.engines.PsqlArgv(ctx, t, sql)
	if err != nil {
		return "", err
	}
	for _, c := range cs {
		if c.State == "running" {
			return o.tiles.Exec(ctx, t.ID, c.ID, argv)
		}
	}
	return "", errs.Conflictf("%s is not running", t.Slug)
}

// replayLSN is how far standby m has replayed its WAL.
func (o *Orchestrator) replayLSN(ctx context.Context, t store.Tile, m InstanceMember) (uint64, error) {
	if err := o.nodeUp(ctx, m.NodeID); err != nil {
		return 0, err
	}
	nctx := docker.WithNode(ctx, m.NodeID)
	cs, err := o.docker.List(nctx, map[string]string{LabelMember: m.ID})
	if err != nil {
		return 0, err
	}
	argv, err := o.engines.PsqlArgv(ctx, t, mflow.SQLReplayLSN)
	if err != nil {
		return 0, err
	}
	for _, c := range cs {
		if c.State == "running" {
			out, err := o.docker.Exec(nctx, c.ID, argv)
			if err != nil {
				return 0, err
			}
			return mflow.ParseLSN(out)
		}
	}
	return 0, errors.New("the standby is not running")
}

// poll runs f every failPoll until it says done, errors, or failWait is up.
func (o *Orchestrator) poll(ctx context.Context, f func() (bool, error)) error {
	deadline := time.Now().Add(failWait)
	for {
		ok, err := f()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("timed out")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(failPoll):
		}
	}
}

// nodeForgotten says whether the node's row is gone (the admin forgot it).
func (o *Orchestrator) nodeForgotten(ctx context.Context, id string) bool {
	_, err := o.store.Nodes.Get(ctx, id)
	return errors.Is(err, errs.ErrNotFound)
}

// adoptPrimary makes a relocated primary the instance tile's own again when
// the tile's env now runs on the primary's node: the data volume row names
// the member's volume, the member's container is removed (the deploy that
// follows makes the tile's own on it) and the primary row follows the tile
// again. On any other node the instance tile's deploy is refused.
func (o *Orchestrator) adoptPrimary(ctx context.Context, t store.Tile) error {
	p, ok := o.relocated(ctx, t.ID)
	if !ok {
		return nil
	}
	en, err := o.envNode(ctx, t.EnvironmentID)
	if err != nil {
		return err
	}
	if en != p.NodeID {
		return errs.Conflictf("%s's primary runs on %s after a failover. Move the environment there, or switch over back, before deploying.", t.Slug, o.nodeName(ctx, p.NodeID))
	}
	v, err := o.dataVolume(ctx, t)
	if err != nil {
		return err
	}
	// a pg-upgrade holds the volume when it is its own restart that adopts
	if err := o.volumeIdle(ctx, v, string(kindPGUpgrade)); err != nil {
		return err
	}
	inst, err := o.operated.GetByTile(ctx, t.ID)
	if err != nil {
		return err
	}
	nctx := docker.WithNode(ctx, p.NodeID)
	cs, err := o.docker.List(nctx, map[string]string{LabelMember: p.ID})
	if err != nil {
		return err
	}
	if inst.PITRDays > 0 {
		// The tile's container archives into <tile>-wal, the member's into
		// its own spool: the chain does not cross the change. Ship what the
		// member has (best effort: the pause below is what keeps the range
		// honest), hold the archive, and start the tile's container on an
		// empty spool; the tick arms the pause once it runs.
		if st, _ := o.pitrState(ctx, inst.ID); st.Paused {
			// already held: the shipper ships nothing, so nothing to drain
		} else if err := o.foDrain(ctx, t, inst, nil); err != nil {
			slog.Warn("pitr: member spool not drained before adoption", "instance", inst.ID, "err", err)
		}
		if err := o.pitrHold(ctx, t, inst, ""); err != nil {
			return err
		}
	}
	for _, c := range cs {
		if err := o.docker.StopRemove(nctx, c.ID); err != nil {
			return err
		}
	}
	// the volume row first: with it pointing at the member's data, a crash
	// before the row below leaves a relocated primary whose container is
	// gone, which the tick starts again from the same volume
	v.Name, v.NodeID = p.Volume, ""
	if err := o.store.Volumes.Update(ctx, v); err != nil {
		return err
	}
	if inst.PITRDays > 0 {
		if err := o.cleanSpool(ctx, t, p); err != nil {
			return err
		}
	}
	p.NodeID, p.Container, p.Volume, p.Subnet = "", "", "", ""
	p.UpdatedAt = time.Now().UTC()
	return o.store.OperatedMembers.Update(ctx, p)
}

// cleanSpool empties the tile's own archive volume (it may hold segments of
// the timeline before the failover) by removing it: the deploy declares and
// creates it again. The member's spool goes with the member. Before the row
// above is cleared, so a failure retries the adoption.
func (o *Orchestrator) cleanSpool(ctx context.Context, t store.Tile, p InstanceMember) error {
	nctx := docker.WithNode(ctx, p.NodeID)
	if sv, err := o.volumes.BySlug(ctx, volume.Scope{Kind: "env", ID: t.EnvironmentID}, mflow.ArchiveSlug(t.Slug)); err == nil {
		cs, err := o.docker.List(nctx, map[string]string{tile.LabelTile: t.ID})
		if err != nil {
			return err
		}
		for _, c := range cs { // the tile's own old container, stopped since the failover
			if err := o.docker.StopRemove(nctx, c.ID); err != nil {
				return err
			}
		}
		if err := o.docker.RemoveVolume(nctx, sv.Name); err != nil {
			return err
		}
	}
	_ = o.docker.RemoveVolume(nctx, mflow.MemberArchiveVolume(p))
	return nil
}

// ---- the primary down ----

// observePrimaries raises the problem of a primary that is down while a
// standby could take over: its node does not answer, or its container is not
// running and the panel's own start does not bring it back. It names the way
// out (the drawer's database tab). Cleared when the primary runs.
func (o *Orchestrator) observePrimaries(ctx context.Context) {
	all, err := o.members.All(ctx)
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, m := range all {
		if m.Role != opmember.Replica || seen[m.InstanceID] || m.State == opmember.Removing || m.Phase == opmember.Rebuild || m.Phase == opmember.Rewind || m.Fenced {
			continue
		}
		seen[m.InstanceID] = true
		o.observePrimary(ctx, m.InstanceID)
	}
	// a stalled failover is judged whatever its members look like: after the
	// swap the only other member is a fenced replica that waits for a rewind
	rows, err := o.store.Settings.ListPrefix(ctx, "pgfail.")
	if err != nil {
		return
	}
	for k := range rows {
		if id := strings.TrimPrefix(k, "pgfail."); !seen[id] {
			seen[id] = true
			o.observePrimary(ctx, id)
		}
	}
}

func (o *Orchestrator) observePrimary(ctx context.Context, instanceID string) {
	inst, err := o.operated.Get(ctx, instanceID)
	if err != nil {
		return
	}
	t, err := o.tiles.Get(ctx, inst.TileID)
	if err != nil {
		return
	}
	stuckKey := "alert." + AlertPGFailoverStuck + "." + instanceID
	if f, ok := o.failoverOf(ctx, instanceID); ok {
		if f.Error != "" || time.Since(f.Started) > stuckAfter {
			o.pitrProblem(ctx, stuckKey, AlertPGFailoverStuck, SevCritical, t.Slug,
				t.Slug+": the "+f.Kind+" is stuck at "+f.Step+" and needs attention. See the database tab.")
		}
		return
	}
	o.pitrProblem(ctx, stuckKey, AlertPGFailoverStuck, "", "", "")
	key := "alert." + AlertPGPrimaryDown + "." + instanceID
	if o.primaryRuns(ctx, t) {
		o.downSince.Delete(instanceID)
		o.pitrProblem(ctx, key, AlertPGPrimaryDown, "", "", "")
		return
	}
	since := time.Now()
	if v, loaded := o.downSince.LoadOrStore(instanceID, since); loaded {
		since = v.(time.Time)
	}
	if time.Since(since) < downAfter {
		return
	}
	o.pitrProblem(ctx, key, AlertPGPrimaryDown, SevCritical, t.Slug,
		t.Slug+": the primary is down and a standby is available. Promote it from the database tab.")
}

// primaryRuns says whether the primary is up, for the alert: one the user
// stopped on purpose is not an outage.
func (o *Orchestrator) primaryRuns(ctx context.Context, t store.Tile) bool {
	if inst, err := o.operated.GetByTile(ctx, t.ID); err == nil {
		if ms, err := o.members.List(ctx, inst.ID); err == nil {
			if p, ok := opmember.PrimaryOf(ms); ok && p.State == opmember.Stopped {
				return true
			}
		}
	}
	up, _ := o.primaryUp(ctx, t)
	return up
}

// primaryUp says whether the primary's container runs; unreachable says its
// node does not answer. A node that cannot be asked reads as down.
func (o *Orchestrator) primaryUp(ctx context.Context, t store.Tile) (up, unreachable bool) {
	pn, err := o.primaryNode(ctx, t)
	if err != nil {
		return true, false
	}
	if o.nodeUp(ctx, pn) != nil {
		return false, true
	}
	cs, err := o.docker.List(docker.WithNode(ctx, pn), map[string]string{tile.LabelTile: t.ID, tile.LabelRole: "replica"})
	if err != nil {
		return true, false // cannot tell: no verdict
	}
	return slices.ContainsFunc(cs, func(c docker.Container) bool { return c.State == "running" || c.State == "restarting" }), false
}

// FailoverOptions is what the drawer offers: the standbys a switchover may go
// to, the ones a promote may (when the primary is down), whether the primary's
// node answers, and the failover in flight.
type FailoverOptions struct {
	Primary     string   // the primary's node
	Switch      []string // node names
	Promote     []string // node names; empty while the primary is up
	Unreachable bool     // the primary's node does not answer
	InFlight    string   // "switchover: promote", "" = none
	Error       string   // why the last run stopped
	CanAbort    bool     // a switchover stuck before its promote step
}

// FailoverOptionsOf is tile tileID's FailoverOptions; ok is false for an
// instance with no standby.
func (o *Orchestrator) FailoverOptionsOf(ctx context.Context, tileID string) (FailoverOptions, bool, error) {
	t, inst, err := o.memberInstance(ctx, tileID)
	if err != nil {
		return FailoverOptions{}, false, err
	}
	ms, err := o.members.List(ctx, inst.ID)
	if err != nil {
		return FailoverOptions{}, false, err
	}
	var fo FailoverOptions
	if !opmember.HasReplica(ms) {
		return fo, false, nil
	}
	pn, err := o.primaryNode(ctx, t)
	if err != nil {
		return fo, false, err
	}
	fo.Primary = o.nodeName(ctx, pn)
	up, unreachable := o.primaryUp(ctx, t)
	fo.Unreachable = unreachable
	type cand struct {
		name string
		lsn  uint64
		ok   bool
	}
	var cands []cand
	for _, m := range ms {
		if !usableStandby(m) {
			continue
		}
		// the chip's notion (standbyWord): the stored phase, or the prober's
		// streaming answer; the phase lags a rejoin by minutes
		if up && (m.Phase == opmember.Streaming || m.Phase == opmember.Starting && o.probeRun(m.ID) > 0) {
			fo.Switch = append(fo.Switch, o.nodeName(ctx, m.NodeID))
		}
		if !up {
			l, err := o.replayLSN(ctx, t, m)
			cands = append(cands, cand{o.nodeName(ctx, m.NodeID), l, err == nil})
		}
	}
	slices.Sort(fo.Switch)
	// most advanced first, unreadable last: the form offers the first
	slices.SortFunc(cands, func(a, b cand) int {
		if a.ok != b.ok {
			if a.ok {
				return -1
			}
			return 1
		}
		if a.lsn != b.lsn {
			if a.lsn > b.lsn {
				return -1
			}
			return 1
		}
		return strings.Compare(a.name, b.name)
	})
	for _, c := range cands {
		fo.Promote = append(fo.Promote, c.name)
	}
	if f, ok := o.failoverOf(ctx, inst.ID); ok {
		fo.InFlight, fo.Error = f.Kind+": "+f.Step, f.Error
		steps := failoverSteps[foSwitchover]
		fo.CanAbort = f.Kind == foSwitchover && slices.Index(steps, f.Step) < slices.Index(steps, stepPromote)
	}
	return fo, true, nil
}

// dropRelocated removes what a deleted instance's primary left on the node a
// failover moved it to: its member container, pooler, WAL archive volume and
// the instance network. The data volume is orphaned by the delete (kept for
// its retention), as on the instance's own node.
func (o *Orchestrator) dropRelocated(ctx context.Context, t store.Tile, p InstanceMember) error {
	nctx := docker.WithNode(ctx, p.NodeID)
	cs, err := o.docker.List(nctx, map[string]string{LabelMember: p.ID})
	if err != nil {
		return err
	}
	ps, err := o.docker.List(nctx, map[string]string{tile.LabelTile: t.ID, tile.LabelRole: mflow.PoolerRole})
	if err != nil {
		return err
	}
	for _, c := range append(cs, ps...) {
		if err := o.docker.StopRemove(nctx, c.ID); err != nil {
			return err
		}
	}
	if err := o.removePoolers(nctx, p); err != nil {
		return err
	}
	_ = o.docker.RemoveVolume(nctx, mflow.MemberArchiveVolume(p))
	return o.envs.DropShared(nctx, operated.Network(p.InstanceID))
}
