package sqlite

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func (s *Store) CreateDeployment(ctx context.Context, d *repo.Deployment) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO deployments (id, tile_id, status, "trigger", commit_sha, image_tag, error, created_at, started_at, finished_at)
		 VALUES (:id, :tile_id, :status, :trigger, :commit_sha, :image_tag, :error, :created_at, :started_at, :finished_at)`, d)
	return err
}

func (s *Store) GetDeployment(ctx context.Context, id string) (*repo.Deployment, error) {
	return get[repo.Deployment](ctx, s, `SELECT * FROM deployments WHERE id = ?`, id)
}

// Newest-first by rowid, i.e. insert order, see the long note over
// LatestConfigPlan in stacks.go. created_at is TEXT, so ordering by it is a
// character comparison that only works while every writer agrees on a
// timestamp format, and one row written by a script or a migration in ISO form
// pins itself at the top forever.
// rowid ignores idx_deployments_tile(tile_id, created_at DESC), so
// this sorts every matching row instead of range-reading LIMIT rows off the
// index (same in notifications/backups/cron_runs). Fine at these row counts;
// re-point the index at rowid if a table ever grows enough to feel it.
func (s *Store) ListDeploymentsByTile(ctx context.Context, tileID string, limit int) ([]repo.Deployment, error) {
	return list[repo.Deployment](ctx, s,
		`SELECT * FROM deployments WHERE tile_id = ? ORDER BY rowid DESC LIMIT ?`, tileID, limit)
}

// ListDeploymentsByStatus: every deployment in one state, the CI gate scans
// waiting_ci rows each tick. Oldest first so grace/timeout fire in order.
func (s *Store) ListDeploymentsByStatus(ctx context.Context, status string) ([]repo.Deployment, error) {
	return list[repo.Deployment](ctx, s,
		`SELECT * FROM deployments WHERE status = ? ORDER BY rowid`, status)
}

func (s *Store) UpdateDeployment(ctx context.Context, d *repo.Deployment) error {
	_, err := s.db.NamedExecContext(ctx,
		`UPDATE deployments SET status = :status, commit_sha = :commit_sha, image_tag = :image_tag,
		 error = :error, started_at = :started_at, finished_at = :finished_at
		 WHERE id = :id`, d)
	return err
}

// SweepStaleRuns finishes work that died with the previous process. A
// deployment is only ever moved out of queued/running by the goroutine driving
// it, so anything still in those states at startup is orphaned; the tile it was
// building is marked stopped rather than error, and the metrics reconciler
// promotes it back to running on its next tick if the container is in fact up.
func (s *Store) SweepStaleRuns(ctx context.Context) error {
	for _, q := range []string{
		`UPDATE deployments SET status = 'error', error = '` + repo.InterruptedMsg + `',
		 finished_at = CURRENT_TIMESTAMP WHERE status IN ('queued', 'running')`,
		`UPDATE tiles SET status = 'stopped', updated_at = CURRENT_TIMESTAMP WHERE status = 'building'`,
		// A backup run is only ever finished by the process that started it, so
		// one left "running" is one that died with a restart, the history
		// would otherwise show it as still in progress forever.
		`UPDATE backup_runs SET status = 'error', error = '` + repo.InterruptedMsg + `',
		 finished_at = CURRENT_TIMESTAMP WHERE status = 'running'`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// storePath is the one spelling of "no path" that reaches the table. Migration
// 017 normalized every row to "/" and put the uniqueness index on (host, path),
// so a writer storing "" would slip a second router past the index for a host
// another tile already claims. Callers disagree ("" from config, "/" from the
// API), so normalize here rather than in each of them.
func storePath(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

func (s *Store) CreateDomain(ctx context.Context, d *repo.Domain) error {
	enc := *d
	enc.Path = storePath(d.Path)
	enc.KeyPEM = secrets.Encrypt(d.KeyPEM)
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO domains (id, tile_id, host, path, container_port, https, force_https, redirect_to, cert_pem, key_pem, auto, position, middlewares, priority, rule, created_at)
		 VALUES (:id, :tile_id, :host, :path, :container_port, :https, :force_https, :redirect_to, :cert_pem, :key_pem, :auto, :position, :middlewares, :priority, :rule, :created_at)`, &enc)
	return err
}

func decryptDomains(ds []repo.Domain, err error) ([]repo.Domain, error) {
	if err != nil {
		return ds, err
	}
	for i := range ds {
		if ds[i].KeyPEM, err = secrets.Decrypt(ds[i].KeyPEM); err != nil {
			return ds, err
		}
	}
	return ds, nil
}

func (s *Store) GetDomain(ctx context.Context, id string) (*repo.Domain, error) {
	d, err := get[repo.Domain](ctx, s, `SELECT * FROM domains WHERE id = ?`, id)
	if err != nil || d == nil {
		return d, err
	}
	d.KeyPEM, err = secrets.Decrypt(d.KeyPEM)
	return d, err
}

// GetDomainByHostPath finds the domain claiming a host+path (nil if free),
// the plain row before any rule row on it. Used to reject duplicate and
// cross-tenant host claims before insert: a rule row owns the host+path for
// its tile as much as a plain one does.
func (s *Store) GetDomainByHostPath(ctx context.Context, host, path string) (*repo.Domain, error) {
	return get[repo.Domain](ctx, s, `SELECT * FROM domains WHERE host = ? AND path = ? ORDER BY rule LIMIT 1`, host, storePath(path))
}

// ListDomainsByTile orders by declaration position first, the primary
// domain (STACKR_PUBLIC_URL) is the first row.
func (s *Store) ListDomainsByTile(ctx context.Context, tileID string) ([]repo.Domain, error) {
	return decryptDomains(list[repo.Domain](ctx, s, `SELECT * FROM domains WHERE tile_id = ? ORDER BY position, host`, tileID))
}

func (s *Store) ListDomains(ctx context.Context) ([]repo.Domain, error) {
	return decryptDomains(list[repo.Domain](ctx, s, `SELECT * FROM domains ORDER BY host`))
}

func (s *Store) SetDomainCert(ctx context.Context, id, certPEM, keyPEM string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE domains SET cert_pem = ?, key_pem = ? WHERE id = ?`,
		certPEM, secrets.Encrypt(keyPEM), id)
	return err
}

func (s *Store) SetDomainHTTPS(ctx context.Context, id string, https bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE domains SET https = ? WHERE id = ?`, https, id)
	return err
}

// SetDomainForceHTTPS flips the plain-HTTP bounce on its own. Serving TLS and
// redirecting onto it are two questions; a host that must still answer on http
// can have a certificate without being redirected.
func (s *Store) SetDomainForceHTTPS(ctx context.Context, id string, force bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE domains SET force_https = ? WHERE id = ?`, force, id)
	return err
}

// SetDomainResourceACME sets the Let's Encrypt account certificates under this
// resource are issued on. Blank uses the instance's.
func (s *Store) SetDomainResourceACME(ctx context.Context, id, email string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE domain_resources SET acme_email = ? WHERE id = ?`, email, id)
	return err
}

func (s *Store) DeleteDomain(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM domains WHERE id = ?`, id)
	return err
}

// --- domain resources ---

func (s *Store) CreateDomainResource(ctx context.Context, r *repo.DomainResource) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO domain_resources (id, level, owner_id, host, include_env_on_default, acme_email, declared, created_at)
		 VALUES (:id, :level, :owner_id, :host, :include_env_on_default, :acme_email, :declared, :created_at)`, r)
	return err
}

func (s *Store) UpdateDomainResource(ctx context.Context, r *repo.DomainResource) error {
	_, err := s.db.NamedExecContext(ctx,
		`UPDATE domain_resources SET host = :host, include_env_on_default = :include_env_on_default,
		 acme_email = :acme_email, declared = :declared WHERE id = :id`, r)
	return err
}

// ListDomainResources returns every resource, a handful of rows; callers
// filter by visibility (own stack, its org, the instance).
func (s *Store) ListDomainResources(ctx context.Context) ([]repo.DomainResource, error) {
	return list[repo.DomainResource](ctx, s, `SELECT * FROM domain_resources ORDER BY created_at, host`)
}

func (s *Store) DeleteDomainResource(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM domain_resources WHERE id = ?`, id)
	return err
}
