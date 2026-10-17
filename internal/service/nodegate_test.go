package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// gateWorld is a cluster panel with one worker, w1, an env pinned to it and
// an image tile in the env. The worker's node row is online.
func gateWorld(t *testing.T) (*Orchestrator, store.Environment, Tile) {
	t.Helper()
	ctx := context.Background()
	o := newClusterPanel(t)
	workerNode(t, o, "w1")
	must(t, o.refreshNodeClients(ctx))
	// No mesh loop: the tests hand it the facts they want.
	o.mesh = newMeshSvc(o.store, func(string) (MeshNode, bool) { return nil, false })

	org, stack, now := uuid.NewString(), uuid.NewString(), time.Now()
	must(t, o.store.Orgs.Create(ctx, store.Org{ID: org, Name: "acme", Slug: "acme", EnvColors: "{}", Settings: "{}", CreatedAt: now}))
	must(t, o.store.Stacks.Create(ctx, store.Stack{ID: stack, OrgID: org, Name: "shop", Slug: "shop", Settings: "{}", CreatedAt: now}))
	env, err := o.CreateEnv(ctx, stack, "edge", EnvSpec{NodeID: "w1", Type: environment.Static, FromKind: environment.FromBranch, FromBranch: "main"})
	must(t, err)
	tl, err := o.CreateTile(ctx, Tile{StackID: stack, EnvironmentID: env.ID, Name: "web", Kind: tile.Image, ImageRef: "nginx:1", ContainerPort: 80})
	must(t, err)
	return o, env, tl
}

func setStatus(t *testing.T, o *Orchestrator, id, status string) {
	t.Helper()
	n, err := o.store.Nodes.Get(context.Background(), id)
	must(t, err)
	n.Status = status
	must(t, o.store.Nodes.Update(context.Background(), n))
}

func jobCount(t *testing.T, o *Orchestrator) int {
	t.Helper()
	js, err := o.store.Jobs.ListByState(context.Background())
	must(t, err)
	return len(js)
}

// Work for an env on an offline node is refused up front, saying when it was
// last seen but not naming the node (members read it), and no job row is
// written.
func TestOfflineNodeRefusesWork(t *testing.T) {
	ctx := context.Background()
	o, env, tl := gateWorld(t)
	setStatus(t, o, "w1", node.StatusOffline)
	must(t, o.store.Settings.Set(ctx, seenKey("w1"), "1700000000"))
	before := jobCount(t, o)

	for name, call := range map[string]func() error{
		"deploy":  func() error { _, err := o.Deploy(ctx, tl.ID); return err },
		"restart": func() error { _, err := o.RestartTile(ctx, tl.ID); return err },
		"start":   func() error { _, err := o.StartTile(ctx, tl.ID); return err },
		"stop":    func() error { _, err := o.StopTile(ctx, tl.ID); return err },
		"promote": func() error { _, err := o.enqueuePromote(ctx, env.ID, "rel", false); return err },
		"envsync": func() error { _, err := o.EnvSync(ctx, env.ID, "other", nil, ""); return err },
		"run":     func() error { _, _, err := o.RunTile(ctx, tl.ID); return err },
	} {
		err := call()
		if !errors.Is(err, errs.ErrBusy) || err == nil || !strings.Contains(err.Error(), "node is offline, last seen") || strings.Contains(err.Error(), "w1") {
			t.Errorf("%s = %v, want node is offline, last seen", name, err)
		}
	}
	if got := jobCount(t, o); got != before {
		t.Errorf("%d job rows written, want none", got-before)
	}

	setStatus(t, o, "w1", node.StatusOnline)
	if _, err := o.Deploy(ctx, tl.ID); err != nil {
		t.Errorf("deploy once online: %v", err)
	}
}

// A node never seen reads "never seen"; only Detail names the node.
func TestNodeDownErrorWords(t *testing.T) {
	if got := (&NodeDownError{Name: "h3"}).Detail(); got != "h3 is offline, never seen" {
		t.Errorf("never seen = %q", got)
	}
	d := &NodeDownError{Name: "h3", LastSeen: time.Now().Add(-14*time.Minute - time.Second)}
	if got := d.Detail(); got != "h3 is offline, last seen 14 min ago" {
		t.Errorf("seen = %q", got)
	}
	if got := d.Error(); got != "the node is offline, last seen 14 min ago" {
		t.Errorf("error = %q", got)
	}
}

// A forgotten node has no row any more; its refusal still names it, not its id.
func TestNodeDownNamesForgottenNode(t *testing.T) {
	ctx := context.Background()
	o, _, _ := gateWorld(t)
	must(t, o.store.ForgottenNodes.Create(ctx, store.ForgottenNode{ID: "gone-id", Name: "h3", ForgottenAt: time.Now()}))
	if got := o.nodeDown(ctx, "gone-id").Name; got != "h3" {
		t.Errorf("name = %q, want h3", got)
	}
	if got := o.nodeDown(ctx, "w1").Name; got != "w1" {
		t.Errorf("live node name = %q, want w1", got)
	}
}

// Under 1 GB free refuses and names the node; under 10% warns; a node that
// does not report its disk passes with a note.
func TestDiskCheck(t *testing.T) {
	ctx := context.Background()
	o, env, _ := gateWorld(t)
	const gb = 1 << 30
	report := func(free, total uint64) {
		o.mesh.Report(ctx, "w1", worker.Facts{DiskFree: free, DiskTotal: total})
	}

	report(gb*8/10, 100*gb)
	if _, err := o.diskCheck(ctx, "w1"); !isConflict(err) || !strings.Contains(err.Error(), "w1 has") ||
		!strings.Contains(err.Error(), "deploys need 1 GB") {
		t.Errorf("0.8 GB free = %v", err)
	}

	report(5*gb, 100*gb)
	warn, err := o.diskCheck(ctx, "w1")
	if err != nil || !strings.HasPrefix(warn, "warning: w1 has 5% (") {
		t.Errorf("5%% free = %q, %v", warn, err)
	}
	var log bytes.Buffer
	must(t, o.preflight(ctx, env, &log))
	if !strings.Contains(log.String(), "warning: w1 has 5%") {
		t.Errorf("preflight log = %q", log.String())
	}

	report(50*gb, 100*gb)
	if warn, err = o.diskCheck(ctx, "w1"); warn != "" || err != nil {
		t.Errorf("50%% free = %q, %v", warn, err)
	}

	report(0, 0)
	warn, err = o.diskCheck(ctx, "w1")
	if err != nil || warn != "disk not checked: w1 does not report its disk" {
		t.Errorf("unknown disk = %q, %v", warn, err)
	}

	// The refusal also stops the enqueue, before a row is written.
	report(gb/2, 100*gb)
	before := jobCount(t, o)
	tl, err := o.tiles.List(ctx, env.ID)
	must(t, err)
	if _, err := o.Deploy(ctx, tl[0].ID); !isConflict(err) {
		t.Errorf("deploy on a full disk = %v", err)
	}
	if jobCount(t, o) != before {
		t.Error("a job row was written for a refused deploy")
	}
	if err := o.preflight(ctx, env, &log); !isConflict(err) {
		t.Errorf("preflight on a full disk = %v", err)
	}
}

// With no mesh facts, the controller's disk is its data dir's.
func TestDiskCheckControllerFallsBackToTheDataDir(t *testing.T) {
	ctx := context.Background()
	o := newClusterPanel(t)
	o.mesh = newMeshSvc(o.store, func(string) (MeshNode, bool) { return nil, false })
	ctl, err := o.controllerID(ctx)
	must(t, err)
	free, total := o.diskOf(ctx, ctl)
	if total == 0 || free == 0 || free > total {
		t.Errorf("controller disk = %d of %d", free, total)
	}
}

// A schedule that fires while the node is offline is recorded as skipped:
// one job row, terminal, with the reason.
func TestScheduledWorkOnAnOfflineNodeIsSkipped(t *testing.T) {
	ctx := context.Background()
	o, env, tl := gateWorld(t)
	vol := store.Volume{ID: uuid.NewString(), ScopeKind: "env", ScopeID: env.ID, Slug: "data", Name: "data", CreatedAt: time.Now()}
	must(t, o.store.Volumes.Create(ctx, vol))
	sch := store.BackupSchedule{ID: uuid.NewString(), VolumeID: vol.ID, Method: "tar", Cron: "* * * * *", Timezone: "UTC", Keep: 3, Mode: "live", CreatedAt: time.Now()}
	must(t, o.store.BackupSchedules.Create(ctx, sch))
	setStatus(t, o, "w1", node.StatusOffline)

	must(t, o.scheduleBackup(ctx, sch))
	must(t, o.scheduleCron(ctx, store.Tile{ID: tl.ID, EnvironmentID: env.ID}))
	js, err := o.store.Jobs.ListByState(ctx, job.Skipped)
	must(t, err)
	if len(js) != 2 {
		t.Fatalf("%d skipped jobs, want 2", len(js))
	}
	for _, j := range js {
		if j.Error != "skipped: node offline" || !job.Terminal(j.State) {
			t.Errorf("job %s = %s %q", j.Kind, j.State, j.Error)
		}
	}
	if live, err := o.store.Jobs.ListByState(ctx, job.Queued, job.Running); err != nil || len(live) != 0 {
		t.Errorf("live jobs = %d, %v", len(live), err)
	}

	setStatus(t, o, "w1", node.StatusOnline)
	must(t, o.scheduleBackup(ctx, sch))
	if live, err := o.store.Jobs.ListByState(ctx, job.Queued, job.Running); err != nil || len(live) != 1 {
		t.Errorf("online: live jobs = %d, %v", len(live), err)
	}
}

// When a node flips offline its last-seen time is kept in the settings, so a
// panel restart does not forget it.
func TestLastSeenSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	o := newClusterPanel(t)
	_, srv, _ := workerNodeWith(t, o, "w1", node.StatusJoined, worker.Config{})
	must(t, o.refreshNodeClients(ctx))
	srv.Close()
	must(t, o.refreshNodeClients(ctx))
	must(t, o.refreshNodeClients(ctx))
	seen := o.lastSeen("w1")
	if time.Since(seen) > time.Minute || seen.IsZero() {
		t.Fatalf("last seen after the flip = %v", seen)
	}
	dir := o.cfg.DataDir
	must(t, o.Close())

	o2, err := New(Config{DataDir: dir, SecretsKey: testKey, Conntrack: dir + "/nf_conntrack", Version: "v2"},
		WithDocker(dockerfake.New()), WithVIP(vipStub{}),
		WithProxy(func(context.Context, json.RawMessage) error { return nil }))
	must(t, err)
	t.Cleanup(func() { _ = o2.Close() })
	if got := o2.lastSeen("w1"); got.Unix() != seen.Unix() {
		t.Errorf("after a restart last seen = %v, want %v", got, seen)
	}
}

// A worker that is not on the panel's version or the one before is refused
// new Docker calls, unless the ctx says otherwise. Extras stay open.
func TestTooOldWorkerIsRefused(t *testing.T) {
	ctx := context.Background()
	o := newClusterPanel(t)
	workerNodeWith(t, o, "w1", node.StatusJoined, worker.Config{Version: "v1"})
	must(t, o.refreshNodeClients(ctx))

	_, err := o.docker.HostInfo(WithNode(ctx, "w1"))
	if !isConflict(err) || !strings.Contains(err.Error(), "w1 runs stackr v1, too old for this panel (v2)") ||
		!strings.Contains(err.Error(), "upgrade it from the Update tab") {
		t.Fatalf("too old = %v", err)
	}
	if _, err := o.docker.HostInfo(allowOld(WithNode(ctx, "w1"))); err != nil {
		t.Errorf("allowOld = %v", err)
	}
	if _, err := o.nodeExtras("w1"); err != nil {
		t.Errorf("extras for a too old worker = %v", err)
	}

	// The version before this one is allowed.
	must(t, o.store.Settings.Set(ctx, "panel_prev_version", "v1"))
	must(t, o.refreshNodeClients(ctx))
	if _, err := o.docker.HostInfo(WithNode(ctx, "w1")); err != nil {
		t.Errorf("panel_prev_version worker = %v", err)
	}
}

func TestVersionAllowed(t *testing.T) {
	o := newClusterPanel(t)
	must(t, o.store.Settings.Set(context.Background(), "panel_prev_version", "v1"))
	for v, want := range map[string]bool{"v2": true, "v1": true, "v0": false, "": false} {
		if got := o.versionAllowed(context.Background(), v); got != want {
			t.Errorf("versionAllowed(%q) = %v, want %v", v, got, want)
		}
	}
}

// The banner reads this: the env's node when it is down, nothing otherwise.
func TestEnvNodeDown(t *testing.T) {
	ctx := context.Background()
	o, env, _ := gateWorld(t)
	if _, down := o.EnvNodeDown(ctx, env.ID); down {
		t.Fatal("an online node reads as down")
	}
	setStatus(t, o, "w1", node.StatusOffline)
	nd, down := o.EnvNodeDown(ctx, env.ID)
	if !down || nd.Name != "w1" {
		t.Errorf("EnvNodeDown = %+v, %v", nd, down)
	}
}

// The env canvas of an offline worker still draws: its tiles read offline
// instead of the page failing on the gate.
func TestCanvasOfOfflineNode(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	w, err := n.st.Nodes.Get(ctx, "w1")
	must(t, err)
	w.Status = node.StatusOffline
	must(t, n.st.Nodes.Update(ctx, w))
	n.orch.docker.(*DockerRouter).Remove("w1") // an offline worker has no client
	v, err := n.orch.Canvas(ctx, CanvasScope{Kind: CanvasEnv, ID: n.edge.ID}, ShowAll)
	if err != nil {
		t.Fatalf("canvas of an offline node: %v", err)
	}
	found := false
	for _, c := range v.Nodes {
		if c.ID == n.edgeT.ID {
			found = c.Status == "offline"
		}
	}
	if !found {
		t.Fatalf("tile card not offline: %+v", v.Nodes)
	}
}

// A scheduled backup whose worker stopped answering but is not yet marked
// offline is skipped as it starts, like one on an offline node.
func TestScheduledBackupSkipsAnUnreachableNode(t *testing.T) {
	ctx := context.Background()
	o, env, _ := gateWorld(t)
	vol := store.Volume{ID: uuid.NewString(), ScopeKind: "env", ScopeID: env.ID, Slug: "data", Name: "data", CreatedAt: time.Now()}
	must(t, o.store.Volumes.Create(ctx, vol))
	must(t, o.skipUnreachable(ctx, vol.ID))

	dead, err := docker.NewRemote("tcp://127.0.0.1:1", &http.Client{})
	must(t, err)
	o.docker.(*DockerRouter).Set("w1", dead)
	err = o.skipUnreachable(ctx, vol.ID)
	if !errors.Is(err, jobs.ErrSkipped) || err.Error() != "skipped: node unreachable" {
		t.Errorf("dropped node = %v", err)
	}
	// A worker too old is refused, not unreachable: the backup reports it.
	o.cluster.mu.Lock()
	o.cluster.old["w1"] = "v0.0.1"
	o.cluster.mu.Unlock()
	o.docker.(*DockerRouter).Set("w1", dead)
	if err := o.skipUnreachable(ctx, vol.ID); err != nil {
		t.Errorf("too old = %v, want the backup to run and say so", err)
	}
	setStatus(t, o, "w1", node.StatusOffline)
	if err := o.skipUnreachable(ctx, vol.ID); err == nil || err.Error() != "skipped: node offline" {
		t.Errorf("offline node = %v", err)
	}
}

// A node with no VIP table is refused as down: members read the error, so
// it names no node; admins get the name from Detail.
func TestNoVIPTableIsNodeDown(t *testing.T) {
	ctx := context.Background()
	o, _, _ := gateWorld(t)
	n, err := o.store.Nodes.Get(ctx, "w1")
	must(t, err)
	n.Name = "edge-east"
	must(t, o.store.Nodes.Update(ctx, n))
	o.vip.(*nodeVIPs).Detach("w1")
	_, err = o.vipFor(WithNode(ctx, "w1"))
	var d *NodeDownError
	if !errors.As(err, &d) || !errors.Is(err, errs.ErrBusy) {
		t.Fatalf("no table = %v", err)
	}
	if strings.Contains(err.Error(), "w1") || strings.Contains(err.Error(), "edge-east") {
		t.Errorf("member error names the node: %q", err)
	}
	if !strings.Contains(d.Detail(), "edge-east") {
		t.Errorf("admin detail = %q", d.Detail())
	}
}
