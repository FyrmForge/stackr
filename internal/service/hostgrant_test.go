package service

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/release"
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

	const ask = "host access: host:/var/run/docker.sock:/var/run/docker.sock:ro"
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

	g, err = w.orch.ApproveHostGrant(ctx, w.stack, "adm", g.Pending)
	must(t, err)
	if !g.Granted || g.ApprovedBy != "adm" || !slices.Equal(g.Lines, []string{"host:/var/run/docker.sock:/var/run/docker.sock:ro"}) || len(g.Pending) != 0 {
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
	if _, err = w.orch.ApproveHostGrant(ctx, w.stack, "adm", g.Pending); err == nil || !strings.Contains(err.Error(), "no job waits") {
		t.Fatalf("approve with nothing waiting = %v", err)
	}

	must(t, w.orch.RevokeHostGrant(ctx, w.stack))
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
	const ask = "host access: host:/var/run/docker.sock:/s:ro"
	if j = w.waiting(t, j.ID); j.State != job.Waiting || j.WaitingParam == nil || *j.WaitingParam != ask {
		t.Fatalf("promote = %s %v %q, want parked on %q", j.State, j.WaitingParam, j.Error, ask)
	}
	pg, err := w.orch.HostGrant(ctx, w.stack)
	must(t, err)
	if _, err = w.orch.ApproveHostGrant(ctx, w.stack, "adm", pg.Pending); err != nil {
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

	_, err = w.orch.ApproveHostGrant(ctx, w.stack, "adm", shown)
	if !isConflict(err) || !strings.Contains(err.Error(), "the ask changed; review it again") {
		t.Fatalf("approve of the old set = %v", err)
	}
	if g, _ = w.orch.HostGrant(ctx, w.stack); g.Granted || len(g.Pending) != 2 {
		t.Fatalf("a refused approval granted: %+v", g)
	}
	g, err = w.orch.ApproveHostGrant(ctx, w.stack, "adm", g.Pending)
	must(t, err)
	if !slices.Equal(g.Lines, []string{"host:/:/h", "host:/srv/a:/a"}) {
		t.Fatalf("grant = %+v", g)
	}
}

func isConflict(err error) bool { _, ok := errs.IsConflict(err); return ok }
