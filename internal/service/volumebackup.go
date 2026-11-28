package service

import (
	"context"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// volumeBackup reads a volume's non-base schedules: the first one's cron and
// keep, and how many there are (0 = no backup).
func (o *Orchestrator) volumeBackup(ctx context.Context, volumeID string) (cron string, keep, n int, err error) {
	ss, err := o.backups.Schedules(ctx, volumeID)
	if err != nil {
		return "", 0, 0, err
	}
	for _, s := range ss {
		if s.Method == "base" {
			continue
		}
		if n == 0 {
			cron, keep = s.Cron, s.Keep
		}
		n++
	}
	return cron, keep, n, nil
}

// setVolumeBackup makes the volume's non-base schedules exactly one with this
// cron and keep; cron "" removes them all. A new one goes to the first
// off-box destination the org can use, else the local disk.
func (o *Orchestrator) setVolumeBackup(ctx context.Context, volumeID, cron string, keep int) error {
	ss, err := o.backups.Schedules(ctx, volumeID)
	if err != nil {
		return err
	}
	var own []BackupSchedule
	for _, s := range ss {
		if s.Method != "base" {
			own = append(own, s)
		}
	}
	cron = strings.Join(strings.Fields(cron), " ")
	if cron == "" {
		for _, s := range own {
			if err := o.deleteBackupSchedule(ctx, s.ID); err != nil {
				return err
			}
		}
		return nil
	}
	if len(own) == 0 {
		spec := ScheduleSpec{Cron: cron, Keep: keep}
		_, org, _, err := o.volumeFacts(ctx, volumeID)
		if err != nil {
			return err
		}
		if d, ok := o.visibleOffBox(ctx, org); ok {
			spec.DestID = &d.ID
		}
		_, err = o.AddBackupSchedule(ctx, volumeID, spec)
		return err
	}
	if f := own[0]; len(own) > 1 || f.Cron != cron || f.Keep != keep {
		_, err := o.UpdateBackupSchedule(ctx, f.ID, ScheduleSpec{
			Method: f.Method, DestID: f.DestID, Cron: cron, Timezone: f.Timezone, Keep: keep, Mode: f.Mode,
		})
		if err != nil {
			return err
		}
	}
	for _, s := range own[1:] {
		if err := o.deleteBackupSchedule(ctx, s.ID); err != nil {
			return err
		}
	}
	return nil
}

// noBackup marks the live, non-archive volumes of vs with no schedule. One
// read for all of them.
func (o *Orchestrator) noBackup(ctx context.Context, vs []store.Volume) map[string]bool {
	ss, err := o.backups.AllSchedules(ctx)
	if err != nil {
		return nil // unknown is not "none"
	}
	has := map[string]bool{}
	for _, s := range ss {
		has[s.VolumeID] = true
	}
	out := map[string]bool{}
	for _, v := range vs {
		if v.OrphanedAt == nil && !IsArchiveVolume(v) && !has[v.ID] {
			out[v.ID] = true
		}
	}
	return out
}

// VolumeHasBackup says whether the volume has any schedule (the drawer shows
// "No backup" when not).
func (o *Orchestrator) VolumeHasBackup(ctx context.Context, volumeID string) (bool, error) {
	ss, err := o.backups.Schedules(ctx, volumeID)
	return len(ss) > 0, err
}
