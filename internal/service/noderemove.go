package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	fbackup "github.com/FyrmForge/stackr/internal/service/internal/flow/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Remove node is a job: stop the node's containers, back up every volume on
// it into one group, check the group, run the uninstall on the box through
// the host helper, then forget the node. A failed backup starts the
// containers again and leaves the node where it is.

const kindRemoveNode jobs.Kind = "remove-node"

// labelFilesTile marks config-file volumes: rebuilt on deploy, not data.
const labelFilesTile = "stackr.files.tile"

// hostUninstall runs `stackrd host-run uninstall --yes` on a node. Tests
// swap it; it must run before the mesh drops the node (dropNode).
var hostUninstall = (*Orchestrator).runHostUninstall

// exitGuard refuses while the node is another node's default exit: dropping
// it would silently clear that node's exit.
func (o *Orchestrator) exitGuard(ctx context.Context, n store.Node) error {
	rungs, err := o.store.ExitLadders.ListByNode(ctx, n.ID)
	if err != nil {
		return err
	}
	for _, r := range rungs {
		if r.ScopeKind == scopeNode {
			return errs.Conflictf("%s is the default exit of %s; point it elsewhere first.", n.Name, o.nodeName(ctx, r.ScopeID))
		}
	}
	return nil
}

// removeGuards are the refusals of a remove, at the click and again when the
// job runs.
func (o *Orchestrator) removeGuards(ctx context.Context, n store.Node) error {
	es, err := o.store.Environments.ListByNode(ctx, n.ID)
	if err != nil {
		return err
	}
	if len(es) > 0 {
		return errs.Conflictf("%s still runs %d envs. Move them first, or forget the node if it is gone for good.", n.Name, len(es))
	}
	if err := o.exitGuard(ctx, n); err != nil {
		return err
	}
	if di, err := o.DefaultIngress(ctx); err != nil {
		return err
	} else if di == n.ID {
		return errs.Conflictf("%s is the default ingress; make another node the default ingress first.", n.Name)
	}
	return o.nodeUp(ctx, n.ID)
}

// RemoveNode (server admin) queues the removal of a worker that has no envs
// left. Move them first.
func (o *Orchestrator) RemoveNode(ctx context.Context, by *Principal, id string) (Job, error) {
	if err := nodeAdmin(by); err != nil {
		return Job{}, err
	}
	n, err := o.workerNode(ctx, id)
	if err != nil {
		return Job{}, err
	}
	if err := o.removeGuards(ctx, n); err != nil {
		return Job{}, err
	}
	return o.enqueue(ctx, kindRemoveNode, nodeJob{NodeID: n.ID, By: by.User.ID, KeyID: by.KeyID}, "node:"+n.ID)
}

// runRemoveNode is the "remove-node" job.
func (o *Orchestrator) runRemoveNode(ctx context.Context, r *jobs.Run, p nodeJob) error {
	n, err := o.workerNode(ctx, p.NodeID)
	if err != nil {
		return err
	}
	if err := o.removeGuards(ctx, n); err != nil {
		return err
	}
	nctx := docker.WithNode(ctx, n.ID)
	stopped, err := o.stopNodeContainers(nctx, r.Log)
	if err != nil {
		o.startContainers(nctx, stopped, r.Log)
		return err
	}
	fail := func(err error) error {
		o.startContainers(nctx, stopped, r.Log)
		return fmt.Errorf("%w; %s not removed", err, n.Name)
	}
	subs, err := o.nodeSubjects(nctx, n)
	if err != nil {
		return fail(err)
	}
	g, dest, err := o.openGroup(ctx, n, groupRemove, groupLabel(n, time.Now(), ", before remove"))
	if err != nil {
		return fail(err)
	}
	if err := o.closeGroup(ctx, g, o.backupSubjects(ctx, r.Log, n, g, dest, subs)); err != nil {
		return fail(err)
	}
	_, _ = fmt.Fprintf(r.Log, "backups checked; cleaning %s\n", n.Name)
	if err := hostUninstall(o, ctx, n.ID, r.Log); err != nil {
		return fail(fmt.Errorf("uninstall: %w", err))
	}
	// The orphans' archives are in the group.
	orphans, err := o.store.Volumes.ListByNode(ctx, n.ID)
	if err != nil {
		return err
	}
	for _, v := range orphans {
		if err := o.store.Volumes.Delete(ctx, v.ID); err != nil {
			return err
		}
	}
	by := &Principal{User: User{ID: p.By}, KeyID: p.KeyID}
	return o.dropNode(ctx, by, n, auditNodeRemove, false)
}

// stopNodeContainers stops what runs on the node (the host helper aside) and
// returns the ids it stopped, in order.
func (o *Orchestrator) stopNodeContainers(ctx context.Context, log io.Writer) ([]string, error) {
	cs, err := o.docker.List(ctx, map[string]string{docker.LabelManaged: "true"})
	if err != nil {
		return nil, err
	}
	var stopped []string
	for _, c := range cs {
		if c.State != "running" || c.Labels[installspec.LabelRole] == "host-helper" {
			continue
		}
		_, _ = fmt.Fprintf(log, "stopping %s\n", c.Name)
		if err := o.docker.Stop(ctx, c.ID); err != nil {
			return stopped, fmt.Errorf("stopping %s: %w", c.Name, err)
		}
		stopped = append(stopped, c.ID)
	}
	return stopped, nil
}

func (o *Orchestrator) startContainers(ctx context.Context, ids []string, log io.Writer) {
	ctx = context.WithoutCancel(ctx)
	for _, id := range ids {
		if err := o.docker.Start(ctx, id); err != nil {
			_, _ = fmt.Fprintf(log, "could not start %s again: %v\n", id, err)
		}
	}
}

// nodeSubjects is one backup subject per stackr Docker volume on the node.
// A volume that is an orphan pinned to the node runs as its row. Any other
// (a move leftover shares its name with the env's row on another node) has
// no row here: it runs by its Docker name, on this node.
func (o *Orchestrator) nodeSubjects(ctx context.Context, n store.Node) ([]fbackup.Subject, error) {
	infos, err := o.docker.ListVolumes(ctx, map[string]string{docker.LabelManaged: "true"})
	if err != nil {
		return nil, err
	}
	orphans, err := o.store.Volumes.ListByNode(ctx, n.ID)
	if err != nil {
		return nil, err
	}
	var out []fbackup.Subject
	for _, vi := range infos {
		if _, files := vi.Labels[labelFilesTile]; files {
			continue
		}
		v := store.Volume{Name: vi.Name, Slug: vi.Name, NodeID: n.ID}
		if i := slices.IndexFunc(orphans, func(v store.Volume) bool { return v.Name == vi.Name }); i >= 0 {
			v = orphans[i]
		}
		out = append(out, fbackup.Subject{Volume: v})
	}
	return out, nil
}

// backupSubjects backs each subject up into the group, then checks that
// every run is done and holds bytes.
func (o *Orchestrator) backupSubjects(
	ctx context.Context, log io.Writer, n store.Node, g store.BackupGroup, dest store.BackupDest, subs []fbackup.Subject,
) error {
	sp := o.groupSpec(n, g, dest)
	for _, s := range subs {
		_, _ = fmt.Fprintf(log, "backing up %s\n", s.Volume.Name)
		if _, err := o.backup.Backup(ctx, s, sp, log); err != nil {
			return fmt.Errorf("backup of %s failed: %w", s.Volume.Name, err)
		}
	}
	rs, err := o.store.BackupRuns.ListByGroup(ctx, g.ID)
	if err != nil {
		return err
	}
	if len(rs) != len(subs) {
		return fmt.Errorf("%d backups recorded for %d volumes", len(rs), len(subs))
	}
	var bad []error
	for _, r := range rs {
		if r.Status != groupDone || r.SizeBytes <= 0 {
			bad = append(bad, fmt.Errorf("backup %s is %s with %d bytes", r.ID, r.Status, r.SizeBytes))
		}
	}
	return errors.Join(bad...)
}

// runHostUninstall runs the uninstall on the box through the host helper.
// The helper only starts a transient unit on the host and exits; the unit
// outlives it and removes the helper container with the rest.
func (o *Orchestrator) runHostUninstall(ctx context.Context, nodeID string, log io.Writer) error {
	if o.cfg.FakeHost != "" {
		_, _ = fmt.Fprintln(log, "fake host: nothing to uninstall")
		return nil
	}
	img, err := o.controllerStackrImage(ctx)
	if err != nil {
		return err
	}
	ref := stackrSystemRef(img)
	nctx := docker.WithNode(ctx, nodeID)
	_ = o.docker.StopRemove(nctx, hostHelperName) // a leftover of an earlier run; a stop is never a swap commit
	if err := o.docker.Pull(nctx, ref, "", log); err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	id, err := o.docker.Run(nctx, hostHelperSpec(ref, "host-run", "uninstall", "--yes"))
	if err != nil {
		return err
	}
	code, err := o.docker.Wait(nctx, id)
	if err != nil {
		return err
	}
	if code != 0 {
		out, _ := o.docker.Logs(nctx, id, 20)
		return fmt.Errorf("host helper exited %d: %s", code, out)
	}
	return nil
}
