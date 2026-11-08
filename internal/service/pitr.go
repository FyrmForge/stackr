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
	"sync"
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
	Since time.Time `json:"since"` // first tick that saw the instance
	// ArchSince is the first tick whose running container had the archive
	// dir, so it was archiving. Bases that started earlier (taken while
	// archiving was off, or on a container that had not started it) are never
	// used: nothing links their WAL to now.
	ArchSince      time.Time `json:"arch_since"`
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
	st, ok, _ := o.readPITRState(ctx, instanceID)
	return st, ok
}

// readPITRState tells "no state yet" (ok false, no error) from a read that
// failed: the shipper must not start a new epoch over a row it could not read.
func (o *Orchestrator) readPITRState(ctx context.Context, instanceID string) (st pitrState, ok bool, err error) {
	v, ok, err := o.store.Settings.Get(ctx, pitrKey(instanceID))
	if err != nil {
		return pitrState{}, false, err
	}
	if !ok {
		return pitrState{}, false, nil
	}
	if err := json.Unmarshal([]byte(v), &st); err != nil {
		return pitrState{}, false, err
	}
	return st, true, nil
}

// pitrLock serialises the shipper's read-modify-save of an instance's state
// with a promote's reset of it: a save never brings back a row the reset
// replaced.
func (o *Orchestrator) pitrLock(instanceID string) *sync.Mutex {
	m, _ := o.repoLocks.LoadOrStore("pitr state "+instanceID, &sync.Mutex{})
	return m.(*sync.Mutex)
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
	was := m
	if m, err = o.managed.SetPITR(ctx, m, days); err != nil {
		return m, Job{}, err
	}
	o.pitrApplied(ctx, t, m)
	note := "point-in-time recovery is on: restarting to archive WAL"
	if days <= 0 {
		note = "point-in-time recovery is off: restarting to stop archiving"
	}
	j, err := o.deployNoted(ctx, t.ID, note)
	if err != nil {
		// The container was not touched: put the row and the schedule back.
		if _, rerr := o.managed.SetPITR(ctx, m, was.PITRDays); rerr != nil {
			slog.Warn("pitr: window not restored after a failed redeploy", "instance", m.ID, "err", rerr)
		}
		if was.PITRDays <= 0 {
			o.pitrRetired(ctx, t, m)
		}
		return was, Job{}, err
	}
	if was.PITRDays <= 0 && days > 0 {
		o.pitrReset(ctx, m) // nothing archived while it was off: older bases cannot reach now
	}
	return m, j, err
}

// pitrApplied follows a moved window: the weekly base backup is added. Turning
// it off removes nothing here: the container keeps archiving until its
// restart, so the shipper keeps draining it, and pitrRetired (called by the
// shipper once the container no longer archives) drops the schedule, the
// shipper's state and the problems. The restart that makes postgres archive
// (or stop) is the caller's.
func (o *Orchestrator) pitrApplied(ctx context.Context, t store.Tile, m ManagedInstance) {
	if m.PITRDays <= 0 {
		return
	}
	if v, err := o.dataVolume(ctx, t); err == nil {
		o.baseSchedule(ctx, v)
	}
}

// pitrRetired ends an instance's PITR bookkeeping once it is off and its
// container no longer archives.
func (o *Orchestrator) pitrRetired(ctx context.Context, t store.Tile, m ManagedInstance) {
	if v, err := o.dataVolume(ctx, t); err == nil {
		o.dropBaseSchedules(ctx, v)
	}
	_ = o.store.Settings.Delete(ctx, pitrKey(m.ID))
	o.clearPITRProblems(ctx, m.ID)
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
			_ = o.deleteBackupSchedule(ctx, s.ID)
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
			id := strings.TrimPrefix(k, "pitr.")
			if live[id] {
				continue
			}
			// Off now, but the running container may still archive: the
			// shipper drains it until it stops.
			if m, err := o.managed.Get(ctx, id); err == nil {
				cctx, cancel := context.WithTimeout(ctx, pitrShipBudget)
				o.pitrShip(cctx, m)
				cancel()
				continue
			}
			_ = o.store.Settings.Delete(ctx, k)
			o.clearPITRProblems(ctx, id)
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
		if err == nil && m.PITRDays <= 0 {
			o.pitrRetired(ctx, it, m) // off and stopped: nothing will archive again
		}
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
	mu := o.pitrLock(m.ID)
	mu.Lock()
	defer mu.Unlock()
	st, _, err := o.readPITRState(ctx, m.ID)
	if err != nil {
		slog.Warn("pitr: state not read, tick skipped", "instance", m.ID, "err", err)
		return // a zero state saved over the row would start a new epoch
	}
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
			if st.DestID != "" && st.DestID != dest.ID {
				st.ArchSince = now // the WAL went elsewhere meanwhile: bases from before cannot reach now
			}
			st.Guarded, st.DestID = true, dest.ID
		}
	}
	if refused == "" {
		def := mflow.Engines[m.Engine].Definition()
		res, err := o.backup.ShipWAL(ctx, fbackup.Ship{Engine: it, Dest: dest, Root: root, Dir: def.ArchiveDir}, io.Discard)
		st.CheckedAt = now
		archiving := err == nil || !errors.Is(err, fbackup.ErrNoArchive)
		if archiving && st.ArchSince.IsZero() {
			st.ArchSince = now
		}
		if !archiving && m.PITRDays <= 0 {
			o.pitrRetired(ctx, it, m) // the restarted container has no archive: drained
			return
		}
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
		// Only a container that archives gets the first base: one taken on
		// the old, non-archiving container has WAL nobody keeps.
		if err == nil && m.PITRDays > 0 {
			o.pitrFirstBase(ctx, v, dest, &st, now)
		}
	} else {
		st.Error = refused
	}
	o.savePITRState(ctx, m.ID, st)
	if m.PITRDays > 0 {
		o.pitrAlerts(ctx, m, it, v, st, now)
	}
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
// queues one. Only a base on the active destination that started once the
// container archived counts.
func (o *Orchestrator) pitrFirstBase(ctx context.Context, v store.Volume, dest store.BackupDest, st *pitrState, now time.Time) {
	if now.Sub(st.BaseTriedAt) < pitrBaseRetry {
		return
	}
	rs, err := o.backups.Runs(ctx, v.ID)
	if err != nil {
		return
	}
	base := o.baseSchedules(ctx, v)
	for _, r := range rs {
		if isBaseRun(r, base) && r.DestID == dest.ID && !r.CreatedAt.Before(st.ArchSince) && (r.Status == backup.Done || r.Status == backup.Running) {
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

// pitrArchive is the archive a restore plans from: the bases on d that
// started after this PITR run's archiving began (none before it did), the WAL
// keys and the timeline lineage.
func (o *Orchestrator) pitrArchive(ctx context.Context, d store.BackupDest, root, instanceID string) ([]store.BackupRun, []string, backup.Lineage, error) {
	st, _ := o.pitrState(ctx, instanceID)
	if st.ArchSince.IsZero() {
		return nil, nil, backup.Lineage{}, nil
	}
	return o.backup.ListArchive(ctx, d, root, st.ArchSince)
}

// walLag is how far the archive may trail the shipper's last tick: a write
// sits in an open segment until archive_timeout (60s) closes it, and the
// closed segment waits for the next tick.
const walLag = 90 * time.Second

// restorableTo is the latest time the shipped WAL surely covers: the last tick
// that left nothing waiting, less walLag; zero before there is one.
func (st pitrState) restorableTo() time.Time {
	if st.LastCleanAt.IsZero() {
		return st.LastCleanAt
	}
	return st.LastCleanAt.Add(-walLag) // not LastShippedAt: a partly shipped backlog is not current
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
	h.To = nonZero(st.restorableTo())
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
		if bases, keys, lin, err := o.pitrArchive(ctx, d, backup.PITRRoot(org, v.ID), m.ID); err == nil {
			// a base newer than the archive's reach (walLag) is not restorable yet
			if from, ok := backup.Restorable(bases, keys, lin); ok && (h.To == nil || !h.To.Before(from)) {
				h.From = &from
			}
		}
	}
	return h, nil
}
