package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

func get[T any](ctx context.Context, s *Store, query string, args ...any) (*T, error) {
	var v T
	err := s.db.GetContext(ctx, &v, query, args...)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &v, nil
}

func list[T any](ctx context.Context, s *Store, query string, args ...any) ([]T, error) {
	var vs []T
	if err := s.db.SelectContext(ctx, &vs, query, args...); err != nil {
		return nil, err
	}
	return vs, nil
}

func (s *Store) GetOrg(ctx context.Context, id string) (*repo.Org, error) {
	return get[repo.Org](ctx, s, `SELECT * FROM orgs WHERE id = ?`, id)
}

func (s *Store) GetOrgBySlug(ctx context.Context, slug string) (*repo.Org, error) {
	return get[repo.Org](ctx, s, `SELECT * FROM orgs WHERE slug = ?`, slug)
}

func (s *Store) ListOrgs(ctx context.Context) ([]repo.Org, error) {
	return list[repo.Org](ctx, s, `SELECT * FROM orgs ORDER BY name`)
}

func (s *Store) CreateOrg(ctx context.Context, o *repo.Org) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO orgs (id, name, slug, created_at, setup_done_at, setup_mode)
		 VALUES (:id, :name, :slug, :created_at, :setup_done_at, :setup_mode)`, o)
	return err
}

func (s *Store) UpdateOrg(ctx context.Context, o *repo.Org) error {
	_, err := s.db.NamedExecContext(ctx,
		`UPDATE orgs SET name = :name, slug = :slug, avatar_path = :avatar_path,
		 config_connector_id = :config_connector_id, config_repo = :config_repo,
		 config_branch = :config_branch, config_path = :config_path,
		 setup_done_at = :setup_done_at, setup_mode = :setup_mode,
		 ui_edits = :ui_edits, env_colors = :env_colors, settings = :settings WHERE id = :id`, o)
	return err
}

// Org config plans mirror config_plans column-for-column (stack_id holds the
// ORG id, see migration 044) so repo.ConfigPlan and the whole plan render
// path are reused verbatim.

func (s *Store) CreateOrgConfigPlan(ctx context.Context, p *repo.ConfigPlan) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO org_config_plans (id, stack_id, env_slug, commit_sha, summary, plan, status, error, created_at)
		 VALUES (:id, :stack_id, :env_slug, :commit_sha, :summary, :plan, :status, :error, :created_at)`, p)
	return err
}

func (s *Store) GetOrgConfigPlan(ctx context.Context, id string) (*repo.ConfigPlan, error) {
	return get[repo.ConfigPlan](ctx, s, `SELECT * FROM org_config_plans WHERE id = ?`, id)
}

func (s *Store) ListOrgConfigPlans(ctx context.Context, orgID string, limit int) ([]repo.ConfigPlan, error) {
	return list[repo.ConfigPlan](ctx, s, `SELECT * FROM org_config_plans WHERE stack_id = ? ORDER BY rowid DESC LIMIT ?`, orgID, limit)
}

func (s *Store) SetOrgConfigPlanStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE org_config_plans SET status = ?, decided_at = CURRENT_TIMESTAMP WHERE id = ?`, status, id)
	return err
}

func (s *Store) SetOrgConfigPlanError(ctx context.Context, id, msg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE org_config_plans SET status = 'error', error = ?, decided_at = CURRENT_TIMESTAMP WHERE id = ?`, msg, id)
	return err
}

// SupersedePendingOrgPlans is SupersedePendingPlans for the org file. Same
// undecided set: 'clean' and 'error' rows are replaced by a newer plan too,
// they were not, so a clean run left the previous clean row sitting beside it
// and a parse failure stayed at the top of the list after the file was fixed.
// (stack_id holds the ORG id on this table, see the header comment.)
func (s *Store) SupersedePendingOrgPlans(ctx context.Context, orgID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE org_config_plans SET status = 'superseded', decided_at = CURRENT_TIMESTAMP
		 WHERE stack_id = ? AND status IN ('pending', 'clean', 'error')`, orgID)
	return err
}

// DeleteOrg removes the org and every canvas layout under it. The org's own
// layout plus one env- and one stack-scoped row set per stack: stacks cascade
// through their FK, but node_positions has none (it also holds org layouts), so
// those rows would be orphaned by the cascade.
func (s *Store) DeleteOrg(ctx context.Context, id string) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	if _, err := tx.ExecContext(ctx, `DELETE FROM node_positions WHERE owner_id = ?`,
		repo.GraphOwner(repo.ScopeOrg, id)); err != nil {
		return err
	}
	// Stack canvases are keyed by stack id; environment canvases by environment
	// id, so those are reached through the environments of this org's stacks.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM node_positions
		 WHERE owner_id IN (
		       SELECT ? || id FROM stacks WHERE org_id = ?
		       UNION ALL
		       SELECT ? || e.id FROM environments e
		         JOIN stacks s ON s.id = e.stack_id WHERE s.org_id = ?)`,
		repo.ScopeStack+":", id, repo.ScopeEnv+":", id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM orgs WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListStacksByOrg(ctx context.Context, orgID string) ([]repo.Stack, error) {
	return list[repo.Stack](ctx, s, `SELECT * FROM stacks WHERE org_id = ? ORDER BY name`, orgID)
}

func (s *Store) SetStackOrg(ctx context.Context, stackID, orgID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE stacks SET org_id = ? WHERE id = ?`, orgID, stackID)
	return err
}

// CreateStack inserts the stack and its home environment together: a stack
// without a home has nowhere to put a shared tile.
func (s *Store) CreateStack(ctx context.Context, st *repo.Stack) error {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.NamedExecContext(ctx,
		`INSERT INTO stacks (id, org_id, name, slug, description, settings,
		 config_connector_id, config_repo, config_branch, config_path, org_declared, proxy_middlewares, created_at)
		 VALUES (:id, :org_id, :name, :slug, :description, :settings,
		 :config_connector_id, :config_repo, :config_branch, :config_path, :org_declared, :proxy_middlewares, :created_at)`, st); err != nil {
		return err
	}
	if _, err := tx.NamedExecContext(ctx,
		`INSERT INTO environments (id, stack_id, name, slug, type, base_env_id, settings, config_branch, apply_policy, color, position, network, created_at)
		 VALUES (:id, :stack_id, :name, :slug, :type, :base_env_id, :settings, :config_branch, :apply_policy, :color, :position, :network, :created_at)`,
		repo.HomeEnv(st.ID, st.CreatedAt)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetStack(ctx context.Context, id string) (*repo.Stack, error) {
	return get[repo.Stack](ctx, s, `SELECT * FROM stacks WHERE id = ?`, id)
}

func (s *Store) GetStackBySlug(ctx context.Context, orgID, slug string) (*repo.Stack, error) {
	return get[repo.Stack](ctx, s, `SELECT * FROM stacks WHERE org_id = ? AND slug = ?`, orgID, slug)
}

func (s *Store) ListStacks(ctx context.Context) ([]repo.Stack, error) {
	return list[repo.Stack](ctx, s, `SELECT * FROM stacks ORDER BY name`)
}

func (s *Store) UpdateStack(ctx context.Context, st *repo.Stack) error {
	_, err := s.db.NamedExecContext(ctx,
		`UPDATE stacks SET name = :name, slug = :slug, description = :description, settings = :settings,
		 config_connector_id = :config_connector_id, config_repo = :config_repo,
		 config_branch = :config_branch, config_path = :config_path, org_declared = :org_declared,
		 ui_edits = :ui_edits, proxy_middlewares = :proxy_middlewares WHERE id = :id`, st)
	return err
}

// --- config plans ---

func (s *Store) CreateConfigPlan(ctx context.Context, p *repo.ConfigPlan) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO config_plans (id, stack_id, env_slug, commit_sha, summary, plan, status, error, created_at, decided_at)
		 VALUES (:id, :stack_id, :env_slug, :commit_sha, :summary, :plan, :status, :error, :created_at, :decided_at)`, p)
	return err
}

func (s *Store) GetConfigPlan(ctx context.Context, id string) (*repo.ConfigPlan, error) {
	return get[repo.ConfigPlan](ctx, s, `SELECT * FROM config_plans WHERE id = ?`, id)
}

// Newest-first here means insert order, not created_at. created_at is TEXT, so
// SQLite compares it character by character, and this table already holds two
// timestamp formats: the Go writes above pass a time.Time, while
// SetConfigPlanStatus and SupersedePendingPlans below write CURRENT_TIMESTAMP.
// A `T` between date and time sorts above a space, so one row in the other
// format pins itself as "latest" forever regardless of when it was written.
// The old `id DESC` tiebreak was no help, plan ids are random uuids, so it
// decided by a coin flip whenever two timestamps did compare equal.
//
// rowid is insert order for free and needs no format agreement. It
// does mean idx_config_plans_stack(stack_id, created_at DESC) no longer serves
// the sort, irrelevant at a few plans per stack; index it if that changes.

// LatestConfigPlan returns the newest plan for a stack (nil if none).
func (s *Store) LatestConfigPlan(ctx context.Context, stackID string) (*repo.ConfigPlan, error) {
	return get[repo.ConfigPlan](ctx, s,
		`SELECT * FROM config_plans WHERE stack_id = ? ORDER BY rowid DESC LIMIT 1`, stackID)
}

// CountStacksAwaitingPlan is how many of an org's stacks have a newest plan
// still waiting on someone (pending or error). Newest only: an old error plan
// that a later plan moved past is history, not a decision to make.
func (s *Store) CountStacksAwaitingPlan(ctx context.Context, orgID string) (int, error) {
	var n int
	err := s.db.GetContext(ctx, &n, `
		SELECT COUNT(*) FROM config_plans cp
		  JOIN stacks st ON st.id = cp.stack_id
		 WHERE st.org_id = ? AND cp.status IN ('pending', 'error')
		   AND cp.rowid = (SELECT MAX(rowid) FROM config_plans WHERE stack_id = cp.stack_id)`, orgID)
	return n, err
}

func (s *Store) ListConfigPlans(ctx context.Context, stackID string, limit int) ([]repo.ConfigPlan, error) {
	return list[repo.ConfigPlan](ctx, s,
		`SELECT * FROM config_plans WHERE stack_id = ? ORDER BY rowid DESC LIMIT ?`, stackID, limit)
}

// SetConfigPlanStatus updates a plan's lifecycle state.
func (s *Store) SetConfigPlanStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE config_plans SET status = ?, decided_at = CURRENT_TIMESTAMP WHERE id = ?`, status, id)
	return err
}

func (s *Store) SetConfigPlanError(ctx context.Context, id, msg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE config_plans SET error = ? WHERE id = ?`, msg, id)
	return err
}

// SupersedePendingPlans marks undecided plans with the same scope superseded
// (a newer plan replaces them). Env-scoped plans only supersede their own env.
//
// 'error' is in the list because a plan that failed to parse is as undecided
// as a pending one: the operator fixes the file, plans again, and the red row
// from the broken commit otherwise stays at the top of the list looking
// current forever. Only 'applied' and 'rejected' are decisions, and those two
// are history.
func (s *Store) SupersedePendingPlans(ctx context.Context, stackID, envSlug string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE config_plans SET status = 'superseded', decided_at = CURRENT_TIMESTAMP
		 WHERE stack_id = ? AND env_slug = ? AND status IN ('pending', 'clean', 'error')`, stackID, envSlug)
	return err
}

func (s *Store) DeleteStack(ctx context.Context, id string) error {
	// node_positions has no foreign key (it also holds org-level layouts), so
	// its rows for this stack's canvas and for each of its environments go
	// explicitly. Environment layouts are keyed by environment id, so they are
	// found through the environments table rather than by the stack id.
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM node_positions
		 WHERE owner_id = ?
		    OR owner_id IN (SELECT ? || id FROM environments WHERE stack_id = ?)`,
		repo.GraphOwner(repo.ScopeStack, id), repo.ScopeEnv+":", id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM stacks WHERE id = ?`, id)
	return err
}

func (s *Store) CreateEnvironment(ctx context.Context, e *repo.Environment) error {
	_, err := s.db.NamedExecContext(ctx,
		`INSERT INTO environments (id, stack_id, name, slug, type, base_env_id, settings, config_branch, apply_policy, color, position, network, created_at)
		 VALUES (:id, :stack_id, :name, :slug, :type, :base_env_id, :settings, :config_branch, :apply_policy, :color, :position, :network, :created_at)`, e)
	return err
}

func (s *Store) GetEnvironment(ctx context.Context, id string) (*repo.Environment, error) {
	return get[repo.Environment](ctx, s, `SELECT * FROM environments WHERE id = ?`, id)
}

func (s *Store) GetEnvironmentBySlug(ctx context.Context, stackID, slug string) (*repo.Environment, error) {
	return get[repo.Environment](ctx, s, `SELECT * FROM environments WHERE stack_id = ? AND slug = ?`, stackID, slug)
}

func (s *Store) ListEnvironmentsByStack(ctx context.Context, stackID string) ([]repo.Environment, error) {
	return list[repo.Environment](ctx, s,
		`SELECT * FROM environments WHERE stack_id = ? AND type != 'stack'
		 ORDER BY type = 'ephemeral', position, created_at`, stackID)
}

func (s *Store) HomeEnvironment(ctx context.Context, stackID string) (*repo.Environment, error) {
	return get[repo.Environment](ctx, s, `SELECT * FROM environments WHERE stack_id = ? AND type = 'stack'`, stackID)
}

func (s *Store) UpdateEnvironment(ctx context.Context, e *repo.Environment) error {
	_, err := s.db.NamedExecContext(ctx,
		`UPDATE environments SET name = :name, type = :type, settings = :settings,
		 config_branch = :config_branch, apply_policy = :apply_policy, color = :color, position = :position WHERE id = :id`, e)
	return err
}

// RenameEnvironment is RenameTile for an environment: same rule, the slug is
// identity and stays out of the general update.
func (s *Store) RenameEnvironment(ctx context.Context, id, name, slug string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE environments SET name = ?, slug = ? WHERE id = ?`, name, slug, id)
	return err
}

// SetEnvironmentProxy records where traefik landed on this env's network.
// Separate from UpdateEnvironment because the writer is the proxy, not the
// panel: folding it into the general update would let any settings save carry a
// stale (or zero) address back over it, the same shape as the bug that had
// UpdateTile reverting tile status.
func (s *Store) SetEnvironmentProxy(ctx context.Context, envID, ip, cidr string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE environments SET proxy_ip = ?, proxy_cidr = ? WHERE id = ?`, ip, cidr, envID)
	return err
}

// SetEnvironmentNetwork records the pooled overlay this env holds. Separate
// from UpdateEnvironment for the same reason as SetEnvironmentProxy: the
// writer is the pool, not a settings form, and a stale blank here would hand
// a live network to the next environment.
func (s *Store) SetEnvironmentNetwork(ctx context.Context, envID, name string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE environments SET network = ? WHERE id = ?`, name, envID)
	return err
}

// SetTileSharedNet records the pooled overlay a shared managed instance holds.
func (s *Store) SetTileSharedNet(ctx context.Context, tileID, name string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tiles SET shared_net = ? WHERE id = ?`, name, tileID)
	return err
}

// ClaimedNetworks lists every pooled network name a row still holds, from
// both owner tables at once. One query rather than walking stacks: the pool
// is global, and a name missed here is a network handed to a second tenant.
func (s *Store) ClaimedNetworks(ctx context.Context) ([]string, error) {
	var out []string
	err := s.db.SelectContext(ctx, &out,
		`SELECT network FROM environments WHERE network <> ''
		 UNION SELECT shared_net FROM tiles WHERE shared_net <> ''`)
	return out, err
}

// EnvironmentsWithoutNetwork lists environments that hold no pooled overlay.
//
// Only ever non-empty on an install upgraded across 009, which added the
// column with an empty default and backfilled nothing: every environment that
// existed before the upgrade sat on no overlay until its next deploy, and the
// proxy quietly left it out of its routing map in the meantime.
func (s *Store) EnvironmentsWithoutNetwork(ctx context.Context) ([]string, error) {
	var out []string
	err := s.db.SelectContext(ctx, &out, `SELECT id FROM environments WHERE network = ''`)
	return out, err
}

func (s *Store) DeleteEnvironment(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM environments WHERE id = ?`, id)
	return err
}
