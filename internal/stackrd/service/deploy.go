package service

import (
	"context"
	"log/slog"

	"github.com/FyrmForge/stackr/internal/deploystate"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/deploy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// DeployService owns the rules about which tiles may be deployed and what a
// deploy a person asked for is allowed to be. The mechanics stay in
// infra/deploy.Engine below the line: it writes the deployment row, supersedes
// what the new one outranks, and hands the work to the queue.
//
// The rules are what had drifted. The panel refused a cron tile ("cron
// services run on their schedule; use Run now") and the API queued one, where
// a cron tile's deploy is not a thing that happens — its container is created
// per run. Both refused an upper environment, with two spellings of the same
// sentence. And a variable write's redeploy-if-running lived as a private copy
// in VariableService while three panel handlers had their own.
//
// One invariant, written down because it keeps being re-derived: a deployment
// needs no atomic claim of its own. Its lifecycle is driven by a work item,
// and work items claim atomically by rows-affected (sqlite/workitems.go), so
// two workers cannot hold the same one. The read-modify-write on the
// deployment row is already guarded a level up.
type DeployService struct {
	store  repo.Store
	engine *deploy.Engine
}

func NewDeployService(store repo.Store, engine *deploy.Engine) *DeployService {
	return &DeployService{store: store, engine: engine}
}

// Trigger queues a deploy a caller asked for. trigger is provenance — "manual"
// from a button, "api" from a key, "push" from a webhook — and stays the
// caller's to name; it is what a deployment list is read by.
func (s *DeployService) Trigger(ctx context.Context, t *repo.Tile, trigger string) (string, error) {
	if err := s.deployable(ctx, t); err != nil {
		return "", err
	}
	return s.engine.Enqueue(ctx, t, trigger)
}

// Rollback re-runs an image this tile already built.
func (s *DeployService) Rollback(ctx context.Context, t *repo.Tile, imageTag string) (string, error) {
	if err := s.deployable(ctx, t); err != nil {
		return "", err
	}
	if imageTag == "" {
		return "", invalid("image", "required")
	}
	return s.engine.EnqueueRollback(ctx, t, imageTag)
}

// deployable is the one rule set. Every refusal here used to be spelled at
// least twice, and the cron one only on the panel.
func (s *DeployService) deployable(ctx context.Context, t *repo.Tile) error {
	if t == nil {
		return svcerr.ErrNotFound
	}
	if t.IsVolume() {
		return svcerr.Invalidf("", "a volume is not deployable")
	}
	if t.IsManaged() {
		return svcerr.Invalidf("", "a managed database is not deployed; it is provisioned")
	}
	if t.Kind == "cron" {
		return svcerr.Invalidf("", "cron services run on their schedule; use Run now")
	}
	if envnet.UpperEnv(ctx, s.store, t) {
		// An upper environment exists to receive promotions and nothing else.
		// Deploying into one directly would ship code no rung below it has
		// running, which is the whole thing the ladder prevents.
		return svcerr.Conflictf("this environment deploys by promote from the releases page")
	}
	// Last, so a request that is wrong whatever this server can do answers
	// "wrong" rather than "not right now".
	if s.engine == nil {
		return svcerr.ErrUnavailable
	}
	return nil
}

// Cancel stops a build or roll-out. A running one is cancelled through its
// context; a queued or parked one by writing the row.
func (s *DeployService) Cancel(ctx context.Context, deploymentID string) {
	if s.engine == nil {
		return
	}
	s.engine.Cancel(ctx, deploymentID)
}

// RedeployIfRunning rolls a tile that is actually up, so a change to what it
// reads at boot reaches the process. Silent when the tile is stopped, managed,
// a volume or not a long-running service: there is nothing to roll, and a
// deploy queued for a stopped tile would start it behind the user's back.
//
// Best effort by design. The write that earned it has already happened, and
// failing it afterwards would report a change that did land as a failure.
func (s *DeployService) RedeployIfRunning(ctx context.Context, tileID, trigger string) {
	if s.engine == nil {
		return
	}
	t, err := s.store.GetTile(ctx, tileID)
	if err != nil || t == nil {
		return
	}
	if t.Kind != "service" || t.IsManaged() || t.IsVolume() || t.Status != "running" {
		return
	}
	if _, err := s.engine.Enqueue(ctx, t, trigger); err != nil {
		slog.Error("redeploy not queued", "tile", t.ID, "trigger", trigger, "error", err)
	}
}

// Live lists the tile's deployments that are still going to happen. One
// predicate, in internal/deploystate, where the CLI and infra/deploy can reach
// it too — four hand-written copies of the set had drifted, and the one that
// showed was `waiting_ci` ending the panel's poll and its SSE stream on a
// deploy that was about to start on its own.
func (s *DeployService) Live(ctx context.Context, tileID string) ([]repo.Deployment, error) {
	ds, err := s.store.ListDeploymentsByTile(ctx, tileID, 20)
	if err != nil {
		return nil, err
	}
	out := ds[:0:0]
	for i := range ds {
		if deploystate.IsLive(ds[i].Status) {
			out = append(out, ds[i])
		}
	}
	return out, nil
}

// --- reads ---

// Get is one deployment by id.
func (s *DeployService) Get(ctx context.Context, id string) (*repo.Deployment, error) {
	d, err := s.store.GetDeployment(ctx, id)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, svcerr.ErrNotFound
	}
	return d, nil
}

// --- the row the engine below writes ---
//
// A deployment row is state, not config: nothing here has a rule to check,
// and the engine is the only caller. What moves is who owns the table, so
// that the next thing wanting to write a deployment has one door to find
// rather than a store handle and a guess.

// Record inserts a deployment row.
func (s *DeployService) Record(ctx context.Context, d *repo.Deployment) error {
	return s.store.CreateDeployment(ctx, d)
}

// Progress persists a deployment row the engine has moved on: a new status, a
// stage, a finish time.
func (s *DeployService) Progress(ctx context.Context, d *repo.Deployment) error {
	return s.store.UpdateDeployment(ctx, d)
}

// ForTile is a tile's most recent deployments, newest first. Live is the
// narrower question — which of them are still running — and stays separate.
func (s *DeployService) ForTile(ctx context.Context, tileID string, limit int) ([]repo.Deployment, error) {
	return s.store.ListDeploymentsByTile(ctx, tileID, limit)
}

// Row is Get without the refusal: nil when there is no such deployment.
//
// Get's ErrNotFound is right for a handler answering a request. The engine
// and the volume mover are asking whether a row is still there at all — a
// cancelled deploy, a tile deleted mid-move — and for them absence is an
// ordinary branch, not something to report.
func (s *DeployService) Row(ctx context.Context, id string) (*repo.Deployment, error) {
	return s.store.GetDeployment(ctx, id)
}

// Latest is the tile's newest deployment, nil when it has never deployed.
//
// Four packages asked this as `ListDeploymentsByTile(id, 1)` and unpacked the
// slice themselves: the config applier deciding whether a build is broken,
// the registry deciding whether a tag is in use, the PR comment reporting
// what happened, and the deploy engine. One question, four spellings of it.
func (s *DeployService) Latest(ctx context.Context, tileID string) (*repo.Deployment, error) {
	ds, err := s.store.ListDeploymentsByTile(ctx, tileID, 1)
	if err != nil || len(ds) == 0 {
		return nil, err
	}
	return &ds[0], nil
}

// CurrentImage is the image tag the tile is actually running: the newest
// deployment that finished, "" when it has never finished one.
//
// This was written twice, identically — infra/deploy.CurrentImage and
// infra/jobs.appImage — each fetching twenty rows and taking the first done
// one with a tag. They differed only in the fallback, which stays with the
// caller: the deploy path has nothing to fall back to, the job path runs the
// tile's configured image ref.
//
// Twenty rows rather than one because a deployment that failed does not
// replace the image that is running; a tile whose last three builds broke is
// still serving the one before them.
func (s *DeployService) CurrentImage(ctx context.Context, tileID string) (string, error) {
	ds, err := s.store.ListDeploymentsByTile(ctx, tileID, 20)
	if err != nil {
		return "", err
	}
	for _, d := range ds {
		if d.Status == "done" && d.ImageTag != "" {
			return d.ImageTag, nil
		}
	}
	return "", nil
}

// Waiting are the deployments held for a CI check to report. The engine
// releases them when a check arrives; the janitor's gate times them out.
func (s *DeployService) Waiting(ctx context.Context) ([]repo.Deployment, error) {
	return s.store.ListDeploymentsByStatus(ctx, "waiting_ci")
}
