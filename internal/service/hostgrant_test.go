package service

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/hostgrant"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// waiting polls until the job is parked.
func (w *world) waiting(t *testing.T, id string) Job {
	t.Helper()
	for range 500 {
		j, err := w.orch.GetJob(context.Background(), id)
		must(t, err)
		if j.State == job.Waiting || j.State == job.Done || j.State == job.Failed {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s never parked", id)
	return Job{}
}

// A deploy with a host line parks with the approval text, approving the
// stack's grant requeues it and it deploys, a redeploy does not park again,
// and a revoke parks the next deploy while approving with nothing waiting
// is refused.
func TestHostAccessParksApprovesRevokes(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	now := time.Now()
	must(t, w.st.Users.Create(ctx, store.User{ID: "adm", Email: "a@b.c", Name: "A", Role: "admin", Active: true, CreatedAt: now, UpdatedAt: now}))
	w.fake.RunID = "c1"
	w.fake.Details = map[string]docker.Detail{"c1": {Running: true, Health: "healthy", Networks: map[string]string{"n": "10.0.0.5"}}}
	tl := w.tile(t, "mon", false)
	_, _, err := w.orch.UpdateTile(ctx, tl.ID, func(t *Tile) error {
		t.Volumes = "host:/var/run/docker.sock:/var/run/docker.sock:ro"
		return nil
	})
	must(t, err)

	const ask = "elevated access: mon host:/var/run/docker.sock:/var/run/docker.sock:ro"
	j, err := w.orch.Deploy(ctx, tl.ID)
	must(t, err)
	if j = w.waiting(t, j.ID); j.State != job.Waiting || j.WaitingParam == nil || *j.WaitingParam != ask {
		t.Fatalf("deploy = %s %v %q, want parked on %q", j.State, j.WaitingParam, j.Error, ask)
	}
	if len(w.fake.Specs) != 0 {
		t.Fatal("a container started before approval")
	}
	g, err := w.orch.HostGrant(ctx, w.stack)
	must(t, err)
	if g.Granted || len(g.Pending) != 1 {
		t.Fatalf("grant before approval = %+v", g)
	}

	g, err = w.orch.ApproveHostGrant(ctx, w.stack, "adm", g.Pending, nil)
	must(t, err)
	if !g.Granted || g.ApprovedBy != "adm" || !slices.Equal(g.Lines, []string{"mon host:/var/run/docker.sock:/var/run/docker.sock:ro"}) || len(g.Pending) != 0 {
		t.Fatalf("grant after approval = %+v", g)
	}
	if j = w.wait(t, j.ID); j.State != job.Done {
		t.Fatalf("approved deploy = %s %q", j.State, j.Error)
	}

	// The same set again: no park.
	j, err = w.orch.Deploy(ctx, tl.ID)
	must(t, err)
	if j = w.wait(t, j.ID); j.State != job.Done {
		t.Fatalf("redeploy = %s %q", j.State, j.Error)
	}
	if _, err = w.orch.ApproveHostGrant(ctx, w.stack, "adm", g.Pending, nil); err == nil || !strings.Contains(err.Error(), "no job waits") {
		t.Fatalf("approve with nothing waiting = %v", err)
	}

	must(t, w.orch.RevokeHostGrant(ctx, w.stack, ""))
	j, err = w.orch.Deploy(ctx, tl.ID)
	must(t, err)
	if j = w.waiting(t, j.ID); j.State != job.Waiting {
		t.Fatalf("deploy after revoke = %s %q", j.State, j.Error)
	}
}

// A promote whose stack file holds a host line queues, parks on the approval
// text, and runs on approval: the same release, no new push.
func TestPromoteParksOnHostAccess(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	now := time.Now()
	must(t, w.st.Users.Create(ctx, store.User{ID: "adm", Email: "a@b.c", Name: "A", Role: "admin", Active: true, CreatedAt: now, UpdatedAt: now}))
	w.fake.RunID = "c1"
	w.fake.Details = map[string]docker.Detail{"c1": {Running: true, Health: "healthy", Networks: map[string]string{"n": "10.0.0.5"}}}
	w.fake.Digests = map[string]string{"nginx:1": "sha256:n1"}
	st, err := w.st.Stacks.Get(ctx, w.stack)
	must(t, err)
	st.ConfigRepo, st.ConfigBranch = "https://github.com/acme/shop", "main"
	must(t, w.st.Stacks.Update(ctx, st))
	w.orch.promote.Config = func(context.Context, store.Stack, string, io.Writer) ([]byte, promote.Fetcher, error) {
		return []byte("version: 1\nstack: shop\nladder: [dev]\nhead: main\nbase:\n  tiles:\n" +
			"    mon:\n      image: nginx:1\n      port: 80\n      volumes: [\"host:/var/run/docker.sock:/s:ro\"]\n"), nil, nil
	}
	rel, err := w.orch.releases.Create(ctx, w.stack, "test",
		[]release.Pin{{Slug: release.ConfigSlug, Repo: st.ConfigRepo, CommitSHA: "c1"}})
	must(t, err)

	pp, err := w.orch.PlanPromote(ctx, w.env, rel.ID)
	must(t, err)
	if !pp.CanDeploy || len(pp.Plan.Blockers) != 1 {
		t.Fatalf("plan = %+v, want queueable with the host access blocker", pp)
	}
	j, err := w.orch.Promote(ctx, w.env, rel.ID)
	must(t, err)
	const ask = "elevated access: mon host:/var/run/docker.sock:/s:ro"
	if j = w.waiting(t, j.ID); j.State != job.Waiting || j.WaitingParam == nil || *j.WaitingParam != ask {
		t.Fatalf("promote = %s %v %q, want parked on %q", j.State, j.WaitingParam, j.Error, ask)
	}
	pg, err := w.orch.HostGrant(ctx, w.stack)
	must(t, err)
	if _, err = w.orch.ApproveHostGrant(ctx, w.stack, "adm", pg.Pending, nil); err != nil {
		t.Fatal(err)
	}
	if j = w.wait(t, j.ID); j.State != job.Done {
		t.Fatalf("approved promote = %s %q", j.State, j.Error)
	}
	if _, err := w.orch.tiles.GetBySlug(ctx, w.env, "mon"); err != nil {
		t.Errorf("the tile was not made: %v", err)
	}
}

// Approve grants what the admin was shown: a second ask parked after the
// admin looked makes the approval a Conflict and nothing is granted; with
// both asks in the body it grants both.
func TestApproveHostGrantRefusesChangedAsk(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	now := time.Now()
	must(t, w.st.Users.Create(ctx, store.User{ID: "adm", Email: "a@b.c", Name: "A", Role: "admin", Active: true, CreatedAt: now, UpdatedAt: now}))
	w.fake.RunID = "c1"
	w.fake.Details = map[string]docker.Detail{"c1": {Running: true, Health: "healthy", Networks: map[string]string{"n": "10.0.0.5"}}}
	park := func(name, vol string) {
		tl := w.tile(t, name, false)
		_, _, err := w.orch.UpdateTile(ctx, tl.ID, func(t *Tile) error { t.Volumes = vol; return nil })
		must(t, err)
		j, err := w.orch.Deploy(ctx, tl.ID)
		must(t, err)
		if j = w.waiting(t, j.ID); j.State != job.Waiting {
			t.Fatalf("%s = %s %q", name, j.State, j.Error)
		}
	}
	park("a", "host:/srv/a:/a")
	g, err := w.orch.HostGrant(ctx, w.stack)
	must(t, err)
	shown := g.Pending
	park("b", "host:/:/h")

	_, err = w.orch.ApproveHostGrant(ctx, w.stack, "adm", shown, nil)
	if !isConflict(err) || !strings.Contains(err.Error(), "the ask changed; review it again") {
		t.Fatalf("approve of the old set = %v", err)
	}
	if g, _ = w.orch.HostGrant(ctx, w.stack); g.Granted || len(g.Pending) != 2 {
		t.Fatalf("a refused approval granted: %+v", g)
	}
	g, err = w.orch.ApproveHostGrant(ctx, w.stack, "adm", g.Pending, nil)
	must(t, err)
	if !slices.Equal(g.Lines, []string{"a host:/srv/a:/a", "b host:/:/h"}) {
		t.Fatalf("grant = %+v", g)
	}
}

func isConflict(err error) bool { _, ok := errs.IsConflict(err); return ok }

// A subset approve grants only those lines; the requeued job re-parks on the
// rest.
func TestApproveHostGrantSubset(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	now := time.Now()
	must(t, w.st.Users.Create(ctx, store.User{ID: "adm", Email: "a@b.c", Name: "A", Role: "admin", Active: true, CreatedAt: now, UpdatedAt: now}))
	w.fake.RunID = "c1"
	w.fake.Details = map[string]docker.Detail{"c1": {Running: true, Health: "healthy", Networks: map[string]string{"n": "10.0.0.5"}}}
	tl := w.tile(t, "mon", false)
	_, _, err := w.orch.UpdateTile(ctx, tl.ID, func(t *Tile) error {
		t.Volumes = "host:/srv/a:/a\nhost:/srv/b:/b"
		return nil
	})
	must(t, err)
	j, err := w.orch.Deploy(ctx, tl.ID)
	must(t, err)
	w.waiting(t, j.ID)
	g, err := w.orch.HostGrant(ctx, w.stack)
	must(t, err)
	if len(g.Pending) != 2 {
		t.Fatalf("pending = %v", g.Pending)
	}
	g, err = w.orch.ApproveHostGrant(ctx, w.stack, "adm", g.Pending, []string{"mon host:/srv/a:/a"})
	must(t, err)
	if !slices.Equal(g.Lines, []string{"mon host:/srv/a:/a"}) {
		t.Fatalf("grant = %+v", g)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if g, err = w.orch.HostGrant(ctx, w.stack); err == nil && slices.Equal(g.Pending, []string{"mon host:/srv/b:/b"}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not re-park on the remainder: %+v %v", g, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(w.fake.Specs) != 0 {
		t.Fatal("a container started on a partial grant")
	}
}

// Renaming a tile drops its grant lines (the new name asks again); deleting
// one drops them too and cancels its parked jobs.
func TestTileRenameAndDeleteDropGrant(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	now := time.Now()
	must(t, w.st.Users.Create(ctx, store.User{ID: "adm", Email: "a@b.c", Name: "A", Role: "admin", Active: true, CreatedAt: now, UpdatedAt: now}))
	w.fake.RunID = "c1"
	w.fake.Details = map[string]docker.Detail{"c1": {Running: true, Health: "healthy", Networks: map[string]string{"n": "10.0.0.5"}}}
	tl := w.tile(t, "mon", false)
	_, _, err := w.orch.UpdateTile(ctx, tl.ID, func(t *Tile) error { t.Devices = "/dev/dri"; return nil })
	must(t, err)
	j, err := w.orch.Deploy(ctx, tl.ID)
	must(t, err)
	w.waiting(t, j.ID)
	g, err := w.orch.HostGrant(ctx, w.stack)
	must(t, err)
	_, err = w.orch.ApproveHostGrant(ctx, w.stack, "adm", g.Pending, nil)
	must(t, err)
	w.wait(t, j.ID)

	tl, err = w.orch.RenameTile(ctx, tl.ID, "mon2")
	must(t, err)
	if g, _ = w.orch.HostGrant(ctx, w.stack); g.Granted {
		t.Fatalf("lines survived the rename: %v", g.Lines)
	}

	j, err = w.orch.Deploy(ctx, tl.ID) // parks on the new name
	must(t, err)
	if j = w.waiting(t, j.ID); j.State != job.Waiting {
		t.Fatalf("deploy after rename = %s", j.State)
	}
	d, err := w.orch.DeleteTile(ctx, tl.ID)
	must(t, err)
	w.wait(t, d.ID)
	if j, _ = w.orch.GetJob(ctx, j.ID); j.State != job.Cancelled {
		t.Fatalf("parked job = %s, want cancelled", j.State)
	}
	if gs, err := w.orch.ListHostGrants(ctx); err != nil || len(gs) != 0 {
		t.Fatalf("grants after delete = %v %v", gs, err)
	}
}

// A parked job whose stack is gone neither fails the admin list nor counts
// on the badge.
func TestHostGrantListSkipsGoneStack(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	_, err := w.orch.jobRows.Create(ctx, "deploy", nil, `{"host_access":{"stack":"gone","what":"elevated access: x device:/d"}}`, nil, t.TempDir())
	must(t, err)
	j, _ := w.orch.jobRows.List(ctx, job.Queued)
	j[0].State = job.Waiting
	must(t, w.st.Jobs.Update(ctx, j[0]))
	if gs, err := w.orch.ListHostGrants(ctx); err != nil || len(gs) != 0 {
		t.Fatalf("list = %v %v", gs, err)
	}
	if n, err := w.orch.HostGrantsWaiting(ctx); err != nil || n != 0 {
		t.Fatalf("badge = %d %v", n, err)
	}
}

// parkedRaw writes a waiting job row directly (no runner race).
func (w *world) parkedRaw(t *testing.T, kind, payload string, lock ...string) Job {
	t.Helper()
	j := store.Job{
		ID: kind + "-" + strings.Join(lock, "-"), Kind: kind, State: job.Waiting, LockSet: lock,
		Payload: payload, LogPath: t.TempDir() + "/x.log", CreatedAt: time.Now().UTC(),
	}
	must(t, w.st.Jobs.Create(context.Background(), j))
	return j
}

func (w *world) jobState(t *testing.T, id string) string {
	t.Helper()
	j, err := w.orch.GetJob(context.Background(), id)
	must(t, err)
	return j.State
}

// Deleting a tile cancels its own parked jobs, not the parked promote of the
// env it sat in.
func TestDeleteTileKeepsEnvWidePromote(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	a := w.tile(t, "a", false)
	own := w.parkedRaw(t, "deploy", `{"tile_id":"`+a.ID+`"}`, a.ID)
	env := w.parkedRaw(t, "promote", `{"env_id":"`+w.env+`"}`, "env:"+w.env, a.ID)
	d, err := w.orch.DeleteTile(ctx, a.ID)
	must(t, err)
	w.wait(t, d.ID)
	if s := w.jobState(t, own.ID); s != job.Cancelled {
		t.Errorf("tile job = %s, want cancelled", s)
	}
	if s := w.jobState(t, env.ID); s != job.Waiting {
		t.Errorf("env promote = %s, want waiting", s)
	}
}

// Closing a PR cancels the jobs parked on its env.
func TestPRClosedCancelsParkedJobs(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	must(t, w.st.Environments.Create(ctx, store.Environment{
		ID: "pr7", StackID: w.stack, Name: "pr-7", Slug: "pr-7", Type: "ephemeral", Settings: "{}",
		Network: "n7", FromKind: "branch", FromBranch: "feat", CreatedAt: time.Now(),
	}))
	j := w.parkedRaw(t, "promote", `{"env_id":"pr7"}`, "env:pr7")
	must(t, w.orch.runPR(ctx, &jobs.Run{Log: io.Discard}, prJob{StackID: w.stack, Action: "closed", Number: 7}))
	if s := w.jobState(t, j.ID); s != job.Cancelled {
		t.Fatalf("parked job = %s, want cancelled", s)
	}
}

// A tile asking for host access is not deployed in a PR env, and does not
// park for an admin.
func TestPREnvTileWithHostAccessNotDeployed(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	must(t, w.st.Environments.Create(ctx, store.Environment{
		ID: "pr7", StackID: w.stack, Name: "pr-7", Slug: "pr-7", Type: "ephemeral", Settings: "{}",
		Network: "n7", FromKind: "branch", FromBranch: "feat", CreatedAt: time.Now(),
	}))
	tl, err := w.orch.CreateTile(ctx, Tile{StackID: w.stack, EnvironmentID: "pr7", Name: "mon", Kind: tile.Image, ImageRef: "nginx:1", ContainerPort: 80, Devices: "/dev/dri"})
	must(t, err)
	j, err := w.orch.Deploy(ctx, tl.ID)
	must(t, err)
	if j = w.wait(t, j.ID); j.State != job.Failed || !strings.Contains(j.Error, "PR envs never get it") {
		t.Fatalf("deploy = %s %q, want failed, not parked", j.State, j.Error)
	}
}

// A promote that removes a tile drops its grant lines.
func TestPromoteRemovingTileDropsGrant(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	now := time.Now()
	must(t, w.st.Users.Create(ctx, store.User{ID: "adm", Email: "a@b.c", Name: "A", Role: "admin", Active: true, CreatedAt: now, UpdatedAt: now}))
	w.fake.RunID = "c1"
	w.fake.Details = map[string]docker.Detail{"c1": {Running: true, Health: "healthy", Networks: map[string]string{"n": "10.0.0.5"}}}
	w.fake.Digests = map[string]string{"nginx:1": "sha256:n1"}
	st, err := w.st.Stacks.Get(ctx, w.stack)
	must(t, err)
	st.ConfigRepo, st.ConfigBranch = "https://github.com/acme/shop", "main"
	must(t, w.st.Stacks.Update(ctx, st))
	w.orch.promote.Config = func(_ context.Context, _ store.Stack, sha string, _ io.Writer) ([]byte, promote.Fetcher, error) {
		tiles := "    web:\n      image: nginx:1\n      port: 80\n"
		if sha == "c1" {
			tiles += "    mon:\n      image: nginx:1\n      port: 80\n      volumes: [\"host:/var/run/docker.sock:/s:ro\"]\n"
		}
		return []byte("version: 1\nstack: shop\nladder: [dev]\nhead: main\nbase:\n  tiles:\n" + tiles), nil, nil
	}
	rel := func(sha string) string {
		r, err := w.orch.releases.Create(ctx, w.stack, "test", []release.Pin{{Slug: release.ConfigSlug, Repo: st.ConfigRepo, CommitSHA: sha}})
		must(t, err)
		return r.ID
	}
	j, err := w.orch.Promote(ctx, w.env, rel("c1"))
	must(t, err)
	w.waiting(t, j.ID)
	pg, err := w.orch.HostGrant(ctx, w.stack)
	must(t, err)
	_, err = w.orch.ApproveHostGrant(ctx, w.stack, "adm", pg.Pending, nil)
	must(t, err)
	if j = w.wait(t, j.ID); j.State != job.Done {
		t.Fatalf("first promote = %s %q", j.State, j.Error)
	}
	j, err = w.orch.Promote(ctx, w.env, rel("c2"))
	must(t, err)
	if j = w.wait(t, j.ID); j.State != job.Done {
		t.Fatalf("second promote = %s %q", j.State, j.Error)
	}
	if gs, err := w.orch.ListHostGrants(ctx); err != nil || len(gs) != 0 {
		t.Fatalf("grants after the tile left = %v %v", gs, err)
	}
}

// failRead is a grant store whose reads fail.
type failRead struct{ store.HostGrantStore }

func (failRead) GetByStack(context.Context, string) (store.HostGrant, error) {
	return store.HostGrant{}, errors.New("read failed")
}

// dropRig is a stack with tile "mon" in dev (w.env) and in a second static
// env, the grant holding the given lines.
func dropRig(t *testing.T, devices string, lines ...string) (*world, Tile, store.HostGrant) {
	t.Helper()
	w := newWorld(t)
	ctx := context.Background()
	must(t, w.st.Environments.Create(ctx, store.Environment{
		ID: "prod", StackID: w.stack, Name: "prod", Slug: "prod", Type: "static", Settings: "{}",
		Network: "np", FromKind: "branch", FromBranch: "main", CreatedAt: time.Now(),
	}))
	dev := w.tile(t, "mon", false)
	other := dev
	other.ID, other.EnvironmentID = "mon-prod", "prod"
	other.Devices = devices
	must(t, w.st.Tiles.Create(ctx, other))
	now := time.Now()
	must(t, w.st.Users.Create(ctx, store.User{ID: "adm", Email: "a@b.c", Name: "A", Role: "admin", Active: true, CreatedAt: now, UpdatedAt: now}))
	row := store.HostGrant{
		ID: "g1", StackID: w.stack, Lines: strings.Join(lines, "\n"), ApprovedBy: "adm",
		CreatedAt: time.Now().Add(-time.Hour).UTC().Truncate(time.Second),
	}
	must(t, w.st.HostGrants.Create(ctx, row))
	return w, dev, row
}

func TestDropTileGrantKeepsLinesSurvivorAsks(t *testing.T) {
	w, dev, row := dropRig(t, "/dev/dri", "mon device:/dev/dri")
	must(t, w.orch.dropTileGrant(context.Background(), dev))
	w.orch.vipAsync.Wait()
	got, err := w.st.HostGrants.GetByStack(context.Background(), w.stack)
	must(t, err)
	if got != row {
		t.Fatalf("row rewritten: %+v, was %+v", got, row)
	}
}

func TestDropTileGrantCutsOnlyExtraLines(t *testing.T) {
	w, dev, row := dropRig(t, "/dev/dri", "mon device:/dev/dri", "mon device:/dev/kvm", "other lan:1.2.3.4:5")
	must(t, w.orch.dropTileGrant(context.Background(), dev))
	w.orch.vipAsync.Wait()
	got, err := w.st.HostGrants.GetByStack(context.Background(), w.stack)
	must(t, err)
	if got.ID != row.ID || got.ApprovedBy != "adm" || !got.CreatedAt.Equal(row.CreatedAt) ||
		got.Lines != "mon device:/dev/dri\nother lan:1.2.3.4:5" {
		t.Fatalf("row = %+v", got)
	}
}

func TestDropTileGrantReadErrorAborts(t *testing.T) {
	w, dev, row := dropRig(t, "/dev/dri", "mon device:/dev/kvm")
	w.orch.hostgrant = hostgrant.New(failRead{w.st.HostGrants})
	if err := w.orch.dropTileGrant(context.Background(), dev); err == nil {
		t.Fatal("read error swallowed")
	}
	got, err := w.st.HostGrants.GetByStack(context.Background(), w.stack)
	must(t, err)
	if got != row {
		t.Fatalf("grant touched: %+v", got)
	}
}
