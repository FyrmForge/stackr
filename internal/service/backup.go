package service

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	fbackup "github.com/FyrmForge/stackr/internal/service/internal/flow/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

type (
	BackupDest     = store.BackupDest
	BackupSchedule = store.BackupSchedule
	BackupRun      = store.BackupRun
	BackupDestSpec = backup.Dest
	ScheduleSpec   = backup.Schedule
)

func (o *Orchestrator) BackupDests(ctx context.Context, orgID string) ([]BackupDest, error) {
	return o.backups.Visible(ctx, orgID)
}

// CreateBackupDest adds an S3 destination; orgID nil = a global one (admin).
func (o *Orchestrator) CreateBackupDest(ctx context.Context, orgID *string, s BackupDestSpec) (BackupDest, error) {
	return o.backups.Create(ctx, orgID, s, o.probeDest())
}

// GlobalBackupDests lists the admin-global destinations, shared or not.
func (o *Orchestrator) GlobalBackupDests(ctx context.Context) ([]BackupDest, error) {
	return o.backups.Global(ctx)
}

// UpdateBackupDest changes a destination orgID owns; orgID "" = a global
// one (admin). A shared global is visible to an org, never its to change.
func (o *Orchestrator) UpdateBackupDest(ctx context.Context, orgID, id string, s BackupDestSpec) (BackupDest, error) {
	d, err := o.ownDest(ctx, orgID, id)
	if err != nil {
		return d, err
	}
	return o.backups.Update(ctx, d, s, o.probeDest())
}

// probeDest is the test write a destination passes before it is saved;
// none when Config.ProbeDests is off (tests with no bucket behind them).
func (o *Orchestrator) probeDest() backup.Probe {
	if !o.cfg.ProbeDests {
		return nil
	}
	return o.backup.Probe
}

func (o *Orchestrator) DeleteBackupDest(ctx context.Context, orgID, id string) error {
	d, err := o.ownDest(ctx, orgID, id)
	if err != nil {
		return err
	}
	panelDest, err := o.settings.Get(ctx, "panel_backup_dest")
	if err != nil {
		return err
	}
	return o.backups.Delete(ctx, d, panelDest == d.ID)
}

func (o *Orchestrator) ownDest(ctx context.Context, orgID, id string) (BackupDest, error) {
	d, err := o.backups.Get(ctx, id)
	if err != nil {
		return d, err
	}
	owner := ""
	if d.OrgID != nil {
		owner = *d.OrgID
	}
	if owner != orgID {
		return BackupDest{}, errs.ErrNotFound
	}
	return d, nil
}

// BackupMethods are what a volume can be backed up with: its engine's
// methods first (the default), then a tar of the volume.
func (o *Orchestrator) BackupMethods(ctx context.Context, volumeID string) ([]string, error) {
	v, err := o.volumes.Get(ctx, volumeID)
	if err != nil {
		return nil, err
	}
	return o.methods(ctx, v)
}

func (o *Orchestrator) methods(ctx context.Context, v store.Volume) ([]string, error) {
	if v.InstanceID == nil {
		return []string{backup.KindVolume}, nil
	}
	it, err := o.instanceTile(ctx, *v.InstanceID)
	if err != nil {
		return nil, err
	}
	ms, err := o.engines.Methods(ctx, it)
	return append(ms, backup.KindVolume), err
}

func (o *Orchestrator) BackupSchedules(ctx context.Context, volumeID string) ([]BackupSchedule, error) {
	return o.backups.Schedules(ctx, volumeID)
}

// AddBackupSchedule adds a schedule and reloads the cron.
func (o *Orchestrator) AddBackupSchedule(ctx context.Context, volumeID string, s ScheduleSpec) (BackupSchedule, error) {
	v, org, ms, err := o.volumeFacts(ctx, volumeID)
	if err != nil {
		return BackupSchedule{}, err
	}
	b, err := o.backups.AddSchedule(ctx, v.ID, org, ms, s)
	if err == nil {
		o.sched.Reload(ctx)
	}
	return b, err
}

func (o *Orchestrator) UpdateBackupSchedule(ctx context.Context, id string, s ScheduleSpec) (BackupSchedule, error) {
	b, err := o.backups.GetSchedule(ctx, id)
	if err != nil {
		return b, err
	}
	_, org, ms, err := o.volumeFacts(ctx, b.VolumeID)
	if err != nil {
		return b, err
	}
	b, err = o.backups.UpdateSchedule(ctx, b, org, ms, s)
	if err == nil {
		o.sched.Reload(ctx)
	}
	return b, err
}

func (o *Orchestrator) DeleteBackupSchedule(ctx context.Context, id string) error {
	err := o.backups.DeleteSchedule(ctx, id)
	if err == nil {
		o.sched.Reload(ctx)
	}
	return err
}

func (o *Orchestrator) BackupRuns(ctx context.Context, volumeID string) ([]BackupRun, error) {
	return o.backups.Runs(ctx, volumeID)
}

// BackupNow queues a manual backup; destID "" = local, method "" = the default.
func (o *Orchestrator) BackupNow(ctx context.Context, volumeID, destID, method, mode string) (Job, error) {
	return o.enqueue(
		ctx,
		kindBackup,
		backupJob{
			VolumeID: volumeID,
			DestID:   destID,
			Method:   method,
			Mode:     mode,
		},
		"volume:"+volumeID,
	)
}

// RestoreBackup queues a restore of a run of source into target (the same
// volume, or another in the same org: cross-volume restore). A stack key
// (by.Access.KeyStack) restores only inside its own stack.
func (o *Orchestrator) RestoreBackup(ctx context.Context, by *Principal, runID, sourceVolumeID, targetVolumeID string) (Job, error) {
	src, err := o.volumes.Get(ctx, sourceVolumeID)
	if err != nil {
		return Job{}, err
	}
	dst, err := o.volumes.Get(ctx, targetVolumeID)
	if err != nil {
		return Job{}, err
	}
	a, err := o.volumeChild(ctx, src.ID)
	if err != nil {
		return Job{}, err
	}
	b, err := o.volumeChild(ctx, dst.ID)
	if err != nil {
		return Job{}, err
	}
	if a.Org != b.Org {
		return Job{}, errs.ErrNotFound
	}
	if by != nil && by.Access.KeyStack != "" && (a.Stack != by.Access.KeyStack || b.Stack != a.Stack) {
		return Job{}, errs.ErrNotFound
	}
	return o.queueRestore(ctx, runID, src, dst)
}

// queueRestore queues the restore of run (of src) into dst; extra locks are
// held too, so jobs on them queued later wait for the restore.
func (o *Orchestrator) queueRestore(ctx context.Context, runID string, src, dst store.Volume, extra ...string) (Job, error) {
	lock := append([]string{"volume:" + dst.ID}, extra...)
	if dst.InstanceID != nil {
		// No Provision or Bind while the dump loads: a deploy locks its own
		// tile only, so the instance, its slices and their consumers are all
		// held.
		it, err := o.instanceTile(ctx, *dst.InstanceID)
		if err != nil {
			return Job{}, err
		}
		lock = append(lock, it.ID)
		ps, err := o.managed.ByInstance(ctx, *dst.InstanceID)
		if err != nil {
			return Job{}, err
		}
		for _, p := range ps {
			lock = append(lock, p.TileID)
		}
		cs, err := o.consumers(ctx, dst)
		if err != nil {
			return Job{}, err
		}
		for _, c := range cs {
			lock = append(lock, c.ID)
		}
	}
	return o.enqueue(
		ctx,
		kindRestore,
		restoreJob{RunID: runID, SourceVolumeID: src.ID, TargetVolumeID: dst.ID},
		lock...,
	)
}

// PanelBackups lists the panel's own archives.
func (o *Orchestrator) PanelBackups(ctx context.Context) ([]BackupRun, error) {
	return o.backups.PanelRuns(ctx)
}

// PanelBackupNow queues an archive of the panel database to dest: a server
// destination's id or name, "local" for the panel's disk, or "" for the
// scheduled destination (local when there is none).
func (o *Orchestrator) PanelBackupNow(ctx context.Context, dest string) (Job, error) {
	id := ""
	switch dest = strings.TrimSpace(dest); dest {
	case "local":
	case "":
		var err error
		if id, err = o.settings.Get(ctx, "panel_backup_dest"); err != nil {
			return Job{}, err
		}
	default:
		ds, err := o.backups.Global(ctx)
		if err != nil {
			return Job{}, err
		}
		i := slices.IndexFunc(ds, func(d BackupDest) bool { return d.ID == dest || d.Name == dest })
		if i < 0 {
			return Job{}, errs.Invalidf("dest", "no server backup destination %q", dest)
		}
		if ds[i].Kind != backup.Local {
			id = ds[i].ID
		}
	}
	return o.enqueue(ctx, kindPanelBackup, panelBackupJob{DestID: id}, o.panelLock(false)...)
}

// panelLock is a panel backup job's lock set: the panel lock, so it never
// overlaps an upgrade, and a key per origin, so a manual click and a
// scheduled run do not supersede (cancel) each other.
func (o *Orchestrator) panelLock(scheduled bool) []string {
	if scheduled {
		return []string{"panel", "panel-backup:scheduled"}
	}
	return []string{"panel", "panel-backup:manual"}
}

func (o *Orchestrator) runBackup(ctx context.Context, r *jobs.Run, p backupJob) error {
	v, org, _, err := o.volumeFacts(ctx, p.VolumeID)
	if err != nil {
		return err
	}
	sp := fbackup.Spec{Trigger: "manual", Mode: p.Mode}
	destID, method := p.DestID, p.Method
	if p.ScheduleID != "" {
		if err := o.skipUnreachable(ctx, o.volumeNode(ctx, v.ID)); err != nil {
			return err
		}
		s, err := o.backups.GetSchedule(ctx, p.ScheduleID)
		if err != nil {
			return err
		}
		sp.ScheduleID = &s.ID
		sp.Trigger = "schedule"
		sp.Mode = s.Mode
		sp.Keep = s.Keep
		method = s.Method
		destID = ""
		if s.DestID != nil {
			destID = *s.DestID
		}
	}
	if destID == "" {
		sp.Dest, err = o.localDest(ctx)
	} else {
		sp.Dest, err = o.backups.For(ctx, org, destID)
	}
	if err != nil {
		return err
	}
	sp.Prefix = backup.Prefix(org, v.ID, p.ScheduleID)
	sub, err := o.subject(ctx, v, method)
	if err != nil {
		return err
	}
	_, err = o.backup.Backup(ctx, sub, sp, r.Log)
	return err
}

func (o *Orchestrator) runRestore(ctx context.Context, r *jobs.Run, p restoreJob) error {
	v, org, _, err := o.volumeFacts(ctx, p.TargetVolumeID)
	if err != nil {
		return err
	}
	run, err := o.backups.GetRun(ctx, p.RunID)
	if err != nil {
		return err
	}
	method := backup.KindVolume
	if strings.HasSuffix(run.ObjectKey, ".sql.gz.age") {
		method = "dump"
	}
	sub, err := o.subject(ctx, v, method)
	if err != nil {
		return err
	}
	var cluster func(context.Context) error
	if method == "dump" {
		if sub.Consumers, err = o.consumers(ctx, v); err != nil {
			return err
		}
		src, err := o.volumes.Get(ctx, p.SourceVolumeID)
		if err != nil {
			return err
		}
		cluster = func(ctx context.Context) error { return o.clusterRestoreOK(ctx, run, src, v) }
	}
	local, err := o.localDest(ctx)
	if err != nil {
		return err
	}
	return o.backup.Restore(ctx, fbackup.Restore{
		RunID:          p.RunID,
		SourceVolumeID: p.SourceVolumeID,
		Target:         sub,
		Pre:            fbackup.Spec{Dest: local, Prefix: backup.Prefix(org, v.ID, "")},
		Cluster:        cluster,
	}, r.Log)
}

// clusterRestoreOK vets a whole-instance dump before its instance is wiped.
// The dump holds the databases and roles of its own instance: on another it
// would drop that one's slices and load names it does not track, and on its
// own it would drop what was made after the backup.
func (o *Orchestrator) clusterRestoreOK(ctx context.Context, run store.BackupRun, src, dst store.Volume) error {
	if src.InstanceID == nil || dst.InstanceID == nil || *src.InstanceID != *dst.InstanceID {
		return errs.Refusedf("restore a cluster dump onto the instance it came from")
	}
	ps, err := o.managed.ByInstance(ctx, *dst.InstanceID)
	if err != nil {
		return err
	}
	var late []string
	for _, p := range ps {
		if p.CreatedAt.After(run.CreatedAt) {
			late = append(late, "slice "+p.DBName)
		}
		bs, err := o.managed.Bindings(ctx, p.ID)
		if err != nil {
			return err
		}
		for _, b := range bs {
			if b.CreatedAt.After(run.CreatedAt) {
				late = append(late, "binding "+b.DBUser)
			}
		}
	}
	if len(late) > 0 {
		return errs.Refusedf(
			"%s made after this backup would be dropped by the restore: %s; remove them first",
			dst.Slug,
			strings.Join(late, ", "),
		)
	}
	return nil
}

// consumers are the tiles bound to a slice of v's instance, once each.
func (o *Orchestrator) consumers(ctx context.Context, v store.Volume) ([]store.Tile, error) {
	if v.InstanceID == nil {
		return nil, nil
	}
	ps, err := o.managed.ByInstance(ctx, *v.InstanceID)
	if err != nil {
		return nil, err
	}
	var out []store.Tile
	seen := map[string]bool{}
	for _, p := range ps {
		bs, err := o.managed.Bindings(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		for _, b := range bs {
			if seen[b.ConsumerTileID] {
				continue
			}
			seen[b.ConsumerTileID] = true
			t, err := o.tiles.Get(ctx, b.ConsumerTileID)
			if err != nil {
				return nil, err
			}
			out = append(out, t)
		}
	}
	return out, nil
}

// subject gathers what flow/backup needs about a volume: who mounts it, and
// for an instance's volume the engine tile and its dump/load argv.
func (o *Orchestrator) subject(ctx context.Context, v store.Volume, method string) (fbackup.Subject, error) {
	s := fbackup.Subject{Volume: v}
	if v.InstanceID != nil {
		it, err := o.instanceTile(ctx, *v.InstanceID)
		if err != nil {
			return s, err
		}
		s.Holders, s.Engine = []store.Tile{it}, it
		if method == backup.KindVolume {
			return s, nil
		}
		if method == "" {
			ms, err := o.engines.Methods(ctx, it)
			if err != nil || len(ms) == 0 {
				return s, err
			}
			method = ms[0]
		}
		if s.Dump, err = o.engines.Backup(ctx, it, method); err != nil {
			return s, err
		}
		if s.Load, err = o.engines.Restore(ctx, it, method, ""); err != nil {
			return s, err
		}
		s.Marker, err = o.engines.DumpMarker(ctx, it)
		return s, err
	}
	var err error
	s.Holders, err = o.mounters(ctx, v)
	return s, err
}

func (o *Orchestrator) instanceTile(ctx context.Context, instanceID string) (store.Tile, error) {
	m, err := o.managed.Get(ctx, instanceID)
	if err != nil {
		return store.Tile{}, err
	}
	return o.tiles.Get(ctx, m.TileID)
}

// volumeFacts: the row, its org, and its backup methods.
func (o *Orchestrator) volumeFacts(ctx context.Context, id string) (store.Volume, string, []string, error) {
	v, err := o.volumes.Get(ctx, id)
	if err != nil {
		return v, "", nil, err
	}
	org, err := o.volumeOrg(ctx, v)
	if err != nil {
		return v, "", nil, err
	}
	ms, err := o.methods(ctx, v)
	return v, org, ms, err
}

// volumeOrg walks a volume's scope up to its org.
func (o *Orchestrator) volumeOrg(ctx context.Context, v store.Volume) (string, error) {
	return o.scopeOrg(ctx, v.ScopeKind, v.ScopeID)
}

func (o *Orchestrator) scopeOrg(ctx context.Context, kind, id string) (string, error) {
	switch kind {
	case "org":
		return id, nil
	case "env":
		e, err := o.envs.Get(ctx, id)
		if err != nil {
			return "", err
		}
		id = e.StackID
	}
	st, err := o.stacks.Get(ctx, id)
	return st.OrgID, err
}

func (o *Orchestrator) localDest(ctx context.Context) (store.BackupDest, error) {
	return o.backups.EnsureLocal(ctx, filepath.Join(o.cfg.DataDir, "backups"))
}

// panelFixedKeep is how many manual and pre-upgrade archives stay; only the
// scheduled ones follow panel_backup_keep.
const panelFixedKeep = 14

// panelBackup archives the panel under trigger schedule, manual or upgrade,
// each with its own prefix and prune. A scheduled run goes to
// panel_backup_dest (a global destination; local when unset or gone, with a
// warning in the log) and keeps panel_backup_keep. A manual one goes to
// destID (the one Back up now picked, "" local) and the pre-upgrade archive
// stays local; both keep panelFixedKeep.
func (o *Orchestrator) panelBackup(ctx context.Context, log io.Writer, trigger, destID string) (store.BackupRun, error) {
	ctx, err := o.withController(ctx)
	if err != nil {
		return store.BackupRun{}, err
	}
	dest, err := o.localDest(ctx)
	if err != nil {
		return store.BackupRun{}, err
	}
	keep := panelFixedKeep
	id := ""
	switch trigger {
	case "schedule":
		if id, err = o.settings.Get(ctx, "panel_backup_dest"); err != nil {
			return store.BackupRun{}, err
		}
		if keep, err = o.settings.Int(ctx, "panel_backup_keep"); err != nil {
			return store.BackupRun{}, err
		}
	case "manual":
		id = destID
	}
	if id != "" {
		d, err := o.backups.Get(ctx, id)
		if err == nil && d.OrgID != nil {
			err = errs.ErrNotFound
		}
		if err != nil {
			_, _ = fmt.Fprintf(log, "warning: panel backup destination %s is gone (%v); using the local one\n", id, err)
		} else {
			dest = d
		}
	}
	_, _ = fmt.Fprintf(log, "panel backup to %s\n", dest.Name)
	// rollMu: no controller cert renewal or root rollover rewrites key files
	// while they are copied. Nothing here takes rolloverMu, so no lock order
	// to keep; a revoke holds rollMu up to controllerAckBound, and the backup
	// waits that out.
	var lock sync.Locker
	if o.cluster != nil {
		lock = &o.cluster.rollMu
	}
	return o.backup.PanelBackup(ctx, fbackup.Panel{
		Lock: lock,
		Vacuum: func(ctx context.Context, path string) error {
			_, err := o.db.ExecContext(ctx, "VACUUM INTO ?", path)
			return err
		},
		Caddy: func(ctx context.Context, w io.Writer) error {
			// live: the proxy keeps writing (ACME renewals) while it is read.
			if _, err := o.docker.InspectVolume(ctx, installspec.ProxyVolume); err != nil {
				return err
			}
			return o.docker.TarVolume(ctx, installspec.ProxyVolume, w, true)
		},
		Trigger:    trigger,
		MasterKey:  o.cfg.SecretsKey,
		Version:    o.cfg.Version,
		Passphrase: o.cfg.Passphrase,
		InstallID:  o.cfg.InstallID,
		DataDir:    o.cfg.DataDir,
	}, dest, keep, log)
}
