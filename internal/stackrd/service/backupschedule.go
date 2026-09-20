package service

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/backup"
	"github.com/FyrmForge/stackr/internal/stackrd/service/scheduler"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// BackupScheduleService owns a tile's backup schedules.
//
// Three creators, three rule sets. The panel derived the kind one way, the API
// another (it accepted a dump for a tile that is not a database), and a config
// apply a third. The panel defaulted "keep" to 7 and everything else to 0,
// which means keep nothing — not a sane default for a backup. The volume guard
// and `backup.Validate` were on two of the three paths. And the `backup:` key
// is a field the config file owns, yet neither surface gated a write to it, so
// a panel edit to a file-owned schedule was silently reverted by the next
// apply.
type BackupScheduleService struct {
	store repo.Store
	dests *BackupDestinationService
	sched *scheduler.Service
	gate  *GateService
}

func NewBackupScheduleService(store repo.Store, dests *BackupDestinationService,
	sched *scheduler.Service, gate *GateService) *BackupScheduleService {
	return &BackupScheduleService{store: store, dests: dests, sched: sched, gate: gate}
}

// DefaultKeep is how many archives a schedule keeps when nobody says. Not 0:
// 0 means keep nothing, which silently turns a backup into a no-op, and 0 is
// what every path but the panel defaulted to.
const DefaultKeep = 7

// ScheduleSpec is a schedule as a caller asks for it. A nil pointer field is
// "leave it alone" on an update and "take the default" on a create.
type ScheduleSpec struct {
	// Dest is a destination id or the file's reference form.
	Dest string
	// Kind is derived when empty: a dump for a managed database, a volume tar
	// for anything else. A caller may ask for a volume tar of a database; it
	// may not ask for a dump of something that is not one.
	Kind string
	// Mode is the container mode for a volume tar. Defaults to pause.
	Mode     string
	Cron     *string
	Timezone *string
	Keep     *int
	Enabled  *bool
}

func str(p *string, def string) string {
	if p == nil {
		return def
	}
	return strings.TrimSpace(*p)
}

// Create adds a schedule to a tile. by decides whether the config gate
// applies; use Adopt for the config engine's own writes.
func (s *BackupScheduleService) Create(ctx context.Context, t *repo.Tile, in ScheduleSpec, by Actor) (*repo.Backup, error) {
	if t == nil {
		return nil, svcerr.ErrNotFound
	}
	// `backup:` is a key the config file owns, so a write to it on a
	// config-managed stack obeys the same gate as any other owned field.
	// Neither surface had this, so a panel edit survived until the next apply
	// silently put the file's schedule back.
	if _, err := s.gate.Gate(ctx, t.StackID, GateFieldEdit, by.Surface()); err != nil {
		return nil, err
	}
	return s.Adopt(ctx, t, in)
}

// Adopt is Create without the gate: the same rules, for the config engine,
// which is the thing the gate exists to protect.
func (s *BackupScheduleService) Adopt(ctx context.Context, t *repo.Tile, in ScheduleSpec) (*repo.Backup, error) {
	if t == nil {
		return nil, svcerr.ErrNotFound
	}
	b := &repo.Backup{
		ID:         uuid.New().String(),
		TileID:     sql.NullString{String: t.ID, Valid: true},
		Kind:       in.Kind,
		Cron:       str(in.Cron, ""),
		Timezone:   str(in.Timezone, ""),
		KeepLatest: DefaultKeep,
		Enabled:    true,
		CreatedAt:  time.Now().UTC(),
	}
	if in.Keep != nil {
		b.KeepLatest = *in.Keep
	}
	if in.Enabled != nil {
		b.Enabled = *in.Enabled
	}
	b.ContainerMode = strings.TrimSpace(in.Mode)
	if err := s.resolve(ctx, t, b, in.Dest); err != nil {
		return nil, err
	}
	if err := s.normalise(t, b); err != nil {
		return nil, err
	}
	if err := s.store.CreateBackup(ctx, b); err != nil {
		return nil, err
	}
	s.sched.ReloadBackups(ctx)
	return b, nil
}

// Update edits a schedule. Every field is a pointer, so clearing the timezone
// is a thing a caller can express — the API and the CLI could not, which was
// an artefact of "patch only non-empty", not a rule.
func (s *BackupScheduleService) Update(ctx context.Context, b *repo.Backup, t *repo.Tile, in ScheduleSpec, by Actor) error {
	if b == nil {
		return svcerr.ErrNotFound
	}
	if t != nil {
		if _, err := s.gate.Gate(ctx, t.StackID, GateFieldEdit, by.Surface()); err != nil {
			return err
		}
	}
	return s.Reconcile(ctx, b, t, in)
}

// Reconcile is Update without the gate: the same rules, for the config engine,
// which is the thing the gate exists to protect. The tile is still needed —
// it is how the destination is checked against an org.
func (s *BackupScheduleService) Reconcile(ctx context.Context, b *repo.Backup, t *repo.Tile, in ScheduleSpec) error {
	if b == nil {
		return svcerr.ErrNotFound
	}
	if in.Dest != "" {
		if err := s.resolve(ctx, t, b, in.Dest); err != nil {
			return err
		}
	}
	if in.Kind != "" {
		b.Kind = in.Kind
	}
	if in.Mode != "" {
		b.ContainerMode = strings.TrimSpace(in.Mode)
	}
	if in.Cron != nil {
		b.Cron = strings.TrimSpace(*in.Cron)
	}
	if in.Timezone != nil {
		b.Timezone = strings.TrimSpace(*in.Timezone)
	}
	if in.Keep != nil {
		b.KeepLatest = *in.Keep
	}
	if in.Enabled != nil {
		b.Enabled = *in.Enabled
	}
	if err := s.normalise(t, b); err != nil {
		return err
	}
	if err := s.store.UpdateBackup(ctx, b); err != nil {
		return err
	}
	s.sched.ReloadBackups(ctx)
	return nil
}

// Delete drops the schedule. Archives already in the bucket are kept; nothing
// here reaches into a bucket.
func (s *BackupScheduleService) Delete(ctx context.Context, b *repo.Backup) error {
	if b == nil {
		return svcerr.ErrNotFound
	}
	if err := s.store.DeleteBackup(ctx, b.ID); err != nil {
		return err
	}
	s.sched.ReloadBackups(ctx)
	return nil
}

// resolve checks the destination against the tile's org. It is the tenant
// boundary on this path: the tile is already known to be the caller's, the
// bucket credentials are what could not be.
func (s *BackupScheduleService) resolve(ctx context.Context, t *repo.Tile, b *repo.Backup, ref string) error {
	orgID := ""
	if t != nil {
		var err error
		if orgID, err = backup.OrgOf(ctx, s.store, t); err != nil {
			return err
		}
	}
	d, err := s.dests.Resolve(ctx, orgID, ref)
	if err != nil {
		return err
	}
	// The panel's own database may only leave the server through a
	// destination the admins own.
	if b.Kind == repo.BackupStackr && !d.Global() {
		return invalid("destination_id", "the panel database needs a server-wide destination")
	}
	b.DestinationID = d.ID
	return nil
}

// normalise derives what the caller left out and refuses what cannot be. One
// copy of the kind derivation, the volume guard and the validator, where there
// were three, two and two.
func (s *BackupScheduleService) normalise(t *repo.Tile, b *repo.Backup) error {
	if b.Kind == "" && t != nil {
		if t.IsManaged() {
			b.Kind = repo.BackupDump
		} else {
			b.Kind = repo.BackupVolume
		}
	}
	if b.Kind == repo.BackupStackr && t != nil {
		return invalid("kind", "the panel's own database is configured from the admin area")
	}
	// A dump is an engine-native dump, so only a database has one. The API
	// accepted this and produced a schedule that failed on every run.
	if b.Kind == repo.BackupDump && t != nil && !t.IsManaged() {
		return invalid("kind", "only a managed database can be dumped; back up its volume instead")
	}
	if b.Kind == repo.BackupVolume {
		if b.ContainerMode == "" {
			b.ContainerMode = repo.ModePause
		}
		if t != nil {
			if _, err := backup.VolumeFor(t); err != nil {
				return invalid("kind", err.Error())
			}
		}
	}
	if err := backup.Validate(b); err != nil {
		return invalid("", err.Error())
	}
	return nil
}

// --- reads ---

// ForTile is a tile's backup schedules.
func (s *BackupScheduleService) ForTile(ctx context.Context, tileID string) ([]repo.Backup, error) {
	return s.store.ListBackupsByTile(ctx, tileID)
}

// ListAll is every backup schedule on the server, for the admin listing and
// the scheduler's own sweep.
func (s *BackupScheduleService) ListAll(ctx context.Context) ([]repo.Backup, error) {
	return s.store.ListBackups(ctx)
}

// Runs are one schedule's most recent runs, newest first.
func (s *BackupScheduleService) Runs(ctx context.Context, backupID string, limit int) ([]repo.BackupRun, error) {
	return s.store.ListBackupRuns(ctx, backupID, limit)
}

// Get is one backup schedule by id.
func (s *BackupScheduleService) Get(ctx context.Context, id string) (*repo.Backup, error) {
	b, err := s.store.GetBackup(ctx, id)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, svcerr.ErrNotFound
	}
	return b, nil
}

// Save writes a schedule row directly, and Remove deletes one.
//
// Create, Update and Delete are the ones with the rules — the destination
// resolution, the cron normalisation, the scheduler re-registration. These two
// are the raw writes, for the panel's server-backup page, which schedules the
// PANEL's own backup: a row with no tile, which every rule above is written
// about a tile for.
func (s *BackupScheduleService) Save(ctx context.Context, b *repo.Backup) error {
	if b.ID == "" {
		return svcerr.Invalid{Field: "id", Msg: "required"}
	}
	if existing, err := s.store.GetBackup(ctx, b.ID); err != nil {
		return err
	} else if existing == nil {
		return s.store.CreateBackup(ctx, b)
	}
	return s.store.UpdateBackup(ctx, b)
}

// Remove deletes a schedule row.
func (s *BackupScheduleService) Remove(ctx context.Context, id string) error {
	return s.store.DeleteBackup(ctx, id)
}

// Run is one recorded backup run by id.
func (s *BackupScheduleService) Run(ctx context.Context, id string) (*repo.BackupRun, error) {
	r, err := s.store.GetBackupRun(ctx, id)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, svcerr.ErrNotFound
	}
	return r, nil
}
