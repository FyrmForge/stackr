// Package deploy runs the build+release pipeline: fetch → build → run new →
// health-gate → retire old. One FIFO worker; logs persist to disk and fan out
// live via the stream hub on topic "deploy:<id>".
package deploy

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/deploystate"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/config/runpolicy"
	"github.com/FyrmForge/stackr/internal/stackrd/config/settings"
	"github.com/FyrmForge/stackr/internal/stackrd/config/varref"
	"github.com/FyrmForge/stackr/internal/stackrd/handlers/stream"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/envnet"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/managedtiles"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/placement"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/registry"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/storagetiles"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/workqueue"
	"github.com/FyrmForge/stackr/internal/stackrd/service/notify"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// gitProtocols stops git from following a redirect or submodule into a local
// or exotic transport. ValidGitURL already refuses those at the URL, this
// covers what git resolves on its own.
const gitProtocols = "GIT_ALLOW_PROTOCOL=https:ssh"

type Engine struct {
	store    repo.Store
	rt       *runtime.Runtime
	clus     *cluster.Cluster
	hub      *stream.Hub
	notifier *notify.Notifier
	dataDir  string

	// GitAuth optionally returns extra environment lines (GIT_CONFIG_*) that
	// authenticate the tile's fetch/clone (e.g. a GitHub App installation
	// token via its connector). Env rather than argv so tokens stay out of
	// /proc. Set once at startup, before the worker runs.
	GitAuth func(ctx context.Context, tile *repo.Tile) []string
	// RegistryAuth optionally returns docker login credentials for pulling
	// the tile's image (e.g. ghcr.io via its connector). "" = none.
	RegistryAuth func(ctx context.Context, tile *repo.Tile) (user, pass string)
	// OnFinish, if set, is called after a deployment reaches a terminal
	// status (done/error/cancelled).
	OnFinish func(tile *repo.Tile, d *repo.Deployment)

	// work is the durable queue. With it, a deploy is a work item and a panel
	// restart requeues it; without it the in-memory channel below is used,
	// which is what tests and any caller that has not wired the queue get.
	//
	// The channel was the reason a redeploy of the panel mid-build left every
	// queued deploy dead, marked "interrupted by a server restart" with
	// nothing to retry it.
	work *workqueue.Queue

	queue   chan string
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

// DeployKind is the work-queue kind one deploy runs under.
const DeployKind = "tile.deploy"

// deployJob is the payload: the deployment row holds everything else.
type deployJob struct {
	DeploymentID string `json:"deployment_id"`
}

// WithWork puts deploys on the durable queue.
//
// Requeue on restart. A deploy is convergent in the way that matters here: it
// re-reads the deployment row, re-fetches or re-pulls, and rolls the service,
// so running it again from the top on a half-finished build finishes the job.
// The dedupe key is the tile, which is exactly what SupersedeWaiting does by
// hand: a newer deploy of the same tile outranks one still waiting.
func (e *Engine) WithWork(q *workqueue.Queue) *Engine {
	if q == nil {
		return e
	}
	e.work = q
	q.Register(DeployKind, func(ctx context.Context, j *workqueue.Job) error {
		var p deployJob
		if err := j.Payload(&p); err != nil {
			return err
		}
		// The boot sweep has already marked anything the last process left in
		// queued or running as "interrupted by a server restart", because
		// without a durable queue nothing would ever retry it. Here something
		// does, and the work item is the authority, so put the row back.
		if err := e.reopen(ctx, p.DeploymentID); err != nil {
			return err
		}
		// run() reports through the deployment row and its log, which is where
		// anyone looks, so the work item only needs to say it ran.
		e.run(p.DeploymentID)
		return nil
	}, workqueue.KindOpts{
		Timeout:   30 * time.Minute,
		OnRestart: workqueue.Requeue,
		// The queue drops the older item; the deployment row it carried
		// would otherwise sit "queued" with a Cancel button and no runner.
		OnSuperseded: func(ctx context.Context, j *workqueue.Job) {
			var p deployJob
			if err := j.Payload(&p); err != nil {
				return
			}
			d, err := e.store.GetDeployment(ctx, p.DeploymentID)
			if err != nil || d == nil || d.Status != "queued" {
				return
			}
			d.Status = "cancelled"
			d.Error = "superseded by a newer deploy"
			d.FinishedAt = sql.NullTime{Time: time.Now().UTC(), Valid: true}
			if err := e.store.UpdateDeployment(ctx, d); err != nil {
				slog.Error("superseded deploy not marked cancelled", "deployment", d.ID, "error", err)
			}
			e.hub.Publish("deploy-status:"+d.ID, "cancelled")
		},
	})
	return e
}

// reopen puts an interrupted deployment back in the queued state run() expects.
// Anything a person decided in the meantime (cancelled, or superseded by a
// newer deploy) is left exactly as it is: the work item is stale then, and
// run() declines it on the status check.
func (e *Engine) reopen(ctx context.Context, deploymentID string) error {
	d, err := e.store.GetDeployment(ctx, deploymentID)
	if err != nil || d == nil {
		return err
	}
	interrupted := d.Status == "running" ||
		(d.Status == "error" && d.Error == repo.InterruptedMsg)
	if !interrupted {
		return nil
	}
	d.Status = "queued"
	d.Error = ""
	d.FinishedAt = sql.NullTime{}
	return e.store.UpdateDeployment(ctx, d)
}

// dispatch hands a queued deployment to whichever runner is wired.
func (e *Engine) dispatch(ctx context.Context, d *repo.Deployment) error {
	if e.work != nil {
		_, err := e.work.Enqueue(ctx, DeployKind, d.TileID, deployJob{DeploymentID: d.ID})
		if err == nil {
			return nil
		}
		return e.failQueued(ctx, d, err.Error())
	}
	select {
	case e.queue <- d.ID:
		return nil
	default:
		return e.failQueued(ctx, d, "deploy queue full")
	}
}

// failQueued closes a deployment nothing is going to run.
func (e *Engine) failQueued(ctx context.Context, d *repo.Deployment, msg string) error {
	d.Status = "error"
	d.Error = msg
	if err := e.store.UpdateDeployment(ctx, d); err != nil {
		slog.Error("queued deploy not marked failed", "deployment", d.ID, "error", err)
	}
	return fmt.Errorf("%s", msg)
}

func NewEngine(store repo.Store, rt *runtime.Runtime, clus *cluster.Cluster, hub *stream.Hub, dataDir string, notifier *notify.Notifier) *Engine {
	e := &Engine{
		store:    store,
		rt:       rt,
		clus:     clus,
		hub:      hub,
		notifier: notifier,
		dataDir:  dataDir,
		queue:    make(chan string, 256),
		cancels:  map[string]context.CancelFunc{},
	}
	for _, d := range []string{"repos", "deploy-logs", "keys", "files"} {
		_ = os.MkdirAll(filepath.Join(dataDir, d), 0o755)
	}
	go e.worker()
	return e
}

// Enqueue creates a deployment for the app and queues it. Returns its ID.
// RepoDir is where a git tile's clone lives; it exists once the tile has
// deployed at least once.
func (e *Engine) RepoDir(app *repo.Tile) string {
	return filepath.Join(e.dataDir, "repos", app.ID)
}

func (e *Engine) Enqueue(ctx context.Context, app *repo.Tile, trigger string) (string, error) {
	if app.IsVolume() {
		return "", fmt.Errorf("volume tiles are not deployable")
	}
	// Any newer deploy outranks a parked one, a waiting_ci row released
	// later would put an older commit back over whatever ships now.
	e.SupersedeWaiting(ctx, app.ID, "superseded by a newer deploy")
	d := &repo.Deployment{
		ID:        uuid.New().String(),
		TileID:    app.ID,
		Status:    "queued",
		Trigger:   trigger,
		CreatedAt: time.Now().UTC(),
	}
	if err := e.store.CreateDeployment(ctx, d); err != nil {
		return "", err
	}
	if err := e.dispatch(ctx, d); err != nil {
		return "", err
	}
	return d.ID, nil
}

// EnqueueRollback queues a deployment that re-runs a previously built image.
func (e *Engine) EnqueueRollback(ctx context.Context, app *repo.Tile, imageTag string) (string, error) {
	return e.enqueueImage(ctx, app, "rollback", imageTag, "", "superseded by a rollback")
}

// EnqueuePromote queues a deployment of the image built at commitSHA: the
// tag is <repo>:<sha7>, run as is when it exists locally (a lower env built
// it) and built at that commit when it does not.
func (e *Engine) EnqueuePromote(ctx context.Context, app *repo.Tile, commitSHA string) (string, error) {
	if len(commitSHA) < 7 {
		return "", fmt.Errorf("promote needs a commit")
	}
	sc, err := envnet.Resolve(ctx, e.store, app)
	if err != nil {
		return "", err
	}
	tag := sc.ImageRepo(app.Slug) + ":" + commitSHA[:7]
	return e.enqueueImage(ctx, app, "promote", tag, commitSHA, "superseded by a promote")
}

// CurrentImage is the tag of the tile's newest successful deployment, "" when
// it has never deployed.
func (e *Engine) CurrentImage(ctx context.Context, app *repo.Tile) string {
	deps, err := e.store.ListDeploymentsByTile(ctx, app.ID, 20)
	if err != nil {
		return ""
	}
	for _, d := range deps {
		if d.Status == "done" && d.ImageTag != "" {
			return d.ImageTag
		}
	}
	return ""
}

// EnqueueCurrent restarts the tile on the image it already runs, with the
// given trigger. Config changes on an env above the bottom rung go through
// here so an apply never ships code nobody promoted. Returns "" and no error
// when the tile has no image yet; the caller decides what to do then.
func (e *Engine) EnqueueCurrent(ctx context.Context, app *repo.Tile, trigger string) (string, error) {
	tag := e.CurrentImage(ctx, app)
	if tag == "" {
		return "", nil
	}
	return e.enqueueImage(ctx, app, trigger, tag, "", "superseded by a newer deploy")
}

func (e *Engine) enqueueImage(ctx context.Context, app *repo.Tile, trigger, imageTag, commitSHA, note string) (string, error) {
	e.SupersedeWaiting(ctx, app.ID, note)
	d := &repo.Deployment{
		ID:        uuid.New().String(),
		TileID:    app.ID,
		Status:    "queued",
		Trigger:   trigger,
		ImageTag:  imageTag,
		CommitSHA: commitSHA,
		CreatedAt: time.Now().UTC(),
	}
	if err := e.store.CreateDeployment(ctx, d); err != nil {
		return "", err
	}
	if err := e.dispatch(ctx, d); err != nil {
		return "", err
	}
	return d.ID, nil
}

func (e *Engine) Cancel(ctx context.Context, deploymentID string) {
	e.mu.Lock()
	cancel := e.cancels[deploymentID]
	e.mu.Unlock()
	if cancel != nil {
		cancel()
		return
	}
	d, err := e.store.GetDeployment(ctx, deploymentID)
	if err != nil || d == nil || !deploystate.IsCancellable(d.Status) {
		return
	}
	d.Status = "cancelled"
	d.FinishedAt = sql.NullTime{Time: time.Now().UTC(), Valid: true}
	if err := e.store.UpdateDeployment(ctx, d); err != nil {
		slog.Error("deploy not marked cancelled", "deployment", d.ID, "error", err)
	}
}

// Park records a push held back by wait_for_ci: a waiting_ci deployment the
// CI gate releases (or fails) once the commit's checks settle. Parked rows
// survive restarts, the boot sweep only clears queued/running.
func (e *Engine) Park(ctx context.Context, app *repo.Tile, trigger, commitSHA string) (string, error) {
	e.SupersedeWaiting(ctx, app.ID, "superseded by a newer push")
	d := &repo.Deployment{
		ID:        uuid.New().String(),
		TileID:    app.ID,
		Status:    "waiting_ci",
		Trigger:   trigger,
		CommitSHA: commitSHA,
		CreatedAt: time.Now().UTC(),
	}
	if err := e.store.CreateDeployment(ctx, d); err != nil {
		return "", err
	}
	e.hub.Publish("deploy-status:"+d.ID, "waiting_ci")
	return d.ID, nil
}

// Release moves a parked deployment into the build queue.
func (e *Engine) Release(ctx context.Context, d *repo.Deployment) error {
	d.Status = "queued"
	if err := e.store.UpdateDeployment(ctx, d); err != nil {
		return err
	}
	return e.dispatch(ctx, d)
}

// FailWaiting errors a parked deployment (CI failed, or the gate cannot
// evaluate it and says why).
func (e *Engine) FailWaiting(ctx context.Context, d *repo.Deployment, msg string) {
	d.Status = "error"
	d.Error = msg
	d.FinishedAt = sql.NullTime{Time: time.Now().UTC(), Valid: true}
	if err := e.store.UpdateDeployment(ctx, d); err != nil {
		slog.Error("parked deploy not marked failed", "deployment", d.ID, "error", err)
	}
	e.hub.Publish("deploy-status:"+d.ID, "error")
}

// SupersedeWaiting cancels a tile's parked waiting_ci deployments.
func (e *Engine) SupersedeWaiting(ctx context.Context, tileID, note string) {
	ds, err := e.store.ListDeploymentsByStatus(ctx, "waiting_ci")
	if err != nil {
		return
	}
	for i := range ds {
		if ds[i].TileID != tileID {
			continue
		}
		d := &ds[i]
		d.Status = "cancelled"
		d.Error = note
		d.FinishedAt = sql.NullTime{Time: time.Now().UTC(), Valid: true}
		if err := e.store.UpdateDeployment(ctx, d); err != nil {
			slog.Error("deploy not marked cancelled", "deployment", d.ID, "error", err)
		}
		e.hub.Publish("deploy-status:"+d.ID, "cancelled")
	}
}

// ListBranches runs `git ls-remote --heads` for an app's repo and returns the
// branch names. Best-effort: returns nil on any error (bad URL, auth, timeout).
func (e *Engine) ListBranches(ctx context.Context, app *repo.Tile) []string {
	if !repo.ValidGitURL(app.GitURL) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--heads", "--", app.GitURL)
	cmd.Env = append(os.Environ(), gitProtocols)
	if e.GitAuth != nil {
		cmd.Env = append(cmd.Env, e.GitAuth(ctx, app)...)
	}
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var branches []string
	for _, line := range strings.Split(string(out), "\n") {
		_, ref, ok := strings.Cut(line, "\t")
		if ok {
			branches = append(branches, strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/"))
		}
	}
	return branches
}

// LogPath returns the on-disk log file for a deployment.
func (e *Engine) LogPath(deploymentID string) string {
	return filepath.Join(e.dataDir, "deploy-logs", deploymentID+".log")
}

// DeployLog reads a deployment's captured build/deploy log ("" if none yet).
func (e *Engine) DeployLog(deploymentID string) (string, error) {
	b, err := os.ReadFile(e.LogPath(deploymentID))
	if os.IsNotExist(err) {
		return "", nil
	}
	return string(b), err
}

func (e *Engine) worker() {
	for id := range e.queue {
		e.run(id)
	}
}

// logWriter tees to file and hub.
type logWriter struct {
	f     *os.File
	hub   *stream.Hub
	topic string
}

func (w *logWriter) Write(p []byte) (int, error) {
	_, _ = w.f.Write(p)
	w.hub.Publish(w.topic, string(p))
	return len(p), nil
}

func (e *Engine) run(deploymentID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	e.mu.Lock()
	e.cancels[deploymentID] = cancel
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.cancels, deploymentID)
		e.mu.Unlock()
	}()

	d, err := e.store.GetDeployment(ctx, deploymentID)
	if err != nil || d == nil || d.Status != "queued" {
		return
	}
	app, err := e.store.GetTile(ctx, d.TileID)
	if err != nil || app == nil {
		return
	}

	f, err := os.Create(e.LogPath(d.ID))
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	w := &logWriter{f: f, hub: e.hub, topic: "deploy:" + d.ID}

	d.Status = "running"
	d.StartedAt = sql.NullTime{Time: time.Now().UTC(), Valid: true}
	if err := e.store.UpdateDeployment(ctx, d); err != nil {
		slog.Error("deploy not marked running", "deployment", d.ID, "error", err)
	}
	if err := e.store.UpdateTileStatus(ctx, app.ID, "building"); err != nil {
		slog.Error("tile not marked building", "tile", app.ID, "error", err)
	}
	e.hub.Publish("deploy-status:"+d.ID, "running")

	err = e.pipeline(ctx, d, app, w)

	now := sql.NullTime{Time: time.Now().UTC(), Valid: true}
	d.FinishedAt = now
	if err != nil {
		if ctx.Err() != nil {
			d.Status = "cancelled"
			_, _ = fmt.Fprintf(w, "\n--- deployment cancelled ---\n")
		} else {
			d.Status = "error"
			_, _ = fmt.Fprintf(w, "\n--- deployment failed: %v ---\n", err)
		}
		d.Error = errString(err)
		// A cancelled deploy is not a broken tile, leaving it "building"
		// would strand it there, and "error" paints a card red for something
		// the operator did on purpose. The metrics reconciler settles
		// stopped-vs-running from the container on its next tick.
		tileStatus := "error"
		if d.Status == "cancelled" {
			tileStatus = "stopped"
		} else if name, ok := varref.Unset(err); ok {
			// A value the config declares and nobody has set is not a broken
			// tile, the plan said the stack could be applied before its
			// credentials existed, so the card says what it is waiting for
			// instead of going red. Setting the value and deploying again is
			// the whole fix.
			tileStatus = WaitingPrefix + name
		}
		if err := e.store.UpdateTileStatus(context.Background(), app.ID, tileStatus); err != nil {
			slog.Error("tile status not saved", "tile", app.ID, "status", tileStatus, "error", err)
		}
	} else {
		d.Status = "done"
		_, _ = fmt.Fprintf(w, "\n--- deployment complete ---\n")
		// A cron's build leaves no container behind, "running" would paint
		// the card as a live service.
		status := "running"
		if pol, ok := runpolicy.For(app.Kind); ok && !pol.KeepAlive {
			status = "idle"
		}
		if err := e.store.UpdateTileStatus(context.Background(), app.ID, status); err != nil {
			slog.Error("tile status not saved", "tile", app.ID, "status", status, "error", err)
		}
	}
	if err := e.store.UpdateDeployment(context.Background(), d); err != nil {
		slog.Error("final deploy status not saved", "deployment", d.ID, "status", d.Status, "error", err)
	}
	e.hub.Publish("deploy-status:"+d.ID, d.Status)
	e.notifier.Project(app.StackID)
	e.notifier.Containers()
	switch d.Status {
	case "error":
		title, body := notify.DeployFailed(app.Name, d.Error)
		e.notifier.Push(context.Background(), notify.KindDeployFailed, title, body, "/deployments/"+d.ID)
	case "done":
		e.notifier.Push(context.Background(), notify.KindDeployDone,
			"Deployed "+app.Name, "", "/deployments/"+d.ID)
	}
	if e.OnFinish != nil {
		e.OnFinish(app, d)
	}
}

func (e *Engine) pipeline(ctx context.Context, d *repo.Deployment, app *repo.Tile, w io.Writer) error {
	var imageRef string
	// The credential blob for the image just pushed, so a task landing on a
	// worker can pull it. Empty for everything else: a public image, or a
	// registry with no credentials at all.
	var regAuth string

	switch {
	case d.Trigger == "rollback":
		imageRef = d.ImageTag
		regAuth = e.managedAuth(ctx, app, imageRef)
		_, _ = fmt.Fprintf(w, "--- rolling back to %s ---\n", imageRef)
	case d.ImageTag != "" && e.hasImage(ctx, d.ImageTag):
		// A preset tag (promote, or a config restart on an upper env) runs
		// as is. A promote whose tag is not here falls through to a build at
		// its commit.
		imageRef = d.ImageTag
		regAuth = e.managedAuth(ctx, app, imageRef)
		_, _ = fmt.Fprintf(w, "--- running %s ---\n", imageRef)
	case app.SourceType == "image":
		imageRef = app.ImageRef
		if imageRef == "" {
			return fmt.Errorf("no image configured")
		}
		// Per pull, in the request's auth header, for the same reason the push
		// below does not `docker login`: the daemon config is shared by every
		// org on the node. Blank auth is an anonymous pull, which is what a
		// public image wants.
		auth := ""
		switch {
		case strings.HasPrefix(imageRef, "ghcr.io/") && e.RegistryAuth != nil:
			if user, pass := e.RegistryAuth(ctx, app); user != "" {
				auth = registry.Auth(user, pass, "ghcr.io")
			}
		default:
			auth = e.managedAuth(ctx, app, imageRef)
		}
		// The service spec needs it too, not just this pull: the manager
		// pulling successfully says nothing about a task placed on a worker,
		// which has its own daemon and no credential of its own.
		regAuth = auth
		_, _ = fmt.Fprintf(w, "--- pulling %s ---\n", imageRef)
		if err := e.clus.PullImageAuth(ctx, imageRef, auth, w); err != nil {
			return fmt.Errorf("pull: %w", err)
		}
	default: // git
		if app.GitURL == "" {
			// Names the setting a person can see and says what to do. The old
			// wording named a struct field and offered nothing, and the tile
			// most likely to hit it is one migration 008 converted from a
			// compose tile, which never had a git URL to carry over.
			return fmt.Errorf("%s has no repository set. Add one under Settings, "+
				"or change its source to a Docker image", app.Name)
		}
		repoDir := filepath.Join(e.dataDir, "repos", app.ID)
		ref := app.GitBranch
		if d.Trigger == "promote" && d.CommitSHA != "" {
			ref = d.CommitSHA
		}
		_, _ = fmt.Fprintf(w, "--- fetching %s (%s) ---\n", app.GitURL, ref)
		sha, err := e.fetch(ctx, app, repoDir, ref, w)
		if err != nil {
			return fmt.Errorf("fetch: %w", err)
		}
		d.CommitSHA = sha
		d.ImageTag = ""
		if err := e.store.UpdateDeployment(ctx, d); err != nil {
			slog.Error("deploy commit not saved", "deployment", d.ID, "sha", sha, "error", err)
		}

		imageRef, err = e.imageRef(ctx, app, d)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(w, "--- building %s ---\n", imageRef)
		buildDir, err := underRepo(repoDir, app.BuildContext)
		if err != nil {
			return fmt.Errorf("build context: %w", err)
		}
		// -f is relative to the context and docker lets it point outside it.
		if _, err := underRepo(repoDir, filepath.Join(app.BuildContext, app.DockerfilePath)); err != nil {
			return fmt.Errorf("dockerfile: %w", err)
		}
		labels := map[string]string{runtime.LabelApp: app.ID, runtime.LabelDeploy: d.ID}
		builder, err := e.builder(ctx, app, w)
		if err != nil {
			return fmt.Errorf("builder: %w", err)
		}
		if err := e.rt.BuildImage(ctx, builder, buildDir, app.DockerfilePath, imageRef, parseKV(app.BuildArgs), labels, w); err != nil {
			return fmt.Errorf("build: %w", err)
		}
		// Every build pushes, and a failed push fails the deploy: under swarm
		// the registry is where a task on any node gets the image from, so a
		// build that only exists in the manager's local image store is not a
		// deployable artifact (docs/plans/30-docker-swarm.md, decision 3).
		// From here on imageRef is the pushed reference, which is what the
		// service spec and the deployment row both have to carry.
		imageRef, regAuth, err = e.pushToRegistry(ctx, app, imageRef, w)
		if err != nil {
			return fmt.Errorf("registry push: %w", err)
		}
	}
	d.ImageTag = imageRef
	if err := e.store.UpdateDeployment(ctx, d); err != nil {
		slog.Error("deploy image tag not saved", "deployment", d.ID, "image", imageRef, "error", err)
	}

	// A non-keep-alive artifact (cron) stops at the build: the scheduler
	// one-shots the built image (jobs.RunApp prefers the last done
	// deployment's ImageTag), so starting a container here would just be a
	// second copy running outside its schedule.
	if pol, ok := runpolicy.For(app.Kind); ok && !pol.KeepAlive {
		_, _ = fmt.Fprintf(w, "--- build complete; the %s runs on its trigger ---\n", app.Kind)
		return nil
	}

	// Run the new container on its environment's network, then retire the old.
	sc, netName, err := envnet.Ensure(ctx, e.store, e.clus, app)
	if err != nil {
		return fmt.Errorf("environment network: %w", err)
	}
	name := sc.ServiceName(app.Slug)
	_, _ = fmt.Fprintf(w, "--- updating service %s ---\n", name)
	cpuLimit, memLimit := settings.ForTile(ctx, e.store, app).EffectiveLimits(app.CPULimit, app.MemLimitMB)
	// Reconcile provisioned deps from their rows (recreate a dropped db/bucket +
	// republish its secret) before resolving secrets, so a self-healed secret is
	// available in this same deploy.
	managedtiles.NewService(e.clus, e.store).EnsureProvisions(ctx, app, w)
	// A dependency parked on an unset value never comes up, so neither does
	// this tile. Park it on the same name instead of starting it against a
	// dependency that is not there.
	if err := DepWaiting(ctx, e.store, app); err != nil {
		return err
	}
	rr := varref.New(e.store)
	resolved, err := rr.Resolve(ctx, app.ID, varref.System)
	if err != nil {
		return err // never run a tile with an unresolved reference
	}
	// Volume lines and the command run through the same resolver as env,
	// ${{ org.MEDIA_ROOT }}-style binds must never reach docker literal.
	binds, err := rr.ExpandStrings(ctx, app.ID, varref.System, e.volumeBinds(ctx, app))
	if err != nil {
		return fmt.Errorf("volumes: %w", err)
	}
	cmd, err := serviceCommand(ctx, rr, app)
	if err != nil {
		return err
	}
	// files: after the varref pass on binds, the materialized host paths are
	// literal and must not be re-expanded.
	fileBinds, err := e.materializeFiles(ctx, rr, app, d.ID, w)
	if err != nil {
		return fmt.Errorf("files: %w", err)
	}
	binds = append(binds, fileBinds...)
	// storage attachments: resolved to docker local-driver volumes, ensured
	// with the right opts (an auto-created bare volume would silently be an
	// empty local dir instead of the share).
	for _, l := range splitLines(app.Storage) {
		bind, err := storagetiles.Resolve(ctx, e.clus, e.store, app, l)
		if err != nil {
			return fmt.Errorf("storage: %w", err)
		}
		binds = append(binds, bind)
	}
	// Networks are part of the spec, not something joined afterwards: a
	// running service cannot take a new network without rolling every task.
	// The env overlay carries the tile's DNS names (traefik resolves tiles by
	// alias); the shared-instance networks the resolver asked for are joined
	// without one, exactly as the old post-create attach did.
	nets := []runtime.NetAttach{{Name: netName, Aliases: []string{app.Slug, envnet.TileAlias(app.ID)}}}
	for _, n := range resolved.Networks {
		if err := e.rt.EnsureOverlay(ctx, n); err != nil {
			_, _ = fmt.Fprintf(w, "warning: shared network %s: %v\n", n, err)
			continue
		}
		nets = append(nets, runtime.NetAttach{Name: n})
	}
	warnUnsupported(app, w)

	// Where this tile is allowed to run. A pinned tile gets its home node
	// here on first deploy and is refused outright if that node has left the
	// swarm, never rescheduled onto an empty volume
	// (docs/plans/30-docker-swarm.md, addendum).
	place, err := placement.For(ctx, e.store, e.rt, app)
	if err != nil {
		return err
	}
	if place.Pinned && app.Replicas > 1 {
		return fmt.Errorf("%s holds a volume, so it cannot run %d replicas; one mounter per volume, always",
			app.Slug, app.Replicas)
	}

	// Noted before the post: awaitService needs it to tell this roll from the
	// previous one, which swarm still reports as completed for a moment.
	since := time.Now()
	created, err := e.rt.EnsureService(ctx, runtime.ServiceSpec{
		Name:         name,
		Image:        imageRef,
		Cmd:          cmd,
		Env:          resolved.Lines(),
		Mounts:       binds,
		Ports:        publishedPorts(app.PublishedPorts, w),
		Networks:     nets,
		Replicas:     place.Replicas,
		RegistryAuth: regAuth,
		User:         app.User,
		CPULimit:     cpuLimit,
		MemLimitMB:   memLimit,
		ShmSizeMB:    app.ShmSizeMB,
		Pinned:       place.Pinned,
		HomeNode:     place.HomeNode,
		NodeGroup:    place.Group,

		HealthCmd:          app.HealthcheckCmd,
		HealthIntervalS:    app.HealthcheckIntervalS,
		HealthTimeoutS:     app.HealthcheckTimeoutS,
		HealthRetries:      app.HealthcheckRetries,
		HealthStartPeriodS: app.HealthcheckStartPeriodS,
		RestartPolicy:      app.RestartPolicy,
		Labels: map[string]string{
			runtime.LabelApp:    app.ID,
			runtime.LabelDeploy: d.ID,
		},
	})
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}

	if err := e.awaitService(ctx, name, app, w, created, since); err != nil {
		return err
	}
	e.retireContainers(ctx, app, w)
	e.pruneImages(ctx, app)
	// Only now: a bind mount reads the host directory live, so the old task
	// needed its folder until the roll replaced it.
	pruneFiles(e.dataDir, app.ID, d.ID)
	return nil
}

// pruneImages keeps the images of the last keepImages successful deployments
// so rollback targets stay available without hoarding disk.
const keepImages = 5

func (e *Engine) pruneImages(ctx context.Context, app *repo.Tile) {
	// The keep set spans every env's copy of this tile: an image promoted
	// upward is referenced by a sibling tile's deployment, not this one's.
	keep := map[string]bool{}
	siblings, _ := e.store.ListTilesByStack(ctx, app.StackID)
	for i := range siblings {
		if siblings[i].Slug != app.Slug {
			continue
		}
		deps, err := e.store.ListDeploymentsByTile(ctx, siblings[i].ID, 100)
		if err != nil {
			continue
		}
		n := 0
		for _, d := range deps { // newest first
			if d.Status == "done" && d.ImageTag != "" && n < keepImages {
				keep[d.ImageTag] = true
				n++
			}
		}
	}
	// By label, not name: the repo name carries no env, so a prefix match
	// would also see images this tile shares with sibling envs.
	tags, err := e.rt.ListImageTagsByLabel(ctx, runtime.LabelApp, app.ID)
	if err != nil {
		return
	}
	for _, t := range tags {
		if !keep[t] {
			_ = e.clus.RemoveImage(ctx, t)
		}
	}
}

// hasImage reports whether a "repo:tag" exists in the local daemon.
func (e *Engine) hasImage(ctx context.Context, ref string) bool {
	tags, _ := e.rt.ListImageTags(ctx, ref)
	return len(tags) > 0
}

// imageRef names a git build: <repo>:<sha7>, where the repo is
// stkr/<org>_<stack>_<tile>. The tag is the commit, so rolling back or
// promoting is "run that commit's image". When the same commit is rebuilt the
// deploy id is appended, or the new build would overwrite the image an older
// deployment still points at.
func (e *Engine) imageRef(ctx context.Context, app *repo.Tile, d *repo.Deployment) (string, error) {
	sc, err := envnet.Resolve(ctx, e.store, app)
	if err != nil {
		return "", err
	}
	ref := sc.ImageRepo(app.Slug) + ":" + shortSHA(d.CommitSHA, d.ID)
	if tags, _ := e.rt.ListImageTags(ctx, ref); len(tags) > 0 {
		ref += "-" + d.ID[:8]
	}
	return ref, nil
}

// shortSHA is the seven-char commit prefix, falling back to the deploy id
// when the fetch produced none.
func shortSHA(sha, deployID string) string {
	if len(sha) >= 7 {
		return sha[:7]
	}
	return deployID[:8]
}

// pushToRegistry tags and pushes the built image to the app's registry (or the
// managed one when none is set) and returns the pushed reference. That
// reference, not the local build tag, is what the service spec runs: a task
// scheduled on any node pulls the image, and a local-only tag exists on
// exactly one machine (docs/plans/30-docker-swarm.md, decision 3).
//
// Two addresses for one registry, and they are not interchangeable. The push
// goes to PushURL(), which for the managed registry is localhost:<port>: the
// only name the manager's own daemon trusts without an insecure-registries
// entry. What comes back is the pull address, which is the TLS domain or the
// manager's advertise address and port, because a service spec saying
// "localhost" means the worker itself on every worker, and the task is
// rejected with "failed to resolve reference"
// (registry.PullAddr, and the same reasoning as agent.publish).
//
// The second return is the auth blob swarm hands each node so it can pull the
// image itself. Without it the manager places fine, because its daemon is
// logged in from the push above, and every worker fails on a private registry.
func (e *Engine) pushToRegistry(ctx context.Context, app *repo.Tile, imageRef string, w io.Writer) (string, string, error) {
	// One registry: the managed one. The per-tile override column never had a
	// writer and is gone (migration 013).
	reg, err := e.store.GetManagedRegistry(ctx)
	if err != nil {
		return "", "", err
	}
	if reg == nil {
		return "", "", fmt.Errorf("no registry configured")
	}
	pushHost := reg.PushURL()
	pullHost, err := registry.PullAddr(ctx, e.rt, reg)
	if err != nil {
		return "", "", err
	}
	// The org's own credential, per call, in the request's auth header. Not
	// `docker login`: the daemon config is shared by every org on the node, so
	// logging in for one org would leave its push credential usable by the
	// next org's build.
	_, org, secret, err := registry.OrgCredential(ctx, e.store, app)
	if err != nil {
		return "", "", err
	}
	path := strings.TrimPrefix(imageRef, envnet.ImagePrefix)
	remote := pushHost + "/" + path
	if err := e.rt.TagImage(ctx, imageRef, remote); err != nil {
		return "", "", err
	}
	_, _ = fmt.Fprintf(w, "--- pushing %s ---\n", remote)
	if err := e.clus.PushImageAuth(ctx, remote, registry.Auth(org.Slug, secret, pushHost), w); err != nil {
		return "", "", err
	}
	return pullHost + "/" + path, registry.Auth(org.Slug, secret, pullHost), nil
}

// managedAuth is the org-scoped credential for an image that lives in this
// install's own managed registry, and "" for anything else.
//
// Every branch of pipeline that does not build needs it. A tile with an
// `image:` pointing at the managed registry, a promote or a rollback of a tag
// the registry holds: all of those used to pull anonymously, so the manager
// 401ed and, where it happened to hold the image already, the spec went to
// the workers with no credential and their tasks failed to place. Only the
// build path was ever covered, through pushToRegistry.
//
// Best-effort by design: a host that is not the managed registry, or an
// install with no registry at all, gets "" and pulls as before.
func (e *Engine) managedAuth(ctx context.Context, app *repo.Tile, imageRef string) string {
	host, _, ok := strings.Cut(imageRef, "/")
	if !ok || !strings.ContainsAny(host, ".:") {
		return "" // docker hub, no host segment
	}
	at, auth, err := registry.OrgPullAuth(ctx, e.store, e.rt, app)
	if err != nil || at != host {
		return ""
	}
	return auth
}

// fetch clones or updates the app repo and returns the checked-out commit SHA.
// fetch checks the tile's repo out at ref (a branch or a commit) and returns
// the resulting HEAD. The clone tracks the tile's branch; ref is fetched on
// top so a commit off that branch still resolves.
func (e *Engine) fetch(ctx context.Context, app *repo.Tile, dir, ref string, w io.Writer) (string, error) {
	if ref == "" {
		ref = app.GitBranch
	}
	if ref == "" {
		// No branch set (an image tile shipping files:): the repo's default.
		ref = "HEAD"
	}
	if !repo.ValidGitURL(app.GitURL) {
		return "", fmt.Errorf("git_url %q is not an allowed repository URL", app.GitURL)
	}
	env := append(os.Environ(), gitProtocols)
	if e.GitAuth != nil {
		env = append(env, e.GitAuth(ctx, app)...)
	}
	git := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = env
		cmd.Stdout = w
		cmd.Stderr = w
		return cmd.Run()
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		_ = os.RemoveAll(dir)
		args := []string{"clone", "--single-branch"}
		if app.GitBranch != "" {
			args = append(args, "--branch", app.GitBranch)
		}
		cmd := exec.CommandContext(ctx, "git", append(args, "--", app.GitURL, dir)...)
		cmd.Env = env
		cmd.Stdout = w
		cmd.Stderr = w
		if err := cmd.Run(); err != nil {
			return "", err
		}
	}
	if err := git("fetch", "origin", ref); err != nil {
		return "", err
	}
	if err := git("checkout", "-f", "FETCH_HEAD"); err != nil {
		return "", err
	}
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Names for the build node's buildkit daemon and the buildx builder that
// points at it.
const (
	buildkitService = "stkr-buildkit"
	// buildkitAlias is the overlay name the panel dials it by; the alias is in
	// the service spec, so it survives a roll.
	buildkitAlias = "buildkit"
	buildkitPort  = "1234"
	remoteBuilder = "stkr-remote"
)

// BuilderFor is the buildx builder an org's builds run in on the manager.
func BuilderFor(orgSlug string) string { return "stkr-" + orgSlug }

// builder returns the buildx builder this tile's build runs in and makes
// sure it exists (docs/plans/37-builds.md, items 3 and 4).
//
// With a build node set (server settings, build_node) it is one rootless
// buildkit service pinned to that node, reached through the swarm mesh on
// the manager's own address. Otherwise it is a docker-container builder per
// org on the manager, each with its own cache and a quarter of the host's
// memory, so one org never hits another's cached step.
func (e *Engine) builder(ctx context.Context, app *repo.Tile, w io.Writer) (string, error) {
	if node := settings.ForServer(ctx, e.store).BuildNode; node != "" {
		// one daemon shared by every org on the build node, so the
		// per-org cache split is a manager-only property. One service per
		// org, each on its own port, when strangers share a build node.
		//
		// No published port. buildkitd authenticates nothing and builds
		// whatever it is handed, so a swarm ingress publish put arbitrary
		// code execution on every node's port 1234
		// (docs/plans/39-codex-review-fixes.md, point 1). It joins the stkr
		// overlay under an alias instead, the same way the registry does, and
		// the panel dials it by that name. mTLS is the upgrade if orgs
		// sharing one install stop trusting each other.
		created, err := e.clus.EnsureService(ctx, buildkitSpec(node))
		if err != nil {
			return "", fmt.Errorf("build node: %w", err)
		}
		if created {
			_, _ = fmt.Fprintf(w, "--- starting buildkit on the build node ---\n")
		}
		return remoteBuilder, e.rt.EnsureRemoteBuilder(ctx, remoteBuilder, "tcp://"+buildkitAlias+":"+buildkitPort)
	}
	// No build node: nothing should be running on one. Missing is fine.
	_ = e.clus.RemoveService(ctx, buildkitService)
	sc, err := envnet.Resolve(ctx, e.store, app)
	if err != nil {
		return "", err
	}
	info, err := e.rt.Info(ctx)
	if err != nil {
		return "", err
	}
	name := BuilderFor(sc.OrgSlug)
	// a quarter of the host per org; a setting when someone needs to tune it.
	return name, e.rt.EnsureBuilder(ctx, name, int(info.MemTotal/4/1024/1024))
}

// buildkitSpec is the build node's buildkit daemon. Split out so a test can
// assert the two properties that matter and cannot be checked without docker:
// no published port, and the overlay alias the panel dials.
func buildkitSpec(node string) runtime.ServiceSpec {
	return runtime.ServiceSpec{
		Name:  buildkitService,
		Image: "moby/buildkit:rootless",
		Cmd:   []string{"--addr", "tcp://0.0.0.0:" + buildkitPort, "--oci-worker-no-process-sandbox"},
		Networks: []runtime.NetAttach{{
			Name:    runtime.NetworkName,
			Aliases: []string{buildkitAlias},
		}},
		// Pinned by node id: the cache lives on that disk.
		Replicas: 1, Pinned: true, HomeNode: node,
		// Rootless buildkit dies under the default seccomp profile.
		Unconfined: true,
	}
}

// underRepo resolves a tile-supplied path against its clone and refuses one
// that lands outside it. The clone sits next to every tile's deploy keys,
// rendered files and other clones, and filepath.Join cleans ".." on the way.
func underRepo(repoDir, p string) (string, error) {
	full := filepath.Join(repoDir, filepath.FromSlash(p))
	rel, err := filepath.Rel(repoDir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q escapes the repo", p)
	}
	return full, nil
}

// materializeFiles copies the tile's declared files: entries out of its git
// repo into <dataDir>/files/<tile-id>/<deploy-id>/, templates the flagged ones
// through the varref resolver, and returns the read-only bind lines. An entry
// naming a folder ships the whole tree, and :template templates every file in
// it. The repo is the file source even for image tiles, those must carry git
// coordinates.
//
// A fresh folder per deploy rather than one rewritten in place: the running
// task bind-mounts its folder live, and a file deleted from the repo has to
// disappear from the next one. pruneFiles removes the old folders once the new
// service has converged.
func (e *Engine) materializeFiles(ctx context.Context, rr *varref.Resolver, app *repo.Tile, deployID string, w io.Writer) ([]string, error) {
	lines := splitLines(app.Files)
	if len(lines) == 0 {
		return nil, nil
	}
	repoDir := filepath.Join(e.dataDir, "repos", app.ID)
	if app.SourceType != "git" {
		// A git build already checked out; anything else fetches here.
		if app.GitURL == "" {
			return nil, fmt.Errorf("files: needs a git source; set git_url (and connector) so stackr knows which repo to ship files from")
		}
		if _, err := e.fetch(ctx, app, repoDir, "", w); err != nil {
			return nil, fmt.Errorf("fetch: %w", err)
		}
	}
	outDir := filepath.Join(e.dataDir, "files", app.ID, deployID)
	binds := make([]string, 0, len(lines))
	for _, l := range lines {
		repoPath, containerPath, template, err := runtime.ParseFileMount(l)
		if err != nil {
			return nil, err
		}
		src, err := underRepo(repoDir, repoPath)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(src)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", repoPath, err)
		}
		dst := filepath.Join(outDir, filepath.FromSlash(repoPath))
		copyOne := func(from, to string) error {
			b, err := os.ReadFile(from)
			if err != nil {
				return err
			}
			if template {
				ex, err := rr.ExpandStrings(ctx, app.ID, varref.System, []string{string(b)})
				if err != nil {
					return fmt.Errorf("template %s: %w", from, err)
				}
				b = []byte(ex[0])
			}
			if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
				return err
			}
			return os.WriteFile(to, b, 0o644)
		}
		if !info.IsDir() {
			if err := copyOne(src, dst); err != nil {
				return nil, fmt.Errorf("%s: %w", repoPath, err)
			}
			_, _ = fmt.Fprintf(w, "file %s -> %s%s\n", repoPath, containerPath, map[bool]string{true: " (templated)"}[template])
			binds = append(binds, dst+":"+containerPath+":ro")
			continue
		}
		n := 0
		// Regular files only: a symlink in the repo could point anywhere on
		// this host, and the folder is about to be mounted into a container.
		err = filepath.WalkDir(src, func(p string, de fs.DirEntry, err error) error {
			if err != nil || !de.Type().IsRegular() {
				return err
			}
			rel, err := filepath.Rel(src, p)
			if err != nil {
				return err
			}
			n++
			return copyOne(p, filepath.Join(dst, rel))
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", repoPath, err)
		}
		if err := os.MkdirAll(dst, 0o755); err != nil { // an empty folder still mounts
			return nil, err
		}
		_, _ = fmt.Fprintf(w, "folder %s -> %s (%d files)%s\n", repoPath, containerPath, n, map[bool]string{true: " (templated)"}[template])
		binds = append(binds, dst+":"+containerPath+":ro")
	}
	return binds, nil
}

// pruneFiles removes every per-deploy files folder of a tile except keep's.
func pruneFiles(dataDir, tileID, keep string) {
	dir := filepath.Join(dataDir, "files", tileID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, en := range entries {
		if en.Name() == keep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, en.Name())); err != nil {
			slog.Error("old files folder not removed", "tile", tileID, "folder", en.Name(), "error", err)
		}
	}
}

// serviceCommand builds the argv override for a service tile: varref-expanded,
// then split like a shell tokenizes words (quotes group, backslash escapes),
// no `sh -c`, because docker appends Cmd to the image entrypoint, and images
// like loki/prometheus take flag lists on their own binary. Cron keeps its
// `sh -c` semantics in jobs.RunApp; the two lifecycles differ on purpose.
func serviceCommand(ctx context.Context, rr *varref.Resolver, app *repo.Tile) ([]string, error) {
	if app.Command == "" {
		return nil, nil
	}
	ex, err := rr.ExpandStrings(ctx, app.ID, varref.System, []string{app.Command})
	if err != nil {
		return nil, fmt.Errorf("command: %w", err)
	}
	argv, err := splitCommand(ex[0])
	if err != nil {
		return nil, fmt.Errorf("command: %w", err)
	}
	return argv, nil
}

// splitCommand tokenizes a command string into argv: whitespace separates,
// single/double quotes group, backslash escapes the next rune (outside single
// quotes). No expansion of any kind, docker gets the words verbatim.
func splitCommand(s string) ([]string, error) {
	var argv []string
	var cur strings.Builder
	inWord := false
	quote := rune(0) // active quote char, 0 = none
	escaped := false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped = true
			inWord = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				argv = append(argv, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if escaped || quote != 0 {
		return nil, fmt.Errorf("unterminated quote or escape in %q", s)
	}
	if inWord {
		argv = append(argv, cur.String())
	}
	return argv, nil
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// volumeBinds assembles a service's mounts: attached volume tiles first,
// then any legacy text-field lines (bind paths live there).
func (e *Engine) volumeBinds(ctx context.Context, app *repo.Tile) []string {
	binds := []string{}
	if tiles, err := e.store.ListTilesByEnv(ctx, app.EnvironmentID); err == nil {
		for _, t := range repo.VolumesAttachedTo(tiles, app.ID) {
			// A volume with no mount path is attached but not yet placed;
			// binding it as "name:" would be a malformed bind string.
			if t.MountPath != "" {
				binds = append(binds, t.DockerVolume()+":"+t.MountPath)
			}
		}
	}
	return append(binds, splitLines(app.Volumes)...)
}

// publishedPorts parses "host:container[/udp]" lines into a port map.
// Bad lines are skipped with a warning in the deploy log rather than
// failing the deploy.
func publishedPorts(s string, w io.Writer) map[string]string {
	lines := splitLines(s)
	if len(lines) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, l := range lines {
		host, cont, ok := strings.Cut(l, ":")
		if !ok || host == "" || cont == "" {
			_, _ = fmt.Fprintf(w, "warning: skipping bad published port %q (want host:container[/udp])\n", l)
			continue
		}
		out[host] = cont
	}
	return out
}

func parseKV(s string) map[string]string {
	out := map[string]string{}
	for _, l := range splitLines(s) {
		k, v, ok := strings.Cut(l, "=")
		if ok {
			out[k] = v
		}
	}
	return out
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// warnUnsupported says out loud which container settings swarm drops. Swarm's
// task spec has no --device and no --privileged, so a tile that sets them runs
// without them; silently ignoring the fields is how a tile comes up looking
// fine and failing at the first hardware access. (--shm-size has a working
// stand-in, a tmpfs on /dev/shm, so it is not in this list.)
func warnUnsupported(app *repo.Tile, w io.Writer) {
	if app.Privileged {
		_, _ = fmt.Fprintf(w, "warning: privileged is not supported for swarm services and is ignored\n")
	}
	if len(splitLines(app.Devices)) > 0 {
		_, _ = fmt.Fprintf(w, "warning: devices are not supported for swarm services and are ignored\n")
	}
}

// awaitService follows swarm's own rolling update and translates it into the
// deploy log. Swarm does the rollout and the rollback; stackrd keeps its own
// deadline because swarm waits forever on a task stuck pulling or starting,
// and forces the rollback itself when the deadline passes.
//
// created says the service was just made, so there is no previous spec to
// roll back to and a task that never comes up is removed instead of restarting
// forever. since is the baseline for an update: right after the post swarm
// still reports the previous update as completed with the old task running,
// which Converged reads as done (see WaitRolled).
func (e *Engine) awaitService(ctx context.Context, name string, app *repo.Tile, w io.Writer, created bool, since time.Time) error {
	// The deadline stretches with the configured start period, as the old
	// hand-rolled health gate did: a slow-boot app that declares one must not
	// be cut down by the fixed 60s.
	deadline := time.Now().Add(60*time.Second + time.Duration(app.HealthcheckStartPeriodS)*time.Second)
	last := ""
	for {
		st, err := e.rt.ServiceStatus(ctx, name)
		if err != nil {
			return fmt.Errorf("service status: %w", err)
		}
		if line := fmt.Sprintf("%d/%d running %s %s", st.Running, st.Wanted, st.Update, st.Message); line != last {
			_, _ = fmt.Fprintf(w, "%d/%d replicas running%s\n", st.Running, st.Wanted, reason(st))
			last = line
		}
		if st.RolledBack() {
			return fmt.Errorf("swarm rolled the update back%s", reason(st))
		}
		if st.Converged() && (created || !st.UpdateStarted.Before(since)) {
			return nil
		}
		if time.Now().After(deadline) {
			if created {
				_, _ = fmt.Fprintf(w, "--- deadline passed, removing the service ---\n")
				if err := e.rt.RemoveService(context.Background(), name); err != nil {
					_, _ = fmt.Fprintf(w, "warning: remove failed: %v\n", err)
				}
			} else {
				_, _ = fmt.Fprintf(w, "--- deadline passed, rolling back ---\n")
				if err := e.rt.RollbackService(context.Background(), name); err != nil {
					_, _ = fmt.Fprintf(w, "warning: rollback failed: %v\n", err)
				}
			}
			return fmt.Errorf("service did not converge in time%s", reason(st))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func reason(st runtime.ServiceState) string {
	if st.Message == "" {
		return ""
	}
	return ": " + st.Message
}

// retireContainers removes containers this tile left behind from before it was
// a service. A swarm task's container carries the swarm task label; anything
// under the tile's label without one is a leftover from the container-based
// deploy path and would otherwise keep serving alongside the service.
func (e *Engine) retireContainers(ctx context.Context, app *repo.Tile, w io.Writer) {
	cs, err := e.clus.ListByLabel(ctx, runtime.LabelApp, app.ID)
	if err != nil {
		return
	}
	for _, c := range cs {
		if c.Labels["com.docker.swarm.task.id"] != "" {
			continue
		}
		_, _ = fmt.Fprintf(w, "removing pre-swarm container %s\n", c.Name)
		_ = e.clus.StopRemove(ctx, e.clus.Self(ctx), c.ID)
	}
}
