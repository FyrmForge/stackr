package service

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strconv"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
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

// staleImage says whether container c was made from another image than the
// spec now has. One made before the image label existed is judged by the
// image it runs, which is the reference it was made from.
func staleImage(c docker.Container, spec docker.ContainerSpec) bool {
	return cmp.Or(c.Labels[mflow.LabelImage], c.Image) != spec.Image
}

// primaryOnImage says whether the instance's primary container runs on image:
// a standby only follows an image change the primary has made (an update that
// failed on the primary leaves the stored image ahead of it). The primary's
// container runs an image ID or the registry copy; the reference it was made
// for is its label. A primary deployed before the label existed runs an ID
// and does not match, so nothing follows until it is redeployed; one made by a
// failover before the label existed was created straight from the tag by
// PromotedSpec, so its image already equals the reference, and standbys left
// behind by earlier minor updates are made again (one per pass, every guard
// applying) right after stackr is upgraded.
func (o *Orchestrator) primaryOnImage(ctx context.Context, t store.Tile, image string) bool {
	node, err := o.primaryNode(ctx, t)
	if err != nil {
		return false
	}
	cs, err := o.docker.List(docker.WithNode(ctx, node), map[string]string{tile.LabelTile: t.ID, tile.LabelRole: "replica"})
	return err == nil && slices.ContainsFunc(cs, func(c docker.Container) bool {
		return c.State == "running" && cmp.Or(c.Labels[mflow.LabelImage], c.Image) == image
	})
}

// othersStreaming says whether every other standby of the instance that is
// not being removed or rebuilt was seen streaming by the last probe: an image
// change takes one down, and a synchronous primary needs another to wait for.
func (o *Orchestrator) othersStreaming(ctx context.Context, m InstanceMember) bool {
	ms, err := o.members.List(ctx, m.InstanceID)
	if err != nil {
		return false
	}
	return !slices.ContainsFunc(ms, func(x InstanceMember) bool {
		return x.ID != m.ID && x.Role == opmember.Replica && x.State != opmember.Removing &&
			x.Phase != opmember.Rebuild && (o.nodeUp(ctx, x.NodeID) != nil || o.probeRun(x.ID) <= 0) // a down node's last probe is stale
	})
}

// standbyEligible says whether member row cur may still be made again: a
// replica not being removed or rebuilt, not fenced, and (when its container
// runs) not stopped since.
func standbyEligible(cur InstanceMember, running bool) bool {
	return cur.Role == opmember.Replica && cur.State != opmember.Removing && cur.Phase != opmember.Rebuild &&
		opmember.MayStart(cur) == nil && (!running || cur.State != opmember.Stopped)
}

// liveJob says whether tile t has a job under way: an extensions or image job
// may still roll the image back.
func (o *Orchestrator) liveJob(ctx context.Context, t store.Tile) (bool, error) {
	j, ok, err := o.jobRows.Last(ctx, t.ID)
	return err == nil && ok && job.Live(j.State), err
}

// standbyStillDue re-reads tile t and rebuilds m's spec from it, under the
// topology lock after the pull of pulled: it returns the fresh tile and
// whether the container is still only image-stale against that spec, the
// spec's image is the one pulled and the primary runs on it.
func (o *Orchestrator) standbyStillDue(ctx context.Context, t store.Tile, m InstanceMember, c docker.Container, pulled string) (store.Tile, bool, error) {
	fresh, err := o.tiles.Get(ctx, t.ID)
	if err != nil {
		return t, false, err
	}
	host, err := o.memberHost(ctx, m, m.NodeID)
	if err != nil {
		return t, false, err
	}
	spec, err := o.engines.StandbySpec(ctx, fresh, m, host)
	if err != nil {
		return t, false, err
	}
	return fresh, spec.Image == pulled && staleImage(c, spec) && !staleConf(c, spec) && o.primaryOnImage(ctx, fresh, pulled), nil
}

// refreshStandby makes standby m's container again from its current spec
// (its data volume stays, and with its completion marker the copy is not
// redone) when it was made with other settings, and the primary's running
// limits allow it, or from another image, once the primary runs on that one
// (and the tile has no job, the other standbys stream and no failover or
// topology change is under way). A container made with other settings is made
// again at once, whatever its image. running: the container is up; it is then
// only made again when it has a restart pending (a reload-only change is
// already live) or its image is old. recreated says it did; byImage that it
// did for the image.
func (o *Orchestrator) refreshStandby(ctx context.Context, t store.Tile, m InstanceMember, c docker.Container, running bool) (recreated, byImage bool, err error) {
	host, err := o.memberHost(ctx, m, m.NodeID)
	if err != nil {
		return false, false, err
	}
	spec, err := o.engines.StandbySpec(ctx, t, m, host)
	if err != nil {
		return false, false, err
	}
	// a settings-stale container is made again at once, whatever its image
	// (it is made from the current spec); only an image-only change waits
	img := staleImage(c, spec) && !staleConf(c, spec)
	if !img && !staleConf(c, spec) {
		return false, false, nil
	}
	if img && !o.primaryOnImage(ctx, t, spec.Image) {
		return false, false, nil
	}
	inst, err := o.operated.Get(ctx, m.InstanceID)
	if err != nil {
		return false, false, err
	}
	live, _, err := o.engines.LiveSettings(ctx, t, slices.Sorted(maps.Keys(recoveryLimits)))
	if err != nil || !standbyMayRestart(inst.Settings, live) {
		return false, false, nil // the primary cannot be asked, or has not taken the lower limit yet
	}
	nctx := docker.WithNode(ctx, m.NodeID)
	if running && !img {
		exec := func(_ context.Context, cmd []string) (string, error) { return o.docker.Exec(nctx, c.ID, cmd) }
		if pending, err := o.engines.StandbyPending(ctx, t, exec); err != nil || !pending {
			return false, false, err
		}
	}
	if img {
		if live, err := o.liveJob(ctx, t); err != nil || live {
			return false, false, err
		}
		if !o.othersStreaming(ctx, m) {
			return false, false, nil
		}
		// a failed pull leaves the old container running
		if _, err := o.images.Ensure(nctx, spec.Image, "", io.Discard); err != nil {
			return false, false, err
		}
		mu := o.topoLock(m.InstanceID)
		if !mu.TryLock() {
			return false, false, nil
		}
		defer mu.Unlock()
		// the pull took a while: the row and the tile may have moved on
		cur, err := o.members.Get(ctx, m.ID)
		if err != nil || !standbyEligible(cur, running) || o.failoverActive(ctx, m.InstanceID) {
			return false, false, nil
		}
		if live, err := o.liveJob(ctx, t); err != nil || live {
			return false, false, err
		}
		m = cur
		// the image may have changed again during the pull: the container is
		// made from the tile as it is now, or the pass is skipped
		fresh, ok, err := o.standbyStillDue(ctx, t, m, c, spec.Image)
		if err != nil || !ok {
			return false, false, err
		}
		t = fresh
	}
	if err := o.docker.StopRemove(nctx, c.ID); err != nil {
		return false, false, err
	}
	o.probeRuns.Delete(m.ID) // not streaming until a probe says so
	return true, img, o.createStandby(ctx, t, m)
}

// syncStandbyConf makes the running standbys of instance tile t carry the
// instance's settings and image: the ones with a restart pending are made
// again, as far as the limits allow (see standbyMayRestart); the tick calls it
// again for the rest. It leaves an instance that is failing over alone.
func (o *Orchestrator) syncStandbyConf(ctx context.Context, t Tile) {
	inst, err := o.operated.GetByTile(ctx, t.ID)
	if err != nil || o.failoverActive(ctx, inst.ID) {
		return
	}
	for _, c := range o.runningStandbys(ctx, inst.ID) {
		cs, err := o.docker.List(c.ctx, map[string]string{LabelMember: c.m.ID})
		if err != nil || len(cs) == 0 {
			continue
		}
		_, byImage, err := o.refreshStandby(ctx, t, c.m, cs[0], true)
		if err != nil {
			slog.Warn("members: standby settings", "member", c.m.ID, "err", err)
		}
		// One standby on a new image per pass, so a synchronous primary keeps
		// another to wait for; the tick comes back for the rest.
		if byImage {
			return
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
