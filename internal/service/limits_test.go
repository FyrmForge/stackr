package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// limitRig is a one-CPU, 1 GiB host with a stack, env and tile in an org.
type limitRig struct {
	env  *servicetest.Env
	org  string
	tile servicetest.Tile
}

func newLimitRig(t *testing.T) limitRig {
	t.Helper()
	env := servicetest.New(t)
	env.Docker.Host = docker.HostInfo{CPUs: 1, MemBytes: 1 << 30}
	org := env.Org(t, "acme")
	return limitRig{env: env, org: org, tile: env.Tile(t, org)}
}

func invalidWith(err error, want string) bool {
	_, ok := errs.IsInvalid(err)
	return ok && strings.Contains(err.Error(), want)
}

// A cpu or memory limit above what the host has is refused at every rung,
// naming the host's limit (QA bug 1: 1.5 CPUs on a 1 CPU box took 7 tiles
// down).
func TestLimitsAboveHostRefused(t *testing.T) {
	ctx := context.Background()
	r := newLimitRig(t)
	o := r.env.Orch

	err := o.SetSettingDefaults(ctx, map[string]string{"cpu_limit": "1.5"})
	if !invalidWith(err, "1 CPU") {
		t.Errorf("server cpu_limit 1.5 = %v, want Invalid naming the host's 1 CPU", err)
	}
	err = o.SetSettingDefaults(ctx, map[string]string{"mem_limit_mb": "2048"})
	if !invalidWith(err, "1024 MB") {
		t.Errorf("server mem_limit_mb 2048 = %v, want Invalid naming 1024 MB", err)
	}
	if d, _ := o.SettingDefaults(ctx); d.CPULimit != nil || d.MemLimitMB != nil {
		t.Errorf("server rung = %+v after the refusals", d)
	}
	if _, err := o.SetOrgSettings(ctx, r.org, `{"cpu_limit":2}`); !invalidWith(err, "1 CPU") {
		t.Errorf("org cpu_limit 2 = %v", err)
	}
	if _, err := o.SetStackSettings(ctx, r.tile.Stack, `{"mem_limit_mb":4096}`); !invalidWith(err, "1024 MB") {
		t.Errorf("stack mem_limit_mb 4096 = %v", err)
	}
	if _, err := o.SetEnvSettings(ctx, r.tile.Env, `{"cpu_limit":1.01}`); !invalidWith(err, "1 CPU") {
		t.Errorf("env cpu_limit 1.01 = %v", err)
	}
	_, _, err = o.UpdateTile(ctx, r.tile.ID, func(t *service.Tile) error { t.CPULimit = 3; return nil })
	if !invalidWith(err, "1 CPU") {
		t.Errorf("tile cpu_limit 3 = %v", err)
	}
	_, err = o.CreateTile(ctx, service.Tile{
		StackID: r.tile.Stack, EnvironmentID: r.tile.Env, Name: "big", Kind: "image",
		ImageRef: "nginx:1", ContainerPort: 80, MemLimitMB: 2048,
	})
	if !invalidWith(err, "1024 MB") {
		t.Errorf("new tile mem_limit_mb 2048 = %v", err)
	}

	// at the host's own size, and unset, they are fine
	if err := o.SetSettingDefaults(ctx, map[string]string{"cpu_limit": "1", "mem_limit_mb": "1024"}); err != nil {
		t.Errorf("limits at the host's size = %v", err)
	}
	if _, err := o.SetOrgSettings(ctx, r.org, `{}`); err != nil {
		t.Errorf("org settings with no limits = %v", err)
	}
}

// An edit that leaves an already-too-big tile limit alone is not refused for
// it: only a changed limit is checked.
func TestLimitsUntouchedTileEditPasses(t *testing.T) {
	ctx := context.Background()
	r := newLimitRig(t)
	r.env.Docker.Host = docker.HostInfo{} // host unknown while the tile is set up
	_, _, err := r.env.Orch.UpdateTile(ctx, r.tile.ID, func(t *service.Tile) error { t.CPULimit = 8; return nil })
	if err != nil {
		t.Fatal(err)
	}
	r.env.Docker.Host = docker.HostInfo{CPUs: 1, MemBytes: 1 << 30}
	_, _, err = r.env.Orch.UpdateTile(ctx, r.tile.ID, func(t *service.Tile) error { t.Name = "api2"; return nil })
	if err != nil {
		t.Errorf("rename of a tile whose limit is over the host's = %v", err)
	}
}

// The server and org files' plans block on a limit above the host's.
func TestLimitsBlockFilePlans(t *testing.T) {
	ctx := context.Background()
	g := servicetest.NewGit(t)
	env := servicetest.NewWith(t, []service.Option{g.Option()})
	env.Docker.Host = docker.HostInfo{CPUs: 1, MemBytes: 1 << 30}
	env.ServerConnector(t, "gh", "whsec")

	pl, err := env.Orch.PlanServerFile(ctx, []byte("version: 1\ndefaults:\n  cpu_limit: 1.5\n"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = env.Orch.ApproveServerPlan(ctx, pl.ID, service.ApproveOpts{Confirm: true})
	if !isConflict(err) || !strings.Contains(err.Error(), "1 CPU") {
		t.Errorf("approve of a server file with cpu_limit 1.5 = %v, want a Conflict naming the host's CPUs", err)
	}

	r := orgRig{env: env, g: g, org: env.Org(t, "acme")}
	r.conn = env.Connector(t, r.org, "whsec")
	r.orgFile(t, "version: 1\norg: acme\ndefaults:\n  mem_limit_mb: 4096\n")
	r.bind(t, false)
	_, err = env.Orch.ApproveOrgPlan(ctx, r.plans(t)[0].ID, service.ApproveOpts{Confirm: true})
	if !isConflict(err) || !strings.Contains(err.Error(), "1024 MB") {
		t.Errorf("approve of an org file with mem_limit_mb 4096 = %v, want a Conflict naming the host's memory", err)
	}
}

// seedDeploy records a finished deploy job of the tile.
func seedDeploy(t *testing.T, env *servicetest.Env, tileID, state string, at time.Time) {
	t.Helper()
	must(t, env.Store.Jobs.Create(context.Background(), store.Job{
		ID: uuid.NewString(), Kind: "deploy", State: state, LockSet: store.StringList{tileID},
		Payload: "{}", CreatedAt: at, FinishedAt: &at,
	}))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// A tile a failed deploy left with no container counts for a cascade change
// and is redeployed by it (QA bug 1: the follow-up plan skipped the three
// dead tiles). A tile that never deployed, or whose last deploy worked, does
// not.
func TestFailedDeployTileStillRedeploys(t *testing.T) {
	ctx := context.Background()
	r := newLimitRig(t)
	o := r.env.Orch
	now := time.Now()

	// never deployed, no job: left alone
	if _, err := o.SetStackSettings(ctx, r.tile.Stack, `{"cpu_limit":0.5}`); err != nil {
		t.Fatal(err)
	}
	if js, _ := o.TileJobs(ctx, []string{r.tile.ID}, 10); len(js) != 0 {
		t.Fatalf("a never-deployed tile got jobs: %+v", js)
	}

	// deployed once, its last deploy failed and nothing runs: counted
	seedDeploy(t, r.env, r.tile.ID, "done", now.Add(-time.Hour))
	seedDeploy(t, r.env, r.tile.ID, "failed", now.Add(-time.Minute))
	file := "version: 1\ndefaults:\n  cpu_limit: 0.5\n"
	pl, err := o.PlanServerFile(ctx, []byte(file))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pl.Plan, "redeploys 1 tile") {
		t.Errorf("plan = %s, want it to count the dead tile", pl.Plan)
	}
	if _, err := o.SetStackSettings(ctx, r.tile.Stack, `{"cpu_limit":0.25}`); err != nil {
		t.Fatal(err)
	}
	js, err := o.TileJobs(ctx, []string{r.tile.ID}, 10)
	if err != nil {
		t.Fatal(err)
	}
	queued := 0
	for _, j := range js {
		if j.Kind == "deploy" && (j.State == "queued" || j.State == "running" || j.State == "done" || j.State == "failed") && j.CreatedAt.After(now) {
			queued++
		}
	}
	if queued != 1 {
		t.Errorf("jobs = %+v, want one new deploy of the dead tile", js)
	}
}

// A deploy that failed first, with nothing to its name before it, is not
// retried by every cascade change.
func TestFirstDeployFailureNotRetried(t *testing.T) {
	ctx := context.Background()
	r := newLimitRig(t)
	seedDeploy(t, r.env, r.tile.ID, "failed", time.Now().Add(-time.Minute))
	pl, err := r.env.Orch.PlanServerFile(ctx, []byte("version: 1\ndefaults:\n  cpu_limit: 0.5\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(pl.Plan, "redeploys") {
		t.Errorf("plan = %s, want no redeploy counted", pl.Plan)
	}
}

// The server apply reports the redeploys it queued: one that fails fails the
// apply job and the plan (QA bug 1: it read applied over 7 failed deploys).
func TestServerApplyFailsWithItsRedeploys(t *testing.T) {
	ctx := context.Background()
	r := newLimitRig(t)
	o := r.env.Orch
	r.env.Docker.Containers = append(r.env.Docker.Containers, docker.Container{
		ID: "c-api", Name: "c-api", State: "running",
		Labels: map[string]string{tile.LabelTile: r.tile.ID, tile.LabelRole: "replica"},
	})
	r.env.Docker.Err = map[string]error{"Pull": errors.New("registry down")}

	pl, err := o.PlanServerFile(ctx, []byte("version: 1\ndefaults:\n  cpu_limit: 0.5\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.ApproveServerPlan(ctx, pl.ID, service.ApproveOpts{Confirm: true}); err != nil {
		t.Fatal(err)
	}
	var last service.ServerPlan
	eventually(t, "the plan ended", func() bool {
		last, _ = o.ServerPlan(ctx, pl.ID)
		return last.Status == "applied" || last.Status == "error"
	})
	if last.Status != "error" || !strings.Contains(last.Error, "1 redeploy failed") ||
		!strings.Contains(last.Error, "dev/api") {
		t.Errorf("plan = %s %q, want error naming the failed redeploy", last.Status, last.Error)
	}
}

// The org apply does the same for the redeploys its defaults queued.
func TestOrgApplyFailsWithItsRedeploys(t *testing.T) {
	ctx := context.Background()
	r := newOrgRig(t)
	tl := r.env.Tile(t, r.org)
	r.env.Docker.Containers = append(r.env.Docker.Containers, docker.Container{
		ID: "c-api", Name: "c-api", State: "running",
		Labels: map[string]string{tile.LabelTile: tl.ID, tile.LabelRole: "replica"},
	})
	r.env.Docker.Err = map[string]error{"Pull": errors.New("registry down")}
	r.orgFile(t, "version: 1\norg: acme\ndefaults:\n  cpu_limit: 0.5\n")
	r.bind(t, false)
	pl := r.plans(t)[0]
	if _, err := r.env.Orch.ApproveOrgPlan(ctx, pl.ID, service.ApproveOpts{Confirm: true}); err != nil {
		t.Fatal(err)
	}
	var last service.OrgPlan
	eventually(t, "the plan ended", func() bool {
		last, _ = r.env.Orch.OrgPlan(ctx, pl.ID)
		return last.Status == "applied" || last.Status == "error"
	})
	if last.Status != "error" || !strings.Contains(last.Error, "1 redeploy failed") {
		t.Errorf("plan = %s %q, want error naming the failed redeploy", last.Status, last.Error)
	}
}
