package service

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/jobs"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/service/notify"
	"github.com/FyrmForge/stackr/internal/stackrd/service/scheduler"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Actor is who asked for a run, and through what. The two used to be one
// string baked into the trigger column ("manual web" / "manual api"), which
// meant the vocabulary grew a member every time a surface was added and a
// query for "every manual run" had to know all of them. Trigger now says what
// kind of run it was; Actor says who, and Via says where from.
type Actor struct {
	ID    string // the user's row id, empty for an API key; staged changes carry it
	Name  string // display name, or an API key's name; what a staged change is signed with
	Email string // a user's address, empty for an API key; what a run row is signed with
	Via   string // "web" or "api"
}

// Surface is where this actor is acting from, which decides whether their
// write queues for review or lands immediately.
func (a Actor) Surface() Surface {
	if a.Via == "web" {
		return SurfaceCanvas
	}
	return SurfaceDirect
}

// String is what lands in cron_runs.actor. The surface is kept because a run
// history that cannot tell a person from a CI key is not a history.
func (a Actor) String() string {
	// Email first: a run history that cannot tell two people with the same
	// display name apart is not a history. An API key has no address, so it
	// signs with its name and the surface says where it came from.
	who := a.Email
	if who == "" {
		who = a.Name
	}
	switch {
	case who != "" && a.Via == "api":
		return who + " (api)"
	case who != "":
		return who
	case a.Via != "":
		return "(" + a.Via + ")"
	}
	return ""
}

// Audit is what this actor signs an audit event with. Deliberately not
// String(): the audit trail has two established spellings — a bare email for
// a person, "api:<key name>" for a key — and a query for "what did this key
// do" matches on that prefix today.
func (a Actor) Audit() string {
	if a.Via == "api" {
		if a.Name != "" {
			return "api:" + a.Name
		}
		return "api:?"
	}
	if a.Email != "" {
		return a.Email
	}
	if a.Name != "" {
		return a.Name
	}
	return "?"
}

// TileLifecycleService owns every runtime state change a tile can be put
// through by hand: stop, restart, pause a schedule, run one now, stop a run
// in flight. It is deliberately separate from TileService, which owns the
// row: these methods change what is running, not what is configured.
//
// It is also the single owner of UpdateTileStatus above the infra line. The
// reconciler and the deploy engine still write the column themselves — they
// sit below the line (D7) and cannot import this package without inverting
// it — so "one writer" here means one writer per *user action*, not one in
// the whole tree. Closing that properly is 2.15's job, not this one.
type TileLifecycleService struct {
	store    repo.Store
	clus     *cluster.Cluster
	jobs     *jobs.Service
	dbs      *managedtiles.Service
	sched    *scheduler.Service
	notifier *notify.Notifier
}

func NewTileLifecycleService(store repo.Store, clus *cluster.Cluster, j *jobs.Service,
	dbs *managedtiles.Service, sched *scheduler.Service, n *notify.Notifier) *TileLifecycleService {
	return &TileLifecycleService{store: store, clus: clus, jobs: j, dbs: dbs, sched: sched, notifier: n}
}

// Stop takes the tile out of service without forgetting it: the row, its
// volumes and its replica count all stay, so a later Restart brings it back
// as it was.
//
// A cron or a function parks as "paused", not "stopped". "stopped" is the
// state the reconciler skips for run-to-completion kinds
// (infra/metrics/reconcile.go), so a cron parked that way is parked for ever
// *and still ticks* (infra/jobs/jobs.go) — the worst of both. "paused" is the
// state the scheduler already honours, which is why the schedule is
// re-registered after the write.
func (s *TileLifecycleService) Stop(ctx context.Context, t *repo.Tile) error {
	if t == nil {
		return svcerr.ErrNotFound
	}
	if t.IsVolume() {
		return svcerr.Invalidf("", "a volume has nothing to stop; detach it from its tile instead")
	}
	if runToCompletion(t) {
		return s.pause(ctx, t, "paused")
	}
	if t.IsManaged() && s.dbs != nil {
		// Same scale-to-zero, but through the engine, which knows the service
		// may not exist yet and says so rather than erroring on an empty name.
		if err := s.dbs.Stop(ctx, t); err != nil {
			return err
		}
		return s.setStatus(ctx, t, "stopped")
	}
	if err := s.clus.ScaleService(ctx, envnet.ServiceFor(ctx, s.store, t), 0); err != nil {
		return err
	}
	return s.setStatus(ctx, t, "stopped")
}

// Restart bounces the tile in place: a forced service update, not scale 0
// then 1, which would drop the replica count a stopped tile is meant to keep.
// A tile that was scaled to zero comes back up.
func (s *TileLifecycleService) Restart(ctx context.Context, t *repo.Tile) error {
	if t == nil {
		return svcerr.ErrNotFound
	}
	if runToCompletion(t) {
		return svcerr.Invalidf("", "a %s has no long-running container to restart; use run instead", t.Kind)
	}
	if t.IsManaged() && s.dbs != nil {
		// Start, not RestartService: an instance comes back at one replica and
		// then reconciles the slices a consumer provisioned while it was down.
		// The API used to take the generic path here, so restarting an
		// instance over the API left those slices missing where restarting the
		// identical instance from the panel healed them.
		//
		// Detached, because Start waits up to 30s for the task to roll and the
		// API gives a request 30s in total — so the synchronous version raced
		// its own caller's deadline and answered 500 for a restart that was
		// working. Its own context for the same reason: the request's is
		// cancelled the moment this returns. The status lands when the roll
		// does and the notifier tells every open canvas, which is what the
		// panel's Deploy button has always done.
		inst := *t
		go func() {
			bg := context.Background()
			if err := s.dbs.Start(bg, &inst); err != nil {
				slog.Error("instance restart failed", "tile", inst.ID, "error", err)
				_ = s.setStatus(bg, &inst, "error")
				return
			}
			_ = s.setStatus(bg, &inst, "running")
		}()
		return nil
	}
	name := envnet.ServiceFor(ctx, s.store, t)
	if name == "" {
		return svcerr.Invalidf("", "nothing deployed to restart")
	}
	// A pinned tile is forced to one replica by placement, so its configured
	// count is not what it comes back as; placement.For is the authority and
	// runs on the next deploy. One is right for everything with a volume.
	if err := s.clus.RestartService(ctx, name, t.Replicas); err != nil {
		// Bounced but not back up: record what is true, or the tile keeps
		// claiming "running" over a dead one.
		if serr := s.setStatus(ctx, t, "stopped"); serr != nil {
			slog.Error("tile status not saved", "tile", t.ID, "status", "stopped", "error", serr)
		}
		return err
	}
	return s.setStatus(ctx, t, "running")
}

// ToggleCron pauses or resumes a schedule and reports the state it landed in.
// Cron only: a service has no schedule to pause (Stop is its equivalent) and
// a function runs on demand. The web path used to accept any tile and park a
// service at "paused", which the reconciler then fought.
func (s *TileLifecycleService) ToggleCron(ctx context.Context, t *repo.Tile) (status string, err error) {
	if t == nil {
		return "", svcerr.ErrNotFound
	}
	if t.Kind != "cron" {
		return "", svcerr.Invalidf("", "only cron tiles have a schedule to pause")
	}
	status = "paused"
	if t.Status == "paused" {
		status = "idle"
	}
	return status, s.pause(ctx, t, status)
}

// pause writes a scheduler-visible status and re-registers the cron table,
// which is the half both surfaces used to skip on Stop.
func (s *TileLifecycleService) pause(ctx context.Context, t *repo.Tile, status string) error {
	if err := s.setStatus(ctx, t, status); err != nil {
		return err
	}
	s.sched.ReloadCron(ctx)
	return nil
}

// RunNow fires one immediate run of a cron or function tile, detached. The
// run row exists before this returns, so a caller that re-renders straight
// away draws the run that is already going rather than an idle badge.
func (s *TileLifecycleService) RunNow(ctx context.Context, t *repo.Tile, by Actor) (*repo.CronRun, error) {
	if t == nil {
		return nil, svcerr.ErrNotFound
	}
	if !runToCompletion(t) {
		return nil, svcerr.Invalidf("", "run applies to cron and function tiles")
	}
	if s.jobs == nil {
		// The panel used to nil-deref here. A build started without a job
		// runner is a configuration choice, not a crash.
		return nil, fmt.Errorf("job runner: %w", svcerr.ErrUnavailable)
	}
	return s.jobs.StartApp(ctx, t.ID, jobs.TriggerManual, by.String())
}

// StopRun ends a run in flight. The row closes as "stopped", not an error.
// Reports false when the run had already finished, which is not a failure.
func (s *TileLifecycleService) StopRun(ctx context.Context, t *repo.Tile, runID string) (bool, error) {
	if t == nil {
		return false, svcerr.ErrNotFound
	}
	run, err := s.store.GetCronRun(ctx, runID)
	if err != nil {
		return false, err
	}
	// Ownership, not just existence: a run id from another tile must read as
	// missing rather than as someone else's row. Checked before the runner is
	// looked at, so the answer to "is this mine" never depends on how this
	// process happens to be configured.
	if run == nil || run.Ref != TileRef(t.ID) {
		return false, svcerr.ErrNotFound
	}
	if s.jobs == nil {
		return false, fmt.Errorf("job runner: %w", svcerr.ErrUnavailable)
	}
	return s.jobs.Stop(ctx, run.ID), nil
}

// setStatus writes the column, keeps the caller's in-memory tile in step, and
// tells every open canvas. The nudge is the half the API skipped: the acting
// tab gets its badge from its own re-render, so without this nobody else in
// the org ever learns the tile moved.
func (s *TileLifecycleService) setStatus(ctx context.Context, t *repo.Tile, status string) error {
	if err := s.store.UpdateTileStatus(ctx, t.ID, status); err != nil {
		return err
	}
	t.Status = status
	if s.notifier != nil {
		s.notifier.Project(t.StackID)
		s.notifier.Containers()
	}
	return nil
}

// runToCompletion: kinds that finish rather than serve. They have runs, not
// replicas, which is why stop, restart and run mean different things for them.
func runToCompletion(t *repo.Tile) bool { return t.Kind == "cron" || t.Kind == "function" }
