// Package jobs is the scheduler behind cron tiles: it fires them on their cron
// expression, runs each as a one-shot container from the tile's own image, and
// records the outcome in cron_runs. Cron expressions support CRON_TZ= prefixes
// for timezone-aware schedules.
//
// It used to also run "scheduled jobs", a second scheduler with its own table,
// UI and overlap rules, attached to a service tile. A cron that needs a
// service's image says so with the same build: or image: and its own command:,
// so the digest matches and it builds once (docs/plans/38-surface-parity.md).
package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"

	"github.com/FyrmForge/stackr/internal/stackrd/config/settings"
	"github.com/FyrmForge/stackr/internal/stackrd/config/varref"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/registry"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/workqueue"
	"github.com/FyrmForge/stackr/internal/stackrd/service/notify"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

const maxOutput = 4096 // stored per run; full output is not persisted

type Service struct {
	store    repo.Store
	c        *cluster.Cluster // every docker call: a job in "exec" mode runs inside the tile's container, wherever that is
	notifier *notify.Notifier
	work     *workqueue.Queue
	// envs owns the environment row's network columns. Running a job in a
	// fresh environment claims that environment's overlay, and the claim
	// writes a row this package does not own.
	envs envnet.Envs

	mu      sync.Mutex
	cron    *cron.Cron
	entries map[string]cron.EntryID // jobID -> entry
	running map[string]bool         // refs ("job:<id>"/"app:<id>") mid-run
	held    map[string]int          // stack ids mid config-apply; schedule ticks skip
}

func NewService(store repo.Store, c *cluster.Cluster, notifier *notify.Notifier, envs envnet.Envs) *Service {
	s := &Service{
		store:    store,
		c:        c,
		notifier: notifier,
		envs:     envs,
		cron:     cron.New(),
		entries:  map[string]cron.EntryID{},
		running:  map[string]bool{},
		held:     map[string]int{},
	}
	s.cron.Start()
	return s
}

// ValidateCron rejects bad expressions at save time.
func ValidateCron(expr string) error {
	_, err := cron.ParseStandard(expr)
	return err
}

// NextRun returns the next fire time for a cron expression, zero if invalid.
func NextRun(expr string) time.Time {
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return time.Time{}
	}
	return sched.Next(time.Now())
}

// begin marks a ref as running. Returns false when it already is and
// overlapping is not allowed, the caller should skip this tick.
func (s *Service) begin(ref string, allowOverlap bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[ref] && !allowOverlap {
		return false
	}
	s.running[ref] = true
	return true
}

func (s *Service) end(ref string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, ref)
}

// Hold parks the stack's schedule ticks for the length of a config apply:
// a cron firing mid-apply runs against tiles that do not exist yet. Manual
// runs are not held, the person asked. Release undoes one Hold.
func (s *Service) Hold(stackID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held[stackID]++
}

func (s *Service) Release(stackID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held[stackID] <= 1 {
		delete(s.held, stackID)
		return
	}
	s.held[stackID]--
}

func (s *Service) isHeld(stackID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held[stackID] > 0
}

// depBlocked reports why one depends_on line is unmet by the dependency's
// current state, "" when it is satisfied. started and healthy both want the
// tile running; completed wants its last run ok. A missing dependency is the
// deploy's problem (validated at parse), not the tick's.
func depBlocked(line string, dep *repo.Tile) string {
	// split rather than import stackconf.ParseDep, stackconf
	// imports this package.
	slug, cond, _ := strings.Cut(strings.TrimSpace(line), ":")
	if slug == "" || dep == nil {
		return ""
	}
	if cond == "completed" {
		if dep.LastStatus != "ok" {
			return slug + " has not completed"
		}
		return ""
	}
	if dep.Status != "running" {
		return slug + " is not running"
	}
	return ""
}

// skipReason decides whether a schedule tick should not run: the stack is
// mid-apply, or a depends_on target is not there yet. Manual runs never skip.
func (s *Service) skipReason(ctx context.Context, app *repo.Tile, trigger string) string {
	if trigger != TriggerSchedule {
		return ""
	}
	if s.isHeld(app.StackID) {
		return "config apply in progress"
	}
	for _, line := range strings.Split(app.DependsOn, "\n") {
		slug, _, _ := strings.Cut(strings.TrimSpace(line), ":")
		if slug == "" {
			continue
		}
		dep, err := s.store.GetTileBySlug(ctx, app.EnvironmentID, slug)
		if err != nil {
			continue
		}
		if why := depBlocked(line, dep); why != "" {
			return "waiting for " + why
		}
	}
	return ""
}

// Kind is the work-queue kind every cron and function run goes through:
// schedule ticks, manual runs and on-deploy runs alike.
const Kind = "cron.run"

// runJob is the payload. The run row is written before the item, and its id
// is also the item's dedupe key, which is how Stop finds the item.
type runJob struct {
	TileID string `json:"tile_id"`
	RunID  string `json:"run_id"`
}

// interrupted is what a run the panel restarted in the middle of records.
const interrupted = "interrupted: stackr restarted while this run was in progress"

// WithWork registers the run kind. Call before the queue starts.
//
// A restart fails a run rather than requeueing it: re-running a half-done job
// blind is not safe. Cleanup removes the job service the run left behind,
// which is what Stop would have done.
func (s *Service) WithWork(q *workqueue.Queue) *Service {
	s.work = q
	q.Register(Kind, func(ctx context.Context, j *workqueue.Job) error {
		var p runJob
		if err := j.Payload(&p); err != nil {
			return err
		}
		run, err := s.store.GetCronRun(ctx, p.RunID)
		if err != nil || run == nil {
			return fmt.Errorf("cron run %s is gone", p.RunID)
		}
		app, err := s.store.GetTile(ctx, p.TileID)
		if err != nil || app == nil {
			s.finishRun(context.WithoutCancel(ctx), run, "error", "tile not found")
			return fmt.Errorf("tile %s is gone", p.TileID)
		}
		s.runApp(ctx, app, run)
		return nil
	}, workqueue.KindOpts{
		// The tile's own timeout is applied inside runApp; this only has to
		// be longer than any of them.
		Timeout:     24 * time.Hour,
		Limit:       func(r settings.Resolved) int { return r.CronRunConcurrency },
		OnRestart:   workqueue.Fail,
		RestartFail: interrupted,
		Cleanup: func(ctx context.Context, j *workqueue.Job) string {
			var p runJob
			if err := j.Payload(&p); err != nil {
				return ""
			}
			if app, err := s.store.GetTile(ctx, p.TileID); err == nil && app != nil {
				if name := s.RunService(ctx, app, p.RunID); name != "" {
					if err := s.c.RemoveService(ctx, name); err != nil {
						slog.Info("cron cleanup: removing the job service", "service", name, "error", err)
					}
				}
			}
			if run, err := s.store.GetCronRun(ctx, p.RunID); err == nil && run != nil && !run.FinishedAt.Valid {
				s.finishRun(ctx, run, "error", interrupted)
			}
			return ""
		},
	})
	return s
}

// Stop cancels a run, waiting or in flight; false when it is already over.
// Cancelling the ctx is enough to kill the work: RunJob removes the job
// service it created when its ctx goes away, which kills the task. A run
// still waiting never started, so its row is closed here.
func (s *Service) Stop(ctx context.Context, runID string) bool {
	w, err := s.store.LatestWorkItem(ctx, Kind, runID)
	if err != nil || w == nil || w.Done() {
		return false
	}
	if err := s.work.Cancel(ctx, w.ID); err != nil {
		slog.Error("cron stop", "run", runID, "error", err)
		return false
	}
	if w.Status == "queued" {
		if run, err := s.store.GetCronRun(ctx, runID); err == nil && run != nil && !run.FinishedAt.Valid {
			s.finishRun(ctx, run, "stopped", "")
		}
	}
	return true
}

// Triggers a run can be started by; recorded on the row so the history says
// what kind of run it was, the way deployments.trigger does. There is one
// "manual" rather than a member per surface: who and where from live in the
// actor column (service.Actor), so adding a surface no longer adds a trigger
// that every "was this manual?" query has to learn.
const (
	TriggerSchedule = "schedule"
	TriggerManual   = "manual"
	TriggerDeploy   = "deploy"
)

// startRun opens a run row before the work starts, with finished_at NULL so
// the panel can show it while it is happening. finishRun closes it.
func (s *Service) startRun(ctx context.Context, ref, trigger, actor string, started time.Time) *repo.CronRun {
	r := &repo.CronRun{
		ID:        uuid.New().String(),
		Ref:       ref,
		Status:    "running",
		Trigger:   trigger,
		Actor:     actor,
		StartedAt: started,
	}
	if err := s.store.CreateCronRun(ctx, r); err != nil {
		slog.Error("cron run not opened", "run", r.ID, "ref", ref, "error", err)
	}
	return r
}

// finishRun records the outcome on an open run and prunes anything older than
// the retention window, piggybacked here so no extra ticker is needed.
func (s *Service) finishRun(ctx context.Context, r *repo.CronRun, status, output string) {
	r.Status, r.Output = status, output
	r.FinishedAt = sql.NullTime{Time: time.Now().UTC(), Valid: true}
	if err := s.store.FinishCronRun(ctx, r); err != nil {
		slog.Error("cron run not closed", "run", r.ID, "ref", r.Ref, "status", status, "error", err)
	}
	days := settings.ForServer(ctx, s.store).RunRetentionDays
	_ = s.store.PruneCronRuns(ctx, time.Now().Add(-time.Duration(days)*24*time.Hour))
}

// LoadSchedules (re)registers cron entries for all enabled jobs and all
// cron-kind apps (standalone CronJob-style services).
func (s *Service) LoadSchedules(ctx context.Context) error {
	// No sweep of open runs here: a run waiting in the queue survives a
	// restart and is still open, and one the restart interrupted is closed by
	// the queue's cleanup.
	apps, err := s.store.ListTiles(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.entries {
		s.cron.Remove(id)
	}
	s.entries = map[string]cron.EntryID{}
	for _, a := range apps {
		if a.Kind != "cron" || a.Cron == "" || a.Status == "paused" {
			continue
		}
		a := a
		id, err := s.cron.AddFunc(a.Cron, func() {
			if _, err := s.StartApp(context.Background(), a.ID, TriggerSchedule, ""); err != nil {
				slog.Error("scheduled run not queued", "tile", a.ID, "error", err)
			}
		})
		if err != nil {
			continue
		}
		s.entries["app:"+a.ID] = id
	}
	return nil
}

// StartApp opens the run row and queues the work, returning as soon as the
// row exists. A handler can then render a panel that already says "running",
// RunNow used to race the insert and always drew idle. Schedule ticks,
// manual runs and on-deploy runs all start here.
func (s *Service) StartApp(ctx context.Context, appID, trigger, actor string) (*repo.CronRun, error) {
	if s.work == nil {
		return nil, fmt.Errorf("job runner unavailable")
	}
	app, err := s.store.GetTile(ctx, appID)
	if err != nil {
		return nil, err
	}
	if app == nil {
		return nil, fmt.Errorf("tile %s not found", appID)
	}
	run := s.startRun(ctx, "app:"+app.ID, trigger, actor, time.Now().UTC())
	if _, err := s.work.Enqueue(ctx, Kind, run.ID, runJob{TileID: app.ID, RunID: run.ID}); err != nil {
		s.finishRun(ctx, run, "error", err.Error())
		return nil, err
	}
	s.notifier.Project(app.StackID)
	return run, nil
}

// runApp executes one cron-kind app: a fresh one-shot container from its
// image with its (decrypted) env, outcome recorded on the app row and on the
// run opened by the caller. Overlapping ticks are skipped unless the app
// allows them.
func (s *Service) runApp(jobCtx context.Context, app *repo.Tile, run *repo.CronRun) {
	// Stop cancels jobCtx; the outcome is still written, so everything after
	// the run itself uses a context that outlives the cancel.
	ctx := context.WithoutCancel(jobCtx)
	ref := run.Ref
	if !s.begin(ref, app.AllowOverlap) {
		s.finishRun(ctx, run, "skipped", "previous run still in progress")
		s.notifier.Project(app.StackID)
		return
	}
	defer s.end(ref)
	if why := s.skipReason(ctx, app, run.Trigger); why != "" {
		s.finishRun(ctx, run, "skipped", why)
		s.notifier.Project(app.StackID)
		return
	}

	image := s.appImage(ctx, app)
	if image == "" {
		_ = s.store.RecordTileRun(ctx, app.ID, "error", "no image set")
		s.finishRun(ctx, run, "error", "no image set")
		s.notifier.Project(app.StackID)
		return
	}
	// A command runs through a shell that replaces the image's entrypoint, so
	// the override is what actually executes even when the image defines one.
	var entrypoint string
	var cmd []string
	if app.Command != "" {
		entrypoint, cmd = "sh", []string{"-c", app.Command}
	} // empty = the image's own ENTRYPOINT/CMD, like a k8s CronJob without command:

	res := settings.ForTile(ctx, s.store, app)
	cpuLimit, memLimit := res.EffectiveLimits(app.CPULimit, app.MemLimitMB)
	runCtx, cancel := context.WithTimeout(jobCtx, time.Duration(res.EffectiveTimeout(app.TimeoutMinutes))*time.Minute)
	defer cancel()
	netName, name := s.runNames(ctx, app, "run", run.ID)
	var out string
	env, nets, err := s.envLines(ctx, app)
	if err == nil {
		spec, warn := s.jobSpec(runCtx, app, netName, image, entrypoint, cmd, env, cpuLimit, memLimit, name, run.ID, nets)
		out, err = s.c.RunJob(runCtx, spec)
		out = warn + out
	}
	status := "ok"
	if err != nil {
		status = "error"
		// A run someone stopped is not a failure: no error text, and the row
		// says stopped so the history and the card stay readable.
		if jobCtx.Err() == context.Canceled {
			status = "stopped"
		} else {
			out = out + "\n" + err.Error()
		}
	}
	if len(out) > maxOutput {
		out = out[len(out)-maxOutput:]
	}
	out = strings.TrimSpace(out)
	_ = s.store.RecordTileRun(ctx, app.ID, status, out)
	s.finishRun(ctx, run, status, out)
	s.notifier.Project(app.StackID)
	// Notify on the ok→error transition only, a cron that keeps failing on
	// every tick must not flood the notification center.
	if status == "error" && app.LastStatus != "error" {
		s.notifier.Push(ctx, notify.KindCronFailed,
			"Cron service failed: "+app.Name, tail(out, 300), "/apps/"+app.ID)
	}
}

// tail keeps the last n characters (error output ends with the useful bit).
func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// envLines is the tile's resolved environment plus the shared networks the
// one-shot must join to reach those hosts, the same source the deploy engine
// attaches from. A cron in an env that borrows a slice from another env's
// instance otherwise cannot run at all.
func (s *Service) envLines(ctx context.Context, app *repo.Tile) ([]string, []string, error) {
	res, err := varref.New(s.store).Resolve(ctx, app.ID, varref.System)
	if err != nil {
		return nil, nil, err
	}
	return res.Lines(), res.Networks, nil
}

// runNames resolves the environment network a one-shot joins (creating it if
// needed) and the container name it takes: the tile's scoped name with a
// run-id suffix, so a run in flight sits next to its tile in `docker ps` as
// stkr_org_stack_env_tile_run-1a2b3c4d.
//
// Both empty only on lookup failure, jobSpec then falls back to the
// shared network and a random name rather than dying on a naming error.
func (s *Service) runNames(ctx context.Context, app *repo.Tile, kind, runID string) (string, string) {
	sc, netName, err := envnet.Ensure(ctx, s.store, s.envs, s.c, app)
	if err != nil {
		return "", ""
	}
	suffix := kind
	if len(runID) >= 8 {
		suffix = kind + "-" + runID[:8]
	}
	return netName, sc.ContainerName(app.Slug, suffix)
}

// RunService is the swarm service a cron tile's run executes as, for anyone
// who wants its logs: the task may sit on a worker, where the manager's
// container list cannot see it, but `docker service logs` can.
func (s *Service) RunService(ctx context.Context, app *repo.Tile, runID string) string {
	_, name := s.runNames(ctx, app, "run", runID)
	return name
}

// appImage resolves the image a one-shot job should run: the last successful
// deployment's tag, falling back to the app's configured image ref.
func (s *Service) appImage(ctx context.Context, app *repo.Tile) string {
	deps, err := s.store.ListDeploymentsByTile(ctx, app.ID, 20)
	if err == nil {
		for _, d := range deps {
			if d.Status == "done" && d.ImageTag != "" {
				return d.ImageTag
			}
		}
	}
	return app.ImageRef
}

// jobSpec builds the replicated-job service one run executes as. Every network
// it needs is in the spec at create time: a service cannot join one afterwards
// without rolling, and a job has nothing to roll into. A network that cannot
// be made is a warning in the output, not a refusal, the same as the deploy
// engine, the returned string is prepended to the run's output.
func (s *Service) jobSpec(ctx context.Context, app *repo.Tile, netName, image, entrypoint string, cmd, env []string, cpu float64, memMB int, name, runID string, nets []string) (runtime.ServiceSpec, string) {
	if netName == "" {
		netName = runtime.NetworkName
	}
	if name == "" {
		name = "stkr-job-" + runID
	}
	attach := []runtime.NetAttach{{Name: netName}}
	var warn string
	for _, n := range nets {
		if err := s.c.EnsureOverlay(ctx, n); err != nil {
			warn += fmt.Sprintf("warning: shared network %s: %v\n", n, err)
			continue
		}
		attach = append(attach, runtime.NetAttach{Name: n})
	}
	spec := runtime.ServiceSpec{
		Name:       name,
		Image:      image,
		Cmd:        cmd,
		Env:        env,
		Networks:   attach,
		Labels:     map[string]string{runtime.LabelRun: runID},
		CPULimit:   cpu,
		MemLimitMB: memMB,
	}
	if entrypoint != "" {
		spec.Entrypoint = []string{entrypoint}
	}
	spec.RegistryAuth = s.pullAuth(ctx, app, image)
	return spec, warn
}

// pullAuth is the credential swarm hands the node that runs this job so it can
// pull the image itself. The deploy engine has always set this on a tile's own
// service; a one-shot run had nothing, so every scheduled run of a tile whose
// image lives in the managed registry died on "No such image" with no hint
// that the pull was a permissions failure.
//
// Scoped to the tile's own org, not the registry's root pair: a run's node
// keeps the credential in its daemon for the life of the task, and the root
// pair reaches every org's images. An image outside the managed registry gets
// nothing, which is what it had before.
func (s *Service) pullAuth(ctx context.Context, app *repo.Tile, image string) string {
	host, _, ok := strings.Cut(image, "/")
	if !ok || !strings.ContainsAny(host, ".:") {
		return "" // docker hub, no host segment
	}
	at, auth, err := registry.OrgPullAuth(ctx, s.store, s.c.Runtime(), app)
	if err != nil || at != host {
		return ""
	}
	return auth
}
