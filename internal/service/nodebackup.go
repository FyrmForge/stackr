package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	fbackup "github.com/FyrmForge/stackr/internal/service/internal/flow/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Back up node: every env on a node, one at a time, into one group. The
// remove job reuses the group machinery (noderemove.go).

const kindNodeBackup jobs.Kind = "node-backup"

// Group reasons and the run trigger of a group's archives.
const (
	groupManual  = "manual"
	groupRemove  = "remove"
	triggerNode  = "node"
	groupRunning = backup.Running
	groupDone    = backup.Done
	groupFailed  = backup.Failed
)

// localFallbackWarning is the group's note when no global destination is set.
const localFallbackWarning = "No backup destination set; archives stay on the controller in %s."

type nodeJob struct {
	NodeID string `json:"node_id"`
	By     string `json:"by,omitempty"`
	KeyID  string `json:"key_id,omitempty"`
}

// nodeOnline is the offline refusal of the node verbs: a node that is not
// online cannot be stopped, backed up or cleaned.
// ponytail: checks the stored status only; switch to lane G's o.nodeUp(ctx, id)
// (which names when the node was last seen) once it is merged.
func (o *Orchestrator) nodeOnline(ctx context.Context, n store.Node) error {
	if n.Status == node.StatusOnline {
		return nil
	}
	return errs.Conflictf("%s is offline; bring it back, or forget it if it is gone for good.", n.Name)
}

// nodeLock is the lock set of a node-wide job: the node and every tile on it,
// so deploys there wait for it.
func (o *Orchestrator) nodeLock(ctx context.Context, n store.Node) ([]string, error) {
	lock := []string{"node:" + n.ID}
	es, err := o.store.Environments.ListByNode(ctx, n.ID)
	if err != nil {
		return nil, err
	}
	for _, e := range es {
		ts, err := o.tiles.List(ctx, e.ID)
		if err != nil {
			return nil, err
		}
		for _, t := range ts {
			lock = append(lock, t.ID)
		}
	}
	return lock, nil
}

// BackupNode (server admin) queues a backup of every volume on the node.
func (o *Orchestrator) BackupNode(ctx context.Context, by *Principal, id string) (Job, error) {
	if err := nodeAdmin(by); err != nil {
		return Job{}, err
	}
	n, err := o.nodeLeaf().Get(ctx, id)
	if err != nil {
		return Job{}, err
	}
	if n.Status == node.StatusPending {
		return Job{}, errs.ErrNotFound
	}
	if err := o.nodeOnline(ctx, n); err != nil {
		return Job{}, err
	}
	lock, err := o.nodeLock(ctx, n)
	if err != nil {
		return Job{}, err
	}
	return o.enqueue(ctx, kindNodeBackup, nodeJob{NodeID: n.ID, By: by.User.ID, KeyID: by.KeyID}, lock...)
}

// BackupEveryNode (server admin) queues one node backup per online node: the
// step before break glass.
func (o *Orchestrator) BackupEveryNode(ctx context.Context, by *Principal) ([]Job, error) {
	if err := nodeAdmin(by); err != nil {
		return nil, err
	}
	ns, err := o.nodeLeaf().List(ctx)
	if err != nil {
		return nil, err
	}
	var out []Job
	for _, n := range ns {
		if n.Status != node.StatusOnline {
			continue
		}
		j, err := o.BackupNode(ctx, by, n.ID)
		if err != nil {
			return out, fmt.Errorf("%s: %w", n.Name, err)
		}
		out = append(out, j)
	}
	return out, nil
}

// NodeBackupsRef is NodeBackups by name (a removed node's groups are read by id).
func (o *Orchestrator) NodeBackupsRef(ctx context.Context, ref string) ([]BackupGroupView, error) {
	id := ref
	if n, err := o.nodeByRef(ctx, ref); err == nil {
		id = n.ID
	}
	return o.NodeBackups(ctx, id)
}

// BackupNodeRef is BackupNode by name.
func (o *Orchestrator) BackupNodeRef(ctx context.Context, by *Principal, ref string) (Job, error) {
	n, err := o.nodeByRef(ctx, ref)
	if err != nil {
		return Job{}, err
	}
	return o.BackupNode(ctx, by, n.ID)
}

// groupDest is where a node-wide group goes: the first global destination
// that is not the local one, else the local one with the fallback warning.
func (o *Orchestrator) groupDest(ctx context.Context) (store.BackupDest, string, error) {
	gs, err := o.backups.Global(ctx)
	if err != nil {
		return store.BackupDest{}, "", err
	}
	for _, g := range gs {
		if g.Kind != backup.Local {
			d, err := o.backups.Get(ctx, g.ID) // Global blanks the keys
			return d, "", err
		}
	}
	d, err := o.localDest(ctx)
	return d, fmt.Sprintf(localFallbackWarning, d.Endpoint), err
}

// openGroup records a running group for the node.
func (o *Orchestrator) openGroup(ctx context.Context, n store.Node, reason, label string) (store.BackupGroup, store.BackupDest, error) {
	dest, warn, err := o.groupDest(ctx)
	if err != nil {
		return store.BackupGroup{}, dest, err
	}
	g := store.BackupGroup{
		ID: uuid.NewString(), NodeID: n.ID, NodeName: n.Name, Label: label, Reason: reason,
		DestID: dest.ID, Warning: warn, Status: groupRunning, CreatedAt: time.Now().UTC(),
	}
	return g, dest, o.store.BackupGroups.Create(ctx, g)
}

// closeGroup records the outcome and passes err through.
func (o *Orchestrator) closeGroup(ctx context.Context, g store.BackupGroup, runErr error) error {
	ctx = context.WithoutCancel(ctx)
	now := time.Now().UTC()
	g.FinishedAt, g.Status, g.Error = &now, groupDone, ""
	if runErr != nil {
		g.Status, g.Error = groupFailed, runErr.Error()
	}
	return errors.Join(runErr, o.store.BackupGroups.Update(ctx, g))
}

func groupLabel(n store.Node, now time.Time, suffix string) string {
	return "node " + n.Name + ", " + now.Format("2 Jan") + suffix
}

func (o *Orchestrator) groupSpec(n store.Node, g store.BackupGroup, dest store.BackupDest) fbackup.Spec {
	// live: the holders are stopped, or (a dump) the engine runs on purpose.
	return fbackup.Spec{
		Dest: dest, Prefix: backup.NodePrefix(n.Name, g.ID), Trigger: triggerNode, Mode: backup.Live, GroupID: g.ID,
	}
}

// runNodeBackup is the "node-backup" job. Per env, one at a time: engine
// volumes by dump while the env runs, then its tiles stop once, the plain
// volumes are tarred and the tiles start again (also when a tar fails).
func (o *Orchestrator) runNodeBackup(ctx context.Context, r *jobs.Run, p nodeJob) error {
	n, err := o.nodeLeaf().Get(ctx, p.NodeID)
	if err != nil {
		return err
	}
	g, dest, err := o.openGroup(ctx, n, groupManual, groupLabel(n, time.Now(), ""))
	if err != nil {
		return err
	}
	sp := o.groupSpec(n, g, dest)
	es, err := o.store.Environments.ListByNode(ctx, n.ID)
	if err != nil {
		return o.closeGroup(ctx, g, err)
	}
	var failed []error
	for _, e := range es {
		_, _ = fmt.Fprintf(r.Log, "backing up env %s\n", e.Slug)
		if err := o.backupEnvInto(ctx, r.Log, e, sp); err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", e.Slug, err))
		}
	}
	if n.Role == node.Controller {
		if err := o.backupControllerScopes(ctx, r.Log, sp); err != nil {
			failed = append(failed, err)
		}
	}
	return o.closeGroup(ctx, g, errors.Join(failed...))
}

// backupEnvInto backs up one env's volumes into the group.
func (o *Orchestrator) backupEnvInto(ctx context.Context, log io.Writer, e store.Environment, sp fbackup.Spec) error {
	vs, err := o.volumes.List(ctx, VolumeScope{Kind: "env", ID: e.ID})
	if err != nil {
		return err
	}
	var failed []error
	var plain []store.Volume
	for _, v := range vs {
		if v.OrphanedAt != nil {
			continue
		}
		ms, err := o.methods(ctx, v)
		if err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", v.Slug, err))
			continue
		}
		if v.InstanceID == nil || len(ms) < 2 { // no dump method: tar it with the rest
			plain = append(plain, v)
			continue
		}
		if err := o.backupOne(ctx, log, v, sp); err != nil {
			failed = append(failed, err)
		}
	}
	if len(plain) == 0 {
		return errors.Join(failed...)
	}
	ts, err := o.tiles.List(ctx, e.ID)
	if err != nil {
		return errors.Join(append(failed, err)...)
	}
	_, _ = fmt.Fprintf(log, "stopping the tiles of %s\n", e.Slug)
	resume, err := o.tiles.Quiesce(ctx, ts)
	if err != nil {
		return errors.Join(append(failed, err)...)
	}
	defer func() {
		_, _ = fmt.Fprintf(log, "starting the tiles of %s\n", e.Slug)
		resume()
	}()
	for _, v := range plain {
		if err := o.backupOne(ctx, log, v, sp); err != nil {
			failed = append(failed, err)
		}
	}
	return errors.Join(failed...)
}

// backupControllerScopes tars the stack and org volumes, which live on the
// controller.
func (o *Orchestrator) backupControllerScopes(ctx context.Context, log io.Writer, sp fbackup.Spec) error {
	ogs, err := o.orgs.ListAll(ctx)
	if err != nil {
		return err
	}
	var failed []error
	for _, og := range ogs {
		scopes := []VolumeScope{{Kind: "org", ID: og.ID}}
		sts, err := o.store.Stacks.ListByOrg(ctx, og.ID)
		if err != nil {
			return err
		}
		for _, st := range sts {
			scopes = append(scopes, VolumeScope{Kind: "stack", ID: st.ID})
		}
		for _, sc := range scopes {
			vs, err := o.volumes.List(ctx, sc)
			if err != nil {
				return err
			}
			for _, v := range vs {
				if v.OrphanedAt == nil {
					failed = append(failed, o.backupOne(ctx, log, v, sp))
				}
			}
		}
	}
	return errors.Join(failed...)
}

// backupOne backs one volume up into the group; an engine volume by its
// default method.
func (o *Orchestrator) backupOne(ctx context.Context, log io.Writer, v store.Volume, sp fbackup.Spec) error {
	sub, err := o.subject(ctx, v, "")
	if err != nil {
		return fmt.Errorf("%s: %w", v.Slug, err)
	}
	if _, err := o.backup.Backup(ctx, sub, sp, log); err != nil {
		return fmt.Errorf("backup of %s failed: %w", cmpName(v), err)
	}
	return nil
}

func cmpName(v store.Volume) string {
	if v.Slug != "" {
		return v.Slug
	}
	return v.Name
}

// BackupGroupVolume is one volume's run in a group.
type BackupGroupVolume struct {
	Volume    string `json:"volume"` // the volume's slug (or Docker name)
	Where     string `json:"where"`  // its env path
	Method    string `json:"method"` // dump | tar
	Status    string `json:"status"`
	SizeBytes int64  `json:"size_bytes"`
	Error     string `json:"error"`
}

// BackupGroupView is a group with its per-volume runs.
type BackupGroupView struct {
	ID         string              `json:"id"`
	Label      string              `json:"label"`
	Reason     string              `json:"reason"`
	NodeName   string              `json:"node"`
	Dest       string              `json:"dest"`
	Warning    string              `json:"warning"`
	Status     string              `json:"status"`
	Error      string              `json:"error"`
	CreatedAt  time.Time           `json:"created_at"`
	FinishedAt *time.Time          `json:"finished_at"`
	Volumes    []BackupGroupVolume `json:"volumes"`
}

// NodeBackups lists the node's backup groups, newest first. A removed
// node's groups stay and are read by its old id.
func (o *Orchestrator) NodeBackups(ctx context.Context, id string) ([]BackupGroupView, error) {
	gs, err := o.store.BackupGroups.ListByNode(ctx, id)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(gs, func(a, b store.BackupGroup) int { return b.CreatedAt.Compare(a.CreatedAt) })
	out := make([]BackupGroupView, 0, len(gs))
	for _, g := range gs {
		v := BackupGroupView{
			ID: g.ID, Label: g.Label, Reason: g.Reason, Warning: g.Warning, Status: g.Status, Error: g.Error,
			NodeName: g.NodeName, CreatedAt: g.CreatedAt, FinishedAt: g.FinishedAt, Volumes: []BackupGroupVolume{},
		}
		if d, err := o.backups.Get(ctx, g.DestID); err == nil {
			v.Dest = d.Name
		}
		rs, err := o.store.BackupRuns.ListByGroup(ctx, g.ID)
		if err != nil {
			return nil, err
		}
		slices.SortFunc(rs, func(a, b store.BackupRun) int { return a.CreatedAt.Compare(b.CreatedAt) })
		for _, r := range rs {
			v.Volumes = append(v.Volumes, o.groupVolume(ctx, r))
		}
		out = append(out, v)
	}
	return out, nil
}

func (o *Orchestrator) groupVolume(ctx context.Context, r store.BackupRun) BackupGroupVolume {
	gv := BackupGroupVolume{Method: "tar", Status: r.Status, SizeBytes: r.SizeBytes, Error: r.Error}
	if strings.HasSuffix(r.ObjectKey, ".sql.gz.age") {
		gv.Method = "dump"
	}
	if r.VolumeID != nil {
		if v, err := o.volumes.Get(ctx, *r.VolumeID); err == nil {
			gv.Volume = v.Slug
			gv.Where = o.scopePath(ctx, v.ScopeKind, v.ScopeID)
			return gv
		}
	}
	// the row is gone (an orphan removed with its node), or never was: the
	// archive is named <utc timestamp>-<docker volume>.<ext>
	if base := path.Base(r.ObjectKey); r.ObjectKey != "" && len(base) > len("20060102-150405-") {
		base = base[len("20060102-150405-"):]
		gv.Volume = strings.TrimSuffix(strings.TrimSuffix(base, ".age"), ".sql.gz")
		gv.Volume = strings.TrimSuffix(gv.Volume, ".tar.gz")
	}
	return gv
}
