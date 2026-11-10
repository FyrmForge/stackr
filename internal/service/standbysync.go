package service

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// What keeps a standby in step with its primary: the settings, the slot cap
// and the slots themselves.

// recoveryLimits are the settings a standby may not have below its primary's
// (postgres refuses to replay WAL otherwise), with postgres's default for
// when no override sets them. Raised, the standbys restart first; lowered,
// the primary does, and a standby follows once the primary runs with the
// lower value (standbyMayRestart).
var recoveryLimits = map[string]int{
	"max_connections":           100,
	"max_worker_processes":      8,
	"max_wal_senders":           10, // owned, never moves; here for completeness
	"max_prepared_transactions": 0,
	"max_locks_per_transaction": 64,
}

// standbyMayRestart says whether a standby may restart with settings cur:
// none of its recovery limits is then below what the running primary has.
func standbyMayRestart(cur map[string]string, live map[string]mflow.LiveSetting) bool {
	for name, def := range recoveryLimits {
		want := def
		if v, ok := cur[name]; ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				return false
			}
			want = n
		}
		have, err := strconv.Atoi(live[name].Value)
		if err != nil || want < have {
			return false
		}
	}
	return true
}

// standbyConn is a running standby's container.
type standbyConn struct {
	m   InstanceMember
	ctx context.Context
	id  string
}

func (c standbyConn) exec(ctx context.Context, o *Orchestrator) func(context.Context, []string) (string, error) {
	return func(_ context.Context, cmd []string) (string, error) { return o.docker.Exec(c.ctx, c.id, cmd) }
}

// runningStandbys are the instance's standbys whose container runs.
func (o *Orchestrator) runningStandbys(ctx context.Context, instanceID string) []standbyConn {
	ms, err := o.members.List(ctx, instanceID)
	if err != nil {
		return nil
	}
	var out []standbyConn
	for _, m := range ms {
		if m.Role != opmember.Replica || m.State == opmember.Removing || o.nodeUp(ctx, m.NodeID) != nil {
			continue
		}
		nctx := docker.WithNode(ctx, m.NodeID)
		cs, err := o.docker.List(nctx, map[string]string{LabelMember: m.ID})
		if err != nil {
			continue
		}
		if i := slices.IndexFunc(cs, func(c docker.Container) bool { return c.State == "running" }); i >= 0 {
			out = append(out, standbyConn{m, nctx, cs[i].ID})
		}
	}
	return out
}

// applyToStandbys writes cur's settings file on every running standby of the
// instance, as the primary's was (a reload, or a reload that leaves them
// pending a restart). One that refuses puts back the ones before it and the
// error is returned; the caller puts the primary back. A standby that is
// not running takes the settings at its next start (startMember).
func (o *Orchestrator) applyToStandbys(ctx context.Context, t Tile, instanceID string, cur map[string]string, changed, unset []string) ([]standbyConn, error) {
	var done []standbyConn
	for _, c := range o.runningStandbys(ctx, instanceID) {
		if _, err := o.engines.ApplySettingsOn(ctx, t, c.exec(ctx, o), cur, changed, unset); err != nil {
			old, _ := o.operated.Get(ctx, instanceID)
			back, backUnset := mflow.ChangedSettings(cur, old.Settings)
			o.undoStandbys(ctx, t, done, old.Settings, back, backUnset)
			return nil, fmt.Errorf("standby on %s: %w", o.nodeName(ctx, c.m.NodeID), err)
		}
		done = append(done, c)
	}
	return done, nil
}

// undoStandbys puts the old settings back on standbys.
func (o *Orchestrator) undoStandbys(ctx context.Context, t Tile, sbs []standbyConn, old map[string]string, back, backUnset []string) {
	for _, c := range sbs {
		_, _ = o.engines.ApplySettingsOn(ctx, t, c.exec(ctx, o), old, back, backUnset)
	}
}

// staleConf says whether container c was made with another settings file than
// the spec now has.
func staleConf(c docker.Container, spec docker.ContainerSpec) bool {
	return c.Labels[mflow.LabelConf] != spec.Labels[mflow.LabelConf]
}

// refreshStandby makes standby m's container again from its current spec
// (its data volume stays, and with its completion marker the copy is not
// redone) when it was made with other settings, and the primary's running
// limits allow it. running: the container is up; it is then only made again
// when it has a restart pending (a reload-only change is already live).
// recreated says it did.
func (o *Orchestrator) refreshStandby(ctx context.Context, t store.Tile, m InstanceMember, c docker.Container, running bool) (bool, error) {
	host, err := o.memberHost(ctx, m, m.NodeID)
	if err != nil {
		return false, err
	}
	spec, err := o.engines.StandbySpec(ctx, t, m, host)
	if err != nil || !staleConf(c, spec) {
		return false, err
	}
	inst, err := o.operated.Get(ctx, m.InstanceID)
	if err != nil {
		return false, err
	}
	live, _, err := o.engines.LiveSettings(ctx, t, slices.Sorted(maps.Keys(recoveryLimits)))
	if err != nil || !standbyMayRestart(inst.Settings, live) {
		return false, nil // the primary cannot be asked, or has not taken the lower limit yet
	}
	nctx := docker.WithNode(ctx, m.NodeID)
	if running {
		exec := func(_ context.Context, cmd []string) (string, error) { return o.docker.Exec(nctx, c.ID, cmd) }
		if pending, err := o.engines.StandbyPending(ctx, t, exec); err != nil || !pending {
			return false, err
		}
	}
	if err := o.docker.StopRemove(nctx, c.ID); err != nil {
		return false, err
	}
	return true, o.createStandby(ctx, t, m)
}

// syncStandbyConf makes the running standbys of instance tile t carry the
// instance's settings: the ones with a restart pending are made again, as far
// as the limits allow (see standbyMayRestart); the tick calls it again for
// the rest.
func (o *Orchestrator) syncStandbyConf(ctx context.Context, t Tile) {
	inst, err := o.operated.GetByTile(ctx, t.ID)
	if err != nil {
		return
	}
	for _, c := range o.runningStandbys(ctx, inst.ID) {
		cs, err := o.docker.List(c.ctx, map[string]string{LabelMember: c.m.ID})
		if err != nil || len(cs) == 0 {
			continue
		}
		if _, err := o.refreshStandby(ctx, t, c.m, cs[0], true); err != nil {
			slog.Warn("members: standby settings", "member", c.m.ID, "err", err)
		}
	}
}

// reconcileSlots drops the stackr slots of a running primary that no member
// row (live or removing) stands for: what a removal left when its drop was cut
// short.
func (o *Orchestrator) reconcileSlots(ctx context.Context, t Tile, inst OperatedInstance) {
	slots, err := o.engines.SlotStates(ctx, t)
	if err != nil || len(slots) == 0 {
		return
	}
	ms, err := o.members.List(ctx, inst.ID)
	if err != nil {
		return
	}
	for slot, st := range slots {
		if st == "" && slices.ContainsFunc(ms, func(m InstanceMember) bool { return m.Role == opmember.Replica && mflow.SlotName(m.ID) == slot }) {
			// no restart_lsn: the slot holds no WAL. Flagged, not recreated: a drop
			// would end a walsender that may be serving the standby right now.
			slog.Warn("members: slot holds no WAL", "instance", inst.ID, "slot", slot)
		}
		if slices.ContainsFunc(ms, func(m InstanceMember) bool { return m.Role == opmember.Replica && mflow.SlotName(m.ID) == slot }) {
			continue
		}
		if err := o.engines.DropSlot(ctx, t, slot); err != nil {
			slog.Warn("members: stray slot", "instance", inst.ID, "slot", slot, "err", err)
		}
	}
}

// watchStandbys is the rest of the tick's standby work, after observeStandbys:
// routing changes still owed to consumers, and per instance with standbys
// the settings the standbys lag, the slot cap as the free disk moves, and
// stray slots.
func (o *Orchestrator) watchStandbys(ctx context.Context) {
	o.routePending(ctx)
	all, err := o.members.All(ctx)
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, m := range all {
		if m.Role != opmember.Replica || seen[m.InstanceID] {
			continue
		}
		seen[m.InstanceID] = true
		inst, err := o.operated.Get(ctx, m.InstanceID)
		if err != nil {
			continue
		}
		t, err := o.tiles.Get(ctx, inst.TileID)
		if err != nil {
			continue
		}
		o.syncStandbyConf(ctx, t)
		// both need the primary running; an exec that finds it down is no news
		// (the sync pass also writes the rest of stackr-owned.conf)
		if err := o.syncInstance(ctx, t, inst); err == nil {
			o.reconcileSlots(ctx, t, inst)
		}
	}
}
