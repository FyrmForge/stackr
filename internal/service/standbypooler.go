package service

import (
	"context"
	"log/slog"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
)

// Every running standby has a pgbouncer of its own beside it (flow/operated
// StandbyPooler), serving url_read_pooled on the standby's node under the read
// pooler alias. The auth lookup function lives in the admin database on the
// primary and replicates, so the standby's pooler needs only its config: the
// panel never writes to a standby (no CREATE or ALTER), it makes the
// container and keeps its ini current by a reload, which is the watch tick's
// job (slices come and go, max_connections moves). The pooler carries
// mflow.LabelMemberPooler, not LabelMember, so nothing that looks for the
// standby's postgres finds it; stopMember, dropMember and the promote's
// recreate take it down with the standby.

// poolerContainers are the poolers of member id on its node.
func (o *Orchestrator) poolerContainers(nctx context.Context, memberID string) ([]docker.Container, error) {
	return o.docker.List(nctx, map[string]string{mflow.LabelMemberPooler: memberID})
}

// syncStandbyPoolers is the tick's pass: each standby that runs, is not
// fenced and is not waiting on a rebuild or rewind has its pooler made,
// started and current.
func (o *Orchestrator) syncStandbyPoolers(ctx context.Context) {
	all, err := o.members.All(ctx)
	if err != nil {
		return
	}
	for _, m := range all {
		if m.Role != opmember.Replica || m.State != opmember.Running || m.Fenced || m.Phase == opmember.Rebuild || m.Phase == opmember.Rewind ||
			o.failoverActive(ctx, m.InstanceID) {
			continue
		}
		if err := o.syncStandbyPooler(ctx, m); err != nil {
			slog.Warn("members: standby pooler", "member", m.ID, "err", err)
		}
	}
}

func (o *Orchestrator) syncStandbyPooler(ctx context.Context, m InstanceMember) error {
	if o.nodeUp(ctx, m.NodeID) != nil {
		return nil
	}
	nctx := docker.WithNode(ctx, m.NodeID)
	if cs, err := o.docker.List(nctx, map[string]string{LabelMember: m.ID}); err != nil || len(cs) == 0 {
		return err // no standby container yet: the start makes it
	}
	inst, err := o.operated.Get(ctx, m.InstanceID)
	if err != nil {
		return err
	}
	t, err := o.tiles.Get(ctx, inst.TileID)
	if err != nil {
		return err
	}
	spec, ini, ok, err := o.engines.StandbyPooler(ctx, t, m)
	if err != nil || !ok {
		return err
	}
	cs, err := o.poolerContainers(nctx, m.ID)
	if err != nil {
		return err
	}
	if len(cs) == 0 {
		if err := o.ensureHelperImage(nctx, spec.Image); err != nil {
			return err
		}
		id, err := o.docker.Create(nctx, spec)
		if err != nil {
			return err
		}
		return o.docker.Start(nctx, id)
	}
	c := cs[0]
	if c.State != "running" {
		return o.docker.Start(nctx, c.ID) // current from the ini it keeps; the next pass reloads a stale one
	}
	cur, err := o.docker.Exec(nctx, c.ID, mflow.PoolerIniArgv())
	if err != nil || strings.TrimSpace(cur) == strings.TrimSpace(ini) {
		return err
	}
	_, err = o.docker.Exec(nctx, c.ID, mflow.PoolerReload(ini))
	return err
}

// removePoolers stops and removes member m's pooler containers.
func (o *Orchestrator) removePoolers(nctx context.Context, m InstanceMember) error {
	cs, err := o.poolerContainers(nctx, m.ID)
	if err != nil {
		return err
	}
	for _, c := range cs {
		if err := o.docker.StopRemove(nctx, c.ID); err != nil {
			return err
		}
	}
	return nil
}
