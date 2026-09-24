// Package backup owns backup destinations, the schedules on volumes and the
// run rows, plus the rules the flow leans on: which runs are restorable,
// where a schedule's archives live (the prefix) and keep-last-N pruning.
// The tar, dump, age and upload mechanics are the flow's.
package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

const (
	Local = "local"
	S3    = "s3"

	// Modes for a tar method; ignored when the engine dumps.
	Pause = "pause"
	Stop  = "stop"
	Live  = "live" // accepts a torn copy

	KindVolume = "volume"
	KindPanel  = "panel"

	Running = "running"
	Done    = "done"
	Failed  = "failed"

	// PreRestore is the trigger of the backup taken before a restore; it never prunes.
	PreRestore = "pre-restore"
)

type Leaf struct {
	dests     store.BackupDestStore
	schedules store.BackupScheduleStore
	runs      store.BackupRunStore
}

func New(dests store.BackupDestStore, schedules store.BackupScheduleStore, runs store.BackupRunStore) *Leaf {
	return &Leaf{dests: dests, schedules: schedules, runs: runs}
}

// ---- destinations ----

// Dest is an S3 destination as the form or API gives it. An empty SecretKey
// on Update keeps the stored one. Shared only means something on a global
// (admin) destination: it lets every org pick it.
type Dest struct {
	Name, Endpoint, Region, Bucket, AccessKey, SecretKey string
	Shared                                               bool
}

// EnsureLocal returns the install's local destination (dir), creating it on
// first boot. Global and shared: it is every org's default.
func (l *Leaf) EnsureLocal(ctx context.Context, dir string) (store.BackupDest, error) {
	all, err := l.dests.ListByOrg(ctx, nil)
	if err != nil {
		return store.BackupDest{}, err
	}
	for _, d := range all {
		if d.Kind == Local {
			if d.Endpoint != dir {
				d.Endpoint = dir
				err = l.dests.Update(ctx, d)
			}
			return d, err
		}
	}
	d := store.BackupDest{ID: uuid.NewString(), Kind: Local, Name: Local, Endpoint: dir,
		ArchiveKey: newKey(), Shared: true, CreatedAt: time.Now().UTC()}
	return d, l.dests.Create(ctx, d)
}

// Create adds an S3 destination; orgID nil makes it admin-global. Every
// destination gets its own random archive key here and keeps it for life.
func (l *Leaf) Create(ctx context.Context, orgID *string, s Dest) (store.BackupDest, error) {
	d := store.BackupDest{ID: uuid.NewString(), OrgID: orgID, Kind: S3, ArchiveKey: newKey(), CreatedAt: time.Now().UTC()}
	if s.SecretKey == "" {
		return d, errs.Invalidf("secret_key", "an S3 destination needs a secret key")
	}
	if err := l.fill(ctx, &d, s); err != nil {
		return d, err
	}
	return d, l.dests.Create(ctx, d)
}

func (l *Leaf) Update(ctx context.Context, d store.BackupDest, s Dest) (store.BackupDest, error) {
	if d.Kind == Local {
		return d, errs.Refusedf("the local destination is part of the install")
	}
	if s.SecretKey == "" {
		s.SecretKey = d.SecretKey
	}
	if err := l.fill(ctx, &d, s); err != nil {
		return d, err
	}
	return d, l.dests.Update(ctx, d)
}

// Delete refuses the local destination and one a schedule still points at
// (the schedule would silently fall back to local). Its run rows go with it;
// the archives stay in the bucket.
func (l *Leaf) Delete(ctx context.Context, d store.BackupDest) error {
	if d.Kind == Local {
		return errs.Refusedf("the local destination is part of the install")
	}
	all, err := l.schedules.List(ctx)
	if err != nil {
		return err
	}
	for _, s := range all {
		if s.DestID != nil && *s.DestID == d.ID {
			return errs.Conflictf("a volume's backup schedule still uses %s", d.Name)
		}
	}
	return l.dests.Delete(ctx, d.ID)
}

// Get is the raw row, keys included, for the flow that uploads.
func (l *Leaf) Get(ctx context.Context, id string) (store.BackupDest, error) {
	return l.dests.Get(ctx, id)
}

// For returns a destination as orgID sees it: its own, or a shared global
// one. Anything else, an unshared global or another org's, is not found.
func (l *Leaf) For(ctx context.Context, orgID, id string) (store.BackupDest, error) {
	d, err := l.dests.Get(ctx, id)
	if err != nil {
		return d, err
	}
	if !visible(d, orgID) {
		return store.BackupDest{}, errs.ErrNotFound
	}
	return d, nil
}

// Visible lists what orgID may pick, keys blanked: its own then shared globals.
func (l *Leaf) Visible(ctx context.Context, orgID string) ([]store.BackupDest, error) {
	own, err := l.dests.ListByOrg(ctx, &orgID)
	if err != nil {
		return nil, err
	}
	global, err := l.dests.ListByOrg(ctx, nil)
	for _, d := range global {
		if d.Shared {
			own = append(own, d)
		}
	}
	return blank(own), err
}

// Global lists the admin-global destinations, keys blanked.
func (l *Leaf) Global(ctx context.Context) ([]store.BackupDest, error) {
	ds, err := l.dests.ListByOrg(ctx, nil)
	return blank(ds), err
}

// refRe is the stack file's dest ref: ${{ org.backups.NAME }} or ${{ stackr.backups.NAME }}.
var refRe = regexp.MustCompile(`^\$\{\{\s*(org|stackr)\.backups\.([A-Za-z0-9_-]+)\s*\}\}$`)

// Resolve turns a stack file's `dest:` into the destination for orgID. Empty
// is the local default. org.* names the org's own, stackr.* a shared global.
func (l *Leaf) Resolve(ctx context.Context, orgID, ref string) (store.BackupDest, error) {
	ref = strings.TrimSpace(ref)
	var pool []store.BackupDest
	var err error
	name := ""
	switch m := refRe.FindStringSubmatch(ref); {
	case ref == "":
		pool, err = l.dests.ListByOrg(ctx, nil)
	case m == nil:
		return store.BackupDest{}, errs.Invalidf("dest", "%q is not ${{ org.backups.NAME }} or ${{ stackr.backups.NAME }}", ref)
	case m[1] == "org":
		pool, err = l.dests.ListByOrg(ctx, &orgID)
		name = m[2]
	default:
		pool, err = l.dests.ListByOrg(ctx, nil)
		name = m[2]
	}
	if err != nil {
		return store.BackupDest{}, err
	}
	for _, d := range pool {
		if (name == "" && d.Kind == Local) || (name != "" && d.Name == name && visible(d, orgID)) {
			return d, nil
		}
	}
	return store.BackupDest{}, errs.Invalidf("dest", "no backup destination %q", ref)
}

func visible(d store.BackupDest, orgID string) bool {
	if d.OrgID == nil {
		return d.Shared
	}
	return *d.OrgID == orgID
}

func blank(ds []store.BackupDest) []store.BackupDest {
	for i := range ds {
		ds[i].SecretKey, ds[i].ArchiveKey = "", ""
	}
	return ds
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (l *Leaf) fill(ctx context.Context, d *store.BackupDest, s Dest) error {
	name := strings.TrimSpace(s.Name)
	if !nameRe.MatchString(name) {
		return errs.Invalidf("name", "a name is letters, digits, - and _ (it is used as ${{ org.backups.NAME }})")
	}
	if name == Local {
		return errs.Invalidf("name", "%q is the install's own destination", name)
	}
	endpoint := strings.TrimRight(strings.TrimSpace(s.Endpoint), "/")
	if endpoint != "" && !strings.HasPrefix(endpoint, "https://") && !strings.HasPrefix(endpoint, "http://") {
		return errs.Invalidf("endpoint", "an endpoint is an http(s) URL, or empty for AWS")
	}
	bucket := strings.TrimSpace(s.Bucket)
	if bucket == "" {
		return errs.Invalidf("bucket", "an S3 destination needs a bucket")
	}
	if strings.TrimSpace(s.AccessKey) == "" {
		return errs.Invalidf("access_key", "an S3 destination needs an access key")
	}
	peers, err := l.dests.ListByOrg(ctx, d.OrgID)
	if err != nil {
		return err
	}
	for _, p := range peers {
		if p.ID != d.ID && p.Name == name {
			return errs.Conflictf("a destination named %s already exists", name)
		}
	}
	d.Name, d.Endpoint, d.Region, d.Bucket = name, endpoint, strings.TrimSpace(s.Region), bucket
	d.AccessKey, d.SecretKey, d.Shared = strings.TrimSpace(s.AccessKey), s.SecretKey, s.Shared && d.OrgID == nil
	return nil
}

// newKey is the destination's archive key: 32 random bytes, hex. The flow
// feeds it to age.
// ponytail: raw bytes, not an age identity; turning it into one (scrypt
// passphrase or X25519) is the flow's call and needs no migration.
func newKey() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails (crypto/rand panics instead)
	return hex.EncodeToString(b)
}

// ---- schedules ----

// Schedule is a volume's backup as the stack file or form gives it.
type Schedule struct {
	Method   string  // "" = the engine's first
	DestID   *string // nil = the local destination
	Cron     string
	Timezone string // "" = UTC
	Keep     int    // 0 = keep everything
	Mode     string // "" = pause; ignored when the method dumps
}

// cronRe checks the shape of a five-field cron line or an @ macro.
// ponytail: shape only; the scheduler's parser is the real check.
var cronRe = regexp.MustCompile(`^(@(yearly|annually|monthly|weekly|daily|midnight|hourly)|([0-9*/,-]+\s+){4}[0-9A-Za-z*/,-]+)$`)

// AddSchedule hangs a schedule off a volume. methods is what the volume's
// engine offers (a fact from the flow); orgID is the volume's org, which the
// destination has to be visible to.
func (l *Leaf) AddSchedule(ctx context.Context, volumeID, orgID string, methods []string, s Schedule) (store.BackupSchedule, error) {
	b := store.BackupSchedule{ID: uuid.NewString(), VolumeID: volumeID, CreatedAt: time.Now().UTC()}
	if err := l.fillSchedule(ctx, &b, orgID, methods, s); err != nil {
		return b, err
	}
	return b, l.schedules.Create(ctx, b)
}

func (l *Leaf) UpdateSchedule(ctx context.Context, b store.BackupSchedule, orgID string, methods []string, s Schedule) (store.BackupSchedule, error) {
	if err := l.fillSchedule(ctx, &b, orgID, methods, s); err != nil {
		return b, err
	}
	return b, l.schedules.Update(ctx, b)
}

func (l *Leaf) Schedules(ctx context.Context, volumeID string) ([]store.BackupSchedule, error) {
	return l.schedules.ListByVolume(ctx, volumeID)
}

// AllSchedules is every schedule, for the cron registry at boot.
func (l *Leaf) AllSchedules(ctx context.Context) ([]store.BackupSchedule, error) {
	return l.schedules.List(ctx)
}

func (l *Leaf) GetSchedule(ctx context.Context, id string) (store.BackupSchedule, error) {
	return l.schedules.Get(ctx, id)
}

func (l *Leaf) DeleteSchedule(ctx context.Context, id string) error {
	return l.schedules.Delete(ctx, id)
}

func (l *Leaf) fillSchedule(ctx context.Context, b *store.BackupSchedule, orgID string, methods []string, s Schedule) error {
	if len(methods) == 0 {
		return errs.Refusedf("this volume has no backup method")
	}
	method := strings.TrimSpace(s.Method)
	if method == "" {
		method = methods[0]
	}
	if !slices.Contains(methods, method) {
		return errs.Invalidf("method", "%q is not one of %s", method, strings.Join(methods, ", "))
	}
	mode := strings.TrimSpace(s.Mode)
	if mode == "" {
		mode = Pause
	}
	if mode != Pause && mode != Stop && mode != Live {
		return errs.Invalidf("mode", "mode is pause, stop or live")
	}
	cron := strings.Join(strings.Fields(s.Cron), " ")
	if !cronRe.MatchString(cron) {
		return errs.Invalidf("schedule", "%q is not a cron line", s.Cron)
	}
	tz := strings.TrimSpace(s.Timezone)
	if tz == "" {
		tz = "UTC"
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return errs.Invalidf("timezone", "%q is not a time zone", s.Timezone)
	}
	if s.Keep < 0 {
		return errs.Invalidf("keep", "keep must not be negative")
	}
	if s.DestID != nil {
		if _, err := l.For(ctx, orgID, *s.DestID); err != nil {
			return err
		}
	}
	b.Method, b.DestID, b.Cron, b.Timezone, b.Keep, b.Mode = method, s.DestID, cron, tz, s.Keep, mode
	return nil
}

// ---- runs ----

// Start records a run as running. volumeID and scheduleID are nil for a
// panel run; scheduleID is nil for a manual or pre-restore one.
func (l *Leaf) Start(ctx context.Context, kind string, volumeID, scheduleID *string, destID, trigger string) (store.BackupRun, error) {
	r := store.BackupRun{ID: uuid.NewString(), Kind: kind, VolumeID: volumeID, ScheduleID: scheduleID,
		DestID: destID, Trigger: trigger, Status: Running, CreatedAt: time.Now().UTC()}
	if kind != KindVolume && kind != KindPanel {
		return r, errs.Invalidf("kind", "a run is a volume or panel run")
	}
	if (kind == KindVolume) != (volumeID != nil) {
		return r, errs.Invalidf("volume", "a volume run names its volume, a panel run none")
	}
	return r, l.runs.Create(ctx, r)
}

// Finish closes a run: done with its object key and size, or failed with runErr.
func (l *Leaf) Finish(ctx context.Context, r store.BackupRun, objectKey string, size int64, runErr error) (store.BackupRun, error) {
	now := time.Now().UTC()
	r.FinishedAt, r.ObjectKey, r.SizeBytes, r.Status, r.Error = &now, objectKey, size, Done, ""
	if runErr != nil {
		r.Status, r.Error = Failed, runErr.Error()
	}
	return r, l.runs.Update(ctx, r)
}

func (l *Leaf) GetRun(ctx context.Context, id string) (store.BackupRun, error) {
	return l.runs.Get(ctx, id)
}

// Runs is a volume's runs, newest first.
func (l *Leaf) Runs(ctx context.Context, volumeID string) ([]store.BackupRun, error) {
	return newest(l.runs.ListByVolume(ctx, volumeID))
}

// PanelRuns is the panel self-backup's runs, newest first.
func (l *Leaf) PanelRuns(ctx context.Context) ([]store.BackupRun, error) {
	return newest(l.runs.ListByKind(ctx, KindPanel))
}

func newest(rs []store.BackupRun, err error) ([]store.BackupRun, error) {
	slices.SortFunc(rs, func(a, b store.BackupRun) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return rs, err
}

// Restorable loads runID and says whether it may be restored from
// sourceVolumeID: the run belongs to that volume, finished done and recorded
// an object key. The key is never taken from a caller: on a shared bucket
// that is a cross-tenant read. Panel runs are refused; their archive holds
// the master key and is restored on the host with the restore script.
// ponytail: a run whose volume is gone (volume_id NULL) is not restorable
// here; see DECIDE 26.
func (l *Leaf) Restorable(ctx context.Context, runID, sourceVolumeID string) (store.BackupRun, error) {
	r, err := l.runs.Get(ctx, runID)
	if err != nil {
		return r, err
	}
	if r.Kind == KindPanel {
		return r, errs.Refusedf("a panel backup is restored on the host with the restore script")
	}
	if r.VolumeID == nil || *r.VolumeID != sourceVolumeID {
		return store.BackupRun{}, errs.ErrNotFound
	}
	if r.Status != Done || r.ObjectKey == "" {
		return r, errs.Refusedf("only a finished backup can be restored")
	}
	return r, nil
}

// ---- where archives live ----

// Prefix is a volume schedule's object-key root. It is derived here, never
// taken from a caller, because Prune deletes everything under it: a
// caller-chosen prefix on a shared bucket is a cross-tenant delete. Each
// schedule gets its own (pruning one must never touch another's). An empty
// scheduleID is the volume's manual runs, which are never pruned.
func Prefix(orgID, volumeID, scheduleID string) string {
	if scheduleID == "" {
		scheduleID = "manual"
	}
	return "stackr/" + orgID + "/" + volumeID + "/" + scheduleID
}

// PanelPrefix never lands under an org's (org ids are uuids, never "_panel").
func PanelPrefix(installID string) string { return "stackr/_panel/" + installID }

// OrphanPrefix is where the orphan job's last archive of a volume goes.
func OrphanPrefix(volumeID string) string { return "stackr/_orphan/" + volumeID }

// ObjectKey is prefix/<utc timestamp>-name; timestamp first so lexical order is chronological.
func ObjectKey(prefix, name string, now time.Time) string {
	return prefix + "/" + now.UTC().Format("20060102-150405") + "-" + name
}

// Objects is the destination's store as the flow opens it (local dir or S3).
type Objects interface {
	List(ctx context.Context, prefix string) ([]string, error)
	Delete(ctx context.Context, key string) error
}

// Prune keeps the newest keep archives under prefix and deletes the rest,
// with their run rows. keep <= 0 keeps everything. Best effort: a failed
// delete is skipped (its row stays), never an error, because a prune must
// not fail a run that already uploaded. The pre-restore run never calls it.
func (l *Leaf) Prune(ctx context.Context, obj Objects, prefix string, keep int) []string {
	if keep <= 0 {
		return nil
	}
	keys, err := obj.List(ctx, prefix+"/")
	if err != nil || len(keys) <= keep {
		return nil
	}
	slices.Sort(keys)
	var gone []string
	for _, k := range keys[:len(keys)-keep] {
		if obj.Delete(ctx, k) == nil {
			gone = append(gone, k)
		}
	}
	rows, _ := l.runs.ListByPrefix(ctx, prefix+"/")
	for _, r := range rows {
		if slices.Contains(gone, r.ObjectKey) {
			_ = l.runs.Delete(ctx, r.ID)
		}
	}
	return gone
}
