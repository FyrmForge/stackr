package service

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	"github.com/FyrmForge/stackr/internal/stackrd/config/staging"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/deploy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	svcproxy "github.com/FyrmForge/stackr/internal/stackrd/service/proxy"
	"github.com/FyrmForge/stackr/internal/stackrd/service/scheduler"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/google/uuid"
)

// TileService owns the tile row and everything that has to happen around a
// write to it. TileLifecycleService is its sibling and owns what is running;
// this one owns what is configured.
//
// **It takes a full *repo.Tile, not a patch** (D6). The three callers — the
// panel's form, the API's JSON merge and a config apply's TileConf — each
// map their own request shape onto a loaded row and hand the whole thing
// over. That is what collapses the eleven copies of the tile field list: the
// service has one input shape, and none of the callers gets to decide which
// side effects their write earns.
//
// It deliberately does **not** import config/stackconf. TileConf would be the
// obvious input shape, but stackconf's Applier is one of this service's
// callers, and the import would close a cycle. A full row is the shape both
// sides already hold anyway.
//
// Note what the store does *not* write: UpdateTile takes a repo.TileConfig,
// so status, shared_net, home_node and the digests are not expressible on that
// path at all, and slug is identity that moves only through Rename. Before
// point 20 those columns were merely absent from the SQL, which meant a
// full-struct write compiled, returned nil and reverted nothing — visibly
// fine, silently a no-op.
type TileService struct {
	store  repo.Store
	clus   *cluster.Cluster
	px     *svcproxy.Service
	dbs    *managedtiles.Service
	sched  *scheduler.Service
	engine *deploy.Engine
	gate   *GateService
}

func NewTileService(store repo.Store, clus *cluster.Cluster, px *svcproxy.Service,
	dbs *managedtiles.Service, sched *scheduler.Service, engine *deploy.Engine,
	gate *GateService) *TileService {
	return &TileService{store: store, clus: clus, px: px, dbs: dbs, sched: sched,
		engine: engine, gate: gate}
}

// Update writes a settings edit to an already-loaded, already-edited tile and
// performs whatever that edit earns.
//
// The caller maps its own request shape onto the row it loaded — a form, a
// JSON merge, a TileConf — and hands the whole row over. What it must not do
// is decide what happens next: the panel used to write the row and always
// rewrite the route, the API used to write the row and do nothing at all, and
// only a config apply chose. That is why a port, limit, healthcheck, command
// or replica edit made from either surface sat dormant until the next manual
// deploy.
//
// extra names changes that are not columns on this row, so a caller that also
// moved something else (a config apply's domain edits) earns the side effects
// for it. Empty from the two HTTP surfaces.
func (s *TileService) Update(ctx context.Context, t *repo.Tile, extra []string, by Actor) (staged bool, err error) {
	if t == nil {
		return false, svcerr.ErrNotFound
	}
	stage, err := s.gate.Gate(ctx, t.StackID, GateFieldEdit, by.Surface())
	if err != nil {
		return false, err
	}
	if err := s.Validate(ctx, t); err != nil {
		return false, err
	}
	if stage {
		return true, staging.Stage(ctx, s.store, t, by.ID, by.Name, "settings",
			staging.OpUpdate, staging.SettingsPatch(t))
	}
	// Read back what is stored before overwriting it: the diff is what picks
	// the side effects, and it is the only honest source for "what did this
	// save actually move".
	old, err := s.store.GetTile(ctx, t.ID)
	if err != nil {
		return false, err
	}
	changed := DiffTiles(old, t)
	for _, f := range extra {
		changed[f] = true
	}
	t.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdateTile(ctx, t.ID, t.TileConfig); err != nil {
		return false, err
	}
	return false, s.afterWrite(ctx, t, changed)
}

// afterWrite performs the side effects a set of changes earns.
//
// It is deliberately unexported. A config apply makes the same decision from
// the same Changed set, but performs it itself, because the applier records
// what it deployed and a deploy queued from in here would be missing from
// that report. The decision is shared through Changed's methods; the doing is
// not shared, and an exported AfterWrite would invite a caller to think it
// was.
//
// A managed instance is skipped: its container is rebuilt by the applier's
// own path, which is point 5's to move.
func (s *TileService) afterWrite(ctx context.Context, t *repo.Tile, changed Changed) error {
	if !changed.Any() {
		return nil
	}
	if t.Kind == "cron" && changed.NeedsCronReload() {
		// Otherwise a changed schedule only takes effect on the next restart.
		s.sched.ReloadCron(ctx)
	}
	switch {
	case t.IsManaged():
		return nil
	case t.Kind == "service":
		// The route carries basic auth, the security headers and the override,
		// and it hashes the password as it writes — so an API patch that set
		// any of them used to do nothing at all, and stored the password in
		// clear for nobody to read.
		if err := s.px.SyncTile(ctx, t); err != nil {
			return err
		}
		if !changed.ProxyOnly() {
			s.redeploy(ctx, t, "settings")
		}
	case runToCompletion(t):
		// Schedule, command and timeouts are read off the row at each run;
		// only the built artifact needs rebuilding.
		if changed.NeedsBuild() {
			s.redeploy(ctx, t, "build")
		}
	}
	return nil
}

// redeploy queues a rebuild if the tile is one that has something running.
// Queued, not run: a settings save must not block on a build, and the deploy
// row is what reports the outcome.
func (s *TileService) redeploy(ctx context.Context, t *repo.Tile, trigger string) {
	if s.engine == nil {
		return
	}
	if _, err := s.engine.Enqueue(ctx, t, trigger); err != nil {
		slog.Error("redeploy after settings change not queued", "tile", t.ID, "error", err)
	}
}

// Delete removes a tile and everything the cluster holds for it.
//
// The order matters and used to differ per surface: containers first (so
// nothing is still writing), then the route (so nothing is still reachable),
// then the shared-db provisions it consumed are *orphaned* rather than
// dropped, then the row, then the two schedule tables — the delete cascades
// this tile's cron_jobs and backups rows, and without the reload the orphaned
// entries keep ticking against a tile that is gone.
//
// On a stack whose canvas queues edits, the tile stays on the canvas struck
// through until the pending set is applied, which is what tears it down.
func (s *TileService) Delete(ctx context.Context, t *repo.Tile, by Actor) (staged bool, err error) {
	if t == nil {
		return false, svcerr.ErrNotFound
	}
	stage, err := s.gate.Gate(ctx, t.StackID, GateStructural, by.Surface())
	if err != nil {
		return false, err
	}
	// Checked before staging, not after: a staged delete that cannot ever
	// apply is worse than a refusal, because the refusal is visible now.
	// The API skipped this guard entirely, so a `DELETE /volumes/:id` on an
	// attached volume took the volume out from under a running service.
	if t.IsVolume() && t.AttachedTileID != "" {
		// Only while the owner is still there. A volume whose tile was
		// deleted keeps the stale id — nothing clears it (the tile row does
		// not cascade to its volumes, which is point 9's mess) — and
		// refusing on the id alone left that volume undeletable for ever.
		if owner, oerr := s.store.GetTile(ctx, t.AttachedTileID); oerr == nil && owner != nil {
			return false, svcerr.Invalidf("", "detach the volume from %s before deleting it", owner.Slug)
		}
	}
	if stage {
		return true, staging.Stage(ctx, s.store, t, by.ID, by.Name, "delete", staging.OpDelete, nil)
	}
	return false, s.TearDown(ctx, t)
}

// TearDown is Delete without the gate or the staging branch: the half a
// config apply and a staged-delete apply already decided to perform. Exported
// because those callers have made the decision Delete makes and must not make
// it twice.
func (s *TileService) TearDown(ctx context.Context, t *repo.Tile) error {
	// Held before the row goes: a volume's former target has to come back
	// without the bind, and a service's volumes have to stop pointing at an
	// id that no longer resolves.
	formerTarget := ""
	if t.IsVolume() {
		formerTarget = t.AttachedTileID
	}
	envnet.TearDown(ctx, s.store, s.clus, t)
	s.px.DropTile(t.ID)
	// Orphan (don't drop) any shared-db provisions this tile consumed: the
	// data belongs to the database, not to the consumer that went away.
	// The API's teardownTile skipped this, leaving rows pointing at a tile id
	// that no longer resolved.
	if s.dbs != nil {
		if ps, _ := s.store.ListProvisionsByConsumer(ctx, t.ID); len(ps) > 0 {
			for i := range ps {
				if err := s.dbs.Detach(ctx, &ps[i]); err != nil {
					slog.Error("provision not detached", "tile", t.ID, "provision", ps[i].ID, "error", err)
				}
			}
		}
	}
	if err := s.store.DeleteTile(ctx, t.ID); err != nil {
		return err
	}
	s.sched.Reload(ctx)
	if formerTarget != "" {
		// The bind is in the service definition, so the mount only goes when
		// the container is recreated. `DELETE /apps/:id` with a volume id
		// never did this — only `DELETE /volumes/:id` did — so a volume
		// deleted the other way left the service still mounting it.
		if target, err := s.store.GetTile(ctx, formerTarget); err == nil && target != nil {
			s.redeploy(ctx, target, "volume")
		}
	}
	if !t.IsVolume() {
		s.orphanVolumes(ctx, t)
	}
	return nil
}

// orphanVolumes clears the attach pointer on the volumes of a tile that has
// gone. The tile row does not cascade to its volumes, so the pointer used to
// survive its target: the volume then read as attached to nothing resolvable,
// and Delete's own attached-guard could not tell that from a live owner.
func (s *TileService) orphanVolumes(ctx context.Context, t *repo.Tile) {
	sibs, err := s.store.ListTilesByEnv(ctx, t.EnvironmentID)
	if err != nil {
		return
	}
	vols := repo.VolumesAttachedTo(sibs, t.ID)
	for i := range vols {
		v := vols[i]
		v.AttachedTileID, v.MountPath = "", ""
		if err := s.store.UpdateTile(ctx, v.ID, v.TileConfig); err != nil {
			slog.Error("volume not detached from its deleted owner", "volume", v.ID, "error", err)
		}
	}
}

// Create adds a tile to an environment.
//
// The caller supplies the row it wants — name, kind, source, whatever its
// wire format carries — and this fills the identity and the defaults, checks
// the name is usable and free, checks a volume's attach target, applies the
// same Validate every update goes through, and then either stages the
// creation or performs it.
//
// stagedPatch is the payload a staged create carries: a full config object,
// which is the config engine's shape and therefore the caller's to build
// (this package cannot import stackconf without closing a cycle — see the
// type comment). It is only ever used by the canvas; a create is a structural
// write, so every other surface either writes through or is refused.
func (s *TileService) Create(ctx context.Context, t *repo.Tile, stagedPatch any, by Actor) (staged bool, err error) {
	if t == nil {
		return false, svcerr.ErrNotFound
	}
	stage, err := s.gate.Gate(ctx, t.StackID, GateStructural, by.Surface())
	if err != nil {
		return false, err
	}
	if err := s.nameTile(ctx, t); err != nil {
		return false, err
	}
	s.applyDefaults(t)
	if err := s.checkAttach(ctx, t); err != nil {
		return false, err
	}
	if err := s.Validate(ctx, t); err != nil {
		return false, err
	}
	if stage {
		return true, staging.Stage(ctx, s.store, t, by.ID, by.Name, "create",
			staging.OpCreate, stagedPatch)
	}
	if err := s.store.CreateTile(ctx, t); err != nil {
		return false, err
	}
	if t.Kind == "cron" {
		// Without this the schedule only starts running after the next restart.
		s.sched.ReloadCron(ctx)
	}
	// Attaching a volume changes the target's mounts, so the target has to
	// come back with them.
	if t.IsVolume() && t.AttachedTileID != "" {
		if target, terr := s.store.GetTile(ctx, t.AttachedTileID); terr == nil && target != nil {
			s.redeploy(ctx, target, "volume")
		}
	}
	return false, nil
}

// nameTile derives and checks the slug. A slug is the tile's identity — its
// DNS alias inside the env and its key in the config file — so it has to be
// derivable, not reserved, and free in this environment. The API used to
// accept an empty one.
func (s *TileService) nameTile(ctx context.Context, t *repo.Tile) error {
	t.Name = strings.TrimSpace(t.Name)
	if t.Name == "" {
		return invalid("name", "a tile needs a name")
	}
	if t.Slug == "" {
		t.Slug = repo.Slugify(t.Name)
	}
	if t.Slug == "" {
		return invalid("name", "a name needs at least one letter or number")
	}
	if repo.ReservedSlug(t.Slug) {
		return invalid("name", "\""+t.Slug+"\" is reserved for variable references; pick another name")
	}
	existing, err := s.store.GetTileBySlug(ctx, t.EnvironmentID, t.Slug)
	if err == nil && existing != nil && existing.ID != t.ID {
		return svcerr.Conflictf("a tile named %q (%s) already exists in this environment",
			existing.Name, t.Slug)
	}
	return nil
}

// applyDefaults fills what every surface filled differently. The webhook token
// was uuid on one path and 24 random bytes on the other; the panel gave every
// kind a 30 minute timeout including services, which have no run to time out.
func (s *TileService) applyDefaults(t *repo.Tile) {
	now := time.Now().UTC()
	if t.ID == "" {
		t.ID = uuid.New().String()
	}
	if t.WebhookToken == "" {
		t.WebhookToken = secrets.RandomHex(24)
	}
	if t.Status == "" {
		t.Status = "idle"
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	if t.SourceType == "" {
		if t.ImageRef != "" {
			t.SourceType = "image"
		} else {
			t.SourceType = "git"
		}
	}
	if t.SourceType == "git" {
		if t.GitBranch == "" {
			t.GitBranch = "main"
		}
		if t.DockerfilePath == "" {
			t.DockerfilePath = "Dockerfile"
		}
		if t.BuildContext == "" {
			t.BuildContext = "."
		}
	}
	if runToCompletion(t) && t.TimeoutMinutes == 0 {
		t.TimeoutMinutes = 30
	}
}

// checkAttach validates a volume's attach target: a plain service in the same
// environment, and an absolute mount path to put it at.
func (s *TileService) checkAttach(ctx context.Context, t *repo.Tile) error {
	if !t.IsVolume() || t.AttachedTileID == "" {
		return nil
	}
	target, err := s.store.GetTile(ctx, t.AttachedTileID)
	if err != nil || target == nil || target.EnvironmentID != t.EnvironmentID ||
		target.Kind != "service" || target.IsManaged() {
		return invalid("attach_tile_id", "attach a volume to a service in the same environment")
	}
	if !strings.HasPrefix(t.MountPath, "/") {
		return invalid("mount_path", "mount path must be absolute (e.g. /data)")
	}
	return nil
}

// Rename changes a tile's name and slug.
//
// The order is the whole content of this method, and each step is here
// because skipping it broke something:
//
//   - tear the containers down first. The swarm service name is built from
//     the slug, so a rename leaves the old service running under the old name
//     for ever if it is not removed before the row moves.
//   - rename the row. Slug moves only through RenameTile — it is identity,
//     not config, so it is not in repo.TileConfig and UpdateTile cannot
//     express it.
//   - rewrite the route. The route file is keyed on the tile id, so the file
//     survives, but its contents carry the slug; the config path used to
//     *remove* the route and never write it back, which left a renamed tile
//     serving nothing until the next domain edit or a full resync.
//
// It does not redeploy. The caller does, because the only caller today is a
// config apply, which records what it deployed and would be missing a deploy
// queued from in here.
func (s *TileService) Rename(ctx context.Context, t *repo.Tile, name, slug string) error {
	if t == nil {
		return svcerr.ErrNotFound
	}
	if slug == "" {
		slug = repo.Slugify(name)
	}
	if slug == "" {
		return invalid("name", "a name needs at least one letter or number")
	}
	if repo.ReservedSlug(slug) {
		return invalid("name", "\""+slug+"\" is reserved for variable references; pick another name")
	}
	if other, _ := s.store.GetTileBySlug(ctx, t.EnvironmentID, slug); other != nil && other.ID != t.ID {
		return svcerr.Conflictf("a tile named %q (%s) already exists in this environment", other.Name, slug)
	}
	envnet.TearDown(ctx, s.store, s.clus, t)
	t.Name, t.Slug = name, slug
	if err := s.store.RenameTile(ctx, t.ID, t.Name, t.Slug); err != nil {
		return err
	}
	if err := s.px.SyncTile(ctx, t); err != nil {
		slog.Error("proxy route not rewritten after rename", "tile", t.ID, "error", err)
	}
	return nil
}

// --- reads ---

// Get is one tile by id, ErrNotFound rather than a nil row — see the note on
// EnvironmentService.Get.
func (s *TileService) Get(ctx context.Context, id string) (*repo.Tile, error) {
	t, err := s.store.GetTile(ctx, id)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, svcerr.ErrNotFound
	}
	return t, nil
}

// ListForEnv is an environment's tiles.
func (s *TileService) ListForEnv(ctx context.Context, envID string) ([]repo.Tile, error) {
	return s.store.ListTilesByEnv(ctx, envID)
}

// ListForStack is every tile in a stack, across all its environments.
func (s *TileService) ListForStack(ctx context.Context, stackID string) ([]repo.Tile, error) {
	return s.store.ListTilesByStack(ctx, stackID)
}

// ListAll is every tile on the server. It is not org-scoped and never will be:
// the two callers are the admin area and the scheduler, and a caller that
// wants one org's tiles wants ListForStack per stack, which the tenancy check
// on the stack has already answered for.
func (s *TileService) ListAll(ctx context.Context) ([]repo.Tile, error) {
	return s.store.ListTiles(ctx)
}

// --- staged changes ---
//
// A staged change is a write this service refused to apply because the config
// file owns the field, parked for the next plan to show as drift. It is
// produced by Update, Create and Delete — the `staged bool` they return — so
// reading and discarding one belongs here rather than in a table of its own.

// Staged is an environment's pending changes.
func (s *TileService) Staged(ctx context.Context, envID string) ([]repo.StagedChange, error) {
	return s.store.ListStagedByEnv(ctx, envID)
}

// StagedCount is how many an environment has, for the banner.
func (s *TileService) StagedCount(ctx context.Context, envID string) (int, error) {
	return s.store.CountStagedByEnv(ctx, envID)
}

// StagedChange is one pending change by id.
func (s *TileService) StagedChange(ctx context.Context, id string) (*repo.StagedChange, error) {
	sc, err := s.store.GetStagedChange(ctx, id)
	if err != nil {
		return nil, err
	}
	if sc == nil {
		return nil, svcerr.ErrNotFound
	}
	return sc, nil
}

// DiscardStaged drops one pending change.
func (s *TileService) DiscardStaged(ctx context.Context, id string) error {
	return s.store.DeleteStagedChange(ctx, id)
}

// DiscardStagedForEnv drops every pending change in an environment.
func (s *TileService) DiscardStagedForEnv(ctx context.Context, envID string) error {
	return s.store.DeleteStagedByEnv(ctx, envID)
}

// BySlug is one tile by its slug within an environment.
func (s *TileService) BySlug(ctx context.Context, envID, slug string) (*repo.Tile, error) {
	t, err := s.store.GetTileBySlug(ctx, envID, slug)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, svcerr.ErrNotFound
	}
	return t, nil
}

// Save writes a tile row back WITHOUT the cascade.
//
// Update is the one with the rules: it gates on the config file, works out
// what changed, and redeploys or stages accordingly. This is the raw write,
// for the three callers that have already decided — a node reassignment the
// scheduler made, a field the caller knows is not config-owned. A new caller
// almost certainly wants Update.
func (s *TileService) Save(ctx context.Context, t *repo.Tile) error {
	return s.store.UpdateTile(ctx, t.ID, t.TileConfig)
}
