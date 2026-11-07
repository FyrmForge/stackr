package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	fbackup "github.com/FyrmForge/stackr/internal/service/internal/flow/backup"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/backup"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Point-in-time recovery, the panel's half. The instance container archives
// finished WAL segments to its archive volume (flow/managed Container); a
// weekly "base" backup is a normal schedule on the data volume; one goroutine
// ships the segments to the base schedule's destination every minute
// (flow/backup ShipWAL), reads the archiver's health, and keeps the problems
// banner honest. Layout and retention rules: leaf/backup pitr.go.

const (
	// AlertPITRLag fires when the WAL archive stops keeping up; AlertPITRDisk
	// when its volume or the node's disk is filling.
	AlertPITRLag  = "pitr.lag"
	AlertPITRDisk = "pitr.disk"

	// DefaultPITRDays is the window a new postgres instance gets when its org
	// can see an off-box destination.
	DefaultPITRDays = 7

	pitrEvery      = time.Minute
	pitrShipBudget = 5 * time.Minute
	pitrLagWarn    = 10 * time.Minute
	pitrLagCrit    = time.Hour
	pitrDiskCap    = 1 << 30 // about an hour of segments at full tilt
	pitrUsageEvery = 10 * time.Minute
	pitrBaseRetry  = time.Hour
	pitrBaseCron   = "@weekly"
)

// IsBaseKey says whether an archive key is a point-in-time base backup, which
// the point-in-time restore uses and a volume restore refuses.
func IsBaseKey(key string) bool { return backup.IsPITRKey(key) }

// IsArchiveVolume says whether v is an instance's archive volume, which
// holds WAL in transit, not data: no backup of its own, no panel.
func IsArchiveVolume(v Volume) bool { return mflow.IsArchiveVolume(v) }

// pitrState is one instance's shipper state, the settings row pitr.<id>.
type pitrState struct {
	Since          time.Time `json:"since"`           // first tick that saw the instance
	DestID         string    `json:"dest_id"`         // where the guard passed
	Guarded        bool      `json:"guarded"`         // the archive prefix is this instance's
	LastSegment    string    `json:"last_segment"`    // newest segment shipped
	LastShippedAt  time.Time `json:"last_shipped_at"` // when a bundle last went
	BaseTriedAt    time.Time `json:"base_tried_at"`   // the last first-base attempt
	LastCleanAt    time.Time `json:"last_clean_at"`   // a tick that left nothing waiting
	Bytes          int64     `json:"bytes"`           // WAL bundles on the destination
	Bundles        int       `json:"bundles"`
	UsageAt        time.Time `json:"usage_at"`
	Waiting        int       `json:"waiting"`   // files still on the box after the last tick
	DirBytes       int64     `json:"dir_bytes"` // the archive volume's size
	ArchiverFailed int64     `json:"archiver_failed"`
	ArchiverSeen   bool      `json:"archiver_seen"`
	Failing        bool      `json:"failing"` // archive_command failed since the last tick
	Error          string    `json:"error"`
	CheckedAt      time.Time `json:"checked_at"`
}

func pitrKey(instanceID string) string { return "pitr." + instanceID }

func (o *Orchestrator) pitrState(ctx context.Context, instanceID string) (pitrState, bool) {
	var st pitrState
	v, ok, err := o.store.Settings.Get(ctx, pitrKey(instanceID))
	if err != nil || !ok || json.Unmarshal([]byte(v), &st) != nil {
		return pitrState{}, false
	}
	return st, true
}

func (o *Orchestrator) savePITRState(ctx context.Context, instanceID string, st pitrState) {
	b, _ := json.Marshal(st)
	if err := o.store.Settings.Set(ctx, pitrKey(instanceID), string(b)); err != nil {
		slog.Warn("pitr: state not kept", "instance", instanceID, "err", err)
	}
}

// ---- turning it on and off ----

// SetPITR sets the instance's recovery window (0 = off) and queues the
// redeploy that makes postgres archive (or stop): a few seconds of downtime.
// On adds the weekly base backup schedule, off removes it; archived objects
// stay on the destination until they age out under whatever else uses it.
func (o *Orchestrator) SetPITR(ctx context.Context, tileID string, days int) (ManagedInstance, Job, error) {
	t, m, err := o.instanceOf(ctx, tileID)
	if err != nil {
		return m, Job{}, err
	}
	if mflow.Engines[m.Engine].Definition().ArchiveDir == "" {
		return m, Job{}, errs.Invalidf("engine", "%s has no point-in-time recovery", m.Engine)
	}
	if m, err = o.managed.SetPITR(ctx, m, days); err != nil {
		return m, Job{}, err
	}
	o.pitrApplied(ctx, t, m)
	note := "point-in-time recovery is on: restarting to archive WAL"
	if days <= 0 {
		note = "point-in-time recovery is off: restarting to stop archiving"
	}
	j, err := o.deployNoted(ctx, t.ID, note)
	return m, j, err
}

// pitrApplied follows a moved window: the weekly base backup is added or
// dropped, and the shipper's state and problems go when it is off. The
// restart that makes postgres archive is the caller's.
func (o *Orchestrator) pitrApplied(ctx context.Context, t store.Tile, m ManagedInstance) {
	if v, err := o.dataVolume(ctx, t); err == nil {
		if m.PITRDays > 0 {
			o.baseSchedule(ctx, v)
		} else {
			o.dropBaseSchedules(ctx, v)
		}
	}
	if m.PITRDays <= 0 {
		_ = o.store.Settings.Delete(ctx, pitrKey(m.ID))
		o.clearPITRProblems(ctx, m.ID)
	}
}

func (o *Orchestrator) clearPITRProblems(ctx context.Context, instanceID string) {
	for _, k := range []string{"alert.pitr.lag." + instanceID, "alert.pitr.disk." + instanceID} {
		_ = o.store.Settings.Delete(ctx, k)
		o.clearProblem(ctx, k)
	}
}

// dataVolume is the volume holding the instance's data (not its archive).
func (o *Orchestrator) dataVolume(ctx context.Context, it store.Tile) (store.Volume, error) {
	return o.volumes.BySlug(ctx, volume.Scope{Kind: "env", ID: it.EnvironmentID}, it.Slug+"-data")
}

// volumeCreated is what a new volume gets: the default daily backup, and for
// an instance's data volume with PITR on, the weekly base backup. The archive
// volume gets neither.
func (o *Orchestrator) volumeCreated(ctx context.Context, v store.Volume) {
	if mflow.IsArchiveVolume(v) || o.restoredVolume(ctx, v) {
		return
	}
	o.defaultBackup(ctx, v)
	o.baseSchedule(ctx, v)
}

// restoredVolume says whether v belongs to a restored candidate: a scratch or
// not-yet-promoted copy gets no backups of its own.
func (o *Orchestrator) restoredVolume(ctx context.Context, v store.Volume) bool {
	if v.InstanceID == nil {
		return false
	}
	m, err := o.managed.Get(ctx, *v.InstanceID)
	if err != nil {
		return false
	}
	_, ok := o.candidateOf(ctx, m.TileID)
	return ok
}

// baseSchedule gives an instance's data volume the weekly base backup when its
// instance has PITR on and none exists. It goes to the first off-box
// destination the org can see, else the local disk.
func (o *Orchestrator) baseSchedule(ctx context.Context, v store.Volume) {
	if v.InstanceID == nil || mflow.IsArchiveVolume(v) {
		return
	}
	if m, err := o.managed.Get(ctx, *v.InstanceID); err != nil || m.PITRDays <= 0 {
		return
	}
	ss, err := o.backups.Schedules(ctx, v.ID)
	if err != nil || slices.ContainsFunc(ss, func(s BackupSchedule) bool { return s.Method == "base" }) {
		return
	}
	_, org, ms, err := o.volumeFacts(ctx, v.ID)
	if err != nil {
		return
	}
	s := ScheduleSpec{Method: "base", Cron: pitrBaseCron}
	if d, ok := o.visibleOffBox(ctx, org); ok {
		s.DestID = &d.ID
	}
	if _, err := o.backups.AddSchedule(ctx, v.ID, org, ms, s); err != nil {
		slog.Warn("pitr: base schedule not added", "volume", v.ID, "err", err)
		return
	}
	if o.sched != nil {
		o.sched.Reload(ctx)
	}
}

func (o *Orchestrator) dropBaseSchedules(ctx context.Context, v store.Volume) {
	ss, _ := o.backups.Schedules(ctx, v.ID)
	for _, s := range ss {
		if s.Method == "base" {
			_ = o.DeleteBackupSchedule(ctx, s.ID)
		}
	}
}

// pitrDest is where an instance's base backups and WAL go: its base
// schedule's destination, else the first off-box one the org sees, else the
// local disk. The base backup always follows it, so base and WAL never part.
func (o *Orchestrator) pitrDest(ctx context.Context, org string, v store.Volume) (store.BackupDest, error) {
	ss, err := o.backups.Schedules(ctx, v.ID)
	if err != nil {
		return store.BackupDest{}, err
	}
	var id string
	if i := slices.IndexFunc(ss, func(s BackupSchedule) bool { return s.Method == "base" }); i >= 0 {
		if ss[i].DestID != nil {
			id = *ss[i].DestID
		}
	} else if d, ok := o.visibleOffBox(ctx, org); ok {
		id = d.ID
	}
	if id == "" {
		return o.localDest(ctx)
	}
	return o.backups.For(ctx, org, id)
}

// ---- the shipper ----

// pitrLoop ships every PITR instance's WAL each minute until the panel stops.
// ponytail: instances are served one after another, a slow destination
// delays the rest by up to pitrShipBudget each; a goroutine per instance
// when one box runs many.
func (o *Orchestrator) pitrLoop() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-o.stopNodes:
			cancel()
		case <-ctx.Done():
		}
	}()
	t := time.NewTicker(pitrEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			o.pitrTick(ctx)
		}
	}
}

func (o *Orchestrator) pitrTick(ctx context.Context) {
	ms, err := o.managed.WithPITR(ctx)
	if err != nil {
		slog.Warn("pitr: instances not listed", "err", err)
		return
	}
	live := map[string]bool{}
	for _, m := range ms {
		live[m.ID] = true
		if ctx.Err() != nil {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, pitrShipBudget)
		o.pitrShip(cctx, m)
		cancel()
	}
	for _, kind := range []string{"pitr.lag.", "pitr.disk."} {
		o.sweep(ctx, kind, func(rest string) bool { return live[rest] })
	}
	if rows, err := o.store.Settings.ListPrefix(ctx, "pitr."); err == nil {
		for k := range rows {
			if !live[strings.TrimPrefix(k, "pitr.")] {
				_ = o.store.Settings.Delete(ctx, k)
			}
		}
	}
}

// pitrShip is one instance's tick: guard (once), ship, read the archiver,
// write the state and the problems.
func (o *Orchestrator) pitrShip(ctx context.Context, m ManagedInstance) {
	it, err := o.tiles.Get(ctx, m.TileID)
	if err != nil {
		return
	}
	cs, err := o.tiles.Replicas(ctx, it)
	if err != nil || !slices.ContainsFunc(cs, func(c docker.Container) bool { return c.State == "running" }) {
		return // stopped or down: nothing is written, nothing to ship
	}
	v, err := o.dataVolume(ctx, it)
	if err != nil {
		return
	}
	org, err := o.volumeOrg(ctx, v)
	if err != nil {
		return
	}
	dest, err := o.pitrDest(ctx, org, v)
	if err != nil {
		slog.Warn("pitr: no destination", "instance", m.ID, "err", err)
		return
	}
	now := time.Now().UTC()
	st, _ := o.pitrState(ctx, m.ID)
	if st.Since.IsZero() {
		st.Since = now
	}
	root := backup.PITRRoot(org, v.ID)
	refused := ""
	if !st.Guarded || st.DestID != dest.ID {
		if err := o.backup.Guard(ctx, dest, root); err != nil {
			refused = err.Error()
			if !errors.Is(err, errs.ErrRefused) {
				slog.Warn("pitr: archive not checked", "instance", m.ID, "err", err)
				return
			}
		} else {
			st.Guarded, st.DestID = true, dest.ID
		}
	}
	if refused == "" {
		def := mflow.Engines[m.Engine].Definition()
		res, err := o.backup.ShipWAL(ctx, fbackup.Ship{Engine: it, Dest: dest, Root: root, Dir: def.ArchiveDir}, io.Discard)
		st.CheckedAt = now
		switch {
		case err != nil:
			st.Error = err.Error()
			slog.Warn("pitr: shipping failed", "instance", m.ID, "err", err)
		default:
			st.Error = ""
			st.Waiting, st.DirBytes = res.Waiting, res.DirBytes
			if res.Waiting == 0 {
				st.LastCleanAt = now
			}
		}
		if res.Bundles > 0 {
			st.LastShippedAt, st.LastSegment = now, cmpMax(st.LastSegment, res.Last)
		}
		if res.Bundles > 0 || st.UsageAt.IsZero() || now.Sub(st.UsageAt) > pitrUsageEvery {
			if n, b, err := o.backup.Usage(ctx, dest, root); err == nil {
				st.Bundles, st.Bytes, st.UsageAt = n, b, now
			}
		}
		if fc, err := o.engines.ArchiverFailed(ctx, it); err == nil {
			st.Failing = st.ArchiverSeen && fc > st.ArchiverFailed
			st.ArchiverFailed, st.ArchiverSeen = fc, true
		}
		o.pitrFirstBase(ctx, v, &st, now)
	} else {
		st.Error = refused
	}
	o.savePITRState(ctx, m.ID, st)
	o.pitrAlerts(ctx, m, it, v, st, now)
}

func cmpMax(a, b string) string {
	if b > a {
		return b
	}
	return a
}

// pitrFirstBase takes the first base backup as soon as the instance
// archives, so the window starts now and not at next week's schedule: no
// good or running base yet and no attempt of ours within the last hour
// queues one.
func (o *Orchestrator) pitrFirstBase(ctx context.Context, v store.Volume, st *pitrState, now time.Time) {
	if now.Sub(st.BaseTriedAt) < pitrBaseRetry {
		return
	}
	rs, err := o.backups.Runs(ctx, v.ID)
	if err != nil {
		return
	}
	base := o.baseSchedules(ctx, v)
	for _, r := range rs {
		if isBaseRun(r, base) && (r.Status == backup.Done || r.Status == backup.Running) {
			return
		}
	}
	st.BaseTriedAt = now
	if _, err := o.BackupNow(ctx, v.ID, "", "base", ""); err != nil {
		slog.Warn("pitr: first base backup not queued", "volume", v.ID, "err", err)
	}
}

// baseSchedules are the ids of v's base backup schedules.
func (o *Orchestrator) baseSchedules(ctx context.Context, v store.Volume) map[string]bool {
	out := map[string]bool{}
	ss, _ := o.backups.Schedules(ctx, v.ID)
	for _, s := range ss {
		if s.Method == "base" {
			out[s.ID] = true
		}
	}
	return out
}

// isBaseRun: a run of a base backup, by the archive key it wrote (a failed
// one keeps the key it tried) or the base schedule that started it.
// ponytail: a manual base that fails before any key is not told apart.
func isBaseRun(r store.BackupRun, baseSchedules map[string]bool) bool {
	return backup.IsPITRKey(r.ObjectKey) || (r.ScheduleID != nil && baseSchedules[*r.ScheduleID])
}

// ---- problems ----

// pitrLag is the lag problem's severity ("" = none) and message for st.
func pitrLag(slug string, st pitrState, now time.Time) (sev, msg string) {
	ref := st.LastCleanAt
	if ref.Before(st.Since) {
		ref = st.Since
	}
	lag := now.Sub(ref)
	switch {
	case strings.HasPrefix(st.Error, "the archive at"):
		return SevCritical, fmt.Sprintf("%s: WAL archiving did not start. %s", slug, st.Error)
	case st.Failing:
		return SevCritical, fmt.Sprintf("%s: Postgres cannot archive its WAL (archive_command is failing).", slug)
	case lag > pitrLagCrit:
		return SevCritical, fmt.Sprintf("%s: the WAL archive has not caught up for %s.", slug, agoWords(lag))
	case lag > pitrLagWarn:
		return SevWarning, fmt.Sprintf("%s: the WAL archive has not caught up for %d minutes.", slug, int(lag.Minutes()))
	}
	return "", ""
}

// pitrDisk is the disk problem's message ("" = none): the archive volume
// holds more than pitrDiskCap, or the node's free disk is under minFree. A
// full disk stops postgres.
func pitrDisk(slug string, dirBytes int64, free, minFree uint64) string {
	switch {
	case dirBytes > pitrDiskCap:
		return fmt.Sprintf("%s: %s of WAL is waiting on the box to be shipped.", slug, gb(uint64(dirBytes)))
	case free > 0 && free < minFree:
		return fmt.Sprintf("%s: the node has %s free, under %s; a full disk stops Postgres.", slug, gb(free), gb(minFree))
	}
	return ""
}

func (o *Orchestrator) pitrAlerts(ctx context.Context, m ManagedInstance, it store.Tile, v store.Volume, st pitrState, now time.Time) {
	sev, msg := pitrLag(it.Slug, st, now)
	o.pitrProblem(ctx, "alert.pitr.lag."+m.ID, AlertPITRLag, sev, it.Slug, msg)
	var free, minFree uint64
	if id := o.volumeNode(ctx, v.ID); id != "" {
		free, _ = o.diskOf(ctx, id)
	}
	if n, err := o.settings.Int(ctx, "observe_min_free"); err == nil {
		minFree = uint64(n) << 30
	}
	msg = pitrDisk(it.Slug, st.DirBytes, free, minFree)
	sev = ""
	if msg != "" {
		sev = SevCritical
	}
	o.pitrProblem(ctx, "alert.pitr.disk."+m.ID, AlertPITRDisk, sev, it.Slug, msg)
}

// pitrProblem opens or clears the problem behind key, with backup.stale's
// debounce: the alert row makes the webhook fire once per episode.
func (o *Orchestrator) pitrProblem(ctx context.Context, key, event, sev, subject, msg string) {
	_, was, err := o.store.Settings.Get(ctx, key)
	if err != nil {
		return
	}
	if sev == "" {
		o.clearProblem(ctx, key)
		if was {
			_ = o.store.Settings.Delete(ctx, key)
		}
		return
	}
	o.openProblem(ctx, key, sev, msg)
	if !was && o.store.Settings.Set(ctx, key, "1") == nil {
		o.alert(Alert{Event: event, Severity: sev, Subject: subject, Message: msg})
	}
}

// ---- health ----

// PITRBase is the newest base backup attempt.
type PITRBase struct {
	At     time.Time `json:"at"`
	OK     bool      `json:"ok"`
	Error  string    `json:"error,omitempty"`
	Active bool      `json:"active,omitempty"` // running now
}

// PITRHealth is what the Backups tab and `stackr tile backups` show.
type PITRHealth struct {
	On   bool `json:"on"`
	Days int  `json:"days"`
	// Dest is where the archive goes; Local says that is this box's disk, so
	// it is not disaster recovery.
	Dest  string `json:"dest,omitempty"`
	Local bool   `json:"local,omitempty"`
	// From is when the oldest kept base finished, To the last moment the
	// archive was known to be current; nil until there is a base or a clean tick.
	From *time.Time `json:"restorable_from,omitempty"`
	To   *time.Time `json:"restorable_to,omitempty"`
	// LastShipped is when a WAL bundle last went to the destination.
	LastShipped *time.Time `json:"last_shipped,omitempty"`
	LastBase    *PITRBase  `json:"last_base,omitempty"`
	Bundles     int        `json:"wal_bundles"`
	Bytes       int64      `json:"wal_bytes"`
	Waiting     int        `json:"segments_waiting"`
	Error       string     `json:"error,omitempty"`
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func nonZero(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// PITRStatus is an instance's point-in-time recovery health.
func (o *Orchestrator) PITRStatus(ctx context.Context, tileID string) (PITRHealth, error) {
	t, m, err := o.instanceOf(ctx, tileID)
	if err != nil {
		return PITRHealth{}, err
	}
	h := PITRHealth{On: m.PITRDays > 0, Days: m.PITRDays}
	if !h.On {
		return h, nil
	}
	st, _ := o.pitrState(ctx, m.ID)
	h.Bundles, h.Bytes, h.Waiting, h.Error = st.Bundles, st.Bytes, st.Waiting, st.Error
	h.LastShipped = nonZero(st.LastShippedAt)
	h.To = nonZero(latest(st.LastCleanAt, st.LastShippedAt))
	v, err := o.dataVolume(ctx, t)
	if err != nil {
		return h, nil // not deployed yet
	}
	org, err := o.volumeOrg(ctx, v)
	if err != nil {
		return h, err
	}
	d, derr := o.pitrDest(ctx, org, v)
	if derr == nil {
		h.Dest, h.Local = d.Name, d.Kind == backup.Local
	}
	rs, err := o.backups.Runs(ctx, v.ID) // newest first
	if err != nil {
		return h, err
	}
	base := o.baseSchedules(ctx, v)
	for _, r := range rs {
		if !isBaseRun(r, base) {
			continue
		}
		if h.LastBase == nil {
			b := &PITRBase{At: r.CreatedAt, OK: r.Status == backup.Done, Error: r.Error, Active: r.Status == backup.Running}
			if r.FinishedAt != nil {
				b.At = *r.FinishedAt
			}
			h.LastBase = b
		}
	}
	if derr == nil {
		// The same rule a restore applies, so the range never counts a base it refuses.
		if bases, keys, err := o.backup.ListArchive(ctx, d, backup.PITRRoot(org, v.ID)); err == nil {
			if from, ok := backup.Restorable(bases, keys); ok {
				h.From = &from
			}
		}
	}
	return h, nil
}
