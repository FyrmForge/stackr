package service

import (
	"context"
	"io"
	"path/filepath"
	"strings"

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

// panelKeep is how many panel archives stay.
// ponytail: fixed; a settings knob when someone needs another number.
const panelKeep = 14

func (o *Orchestrator) BackupDests(ctx context.Context, orgID string) ([]BackupDest, error) {
	return o.backups.Visible(ctx, orgID)
}

// CreateBackupDest adds an S3 destination; orgID nil = a global one (admin).
func (o *Orchestrator) CreateBackupDest(ctx context.Context, orgID *string, s BackupDestSpec) (BackupDest, error) {
	return o.backups.Create(ctx, orgID, s)
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
	return o.backups.Update(ctx, d, s)
}

func (o *Orchestrator) DeleteBackupDest(ctx context.Context, orgID, id string) error {
	d, err := o.ownDest(ctx, orgID, id)
	if err != nil {
		return err
	}
	return o.backups.Delete(ctx, d)
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
// volume, or another in the same org: cross-volume restore).
func (o *Orchestrator) RestoreBackup(ctx context.Context, runID, sourceVolumeID, targetVolumeID string) (Job, error) {
	src, err := o.volumes.Get(ctx, sourceVolumeID)
	if err != nil {
		return Job{}, err
	}
	dst, err := o.volumes.Get(ctx, targetVolumeID)
	if err != nil {
		return Job{}, err
	}
	a, err := o.volumeOrg(ctx, src)
	if err != nil {
		return Job{}, err
	}
	if b, err := o.volumeOrg(ctx, dst); err != nil {
		return Job{}, err
	} else if a != b {
		return Job{}, errs.ErrNotFound
	}
	return o.enqueue(
		ctx,
		kindRestore,
		restoreJob{RunID: runID, SourceVolumeID: src.ID, TargetVolumeID: dst.ID},
		"volume:"+dst.ID,
	)
}

// PanelBackups lists the panel's own archives.
func (o *Orchestrator) PanelBackups(ctx context.Context) ([]BackupRun, error) {
	return o.backups.PanelRuns(ctx)
}

// PanelBackupNow queues an archive of the panel database.
func (o *Orchestrator) PanelBackupNow(ctx context.Context) (Job, error) {
	return o.enqueue(ctx, kindPanelBackup, nil, "panel-backup")
}

func (o *Orchestrator) runBackup(ctx context.Context, r *jobs.Run, p backupJob) error {
	v, org, _, err := o.volumeFacts(ctx, p.VolumeID)
	if err != nil {
		return err
	}
	sp := fbackup.Spec{Trigger: "manual", Mode: p.Mode}
	destID, method := p.DestID, p.Method
	if p.ScheduleID != "" {
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
	local, err := o.localDest(ctx)
	if err != nil {
		return err
	}
	return o.backup.Restore(ctx, fbackup.Restore{
		RunID:          p.RunID,
		SourceVolumeID: p.SourceVolumeID,
		Target:         sub,
		Pre:            fbackup.Spec{Dest: local, Prefix: backup.Prefix(org, v.ID, "")},
	}, r.Log)
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
		s.Load, err = o.engines.Restore(ctx, it, method, "")
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

func (o *Orchestrator) panelBackup(ctx context.Context, log io.Writer) (store.BackupRun, error) {
	local, err := o.localDest(ctx)
	if err != nil {
		return store.BackupRun{}, err
	}
	return o.backup.PanelBackup(ctx, fbackup.Panel{
		Vacuum: func(ctx context.Context, path string) error {
			_, err := o.db.ExecContext(ctx, "VACUUM INTO ?", path)
			return err
		},
		MasterKey:  o.cfg.SecretsKey,
		Version:    o.cfg.Version,
		Passphrase: o.cfg.Passphrase,
		InstallID:  o.cfg.InstallID,
	}, local, panelKeep, log)
}
