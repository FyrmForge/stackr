// Package backup owns backup destinations, the schedules on volumes and the
// run rows, plus the rules the flow leans on: which runs are restorable,
// where a schedule's archives live (the prefix) and keep-last-N pruning.
// The tar, dump, age and upload mechanics are the flow's.
package backup

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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
// (admin) destination: it lets every org pick it. ArchiveKey only counts on
// Create: empty generates one, set keeps the key old archives were sealed with.
type Dest struct {
	Name, Endpoint, Region, Bucket, AccessKey, SecretKey, ArchiveKey string
	Shared                                                           bool
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
	d := store.BackupDest{
		ID:         uuid.NewString(),
		Kind:       Local,
		Name:       Local,
		Endpoint:   dir,
		ArchiveKey: newKey(),
		Shared:     true,
		CreatedAt:  time.Now().UTC(),
	}
	return d, l.dests.Create(ctx, d)
}

// Probe tries a destination before it is saved (a test write); nil skips it.
type Probe func(context.Context, store.BackupDest) error

// Create adds an S3 destination; orgID nil makes it admin-global. Every
// destination gets its own random archive key here and keeps it for life.
// It is saved only once probe can write to it.
func (l *Leaf) Create(ctx context.Context, orgID *string, s Dest, probe Probe) (store.BackupDest, error) {
	d := store.BackupDest{
		ID:         uuid.NewString(),
		OrgID:      orgID,
		Kind:       S3,
		ArchiveKey: cmp.Or(s.ArchiveKey, newKey()),
		CreatedAt:  time.Now().UTC(),
	}
	var noSecret error
	if s.SecretKey == "" {
		noSecret = errs.Invalidf("secret_key", "An S3 destination needs a secret key.")
	}
	if err := l.fill(ctx, &d, s, probe, noSecret); err != nil {
		return d, err
	}
	return d, l.dests.Create(ctx, d)
}

// Update changes a destination; like Create, only once probe can write to it.
func (l *Leaf) Update(ctx context.Context, d store.BackupDest, s Dest, probe Probe) (store.BackupDest, error) {
	if d.Kind == Local {
		return d, errs.Refusedf("the local destination is part of the install")
	}
	if s.SecretKey == "" {
		s.SecretKey = d.SecretKey
	}
	if s.AccessKey == "" {
		s.AccessKey = d.AccessKey
	}
	if err := l.fill(ctx, &d, s, probe, nil); err != nil {
		return d, err
	}
	return d, l.dests.Update(ctx, d)
}

// Delete refuses the local destination and one a schedule or the panel's own
// backup (usedByPanel, a fact from the caller) still points at: they would
// silently fall back to local. Its run rows go with it; the archives stay in
// the bucket.
func (l *Leaf) Delete(ctx context.Context, d store.BackupDest, usedByPanel bool) error {
	if d.Kind == Local {
		return errs.Refusedf("the local destination is part of the install")
	}
	if usedByPanel {
		return errs.Conflictf("the panel backup still uses %s", d.Name)
	}
	all, err := l.schedules.List(ctx)
	if err != nil {
		return err
	}
	inUse := slices.ContainsFunc(all, func(s store.BackupSchedule) bool {
		return s.DestID != nil && *s.DestID == d.ID
	})
	if inUse {
		return errs.Conflictf("a volume's backup schedule still uses %s", d.Name)
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

// fill checks s into d; every refused field at once, more (nil = none) last.
func (l *Leaf) fill(ctx context.Context, d *store.BackupDest, s Dest, probe Probe, more error) error {
	var bad []error
	name := strings.TrimSpace(s.Name)
	if !nameRe.MatchString(name) {
		bad = append(bad, errs.Invalidf("name", "A name is letters, digits, - and _ (it is used as ${{ org.backups.NAME }})."))
	}
	if name == Local {
		bad = append(bad, errs.Invalidf("name", "%q is the install's own destination.", name))
	}
	endpoint := strings.TrimRight(strings.TrimSpace(s.Endpoint), "/")
	if endpoint != "" && !strings.HasPrefix(endpoint, "https://") && !strings.HasPrefix(endpoint, "http://") {
		bad = append(bad, errs.Invalidf("endpoint", "An endpoint is an http(s) URL, or empty for AWS."))
	}
	bucket := strings.TrimSpace(s.Bucket)
	if bucket == "" {
		bad = append(bad, errs.Invalidf("bucket", "An S3 destination needs a bucket."))
	}
	if strings.TrimSpace(s.AccessKey) == "" {
		bad = append(bad, errs.Invalidf("access_key", "An S3 destination needs an access key."))
	}
	if err := errors.Join(append(bad, more)...); err != nil {
		return err
	}
	peers, err := l.dests.ListByOrg(ctx, d.OrgID)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(peers, func(p store.BackupDest) bool { return p.ID != d.ID && p.Name == name }) {
		return errs.Conflictf("a destination named %s already exists", name)
	}
	d.Name = name
	d.Endpoint = endpoint
	d.Region = strings.TrimSpace(s.Region)
	d.Bucket = bucket
	d.AccessKey, d.SecretKey, d.Shared = strings.TrimSpace(s.AccessKey), s.SecretKey, s.Shared && d.OrgID == nil
	if probe == nil {
		return nil
	}
	if err := probe(ctx, *d); err != nil {
		// An S3 error can quote the request; never let a key through.
		msg := redact(err.Error(), d.SecretKey, d.AccessKey)
		return errs.Invalidf("endpoint", "A test file could not be written to bucket %s. Check the endpoint, bucket and keys.\n%s", d.Bucket, msg)
	}
	return nil
}

// redact hides each key in msg. A key of 8 or more characters goes wherever
// it appears; a shorter one only as a whole token, so a test key "x" never
// garbles "exceeded" (inside a word it is not the key, only a letter).
func redact(msg string, keys ...string) string {
	for _, k := range keys {
		if k == "" {
			continue
		}
		var b strings.Builder
		for rest := msg; ; {
			i := strings.Index(rest, k)
			if i < 0 {
				b.WriteString(rest)
				break
			}
			end := i + len(k)
			b.WriteString(rest[:i])
			if len(k) >= 8 || (!wordByte(rest, i-1) && !wordByte(rest, end)) {
				b.WriteString("***")
			} else {
				b.WriteString(k)
			}
			rest = rest[end:]
		}
		msg = b.String()
	}
	return msg
}

// wordByte: s[i] is a letter or digit; a position outside s is not.
func wordByte(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	c := s[i]
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// newKey is the destination's archive key: 32 random bytes, hex. The flow
// feeds it to age.
// ponytail: raw bytes, not an age identity; turning it into one (scrypt
// or X25519) is the flow's call and needs no migration.
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
func (l *Leaf) AddSchedule(
	ctx context.Context,
	volumeID, orgID string,
	methods []string,
	s Schedule,
) (store.BackupSchedule, error) {
	b := store.BackupSchedule{ID: uuid.NewString(), VolumeID: volumeID, CreatedAt: time.Now().UTC()}
	if err := l.fillSchedule(ctx, &b, orgID, methods, s); err != nil {
		return b, err
	}
	return b, l.schedules.Create(ctx, b)
}

func (l *Leaf) UpdateSchedule(
	ctx context.Context,
	b store.BackupSchedule,
	orgID string,
	methods []string,
	s Schedule,
) (store.BackupSchedule, error) {
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

func (l *Leaf) fillSchedule(
	ctx context.Context,
	b *store.BackupSchedule,
	orgID string,
	methods []string,
	s Schedule,
) error {
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
	b.Method = method
	b.DestID = s.DestID
	b.Cron = cron
	b.Timezone = tz
	b.Keep = s.Keep
	b.Mode = mode
	return nil
}

// ---- runs ----

// Start records a run as running. volumeID and scheduleID are nil for a
// panel run; scheduleID is nil for a manual or pre-restore one.
func (l *Leaf) Start(
	ctx context.Context,
	kind string,
	volumeID, scheduleID *string,
	destID, trigger string,
) (store.BackupRun, error) {
	return l.StartIn(ctx, kind, volumeID, scheduleID, destID, trigger, nil)
}

// StartIn is Start for a run that belongs to a node backup group. Such a run
// may carry no volume id: a Docker volume with no row (a move leftover) is
// archived all the same.
func (l *Leaf) StartIn(
	ctx context.Context,
	kind string,
	volumeID, scheduleID *string,
	destID, trigger string,
	groupID *string,
) (store.BackupRun, error) {
	r := store.BackupRun{
		ID:         uuid.NewString(),
		Kind:       kind,
		VolumeID:   volumeID,
		ScheduleID: scheduleID,
		DestID:     destID,
		Trigger:    trigger,
		Status:     Running,
		CreatedAt:  time.Now().UTC(),
		GroupID:    groupID,
	}
	if kind != KindVolume && kind != KindPanel {
		return r, errs.Invalidf("kind", "a run is a volume or panel run")
	}
	if (kind == KindVolume && volumeID == nil && groupID == nil) || (kind == KindPanel && volumeID != nil) {
		return r, errs.Invalidf("volume", "a volume run names its volume, a panel run none")
	}
	return r, l.runs.Create(ctx, r)
}

// Finish closes a run: done with its object key and size, or failed with runErr.
func (l *Leaf) Finish(
	ctx context.Context,
	r store.BackupRun,
	objectKey string,
	size int64,
	runErr error,
) (store.BackupRun, error) {
	// The job may have been cancelled (a superseded run): the row still closes.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	now := time.Now().UTC()
	r.FinishedAt = &now
	r.ObjectKey = objectKey
	r.SizeBytes = size
	r.Status = Done
	r.Error = ""
	if runErr != nil {
		r.Status, r.Error = Failed, runErr.Error()
	}
	return r, l.runs.Update(ctx, r)
}

// SettleRestored closes the panel runs a restored database still has as
// running: the archive was taken while its own run was writing it. They
// finish done, with note, since that archive is the one restored. With one
// such run, it is that archive's: it gets key and size (the restore read
// them; "" and 0 = unknown).
func (l *Leaf) SettleRestored(ctx context.Context, note, key string, size int64) error {
	rs, err := l.runs.ListByKind(ctx, KindPanel)
	if err != nil {
		return err
	}
	rs = slices.DeleteFunc(rs, func(r store.BackupRun) bool { return r.Status != Running })
	for _, r := range rs {
		now := time.Now().UTC()
		r.Status, r.Error, r.FinishedAt = Done, note, &now
		if len(rs) == 1 {
			r.ObjectKey = cmp.Or(r.ObjectKey, key)
			r.SizeBytes = cmp.Or(r.SizeBytes, size)
		}
		if err := l.runs.Update(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// Interrupted fails every run left running: its job died with the process
// that ran it, so nothing else closes it. Boot calls it after SettleRestored.
func (l *Leaf) Interrupted(ctx context.Context) error {
	for _, kind := range []string{KindVolume, KindPanel} {
		rs, err := l.runs.ListByKind(ctx, kind)
		if err != nil {
			return err
		}
		for _, r := range rs {
			if r.Status != Running {
				continue
			}
			now := time.Now().UTC()
			r.Status, r.Error, r.FinishedAt = Failed, "stackrd restarted while this backup ran", &now
			if err := l.runs.Update(ctx, r); err != nil {
				return err
			}
		}
	}
	return nil
}

func (l *Leaf) GetRun(ctx context.Context, id string) (store.BackupRun, error) {
	return l.runs.Get(ctx, id)
}

// Runs is a volume's runs, newest first, each naming its destination.
func (l *Leaf) Runs(ctx context.Context, volumeID string) ([]store.BackupRun, error) {
	rs, err := l.runs.ListByVolume(ctx, volumeID)
	if err != nil {
		return nil, err
	}
	return l.newest(ctx, rs)
}

// PanelRuns is the panel self-backup's runs, newest first, each naming its
// destination.
func (l *Leaf) PanelRuns(ctx context.Context) ([]store.BackupRun, error) {
	rs, err := l.runs.ListByKind(ctx, KindPanel)
	if err != nil {
		return nil, err
	}
	return l.newest(ctx, rs)
}

func (l *Leaf) newest(ctx context.Context, rs []store.BackupRun) ([]store.BackupRun, error) {
	slices.SortFunc(rs, func(a, b store.BackupRun) int { return b.CreatedAt.Compare(a.CreatedAt) })
	names := map[string]string{}
	for i := range rs {
		id := rs[i].DestID
		if _, ok := names[id]; !ok {
			d, err := l.dests.Get(ctx, id)
			if err != nil {
				return rs, err
			}
			names[id] = d.Name
		}
		rs[i].Dest = names[id]
	}
	return rs, nil
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
// Each trigger has its own, so pruning scheduled archives never reaches the
// pre-upgrade or manual ones.
func PanelPrefix(installID, trigger string) string {
	dir := trigger
	if trigger == "schedule" {
		dir = "scheduled"
	}
	return "stackr/_panel/" + installID + "/" + dir
}

// NodePrefix is where a node backup group's archives go. Like the manual
// prefix it is never pruned.
func NodePrefix(nodeName, groupID string) string {
	return "stackr/_node/" + nodeName + "/" + groupID
}

// OrphanPrefix is where the orphan job's last archive of a volume goes.
func OrphanPrefix(volumeID string) string {
	return "stackr/_orphan/" + volumeID
}

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
// A protect key (the one the run just wrote) is never deleted.
func (l *Leaf) Prune(ctx context.Context, obj Objects, prefix string, keep int, protect ...string) []string {
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
		if !slices.Contains(protect, k) && obj.Delete(ctx, k) == nil {
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
