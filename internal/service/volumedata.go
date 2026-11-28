package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

const (
	kindVolumeWipe   jobs.Kind = "volume-wipe"
	kindVolumeDelete jobs.Kind = "volume-delete"
)

func init() {
	dataCaps[kindVolumeWipe], dataCaps[kindVolumeDelete] = 12*time.Hour, 12*time.Hour
}

// volumeDataJob is the payload of a wipe or a delete with "Back up first".
type volumeDataJob struct {
	VolumeID    string `json:"volume_id"`
	StopTiles   bool   `json:"stop_tiles,omitempty"`
	BackupFirst bool   `json:"backup_first,omitempty"`
	DestID      string `json:"dest_id,omitempty"` // "" = local
}

// deleteBackupFailed starts the error of a delete job whose backup-first step
// failed: nothing was deleted. The job row keeps only text, so this prefix is
// the record; DeleteBackupFailed is the one reader.
const deleteBackupFailed = "backup before the delete failed, nothing was deleted"

// DeleteBackupFailed says whether j is a volume delete that failed in its
// backup-first step, before anything was deleted.
func DeleteBackupFailed(j Job) bool {
	return j.Kind == string(kindVolumeDelete) && j.State == "failed" && strings.HasPrefix(j.Error, deleteBackupFailed)
}

// destArg is the id runBackup takes for d: "" means the local disk.
func destArg(d BackupDest) string {
	if d.Kind == backup.Local {
		return ""
	}
	return d.ID
}

// BackupFirstDest is where "Back up first" writes: the destination of the
// volume's newest good scheduled run when that is off-box, else the first
// off-box destination the org sees, else the local disk (which always exists).
func (o *Orchestrator) BackupFirstDest(ctx context.Context, volumeID string) (BackupDest, error) {
	v, err := o.volumes.Get(ctx, volumeID)
	if err != nil {
		return BackupDest{}, err
	}
	org, err := o.volumeOrg(ctx, v)
	if err != nil {
		return BackupDest{}, err
	}
	rs, err := o.backups.Runs(ctx, v.ID)
	if err != nil {
		return BackupDest{}, err
	}
	for _, r := range rs { // newest first
		if r.Trigger == "schedule" && r.Status == backup.Done {
			if d, err := o.backups.For(ctx, org, r.DestID); err == nil && d.Kind != backup.Local {
				return d, nil
			}
			break
		}
	}
	if d, ok := o.visibleOffBox(ctx, org); ok {
		return d, nil
	}
	return o.localDest(ctx)
}

// wipeChecks are the refusals of a wipe: the tiles that mount the volume, and
// those of them that run now.
func (o *Orchestrator) wipeChecks(ctx context.Context, v Volume) (mounting, running []Tile, err error) {
	switch {
	case v.ScopeKind != "env":
		return nil, nil, errs.Refusedf("Only environment volumes can be wiped.")
	case v.OrphanedAt != nil:
		return nil, nil, errs.Conflictf("%s is removed. Restore it first.", v.Slug)
	case v.InstanceID != nil:
		return nil, nil, errs.Conflictf("%s belongs to an operated instance and cannot be wiped", v.Slug)
	}
	if mounting, err = o.mounters(ctx, v); err != nil {
		return nil, nil, err
	}
	for _, t := range mounting {
		up, err := o.tiles.AnyRunning(ctx, []Tile{t})
		if err != nil {
			return nil, nil, err
		}
		if up {
			running = append(running, t)
		}
	}
	return mounting, running, nil
}

func tileSlugs(ts []Tile) string {
	var s []string
	for _, t := range ts {
		s = append(s, t.Slug)
	}
	return strings.Join(s, ", ")
}

func inUse(v Volume, running []Tile) error {
	return errs.Conflictf("%s is in use by %s. Stop them, or turn on Stop tiles using it.", v.Slug, tileSlugs(running))
}

// WipeVolume queues the emptying of an env volume: its data goes, the volume,
// its mounts, cap and backup settings stay. Refused while a tile that mounts
// it runs, unless stopTiles stops them first (they start again afterwards).
// backupFirst backs the volume up first; if that fails nothing is wiped.
func (o *Orchestrator) WipeVolume(ctx context.Context, id string, stopTiles, backupFirst bool) (Job, error) {
	v, err := o.volumes.Get(ctx, id)
	if err != nil {
		return Job{}, err
	}
	mounting, running, err := o.wipeChecks(ctx, v)
	if err != nil {
		return Job{}, err
	}
	if len(running) > 0 && !stopTiles {
		return Job{}, inUse(v, running)
	}
	if err := o.volumeNodeUp(ctx, v.ID); err != nil {
		return Job{}, err
	}
	if err := o.volumeBusy(ctx, v); err != nil {
		return Job{}, err
	}
	if err := o.stoppedByOtherJob(ctx, v, mounting); err != nil {
		return Job{}, err
	}
	p := volumeDataJob{VolumeID: v.ID, StopTiles: stopTiles, BackupFirst: backupFirst}
	if backupFirst {
		d, err := o.BackupFirstDest(ctx, v.ID)
		if err != nil {
			return Job{}, err
		}
		p.DestID = destArg(d)
	}
	lock := []string{"volume:" + v.ID}
	for _, t := range mounting {
		lock = append(lock, t.ID)
	}
	return o.enqueueExclusive(ctx, kindVolumeWipe, p, lock[:1], errs.Conflictf("%s is already being wiped.", v.Slug), lock...)
}

// stoppedByOtherJob refuses while a live backup or restore of another volume
// holds one of the tiles (it locks only its own volume, stops the tile and
// starts it again by itself, which would be mid-wipe).
func (o *Orchestrator) stoppedByOtherJob(ctx context.Context, v Volume, mounting []Tile) error {
	if len(mounting) == 0 {
		return nil
	}
	live, err := o.jobRows.List(ctx, job.Queued, job.Waiting, job.Running)
	if err != nil {
		return err
	}
	for _, j := range live {
		var id string
		switch j.Kind {
		case string(kindBackup):
			var p backupJob
			if json.Unmarshal([]byte(j.Payload), &p) != nil || p.Mode == backup.Live {
				continue
			}
			id = p.VolumeID
		case string(kindRestore):
			var p restoreJob
			if json.Unmarshal([]byte(j.Payload), &p) != nil {
				continue
			}
			id = p.TargetVolumeID
		default:
			continue
		}
		if id == v.ID {
			continue
		}
		other, err := o.volumes.Get(ctx, id)
		if err != nil {
			continue
		}
		ts, err := o.mounters(ctx, other)
		if err != nil {
			return err
		}
		for _, t := range ts {
			if slices.ContainsFunc(mounting, func(m Tile) bool { return m.ID == t.ID }) {
				return errs.Conflictf("%s is stopped by a backup of %s. Try again when it is done.", t.Slug, other.Slug)
			}
		}
	}
	return nil
}

func (o *Orchestrator) runVolumeWipe(ctx context.Context, r *jobs.Run, p volumeDataJob) (runErr error) {
	v, err := o.volumes.Get(ctx, p.VolumeID)
	if err != nil {
		return err
	}
	mounting, running, err := o.wipeChecks(ctx, v)
	if err != nil {
		return err
	}
	if len(running) > 0 && !p.StopTiles {
		return inUse(v, running)
	}
	// before any tile stops: an image pull is not downtime
	if err := o.volumes.EnsureTool(ctx, v); err != nil {
		return err
	}
	if p.StopTiles && len(running) > 0 {
		_, _ = fmt.Fprintf(r.Log, "stopping %s\n", tileSlugs(running))
		resume, err := o.tiles.QuiesceChecked(ctx, mounting)
		if err != nil {
			return err
		}
		defer func() {
			if rerr := resume(); rerr != nil {
				_, _ = fmt.Fprintf(r.Log, "could not start the tiles again: %v\n", rerr)
				runErr = errors.Join(runErr, fmt.Errorf("tiles did not start again (%s), start them from their pages: %w", tileSlugs(running), rerr))
			} else {
				_, _ = fmt.Fprintf(r.Log, "started %s again\n", tileSlugs(running))
			}
		}()
	}
	if p.BackupFirst {
		_, _ = fmt.Fprintln(r.Log, "backing up first")
		if err := o.runBackup(ctx, r, backupJob{VolumeID: v.ID, DestID: p.DestID}); err != nil {
			_, _ = fmt.Fprintln(r.Log, "the backup failed, nothing was wiped")
			return fmt.Errorf("backup before the wipe failed, nothing was wiped: %w", err)
		}
	}
	_, _ = fmt.Fprintf(r.Log, "wiping %s\n", v.Slug)
	if err := o.volumes.Wipe(ctx, v); err != nil {
		return fmt.Errorf("wiping %s: %w", v.Slug, err)
	}
	_, _ = fmt.Fprintln(r.Log, "wiped")
	return nil
}

func (o *Orchestrator) runVolumeDelete(ctx context.Context, r *jobs.Run, p volumeDataJob) error {
	v, err := o.volumes.Get(ctx, p.VolumeID)
	if err != nil {
		return err
	}
	if _, _, err := o.deleteChecks(ctx, v); err != nil {
		return err
	}
	if p.BackupFirst {
		_, _ = fmt.Fprintln(r.Log, "backing up first")
		if err := o.runBackup(ctx, r, backupJob{VolumeID: v.ID, DestID: p.DestID}); err != nil {
			_, _ = fmt.Fprintln(r.Log, "the backup failed, nothing was deleted")
			return fmt.Errorf(deleteBackupFailed+": %w", err)
		}
	}
	// again: a tile may have mounted it while the backup ran
	by, gone, err := o.deleteChecks(ctx, v)
	if err != nil {
		return err
	}
	if err := o.deleteVolumeNow(ctx, v, by, gone); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(r.Log, "deleted %s\n", v.Slug)
	return nil
}

// VolumeDataJobs is VolumeDataJob for every volume at once: the newest wipe
// or delete job of each, live or ended in the last ten minutes, by volume id.
// Rows only; one read of the job list however many volumes a page shows.
func (o *Orchestrator) VolumeDataJobs(ctx context.Context) (map[string]Job, error) {
	// Live jobs by state, never cut by a count: a long wipe must not drop out
	// behind newer jobs. Terminal ones only matter for ten minutes.
	live, err := o.jobRows.List(ctx, job.Queued, job.Waiting, job.Running)
	if err != nil {
		return nil, err
	}
	ended, err := o.store.Jobs.ListFinishedAfter(ctx, time.Now().Add(-10*time.Minute))
	if err != nil {
		return nil, err
	}
	data := slices.DeleteFunc(append(live, ended...), func(j store.Job) bool {
		return j.Kind != string(kindVolumeWipe) && j.Kind != string(kindVolumeDelete)
	})
	js, err := o.annotate(ctx, data)
	if err != nil {
		return nil, err
	}
	out := map[string]Job{}
	for _, j := range js { // live first, then ended, each newest first
		var p volumeDataJob
		if json.Unmarshal([]byte(j.Payload), &p) != nil {
			continue
		}
		if _, seen := out[p.VolumeID]; seen {
			continue
		}
		if j.FinishedAt != nil && time.Since(*j.FinishedAt) > 10*time.Minute {
			out[p.VolumeID] = Job{} // the newest ended long ago: none
			continue
		}
		out[p.VolumeID] = j
	}
	return out, nil
}

// VolumeDataJob is the newest wipe or delete job of the volume: a live one,
// or one that ended in the last ten minutes. Rows only.
func (o *Orchestrator) VolumeDataJob(ctx context.Context, volumeID string) (Job, bool, error) {
	m, err := o.VolumeDataJobs(ctx)
	j, ok := m[volumeID]
	return j, ok && j.ID != "", err
}
