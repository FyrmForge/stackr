package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	"github.com/FyrmForge/stackr/internal/stackrd/config/staging"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/service/notify"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// ManagedInstanceService owns a managed database instance: the row, the
// container behind it, and the teardown that has to take both away together.
//
// It exists because "delete an instance" was four teardowns and none of them
// was complete (02-findings 2.1): the panel released the network pool but
// never dropped the Traefik route, the API dropped the route but never
// released the pool and with force=true left every provision and resource row
// behind, a config apply had no held-slices gate at all, and orgconf did the
// pool and nothing else. Each surface's omission is a leak, so the one method
// here does the union.
//
// It is a sibling of TileService rather than part of it: a managed instance
// is a tile row, but almost nothing TileService decides applies to it. It
// never builds, its container is recreated rather than rolled, its settings
// are a different (much shorter) list, and it provides slices that other
// tiles consume.
type ManagedInstanceService struct {
	store    repo.Store
	dbs      *managedtiles.Service
	tiles    *TileService
	gate     *GateService
	notifier *notify.Notifier
}

func NewManagedInstanceService(store repo.Store, dbs *managedtiles.Service,
	tiles *TileService, gate *GateService, n *notify.Notifier) *ManagedInstanceService {
	return &ManagedInstanceService{store: store, dbs: dbs, tiles: tiles, gate: gate, notifier: n}
}

// ApplyScope resolves a scope name onto the tile. ScopeID is derived from the
// stack, never taken from the caller, so no request can point an instance at
// another tenant. SP1: an unknown scope is refused, not coerced — the panel
// used to fall through to "env", so a typo silently narrowed the sharing of
// an instance other stacks were already provisioning from.
func ApplyScope(t *repo.Tile, s *repo.Stack, scope string) error {
	switch scope {
	case "", "env":
		t.ScopeKind, t.ScopeID = "env", ""
	case "stack":
		t.ScopeKind, t.ScopeID = "stack", s.ID
	case "org":
		t.ScopeKind, t.ScopeID = "org", s.OrgID
	default:
		return invalid("scope", "must be env, stack, or org")
	}
	return nil
}

// fileUnowned refuses a write the stack's config file owns, with the
// org-scope exception: a stack file cannot declare an org-scoped instance
// (`shared:` stops at stack scope), so those stay panel-owned even on a
// config-managed stack. The API had no such exception, so an org-scoped
// instance took a settings edit from a browser and 409'd the identical CLI
// edit.
//
// Unlike a service tile, a managed instance's settings never stage, on any
// surface or any ui_edits setting: there is no staged shape the config engine
// could apply for one. `ui_edits: stage` therefore refuses here where it
// would queue a service tile's edit.
func (s *ManagedInstanceService) fileUnowned(ctx context.Context, t *repo.Tile) error {
	if t.ScopeKind == "org" {
		return nil
	}
	return s.unmanaged(ctx, t.StackID)
}

// unmanaged is fileUnowned without the org exception, for the writes that
// change the scope itself. Moving an instance in or out of org scope on a
// managed stack makes it appear or vanish from the config snapshot
// mid-flight, so that one is never exempt.
func (s *ManagedInstanceService) unmanaged(ctx context.Context, stackID string) error {
	st, err := s.store.GetStack(ctx, stackID)
	if err != nil || st == nil {
		return svcerr.ErrNotFound
	}
	if st.ConfigManaged() {
		return ManagedConflict(st)
	}
	return nil
}

// Create adds a managed instance to an environment.
//
// The caller hands over the row it wants — name, engine, and whatever its
// wire format carries — and this fills the identity, the generated
// credentials and the engine defaults, then either stages the creation or
// performs it and deploys.
//
// stagedPatch is the payload a staged create carries: a config object, which
// is the config engine's shape and therefore the caller's to build (see
// TileService.Create for why this package cannot name the type).
func (s *ManagedInstanceService) Create(ctx context.Context, t *repo.Tile, scope string,
	stagedPatch any, by Actor) (staged bool, err error) {
	if t == nil {
		return false, svcerr.ErrNotFound
	}
	st, err := s.store.GetStack(ctx, t.StackID)
	if err != nil || st == nil {
		return false, svcerr.ErrNotFound
	}
	stage, err := s.gate.Gate(ctx, t.StackID, GateStructural, by.Surface())
	if err != nil {
		return false, err
	}
	t.Name = strings.TrimSpace(t.Name)
	if t.Name == "" {
		return false, invalid("name", "required")
	}
	if _, ok := managedtiles.Engines[t.Engine]; !ok {
		// SP1 again: the API accepted an unknown engine and only failed later
		// inside NewDB, after the tenancy checks had already passed.
		return false, svcerr.Invalidf("engine", "unknown engine %q", t.Engine)
	}
	if err := ApplyScope(t, st, scope); err != nil {
		return false, err
	}
	// The API accepted an empty slug where the panel refused it, so a database
	// named "!!" landed with no address anything could reference.
	t.Slug = repo.Slugify(t.Name)
	if t.Slug == "" {
		return false, invalid("name", "needs at least one letter or number")
	}
	if repo.ReservedSlug(t.Slug) {
		return false, svcerr.Invalidf("name", "%q is reserved for variable references; pick another name", t.Slug)
	}
	if existing, _ := s.store.GetTileBySlug(ctx, t.EnvironmentID, t.Slug); existing != nil {
		return false, svcerr.Conflictf("a tile named %q already exists in this environment", existing.Name)
	}
	now := time.Now().UTC()
	if t.ID == "" {
		t.ID = uuid.New().String()
	}
	t.Kind, t.SourceType = "service", "image"
	if t.WebhookToken == "" {
		t.WebhookToken = secrets.RandomHex(24)
	}
	t.Status, t.CreatedAt, t.UpdatedAt = "idle", now, now
	if stage {
		// Credentials are generated at apply time by the reconcile engine, the
		// same as a config-managed create, so NewDB is deliberately not run on
		// this branch: a password staged here would never be the one used.
		return true, staging.Stage(ctx, s.store, t, by.ID, by.Name, "create",
			staging.OpCreate, stagedPatch)
	}
	if err := managedtiles.NewDB(t); err != nil {
		return false, svcerr.Invalidf("engine", "%v", err)
	}
	if err := s.store.CreateTile(ctx, t); err != nil {
		return false, err
	}
	managedtiles.PublishConnection(ctx, s.store, t)
	// Reported, not swallowed: a create whose container never came up is the
	// one thing the caller most needs to hear about, and the row is already
	// there either way (the status column says which).
	return false, s.Deploy(ctx, t)
}

// Update writes a settings edit to an already-loaded, already-edited instance
// row and recreates the container if the edit is one the container embodies.
//
// Same contract as TileService.Update: the caller maps its own request shape
// onto the row it loaded and hands the whole row over, and does not get to
// decide what happens next. The panel redeployed on every save whatever
// moved, the API had its own five-field list, a config apply had an
// eight-field one and orgconf compared three.
func (s *ManagedInstanceService) Update(ctx context.Context, t *repo.Tile, by Actor) error {
	if t == nil || !t.IsManaged() {
		return svcerr.ErrNotFound
	}
	if err := s.fileUnowned(ctx, t); err != nil {
		return err
	}
	if err := s.Validate(t); err != nil {
		return err
	}
	old, err := s.store.GetTile(ctx, t.ID)
	if err != nil {
		return err
	}
	changed := DiffTiles(old, t)
	t.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdateTile(ctx, t); err != nil {
		return err
	}
	return s.AfterWrite(ctx, t, changed)
}

// AfterWrite recreates the container when a change needs it.
//
// Exported, unlike TileService.afterWrite: an instance's redeploy is
// synchronous (there is no build to queue and no deployment row to record),
// so a config apply can and does call this rather than keeping a fifth copy
// of the decision and the status bookkeeping.
func (s *ManagedInstanceService) AfterWrite(ctx context.Context, t *repo.Tile, changed Changed) error {
	// "error" redeploys as well as "running": an errored instance is a
	// failure — often the last write's own, since a rejected setting takes the
	// container down — and the next write is the natural place to heal it. A
	// deliberately stopped instance is left alone.
	if t.Status != "running" && t.Status != "error" {
		return nil
	}
	if !changed.NeedsDBRedeploy() {
		return nil
	}
	return s.Deploy(ctx, t)
}

// Validate is the union of the four surfaces' rules on an instance's
// settings. SP1 throughout: a value out of range is refused, not folded to
// something else, because the panel's coercions turned a typo into a silent
// setting change (a bad port became "not published at all").
//
// The two things it does normalise are not the user's choice: an empty image
// means the engine default, and docker itself rejects a memory cap under 6MB.
func (s *ManagedInstanceService) Validate(t *repo.Tile) error {
	eng, ok := managedtiles.Engines[t.Engine]
	if !ok {
		return svcerr.Invalidf("engine", "unknown engine %q", t.Engine)
	}
	if t.ExternalPort < 0 || t.ExternalPort > 65535 {
		return invalid("external_port", "must be between 0 and 65535")
	}
	if t.CPULimit < 0 {
		return invalid("cpu_limit", "must not be negative")
	}
	if t.MemLimitMB < 0 {
		return invalid("mem_limit_mb", "must not be negative")
	}
	if t.MemLimitMB > 0 && t.MemLimitMB < 6 {
		t.MemLimitMB = 6 // docker rejects memory caps under 6MB
	}
	if t.ShmSizeMB < 0 {
		return invalid("shm_size_mb", "must not be negative")
	}
	if t.ImageRef == "" {
		// A per-instance image is a version pin or a wire-compatible build;
		// clearing it means "back to whatever the engine ships with".
		t.ImageRef = eng.DefaultImage
	}
	switch t.UpdatePolicy {
	case "", "off":
		t.UpdatePolicy = "off"
	case "notify", "auto":
	default:
		return invalid("update_policy", "must be off, notify or auto")
	}
	return nil
}

// SetScope changes how widely an instance is shared. No redeploy: scope
// governs who may provision from it, and the running container knows nothing
// about it.
func (s *ManagedInstanceService) SetScope(ctx context.Context, t *repo.Tile, scope string, by Actor) error {
	if err := s.PlanScope(ctx, t, scope); err != nil {
		return err
	}
	t.UpdatedAt = time.Now().UTC()
	return s.store.UpdateTile(ctx, t)
}

// PlanScope is SetScope's gate and mapping without the write, for a caller
// that is about to write the row anyway. The API's PATCH merges the scope
// with the settings, and doing both writes would mean Update diffing against
// a row this had already saved — so the settings would persist and the
// container would never be recreated, which is the exact bug this point
// exists to close.
func (s *ManagedInstanceService) PlanScope(ctx context.Context, t *repo.Tile, scope string) error {
	if t == nil || !t.IsManaged() {
		return svcerr.ErrNotFound
	}
	if err := s.unmanaged(ctx, t.StackID); err != nil {
		return err
	}
	st, err := s.store.GetStack(ctx, t.StackID)
	if err != nil || st == nil {
		return svcerr.ErrNotFound
	}
	return ApplyScope(t, st, scope)
}

// Deploy recreates the instance's container and records the outcome on the
// row. The "deploy then write a status then tell the canvas" sequence was
// hand-copied at six call sites, and one of them (the image watcher) wrote no
// status at all; this is the one copy above the infra line.
func (s *ManagedInstanceService) Deploy(ctx context.Context, t *repo.Tile) error {
	if s.dbs == nil {
		return fmt.Errorf("database engine: %w", svcerr.ErrUnavailable)
	}
	if err := s.dbs.Deploy(ctx, t); err != nil {
		s.setStatus(ctx, t, "error")
		return err
	}
	s.setStatus(ctx, t, "running")
	return nil
}

// Delete removes an instance, the container behind it, and every slice cut
// from it.
//
// force is the caller's, not a constant: deleting a shared instance destroys
// the data of every consumer holding a slice on it, so the default is to
// refuse and make the caller say it means it.
func (s *ManagedInstanceService) Delete(ctx context.Context, t *repo.Tile, force bool, by Actor) error {
	if t == nil || !t.IsManaged() {
		return svcerr.ErrNotFound
	}
	// Checked before the provisions gate so a config-managed stack gets the
	// actionable answer ("edit the config file") rather than "drop them first".
	if err := s.fileUnowned(ctx, t); err != nil {
		return err
	}
	return s.TearDown(ctx, t, force)
}

// TearDown is Delete without the gate: the half a config apply and orgconf
// have already decided. It is the union of what the four surfaces each did
// part of.
func (s *ManagedInstanceService) TearDown(ctx context.Context, t *repo.Tile, force bool) error {
	held, err := s.store.ListProvisionsByInstance(ctx, t.ID)
	if err != nil {
		return err
	}
	if len(held) > 0 && !force {
		return svcerr.Conflictf("%d consumer(s) hold slices on this instance; "+
			"detach them first, or force the delete to destroy the data with it", len(held))
	}
	if s.dbs != nil {
		// The slices go before the container does, so the engine drop can
		// still reach it. Rows the drop could not clear are removed below
		// anyway: a force delete used to leave both the provision and the
		// resource rows behind, pointing at an instance id that no longer
		// resolved, and nothing ever collected them.
		seen := map[string]bool{}
		for i := range held {
			if seen[held[i].DBName] {
				continue
			}
			seen[held[i].DBName] = true
			if derr := s.dbs.DropDB(ctx, t, held[i].DBName); derr != nil {
				slog.Error("slice not dropped with its instance",
					"instance", t.ID, "slice", held[i].DBName, "error", derr)
			}
		}
		if err := s.dbs.Remove(ctx, t); err != nil {
			return err
		}
	}
	s.reapProvisions(ctx, t)
	// The rest is the same teardown every other tile gets: containers, the
	// Traefik route the panel path used to leave standing until the next
	// Resync, the provisions this tile itself consumed, the row, and the two
	// schedule tables its rows cascaded out of.
	return s.tiles.TearDown(ctx, t)
}

// reapProvisions removes any provision or resource row still pointing at the
// instance after the drops, so a slice whose engine drop failed (a stopped
// instance, an engine with no Drop hook) does not outlive the thing that
// provided it.
//
// Both tables, not just provisions: the resource row is what a consumer's
// ${{ tile.<slice>.<OUTPUT> }} resolves through, and one left behind names a
// provider tile id that no longer exists.
func (s *ManagedInstanceService) reapProvisions(ctx context.Context, t *repo.Tile) {
	if left, err := s.store.ListProvisionsByInstance(ctx, t.ID); err == nil {
		for i := range left {
			if derr := s.store.DeleteProvision(ctx, left[i].ID); derr != nil {
				slog.Error("provision row not removed with its instance",
					"instance", t.ID, "provision", left[i].ID, "error", derr)
			}
		}
	}
	if res, err := s.store.ListResourcesByProvider(ctx, t.ID); err == nil {
		for i := range res {
			if derr := s.store.DeleteResource(ctx, res[i].ID); derr != nil {
				slog.Error("resource row not removed with its instance",
					"instance", t.ID, "resource", res[i].ID, "error", derr)
			}
		}
	}
}

// setStatus writes the column, keeps the caller's row in step, and tells
// every open canvas. Best-effort on the write: the deploy has already
// happened either way, and losing the status must not turn a successful
// deploy into a failed request.
func (s *ManagedInstanceService) setStatus(ctx context.Context, t *repo.Tile, status string) {
	if err := s.store.UpdateTileStatus(ctx, t.ID, status); err != nil {
		slog.Error("database status not saved", "tile", t.ID, "status", status, "error", err)
	}
	t.Status = status
	if s.notifier != nil {
		s.notifier.Project(t.StackID)
		s.notifier.Containers()
	}
}

// --- managed resources ---
//
// A managed resource is something a config file declared that stackr provisions
// outside the container graph — a bucket, a queue — with outputs a tile binds
// to as variables. The canvas draws them and the variable resolver reads them,
// and both were walking these four tables themselves.

// Resources are an environment's managed resources.
func (s *ManagedInstanceService) Resources(ctx context.Context, envID string) ([]repo.ManagedResource, error) {
	return s.store.ListResourcesByEnv(ctx, envID)
}

// Resource is one managed resource by id.
func (s *ManagedInstanceService) Resource(ctx context.Context, id string) (*repo.ManagedResource, error) {
	r, err := s.store.GetResource(ctx, id)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, svcerr.ErrNotFound
	}
	return r, nil
}

// Outputs are what a provisioned resource published for its consumers to bind.
func (s *ManagedInstanceService) Outputs(ctx context.Context, resourceID string) ([]repo.ResourceOutput, error) {
	return s.store.ListOutputs(ctx, resourceID)
}

// Bindings are the resource outputs one tile consumes.
func (s *ManagedInstanceService) Bindings(ctx context.Context, consumerTileID string) ([]repo.ResourceBinding, error) {
	return s.store.BindingsForConsumer(ctx, consumerTileID)
}

// Intended is an environment's declared-but-not-yet-applied variable values,
// the left-hand column of the environment comparison.
func (s *ManagedInstanceService) Intended(ctx context.Context, envID string) ([]repo.Intended, error) {
	return s.store.ListIntended(ctx, envID)
}

// SetIntended records one declared value.
func (s *ManagedInstanceService) SetIntended(ctx context.Context, row *repo.Intended) error {
	return s.store.SetIntended(ctx, row)
}
