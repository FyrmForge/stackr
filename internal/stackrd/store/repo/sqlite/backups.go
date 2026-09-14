package sqlite

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// --- destinations ---

func (s *Store) CreateBackupDestination(ctx context.Context, d *repo.BackupDestination) error {
	enc := *d
	enc.SecretKey = secrets.Encrypt(d.SecretKey)
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO backup_destinations (id, org_id, name, endpoint, region, bucket, access_key, secret_key, shared, created_at)
		 VALUES (:id, :org_id, :name, :endpoint, :region, :bucket, :access_key, :secret_key, :shared, :created_at)`, &enc)
	return err
}

func (s *Store) GetBackupDestination(ctx context.Context, id string) (*repo.BackupDestination, error) {
	d, err := get[repo.BackupDestination](ctx, s, `SELECT * FROM backup_destinations WHERE id = ?`, id)
	if err != nil || d == nil {
		return d, err
	}
	d.SecretKey, err = secrets.Decrypt(d.SecretKey)
	return d, err
}

func (s *Store) ListBackupDestinations(ctx context.Context) ([]repo.BackupDestination, error) {
	out, err := list[repo.BackupDestination](ctx, s, `SELECT * FROM backup_destinations ORDER BY name`)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].SecretKey, err = secrets.Decrypt(out[i].SecretKey); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// UpdateBackupDestination rewrites the editable fields. An empty SecretKey
// keeps the stored one, the form never renders the secret back, so a save
// that leaves the field blank must not blank the credential.
func (s *Store) UpdateBackupDestination(ctx context.Context, d *repo.BackupDestination) error {
	enc := *d
	if d.SecretKey == "" {
		cur, err := s.GetBackupDestination(ctx, d.ID)
		if err != nil {
			return err
		}
		if cur != nil {
			enc.SecretKey = cur.SecretKey
		}
	}
	enc.SecretKey = secrets.Encrypt(enc.SecretKey)
	_, err := s.db.NamedExecContext(ctx,
		`UPDATE backup_destinations SET name = :name, endpoint = :endpoint, region = :region,
		 bucket = :bucket, access_key = :access_key, secret_key = :secret_key, shared = :shared WHERE id = :id`, &enc)
	return err
}

func (s *Store) DeleteBackupDestination(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM backup_destinations WHERE id = ?`, id)
	return err
}

// --- backup configs ---

func (s *Store) CreateBackup(ctx context.Context, b *repo.Backup) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO backups (id, tile_id, destination_id, kind, container_mode, cron, timezone, keep_latest, enabled, created_at)
		 VALUES (:id, :tile_id, :destination_id, :kind, :container_mode, :cron, :timezone, :keep_latest, :enabled, :created_at)`, b)
	return err
}

func (s *Store) GetBackup(ctx context.Context, id string) (*repo.Backup, error) {
	return get[repo.Backup](ctx, s, `SELECT * FROM backups WHERE id = ?`, id)
}

func (s *Store) ListBackupsByTile(ctx context.Context, tileID string) ([]repo.Backup, error) {
	return list[repo.Backup](ctx, s, `SELECT * FROM backups WHERE tile_id = ? ORDER BY created_at`, tileID)
}

func (s *Store) ListBackups(ctx context.Context) ([]repo.Backup, error) {
	return list[repo.Backup](ctx, s, `SELECT * FROM backups ORDER BY created_at`)
}

func (s *Store) UpdateBackup(ctx context.Context, b *repo.Backup) error {
	_, err := s.db.NamedExecContext(ctx,
		`UPDATE backups SET destination_id = :destination_id, container_mode = :container_mode,
		 cron = :cron, timezone = :timezone, keep_latest = :keep_latest, enabled = :enabled
		 WHERE id = :id`, b)
	return err
}

func (s *Store) DeleteBackup(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM backups WHERE id = ?`, id)
	return err
}

// --- run history ---

func (s *Store) CreateBackupRun(ctx context.Context, r *repo.BackupRun) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO backup_runs (id, backup_id, trigger, status, object_key, size_bytes, error, created_at, finished_at)
		 VALUES (:id, :backup_id, :trigger, :status, :object_key, :size_bytes, :error, :created_at, :finished_at)`, r)
	return err
}

func (s *Store) GetBackupRun(ctx context.Context, id string) (*repo.BackupRun, error) {
	return get[repo.BackupRun](ctx, s, `SELECT * FROM backup_runs WHERE id = ?`, id)
}

// rowid rather than created_at, see ListDeploymentsByTile / stacks.go.
// Display-only: keep-count pruning does NOT read this list (backup.go's prune
// sorts S3 object keys), so the only consumers are the two limit-10 history
// views in internal/web/handler.
func (s *Store) ListBackupRuns(ctx context.Context, backupID string, limit int) ([]repo.BackupRun, error) {
	if limit <= 0 {
		limit = 20
	}
	return list[repo.BackupRun](ctx, s,
		`SELECT * FROM backup_runs WHERE backup_id = ? ORDER BY rowid DESC LIMIT ?`, backupID, limit)
}

func (s *Store) UpdateBackupRun(ctx context.Context, r *repo.BackupRun) error {
	_, err := s.db.NamedExecContext(ctx,
		`UPDATE backup_runs SET status = :status, object_key = :object_key, size_bytes = :size_bytes,
		 error = :error, finished_at = :finished_at WHERE id = :id`, r)
	return err
}
