package service

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/envcolor"
	"github.com/FyrmForge/stackr/internal/stackrd/service/scheduler"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// EnvOps is the half of an environment's lifecycle that lives below this
// package: cloning a base env's tiles, and tearing an env's containers,
// routes, networks and pooled names down. It is an interface because the
// implementation (config/envops) imports this package.
type EnvOps interface {
	CloneTiles(ctx context.Context, env *repo.Environment) error
	Teardown(ctx context.Context, stack *repo.Stack, env *repo.Environment) error
}

// EnvironmentService owns the environment row and what creating, deleting or
// resetting one has to cascade.
//
// Environment creation had four rule sets and a fifth creator with none at
// all: the panel checked two reserved names, the API's create checked only
// that the name was non-empty, the API's copy checked all three, a config
// apply checked nothing, and the pull-request hook checked nothing either.
// The gap that bit was the home environment's reserved slug: the API knew
// about it and the panel did not, so an environment a user named "Stack" hit
// a UNIQUE constraint and surfaced as a raw 500.
type EnvironmentService struct {
	store repo.Store
	ops   EnvOps
	sched *scheduler.Service
	plan  Replanner
	gate  *GateService
}

func NewEnvironmentService(store repo.Store, ops EnvOps, sched *scheduler.Service,
	plan Replanner, gate *GateService) *EnvironmentService {
	return &EnvironmentService{store: store, ops: ops, sched: sched, plan: plan, gate: gate}
}

// CreateEnv is what a caller asks for. BaseEnvID makes it a clone: the same
// tiles, nothing deployed.
type CreateEnv struct {
	Name      string
	Type      string // "static" (default) or "ephemeral"
	BaseEnvID string

	// The three the config file owns and the panel does not. They were the
	// reason a config apply built the row itself instead of calling Adopt,
	// and building it itself is how it skipped the reserved-slug and
	// duplicate checks the other four callers get.
	Color       string
	ApplyPolicy string
	Position    int
}

// reservedEnvSlug names the slugs an environment may not take. "settings" and
// "list" are URL segments one level up from an environment's own; HomeSlug is
// the hidden stack-scoped environment, a real row with a UNIQUE index behind
// it.
func reservedEnvSlug(slug string) bool {
	return slug == "settings" || slug == "list" || slug == repo.HomeSlug
}

// Create adds an environment to a stack. One rule set for every caller.
func (s *EnvironmentService) Create(ctx context.Context, stack *repo.Stack, in CreateEnv, by Actor) (*repo.Environment, error) {
	if stack == nil {
		return nil, svcerr.ErrNotFound
	}
	// Structural: the file owns which environments a managed stack has, so
	// this refuses there whatever ui_edits says.
	if _, err := s.gate.Gate(ctx, stack.ID, GateStructural, by.Surface()); err != nil {
		return nil, err
	}
	return s.Adopt(ctx, stack, in)
}

// Adopt is Create without the gate: the same rules, for the creators that are
// the config engine rather than a person. The pull-request hook is the one
// that matters — it creates a preview environment on a stack the file owns,
// which is exactly what Create's gate exists to refuse — and it is also the
// creator that had no rules at all, so a branch whose slug collided with an
// existing environment surfaced as a raw UNIQUE error.
func (s *EnvironmentService) Adopt(ctx context.Context, stack *repo.Stack, in CreateEnv) (*repo.Environment, error) {
	if stack == nil {
		return nil, svcerr.ErrNotFound
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, invalid("name", "required")
	}
	slug := repo.Slugify(name)
	if slug == "" {
		return nil, invalid("name", "needs at least one letter or number")
	}
	if reservedEnvSlug(slug) {
		return nil, svcerr.Invalidf("name", "%q is reserved", slug)
	}
	// The duplicate check the panel never had. Without it the UNIQUE index on
	// (stack_id, slug) is what refuses, as an unmapped 500.
	if existing, _ := s.store.GetEnvironmentBySlug(ctx, stack.ID, slug); existing != nil {
		return nil, svcerr.Conflictf("an environment named %q already exists in this stack", existing.Name)
	}
	envType := "static"
	if in.Type == "ephemeral" {
		envType = "ephemeral"
	}
	env := &repo.Environment{ID: uuid.New().String(), StackID: stack.ID, Name: name,
		Slug: slug, Type: envType, Settings: "{}", CreatedAt: time.Now().UTC(),
		Color: in.Color, ApplyPolicy: in.ApplyPolicy, Position: in.Position}
	if in.BaseEnvID != "" {
		base, err := s.store.GetEnvironment(ctx, in.BaseEnvID)
		if err != nil {
			return nil, err
		}
		if base == nil || base.StackID != stack.ID {
			return nil, invalid("base_env_id", "invalid base environment")
		}
		env.BaseEnvID, env.Settings = base.ID, base.Settings
	}
	if err := s.store.CreateEnvironment(ctx, env); err != nil {
		return nil, err
	}
	if env.BaseEnvID != "" && s.ops != nil {
		if err := s.ops.CloneTiles(ctx, env); err != nil {
			return nil, err
		}
		// A clone brings its base's crons and backups with it, and they only
		// start ticking once the tables are re-read. The panel reloaded only
		// on the clone branch and the API reloaded always; reloading always is
		// right and costs nothing on an empty env.
	}
	if s.sched != nil {
		s.sched.Reload(ctx)
	}
	return env, nil
}

// Delete tears an environment down and removes its row.
//
// force covers the running check. It is the caller's, and on the CLI `-y` has
// been doubling as it, so a scripted "don't prompt me" tore down running
// tiles; that is the CLI's to fix, and the refusal has to exist here for the
// fix to have anything to land on. The panel had no running check at all.
func (s *EnvironmentService) Delete(ctx context.Context, stack *repo.Stack, env *repo.Environment, force bool, by Actor) error {
	if stack == nil || env == nil {
		return svcerr.ErrNotFound
	}
	if _, err := s.gate.Gate(ctx, stack.ID, GateStructural, by.Surface()); err != nil {
		return err
	}
	envs, err := s.store.ListEnvironmentsByStack(ctx, stack.ID)
	if err != nil {
		return err
	}
	if len(envs) <= 1 {
		return invalid("", "a stack needs at least one environment")
	}
	if !force {
		if running, err := s.running(ctx, env.ID); err != nil {
			return err
		} else if running {
			return svcerr.Conflictf("tiles are running in %s; force the delete to tear them down", env.Slug)
		}
	}
	return s.teardown(ctx, stack, env)
}

// Reset tears a config-managed environment down so the next apply rebuilds it
// from the file, keeping the row. It exists because a half-applied env had no
// way out inside the product.
func (s *EnvironmentService) Reset(ctx context.Context, stack *repo.Stack, env *repo.Environment, force bool, by Actor) error {
	if stack == nil || env == nil {
		return svcerr.ErrNotFound
	}
	// The inverse of Delete's gate: reset is *for* the managed stacks.
	if !stack.ConfigManaged() {
		return invalid("", "reset is for config-managed stacks; delete the environment instead")
	}
	if !force {
		if running, err := s.running(ctx, env.ID); err != nil {
			return err
		} else if running {
			return svcerr.Conflictf("tiles are running in %s; force the reset to tear them down", env.Slug)
		}
	}
	if s.ops != nil {
		if err := s.ops.Teardown(ctx, stack, env); err != nil {
			return err
		}
	}
	if s.sched != nil {
		s.sched.Reload(ctx)
	}
	// The half the API skipped. A reset leaves the stack's plans describing
	// tiles that no longer exist, and without this the canvas keeps offering
	// the stale one to apply.
	if s.plan != nil {
		s.plan.Replan(ctx, stack)
	}
	return nil
}

// teardown is the delete's cascade, including the two tables nothing cleaned
// up on any surface.
func (s *EnvironmentService) teardown(ctx context.Context, stack *repo.Stack, env *repo.Environment) error {
	if s.ops != nil {
		if err := s.ops.Teardown(ctx, stack, env); err != nil {
			return err
		}
	}
	// staged_changes has no foreign key to environments (001_initial), so a
	// deleted env's pending set survived it on every surface — and a later env
	// reusing the id, or simply the stack-wide listing, then showed edits to
	// tiles that no longer exist.
	if staged, err := s.store.ListStagedByEnv(ctx, env.ID); err == nil {
		for i := range staged {
			if derr := s.store.DeleteStagedChange(ctx, staged[i].ID); derr != nil {
				slog.Error("staged change not removed with its environment",
					"env", env.ID, "staged", staged[i].ID, "error", derr)
			}
		}
	}
	// Same for the canvas layout. Deleting a *stack* cleans its positions up;
	// deleting one environment never did.
	if err := s.store.DeleteNodePositions(ctx, env.ID); err != nil {
		slog.Error("canvas layout not removed with its environment", "env", env.ID, "error", err)
	}
	if s.sched != nil {
		s.sched.Reload(ctx)
	}
	return nil
}

// running reports whether anything in the environment is up. What "force"
// exists to override.
func (s *EnvironmentService) running(ctx context.Context, envID string) (bool, error) {
	tiles, err := s.store.ListTilesByEnv(ctx, envID)
	if err != nil {
		return false, err
	}
	for i := range tiles {
		if tiles[i].Status == "running" {
			return true, nil
		}
	}
	return false, nil
}

// EnvPatch is the per-environment settings both surfaces expose. A nil field
// is "not sent", so a caller that knows about one key cannot clear another.
type EnvPatch struct {
	Color        *string
	ApplyPolicy  *string
	ConfigBranch *string
}

// Update writes the per-environment settings.
//
// The colour rule used to differ by surface — the panel refused it on a
// config-managed stack, the API accepted it — and the apply policy differed
// the other way, the panel allowing it only on a managed stack. Neither key
// is declared in a stack file today, so neither is the file's: both are
// accepted on any stack, and it is the config engine that stopped overwriting
// them (see the apply-side note in 04-progress).
func (s *EnvironmentService) Update(ctx context.Context, env *repo.Environment, in EnvPatch, by Actor) error {
	if env == nil {
		return svcerr.ErrNotFound
	}
	if in.Color != nil {
		c := strings.TrimSpace(*in.Color)
		// SP1: an unrecognised colour is refused, not folded to the default.
		if c != "" && !envcolor.Valid(c) {
			return invalid("color", "not a known colour")
		}
		env.Color = c
	}
	if in.ApplyPolicy != nil {
		switch p := strings.TrimSpace(*in.ApplyPolicy); p {
		case "", "auto", "manual":
			env.ApplyPolicy = p
		default:
			return invalid("apply_policy", "must be auto or manual")
		}
	}
	if in.ConfigBranch != nil {
		env.ConfigBranch = strings.TrimSpace(*in.ConfigBranch)
	}
	return s.store.UpdateEnvironment(ctx, env)
}

// --- reads ---
//
// The store answers a missing row with (nil, nil), so every one of the forty
// handlers that read an environment wrote the same two checks: the error, then
// the nil. These answer svcerr.ErrNotFound instead, which the handlers' error
// mapping already turns into a 404. A caller that tolerates absence — the
// pull-request hook asking whether a preview environment exists yet — says so
// with errors.Is rather than by reading a nil.

// Get is one environment by id.
func (s *EnvironmentService) Get(ctx context.Context, id string) (*repo.Environment, error) {
	e, err := s.store.GetEnvironment(ctx, id)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, svcerr.ErrNotFound
	}
	return e, nil
}

// BySlug is one environment by its slug within a stack.
func (s *EnvironmentService) BySlug(ctx context.Context, stackID, slug string) (*repo.Environment, error) {
	e, err := s.store.GetEnvironmentBySlug(ctx, stackID, slug)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, svcerr.ErrNotFound
	}
	return e, nil
}

// ListForStack is a stack's environments in ladder order: static ones by
// position, then the ephemeral ones. The order is the store's, and the panel's
// environment switcher depends on it.
func (s *EnvironmentService) ListForStack(ctx context.Context, stackID string) ([]repo.Environment, error) {
	return s.store.ListEnvironmentsByStack(ctx, stackID)
}

// Home is a stack's hidden stack-scoped environment, the one that holds
// whatever belongs to the stack rather than to any of its environments. Nil
// when the stack has none.
func (s *EnvironmentService) Home(ctx context.Context, stackID string) (*repo.Environment, error) {
	return s.store.HomeEnvironment(ctx, stackID)
}

// --- writes the row's other owners used to make themselves ---
//
// Everything below exists because something outside this package was writing
// the environments table directly: the config applier, the env ops, the
// network pool, the proxy recorder. None of those writes had a rule to skip,
// which is exactly why they were easy to leave — and why the table ended up
// with five writers and one of them (the applier) quietly not running the
// name checks the other four did.

// Save persists an environment row a caller has already mutated. The config
// applier builds the whole row from the file and writes it wholesale; Update
// is the patch-shaped door for the surfaces that change one field.
func (s *EnvironmentService) Save(ctx context.Context, env *repo.Environment) error {
	if env == nil {
		return svcerr.ErrNotFound
	}
	return s.store.UpdateEnvironment(ctx, env)
}

// Rename changes an environment's display name and slug together. The two
// always move as a pair — a slug that does not match its name is how the
// generated hostname stops matching the panel.
func (s *EnvironmentService) Rename(ctx context.Context, env *repo.Environment, name, slug string) error {
	if env == nil {
		return svcerr.ErrNotFound
	}
	name, slug = strings.TrimSpace(name), strings.TrimSpace(slug)
	if slug == "" {
		slug = repo.Slugify(name)
	}
	if slug == "" {
		return invalid("name", "needs at least one letter or number")
	}
	if reservedEnvSlug(slug) {
		return svcerr.Invalidf("name", "%q is reserved", slug)
	}
	if other, _ := s.store.GetEnvironmentBySlug(ctx, env.StackID, slug); other != nil && other.ID != env.ID {
		return svcerr.Conflictf("an environment named %q already exists in this stack", other.Name)
	}
	if err := s.store.RenameEnvironment(ctx, env.ID, name, slug); err != nil {
		return err
	}
	env.Name, env.Slug = name, slug
	return nil
}

// Remove deletes the row and nothing else. Delete is the door with the
// teardown behind it; this is for callers that have already torn down.
func (s *EnvironmentService) Remove(ctx context.Context, id string) error {
	return s.store.DeleteEnvironment(ctx, id)
}

// SetNetwork records the overlay network an environment holds, or clears it.
// The pool below decides the name; the row is this package's.
func (s *EnvironmentService) SetNetwork(ctx context.Context, envID, network string) error {
	return s.store.SetEnvironmentNetwork(ctx, envID, network)
}

// SetProxy records where an environment's proxy answered. Best-effort state,
// written after the container is up.
func (s *EnvironmentService) SetProxy(ctx context.Context, envID, ip, cidr string) error {
	return s.store.SetEnvironmentProxy(ctx, envID, ip, cidr)
}

// EnsureHome puts a stack's hidden stack-scoped environment back. A stack
// made before the home existed, or one whose row was wiped, has none; the
// store owns what a home looks like, so this is a restore rather than a
// create and skips every name rule on purpose.
func (s *EnvironmentService) EnsureHome(ctx context.Context, stackID string, now time.Time) (*repo.Environment, error) {
	if env, err := s.store.GetEnvironmentBySlug(ctx, stackID, repo.HomeSlug); err != nil {
		return nil, err
	} else if env != nil {
		return env, nil
	}
	env := repo.HomeEnv(stackID, now)
	if err := s.store.CreateEnvironment(ctx, env); err != nil {
		return nil, err
	}
	return env, nil
}
