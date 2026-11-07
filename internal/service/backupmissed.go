package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// AlertBackupStale fires when a scheduled volume has no good backup for two
// of its intervals.
const AlertBackupStale = "backup.stale"

// scheduleInterval is the gap between two firings of s, read from its cron
// line the way the scheduler reads it. ok is false for a line that will not
// parse.
// ponytail: the gap after the next firing, so an uneven line (0 3 * * 1,3)
// reads as one of its gaps; good enough for "is it overdue".
func scheduleInterval(s store.BackupSchedule, now time.Time) (time.Duration, bool) {
	spec := s.Cron
	if s.Timezone != "" {
		spec = "CRON_TZ=" + s.Timezone + " " + s.Cron
	}
	sc, err := cron.ParseStandard(spec)
	if err != nil {
		return 0, false
	}
	n1 := sc.Next(now)
	n2 := sc.Next(n1)
	if n1.IsZero() || n2.IsZero() {
		return 0, false
	}
	return n2.Sub(n1), true
}

type vol struct {
	interval time.Duration
	since    time.Time // the oldest schedule's creation
}

// backupWatch runs at boot and daily (dailyChores). A schedule whose newest
// run, of any outcome, is older than its interval missed a firing (the panel
// was down): it runs once now. A volume whose newest good run is older than
// two intervals raises the backup.stale alert; a good run clears it. A
// failing run is not retried here: backupResult already alerts on it.
func (o *Orchestrator) backupWatch(ctx context.Context) {
	ss, err := o.backups.AllSchedules(ctx)
	if err != nil {
		slog.Warn("backup: watch not run", "err", err)
		return
	}
	now := time.Now().UTC()
	vols := map[string]*vol{}
	for _, s := range ss {
		iv, ok := scheduleInterval(s, now)
		if !ok {
			continue
		}
		runs, err := o.store.BackupRuns.ListByVolume(ctx, s.VolumeID)
		if err != nil {
			continue
		}
		last := s.CreatedAt
		for _, r := range runs {
			if r.ScheduleID != nil && *r.ScheduleID == s.ID && r.CreatedAt.After(last) {
				last = r.CreatedAt
			}
		}
		if now.Sub(last) > iv {
			slog.Info("backup: catching up a missed schedule", "schedule", s.ID, "volume", s.VolumeID)
			if err := o.scheduleBackup(ctx, s); err != nil {
				slog.Warn("backup: catch-up not queued", "schedule", s.ID, "err", err)
			}
		}
		v := vols[s.VolumeID]
		if v == nil {
			v = &vol{interval: iv, since: s.CreatedAt}
			vols[s.VolumeID] = v
		}
		v.interval = min(v.interval, iv)
		if s.CreatedAt.Before(v.since) {
			v.since = s.CreatedAt
		}
	}
	for id, v := range vols {
		o.staleAlert(ctx, id, v.interval, v.since, now)
	}
	o.unscheduled(ctx, vols)
	// Rows of a volume with no schedule left go.
	o.sweep(ctx, "backup.stale.", func(rest string) bool { _, ok := vols[rest]; return ok })
}

// unscheduled gives a volume that never had a schedule or a backup the
// default (@daily, keep 7) and opens the backup.none warning on every volume
// left without a schedule (one the user took off included). scheduled holds
// the ids that have one.
func (o *Orchestrator) unscheduled(ctx context.Context, scheduled map[string]*vol) {
	vs, err := o.store.Volumes.ListLive(ctx)
	if err != nil {
		return
	}
	keep := map[string]bool{}
	for _, v := range vs {
		if IsArchiveVolume(v) {
			continue // WAL in transit; the data volume's base backup covers it
		}
		key := noneKey(v.ID)
		if scheduled[v.ID] != nil {
			o.clearProblem(ctx, key)
			continue
		}
		if runs, err := o.store.BackupRuns.ListByVolume(ctx, v.ID); err == nil && len(runs) == 0 {
			if _, err := o.AddBackupSchedule(ctx, v.ID, ScheduleSpec{Cron: "@daily", Keep: 7}); err == nil {
				o.clearProblem(ctx, key)
				continue
			}
		}
		keep[v.ID] = true
		o.openProblem(ctx, key, SevWarning, v.Slug+" has no backup.")
	}
	o.sweep(ctx, "backup.none.", func(rest string) bool { return keep[rest] })
}

func noneKey(volumeID string) string { return "alert.backup.none." + volumeID }

func staleKey(volumeID string) string { return "alert.backup.stale." + volumeID }

// staleAlert alerts once when the newest good run of a volume is over two
// intervals old (or, with no good run, the schedule is). A good run clears it.
func (o *Orchestrator) staleAlert(ctx context.Context, volumeID string, interval time.Duration, since, now time.Time) {
	runs, err := o.store.BackupRuns.ListByVolume(ctx, volumeID)
	if err != nil {
		return
	}
	good := since
	for _, r := range runs {
		if r.Status == backup.Done && r.ObjectKey != "" && r.CreatedAt.After(good) {
			good = r.CreatedAt
		}
	}
	key := staleKey(volumeID)
	_, was, err := o.store.Settings.Get(ctx, key)
	if err != nil {
		return
	}
	if now.Sub(good) <= 2*interval {
		o.clearProblem(ctx, key)
		if was {
			_ = o.store.Settings.Delete(ctx, key)
		}
		return
	}
	slug := volumeID
	if v, err := o.volumes.Get(ctx, volumeID); err == nil {
		slug = v.Slug
	} else if errors.Is(err, errs.ErrNotFound) {
		return
	}
	msg := fmt.Sprintf("No good backup of %s in %s.", slug, agoWords(now.Sub(good)))
	o.openProblem(ctx, key, SevWarning, msg)
	if !was && o.store.Settings.Set(ctx, key, "1") == nil {
		o.alert(Alert{Event: AlertBackupStale, Severity: SevWarning, Subject: slug, Message: msg})
	}
}

// agoWords reads a span as "3 days", or "5 hours" under two days.
func agoWords(d time.Duration) string {
	if d < 48*time.Hour {
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}
