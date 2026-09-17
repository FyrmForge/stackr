package backup

// Backups and restores run on the work queue (docs/plans/33-workqueue.md,
// steps 4 and 5). A button enqueues and returns; the job runs on the queue's
// own context, and a restart mid-job is cleaned up on boot instead of leaving
// a container paused or a service scaled to zero.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/config/settings"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/workqueue"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Work-queue kinds.
const (
	RunKind     = "backup.run"
	RestoreKind = "backup.restore"
)

// Steps written while a job runs. Restart cleanup reads them to decide what
// has to be put back.
const (
	stepPaused    = "paused"    // containers frozen for the tar
	stepStopped   = "stopped"   // service scaled to zero for the tar
	stepRestoring = "restoring" // writing over live data
)

type runJob struct {
	BackupID string `json:"backup_id"`
	RunID    string `json:"run_id"`
}

type restoreJob struct {
	BackupID string `json:"backup_id"`
	RunID    string `json:"run_id"`
}

type restoreProgress struct {
	PreRun string `json:"pre_run"`
}

// WithWork registers both kinds. Call before the queue starts, or boot
// recovery finds no handler for an interrupted backup.
func (s *Service) WithWork(q *workqueue.Queue) *Service {
	s.work = q
	q.Register(RunKind, func(ctx context.Context, j *workqueue.Job) error {
		var p runJob
		if err := j.Payload(&p); err != nil {
			return err
		}
		run, err := s.store.GetBackupRun(ctx, p.RunID)
		if err != nil || run == nil {
			return fmt.Errorf("backup run %s is gone", p.RunID)
		}
		b, err := s.store.GetBackup(ctx, p.BackupID)
		if err != nil || b == nil {
			err = fmt.Errorf("backup not found")
			s.closeRun(run, err)
			return err
		}
		if !s.begin(b.ID) {
			err = fmt.Errorf("a run or restore for this backup is already in progress")
			s.closeRun(run, err)
			return err
		}
		defer s.end(b.ID)
		_, err = s.run(ctx, b, run, func(st string) { j.SetStep(ctx, st) })
		return err
	}, workqueue.KindOpts{
		// As long as it takes: a volume archive has no size ceiling.
		Timeout:   24 * time.Hour,
		Limit:     func(r settings.Resolved) int { return r.BackupRunConcurrency },
		OnRestart: workqueue.Fail,
		Cleanup:   s.cleanupRun,
	})
	q.Register(RestoreKind, func(ctx context.Context, j *workqueue.Job) error {
		var p restoreJob
		if err := j.Payload(&p); err != nil {
			return err
		}
		b, err := s.store.GetBackup(ctx, p.BackupID)
		if err != nil || b == nil {
			return fmt.Errorf("backup not found")
		}
		return s.restore(ctx, j, b, p.RunID)
	}, workqueue.KindOpts{
		Timeout:   24 * time.Hour,
		Limit:     func(r settings.Resolved) int { return r.BackupRestoreConcurrency },
		OnRestart: workqueue.Fail,
		Cleanup:   s.cleanupRestore,
	})
	return s
}

// Start queues one backup and returns its run row, already written so the
// history can show it waiting. trigger is "schedule" or "manual".
func (s *Service) Start(ctx context.Context, backupID, trigger string) (*repo.BackupRun, error) {
	if s.work == nil {
		return nil, fmt.Errorf("backups are not available")
	}
	b, err := s.store.GetBackup(ctx, backupID)
	if err != nil || b == nil {
		return nil, fmt.Errorf("backup not found")
	}
	run, err := s.openRun(ctx, b, trigger, "queued")
	if err != nil {
		return nil, err
	}
	// Keyed by the run, so two clicks are two runs: the claim in the handler
	// refuses the second, and its row says why.
	if _, err := s.work.Enqueue(ctx, RunKind, run.ID, runJob{BackupID: b.ID, RunID: run.ID}); err != nil {
		s.closeRun(run, err)
		return nil, err
	}
	return run, nil
}

// StartRestore queues a restore of one run and returns the work item id. The
// checks that can fail at once are made here, so the button answers.
func (s *Service) StartRestore(ctx context.Context, backupID, runID string) (string, error) {
	if s.work == nil {
		return "", fmt.Errorf("backups are not available")
	}
	b, err := s.store.GetBackup(ctx, backupID)
	if err != nil || b == nil {
		return "", fmt.Errorf("backup not found")
	}
	if _, err := s.restorable(ctx, b, runID); err != nil {
		return "", err
	}
	return s.work.Enqueue(ctx, RestoreKind, b.ID, restoreJob{BackupID: b.ID, RunID: runID})
}

// LatestRestore is the newest restore of a backup, nil when there has been
// none. The page and the CLI read a restore's state from here.
func (s *Service) LatestRestore(ctx context.Context, backupID string) (*repo.WorkItem, error) {
	return s.store.LatestWorkItem(ctx, RestoreKind, backupID)
}

// cleanupRun deals with a backup a restart interrupted: the containers it
// froze go back, the scratch file goes, and the run row is closed.
func (s *Service) cleanupRun(ctx context.Context, j *workqueue.Job) string {
	var p runJob
	_ = j.Payload(&p)
	s.release(ctx, p.BackupID, j.Item.Step)
	s.removeScratch(p.BackupID)
	msg := "The panel restarted mid backup. The partial archive was deleted."
	if run, _ := s.store.GetBackupRun(ctx, p.RunID); run != nil && !run.FinishedAt.Valid {
		s.closeRun(run, fmt.Errorf("%s", msg))
	}
	return msg
}

// cleanupRestore deals with a restore a restart interrupted. Before the
// restore touched the data it is the same as a backup. After, the service is
// left stopped: starting an app on half its data is worse than leaving it down.
func (s *Service) cleanupRestore(ctx context.Context, j *workqueue.Job) string {
	var p restoreJob
	_ = j.Payload(&p)
	s.removeScratch(p.BackupID)
	msg := "The panel restarted before the restore wrote anything. Nothing changed."
	if j.Item.Step == stepRestoring {
		msg = "The panel restarted mid restore. The data may be half restored, so the service was left stopped."
		s.stopFor(ctx, p.BackupID)
	} else {
		s.release(ctx, p.BackupID, j.Item.Step)
	}
	var pp restoreProgress
	if j.Item.Progress != "" {
		_ = json.Unmarshal([]byte(j.Item.Progress), &pp)
	}
	if pp.PreRun != "" {
		if run, _ := s.store.GetBackupRun(ctx, pp.PreRun); run != nil && !run.FinishedAt.Valid {
			s.closeRun(run, fmt.Errorf("the panel restarted mid backup"))
		}
	}
	return msg
}

// release puts back what a backup froze, by the step it had reached.
func (s *Service) release(ctx context.Context, backupID, step string) {
	if step != stepPaused && step != stepStopped {
		return
	}
	b, t := s.backupTile(ctx, backupID)
	if b == nil || t == nil {
		return
	}
	if step == stepStopped {
		if name := s.serviceOf(ctx, t); name != "" {
			if err := s.c.ScaleService(ctx, name, 1); err != nil {
				slog.Error("backup cleanup: starting the service again", "backup", backupID, "error", err)
			}
		}
		return
	}
	node, err := s.on(ctx, t)
	if err != nil {
		return
	}
	cids, err := s.containersFor(ctx, t)
	if err != nil {
		slog.Error("backup cleanup: finding paused containers", "backup", backupID, "error", err)
		return
	}
	for _, cid := range cids {
		// Errors on a container that is not paused, which is fine.
		_ = s.c.UnpauseContainer(ctx, node, cid)
	}
}

// stopFor scales the tile's service to zero. A dump restore runs against a
// live database, so an interrupted one has to be stopped here; a volume
// restore already is, and swarm keeps it at zero.
func (s *Service) stopFor(ctx context.Context, backupID string) {
	_, t := s.backupTile(ctx, backupID)
	if t == nil {
		return
	}
	if _, err := s.quiesce(ctx, t); err != nil {
		slog.Error("restore cleanup: stopping the service", "backup", backupID, "error", err)
	}
}

func (s *Service) backupTile(ctx context.Context, backupID string) (*repo.Backup, *repo.Tile) {
	b, err := s.store.GetBackup(ctx, backupID)
	if err != nil || b == nil || !b.TileID.Valid {
		return b, nil
	}
	t, _ := s.store.GetTile(ctx, b.TileID.String)
	return b, t
}

// removeScratch deletes the local spool files a backup or restore of this id
// left behind. Nothing was uploaded: PutObject either lands whole or not at all.
func (s *Service) removeScratch(backupID string) {
	if backupID == "" {
		return
	}
	matches, _ := filepath.Glob(filepath.Join(s.scratchDir(), backupID+"-*"))
	for _, m := range matches {
		_ = os.Remove(m)
	}
}
