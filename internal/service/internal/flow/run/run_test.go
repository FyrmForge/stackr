package run_test

import (
	"context"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/run"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/credential"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domain"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/image"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/org"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/params"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	lrun "github.com/FyrmForge/stackr/internal/service/internal/leaf/run"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/settings"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/stack"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

var ctx = context.Background()

type vipStub struct{}

func (vipStub) Set(context.Context, string, []string) error { return nil }
func (vipStub) Remove(context.Context, string) error        { return nil }

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// setup: one env and a cron tile "sweep" running alpine:3.
func setup(t *testing.T) (*run.Flow, *dockerfake.Fake, store.Tile) {
	st := servicetest.Store(t)
	fake := dockerfake.New()
	fake.RunID = "c1"
	tiles := tile.New(st.Tiles, fake, vipStub{})
	envs := environment.New(st.Environments, fake)
	d := &deploy.Flow{
		Tiles: tiles, Envs: envs, Stacks: stack.New(st.Stacks),
		Orgs: org.New(st.Orgs, st.OrgMembers, st.Invites), Volumes: volume.New(st.Volumes, fake),
		Images: image.New(st.Images, fake), Releases: release.New(st.Releases, st.ReleaseTiles),
		Params: params.New(st.Params), Managed: managed.New(st.ManagedInstances, st.Provisions),
		Domains: domain.New(st.Domains, fake, "proxy"), Creds: credential.New(st.Credentials),
		Settings: settings.New(st.Settings, nil), Jobs: job.New(st.Jobs),
	}
	o, s, e := uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now()
	must(t, st.Orgs.Create(ctx, store.Org{ID: o, Name: "acme", Slug: "acme", EnvColors: "{}", Settings: "{}", CreatedAt: now}))
	must(t, st.Stacks.Create(ctx, store.Stack{ID: s, OrgID: o, Name: "shop", Slug: "shop", Settings: "{}", Domains: "[]", CreatedAt: now}))
	must(t, st.Environments.Create(ctx, store.Environment{ID: e, StackID: s, Name: "dev", Slug: "dev", Type: "static",
		Settings: "{}", Network: "n", FromKind: "branch", FromBranch: "main", CreatedAt: now}))
	sweep, err := tiles.Create(ctx, store.Tile{StackID: s, EnvironmentID: e, Name: "sweep", Kind: tile.Cron,
		ImageRef: "alpine:3", Schedule: "*/5 * * * *", Command: "sh -c 'echo hi'"})
	must(t, err)
	f := &run.Flow{Tiles: tiles, Envs: envs, Runs: lrun.New(st.Runs, t.TempDir()), Jobs: d.Jobs, Deploy: d, Minute: 20 * time.Millisecond}
	return f, fake, sweep
}

func queue(t *testing.T, f *run.Flow, tileID string) store.Run {
	t.Helper()
	r, ok, err := f.Queue(ctx, tileID, lrun.Manual)
	if err != nil || !ok || r.Status != lrun.Queued {
		t.Fatalf("queue = %+v %v %v", r, ok, err)
	}
	return r
}

func TestRunOK(t *testing.T) {
	f, fake, sweep := setup(t)
	fake.StreamOut = []string{"O hi", "E warn"}
	r := queue(t, f, sweep.ID)
	must(t, f.Do(ctx, r.ID, io.Discard))
	r, _ = f.Runs.Get(ctx, sweep.ID, r.ID)
	if r.Status != lrun.OK || r.ExitCode == nil || *r.ExitCode != 0 || r.StartedAt == nil || r.FinishedAt == nil {
		t.Fatalf("run = %+v", r)
	}
	if b, _ := os.ReadFile(f.Runs.LogPath(r)); string(b) != "O hi\nE warn\n" {
		t.Errorf("log = %q", b)
	}
	s := fake.Specs[0]
	if s.Image != "alpine:3" || s.Restart != "no" || s.Labels[tile.LabelRole] != "run" || s.Labels[tile.LabelRun] != r.ID ||
		!slices.Equal(s.Cmd, []string{"sh", "-c", "echo hi"}) || len(s.Networks) == 0 {
		t.Errorf("spec = %+v", s)
	}
	if !called(fake, "StopRemove") {
		t.Error("the run container was not removed")
	}
}

func TestRunNonZero(t *testing.T) {
	f, fake, sweep := setup(t)
	fake.ExitCode = 2
	r := queue(t, f, sweep.ID)
	if err := f.Do(ctx, r.ID, io.Discard); err == nil || !strings.Contains(err.Error(), "exited with code 2") {
		t.Errorf("do = %v", err)
	}
	r, _ = f.Runs.Get(ctx, sweep.ID, r.ID)
	if r.Status != lrun.Failed || r.ExitCode == nil || *r.ExitCode != 2 {
		t.Errorf("run = %+v", r)
	}
}

func TestRunTimeout(t *testing.T) {
	f, fake, sweep := setup(t)
	fake.WaitBlock = true
	r := queue(t, f, sweep.ID) // timeout_minutes defaulted to 30 → 600ms here
	if err := f.Do(ctx, r.ID, io.Discard); err == nil || !strings.Contains(err.Error(), "timed out after 30 minutes") {
		t.Errorf("do = %v", err)
	}
	r, _ = f.Runs.Get(ctx, sweep.ID, r.ID)
	if r.Status != lrun.Failed || r.ExitCode != nil || !called(fake, "StopRemove") {
		t.Errorf("run = %+v", r)
	}
}

func TestRunCancelled(t *testing.T) {
	f, fake, sweep := setup(t)
	fake.WaitBlock = true
	r := queue(t, f, sweep.ID)
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_ = f.Do(cctx, r.ID, io.Discard)
	if r, _ = f.Runs.Get(ctx, sweep.ID, r.ID); r.Status != lrun.Cancelled {
		t.Errorf("run = %+v", r)
	}
}

// A second run while the first is going is written cancelled, no job.
func TestOverlap(t *testing.T) {
	f, _, sweep := setup(t)
	first := queue(t, f, sweep.ID)
	r, ok, err := f.Queue(ctx, sweep.ID, lrun.Schedule)
	if err != nil || ok || r.Status != lrun.Cancelled || r.Reason != run.Overlap {
		t.Fatalf("second = %+v %v %v", r, ok, err)
	}
	if a, _, _ := f.Runs.Active(ctx, sweep.ID); a.ID != first.ID {
		t.Errorf("active = %s, want the first", a.ID)
	}
}

// A queued row whose job was cancelled does not block the next run.
func TestLostJobUnblocks(t *testing.T) {
	f, _, sweep := setup(t)
	first := queue(t, f, sweep.ID)
	j, err := f.Jobs.Create(ctx, "run", []string{sweep.ID}, "{}", nil, t.TempDir())
	must(t, err)
	_, err = f.Runs.SetJob(ctx, first.ID, j.ID)
	must(t, err)
	must(t, f.Jobs.Finish(ctx, j, job.Cancelled, ""))
	queue(t, f, sweep.ID)
	if r, _ := f.Runs.Get(ctx, sweep.ID, first.ID); r.Status != lrun.Cancelled {
		t.Errorf("first = %+v", r)
	}
}

func TestQueueRefusesAService(t *testing.T) {
	f, _, sweep := setup(t)
	api, err := f.Tiles.Create(ctx, store.Tile{StackID: sweep.StackID, EnvironmentID: sweep.EnvironmentID, Name: "api",
		Kind: tile.Image, ImageRef: "nginx:1"})
	must(t, err)
	if _, _, err := f.Queue(ctx, api.ID, lrun.Manual); err == nil || !strings.Contains(err.Error(), "run applies to cron and function tiles") {
		t.Errorf("queue a service = %v", err)
	}
}

func called(f *dockerfake.Fake, method string) bool {
	return slices.ContainsFunc(f.Calls(), func(c dockerfake.Call) bool { return c.Method == method })
}
