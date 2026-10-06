package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// syncRig is shop with dev (tile api, image nginx:1, port 80) as the source
// and an empty staging as the env that syncs.
type syncRig struct {
	e       *servicetest.Env
	org     string
	dev     servicetest.Tile
	staging service.Environment
}

func newSyncRig(t *testing.T) *syncRig {
	t.Helper()
	e := servicetest.New(t)
	org := e.Org(t, "acme")
	dev := e.Tile(t, org)
	st, err := e.Orch.CreateEnv(
		context.Background(),
		dev.Stack,
		"staging",
		service.EnvSpec{Type: "static", FromKind: "branch", FromBranch: "main"},
	)
	if err != nil {
		t.Fatal(err)
	}
	return &syncRig{e: e, org: org, dev: dev, staging: st}
}

// tile makes an image tile in env through the verb.
func (r *syncRig) tile(t *testing.T, envID, name string, port int) service.Tile {
	t.Helper()
	tl, err := r.e.Orch.CreateTile(context.Background(), service.Tile{
		StackID:       r.dev.Stack,
		EnvironmentID: envID,
		Name:          name,
		Kind:          "image",
		ImageRef:      "nginx:1",
		ContainerPort: port,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

// sync plans staging from dev and queues keep with the plan's sig, then
// waits for the job to finish.
func (r *syncRig) sync(t *testing.T, keep ...string) service.Job {
	t.Helper()
	ctx := context.Background()
	pl, err := r.e.Orch.PlanEnvSync(ctx, r.staging.ID, "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	j, err := r.e.Orch.EnvSync(ctx, r.staging.ID, "dev", keep, pl.Plan.Sig)
	if err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if j, err = r.e.Orch.GetJob(ctx, j.ID); err != nil || j.FinishedAt != nil {
			break
		}
	}
	if err != nil || j.State != "done" {
		t.Fatalf("sync job = %s, %v", j.State, err)
	}
	return j
}

func (r *syncRig) row(t *testing.T, envID, slug string) store.Tile {
	t.Helper()
	tl, err := r.e.Store.Tiles.GetBySlug(context.Background(), envID, slug)
	if err != nil {
		t.Fatalf("tile %s: %v", slug, err)
	}
	return tl
}

// A stack with one env cannot sync, then does; the default source is the
// env below on the ladder, and the bottom rung syncs from the next one up.
// A stack bound to a config file never can.
func TestEnvSyncSources(t *testing.T) {
	ctx := context.Background()
	e := servicetest.New(t)
	org := e.Org(t, "acme")
	dev := e.Tile(t, org)
	one, err := e.Orch.EnvSyncSources(ctx, dev.Env)
	if err != nil || one.Can || one.Why != "The stack has one environment." {
		t.Fatalf("one env = %+v, %v", one, err)
	}
	st, err := e.Orch.CreateEnv(ctx, dev.Stack, "staging", service.EnvSpec{
		Type: "static", FromKind: "branch", FromBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if s, err := e.Orch.EnvSyncSources(ctx, st.ID); err != nil || !s.Can || s.Default != "dev" || len(s.Envs) != 1 {
		t.Errorf("staging sources = %+v, %v", s, err)
	}
	if s, err := e.Orch.EnvSyncSources(ctx, dev.Env); err != nil || !s.Can || s.Default != "staging" {
		t.Errorf("dev sources = %+v, %v", s, err)
	}
	conn := e.Connector(t, org, "s")
	if _, err := e.Orch.SetConfigRepo(ctx, dev.Stack, conn, "acme/shop", "", ""); err != nil {
		t.Fatal(err)
	}
	if s, err := e.Orch.EnvSyncSources(ctx, st.ID); err != nil || s.Can || s.Why != "Managed by the stack file." {
		t.Errorf("bound = %+v, %v", s, err)
	}
}

// A review whose sig is stale, a blocked plan and an unknown source all
// refuse, and none queues a job.
func TestEnvSyncRefuses(t *testing.T) {
	ctx := context.Background()
	r := newSyncRig(t)
	o := r.e.Orch
	before, _ := o.Jobs(ctx, 0)
	pl, err := o.PlanEnvSync(ctx, r.staging.ID, "dev", nil)
	if err != nil || !pl.CanDeploy {
		t.Fatalf("plan = %+v, %v", pl, err)
	}
	if _, err := o.EnvSync(ctx, r.staging.ID, "dev", []string{"api"}, "stale"); !isConflict(err) {
		t.Errorf("stale sig = %v, want a conflict", err)
	}
	self, err := o.PlanEnvSync(ctx, r.staging.ID, "staging", nil)
	if err != nil || self.CanDeploy || len(self.Plan.Blockers) == 0 {
		t.Fatalf("self plan = %+v, %v", self, err)
	}
	if _, err := o.EnvSync(ctx, r.staging.ID, "staging", nil, self.Plan.Sig); !isConflict(err) {
		t.Errorf("blocked = %v, want a conflict", err)
	}
	if _, err := o.PlanEnvSync(ctx, r.staging.ID, "nope", nil); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("unknown source = %v, want not found", err)
	}
	if after, _ := o.Jobs(ctx, 0); len(after) != len(before) {
		t.Errorf("jobs queued: %d -> %d", len(before), len(after))
	}
}

// A New image tile is created in staging and deployed there.
func TestEnvSyncCreatesAndDeploys(t *testing.T) {
	r := newSyncRig(t)
	r.e.Healthy(r.staging.ID)
	r.sync(t, "api")
	got := r.row(t, r.staging.ID, "api")
	if got.ImageRef != "nginx:1" || got.ContainerPort != 80 {
		t.Errorf("staging api = %+v", got)
	}
	var ran bool
	for _, c := range r.e.Docker.Calls() {
		ran = ran || c.Method == "Run"
	}
	if !ran {
		t.Error("the new tile was not deployed")
	}
}

// An Edited tile takes the source's settings and keeps the target's image
// and paused; a paused tile has no replicas, so nothing redeploys. The
// New api beside it is dropped.
func TestEnvSyncEditedKeepsVersion(t *testing.T) {
	r := newSyncRig(t)
	ctx := context.Background()
	for _, c := range []struct {
		env, sched, image string
		paused            bool
	}{
		{r.dev.Env, "0 3 * * *", "nginx:1", false},
		{r.staging.ID, "0 4 * * *", "nginx:2", true},
	} {
		row := tileRow(r.dev.Stack, c.env, "nightly", "cron", func(t *store.Tile) {
			t.Schedule, t.ImageRef, t.Paused = c.sched, c.image, c.paused
		})
		if err := r.e.Store.Tiles.Create(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	r.sync(t, "nightly")
	got := r.row(t, r.staging.ID, "nightly")
	if got.Schedule != "0 3 * * *" || got.ImageRef != "nginx:2" || !got.Paused {
		t.Errorf("staging nightly = %q image %s paused %v", got.Schedule, got.ImageRef, got.Paused)
	}
	if _, err := r.e.Store.Tiles.GetBySlug(ctx, r.staging.ID, "api"); err == nil {
		t.Error("the dropped api was created")
	}
	for _, c := range r.e.Docker.Calls() {
		if c.Method == "Run" {
			t.Errorf("a paused tile was redeployed: %s", c)
		}
	}
}

// A Removed managed tile is deleted and its instance volume orphaned.
func TestEnvSyncRemovedOrphansVolume(t *testing.T) {
	ctx := context.Background()
	r := newSyncRig(t)
	r.tile(t, r.staging.ID, "api", 80)
	pg, err := r.e.Orch.CreateManagedTile(ctx, service.Tile{EnvironmentID: r.staging.ID, Name: "pg"}, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	ms, err := r.e.Orch.ManagedInstances(ctx, r.staging.ID)
	if err != nil || len(ms) != 1 {
		t.Fatalf("instances = %v, %v", ms, err)
	}
	if err := r.e.Store.Volumes.Create(ctx, store.Volume{
		ID:         "vol-pg",
		ScopeKind:  "env",
		ScopeID:    r.staging.ID,
		InstanceID: &ms[0].ID,
		Slug:       "pg-data",
		Name:       "pg-data",
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	r.sync(t, "pg")
	if _, err := r.e.Store.Tiles.Get(ctx, pg.ID); err == nil {
		t.Error("pg is still in staging")
	}
	v, err := r.e.Store.Volumes.Get(ctx, "vol-pg")
	if err != nil || v.OrphanedAt == nil {
		t.Errorf("instance volume = %+v, %v", v, err)
	}
}

// A plain param a New tile reads arrives with its value.
func TestEnvSyncCopiesParam(t *testing.T) {
	ctx := context.Background()
	r := newSyncRig(t)
	r.e.Healthy(r.staging.ID)
	row, err := r.e.Store.Tiles.Get(ctx, r.dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	row.EnvJSON = `{"MODE":"${{ params.app.mode }}"}`
	if err := r.e.Store.Tiles.Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.Orch.SetParams(ctx, service.ParamScope{Kind: "env", ID: r.dev.Env}, []service.ParamEntry{
		{Collection: "app", Name: "mode", Kind: "param", Value: "fast"},
	}); err != nil {
		t.Fatal(err)
	}
	r.sync(t, "api")
	ps, err := r.e.Orch.Params(ctx, service.ParamScope{Kind: "env", ID: r.staging.ID}, true)
	if err != nil || len(ps) != 1 || ps[0].Name != "mode" || ps[0].Value != "fast" {
		t.Errorf("staging params = %+v, %v", ps, err)
	}
}

// The overlay tags an Edited card and draws a ghost for a New tile.
func TestEnvSyncCanvas(t *testing.T) {
	ctx := context.Background()
	r := newSyncRig(t)
	api := r.tile(t, r.staging.ID, "api", 81)
	r.tile(t, r.dev.Env, "worker", 80)
	v, pl, err := r.e.Orch.EnvSyncCanvas(ctx, r.staging.ID, "dev", nil, service.ShowAll)
	if err != nil || !pl.CanDeploy {
		t.Fatalf("canvas = %v, plan %+v", err, pl)
	}
	ns := nodes(v)
	if n := ns[api.ID]; n.Sync != "edited" {
		t.Errorf("api tag = %q, want edited", n.Sync)
	}
	if g, ok := ns["sync:worker"]; !ok || g.Sync != "new" || !g.Static {
		t.Errorf("ghost = %+v, %v", g, ok)
	}
	v, _, err = r.e.Orch.EnvSyncCanvas(ctx, r.staging.ID, "dev", []string{"worker"}, service.ShowAll)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := nodes(v)["sync:worker"]; ok {
		t.Error("a dropped tile is still drawn")
	}
}

// jobIs polls the job until it is in state, or fails after a while: a parked
// job comes back on the runner's poll, a few seconds.
func (r *syncRig) jobIs(t *testing.T, id, state string) service.Job {
	t.Helper()
	var j service.Job
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		var err error
		if j, err = r.e.Orch.GetJob(context.Background(), id); err != nil || j.State == state || j.FinishedAt != nil {
			break
		}
	}
	if j.State != state {
		t.Fatalf("job = %s (%s), want %s", j.State, j.Error, state)
	}
	return j
}

// runs counts the tile containers started (the pause container is not one).
func (r *syncRig) runs() (n int) {
	for _, c := range r.e.Docker.Calls() {
		if c.Method == "Run" && c.Args[1] == "nginx:1" {
			n++
		}
	}
	return n
}

// A sync whose deploy parks on a secret the target lacks has already written
// its rows. When it resumes it does not re-plan against them: it deploys the
// tiles it still owes once the secret is set, and ends done.
func TestEnvSyncResumesAfterPark(t *testing.T) {
	ctx := context.Background()
	r := newSyncRig(t)
	o := r.e.Orch
	r.e.Healthy(r.staging.ID)
	row, err := r.e.Store.Tiles.Get(ctx, r.dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	row.EnvJSON = `{"TOKEN":"${{ params.app.token }}"}`
	if err := r.e.Store.Tiles.Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	worker := tileRow(r.dev.Stack, r.dev.Env, "worker", "image", func(t *store.Tile) {
		t.ImageRef, t.ContainerPort, t.DependsOn = "nginx:1", 80, "api"
	})
	if err := r.e.Store.Tiles.Create(ctx, worker); err != nil {
		t.Fatal(err)
	}
	secret := func(env string) {
		t.Helper()
		if _, err := o.SetParams(ctx, service.ParamScope{Kind: "env", ID: env}, []service.ParamEntry{
			{Collection: "app", Name: "token", Kind: "secret", Value: "s3cret"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	secret(r.dev.Env)
	pl, err := o.PlanEnvSync(ctx, r.staging.ID, "dev", nil)
	if err != nil || !pl.CanDeploy {
		t.Fatalf("plan = %+v, %v", pl, err)
	}
	j, err := o.EnvSync(ctx, r.staging.ID, "dev", []string{"api", "worker"}, pl.Plan.Sig)
	if err != nil {
		t.Fatal(err)
	}
	r.jobIs(t, j.ID, "waiting")
	if n := r.runs(); n != 0 {
		t.Fatalf("%d containers ran before the secret was set", n)
	}
	r.row(t, r.staging.ID, "worker") // the rows are written
	parked, err := o.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	var pay struct {
		Owed  []string `json:"owed"`
		OrgID string   `json:"org_id"`
	}
	if err := json.Unmarshal([]byte(parked.Payload), &pay); err != nil {
		t.Fatal(err)
	}
	want := []string{r.row(t, r.staging.ID, "api").ID, r.row(t, r.staging.ID, "worker").ID}
	slices.Sort(pay.Owed)
	slices.Sort(want)
	if !slices.Equal(pay.Owed, want) || pay.OrgID == "" {
		t.Errorf("parked payload = %s, want owed %v and org_id", parked.Payload, want)
	}

	secret(r.staging.ID)
	if got := r.jobIs(t, j.ID, "done"); got.Error != "" {
		t.Errorf("job error = %q", got.Error)
	}
	if n := r.runs(); n != 2 {
		t.Errorf("%d containers ran, want api and worker once each", n)
	}
}
