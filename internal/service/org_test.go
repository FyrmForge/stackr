package service

import (
	"strconv"
	"time"

	"github.com/google/uuid"

	"context"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"slices"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

// The org rung (DECIDE 166): settings and env colours save, and each save
// queues a redeploy of the org's running tiles only; bad colours are
// refused before anything moves.
func TestOrgRungRedeploysRunningTiles(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	up := w.tile(t, "api", true)
	down := w.tile(t, "worker", false)

	og, err := w.orch.SetOrgSettings(ctx, w.org, `{"cpu_limit":1}`)
	must(t, err)
	if og.Settings != `{"cpu_limit":1}` {
		t.Errorf("settings = %s", og.Settings)
	}
	deploys := func() int {
		js, err := w.orch.TileJobs(ctx, []string{up.ID, down.ID}, 10)
		must(t, err)
		for _, j := range js {
			if j.Kind != string(kindDeploy) || !slices.Contains(j.LockSet, up.ID) {
				t.Fatalf("job %+v touches more than the running tile", j)
			}
		}
		return len(js)
	}
	if n := deploys(); n != 1 {
		t.Fatalf("deploys after settings = %d, want 1", n)
	}

	if _, err := w.orch.SetOrgEnvColors(ctx, w.org, "red"); err == nil {
		t.Fatal("colours that are not JSON saved")
	} else if v, ok := errs.IsInvalid(err); !ok || v.Field != "env_colors" {
		t.Errorf("bad colours = %v, want Invalid on env_colors", err)
	}
	og, err = w.orch.SetOrgEnvColors(ctx, w.org, `{"prod":"rose"}`)
	must(t, err)
	if og.EnvColors != `{"prod":"rose"}` {
		t.Errorf("env colours = %s", og.EnvColors)
	}
	if n := deploys(); n < 1 {
		t.Errorf("deploys after colours = %d", n)
	}
}

// CreateNamedOrg names and finishes in one go; a refused name leaves no
// draft behind, and the caller's wizard draft is never reused.
func TestCreateNamedOrg(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	u := store.User{
		ID: uuid.NewString(), Email: "n@x.io", PasswordHash: "x", Name: "n",
		Role: "user", Active: true, Theme: "system", CreatedAt: time.Now().UTC(),
	}
	must(t, w.st.Users.Create(ctx, u))

	before, err := w.orch.AllOrgs(ctx)
	must(t, err)
	if _, err := w.orch.CreateNamedOrg(ctx, u.ID, "!!!"); err == nil {
		t.Fatal("unusable name created an org")
	}
	if after, _ := w.orch.AllOrgs(ctx); len(after) != len(before) {
		t.Fatalf("refused name left %d orgs, had %d", len(after), len(before))
	}

	og, err := w.orch.CreateNamedOrg(ctx, u.ID, "Fresh Co")
	must(t, err)
	if og.Name != "Fresh Co" || og.Slug != "fresh-co" || og.SetupDoneAt == nil {
		t.Errorf("org = %+v, want named, slugged and finished", og)
	}
}

// Jobs is newest first and ?limit= keeps the newest.
func TestJobsNewestFirstLimited(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := range 3 {
		must(t, w.st.Jobs.Create(ctx, store.Job{
			ID: strconv.Itoa(i), Kind: "deploy", State: "done", Payload: "{}",
			CreatedAt: now.Add(time.Duration(i) * time.Minute),
		}))
	}
	js, err := w.orch.Jobs(ctx, 2)
	must(t, err)
	if len(js) != 2 || js[0].ID != "2" || js[1].ID != "1" {
		t.Errorf("jobs = %+v, want ids 2,1", js)
	}
}

// Members names each member by email.
func TestMembersCarryEmail(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	u := store.User{
		ID: uuid.NewString(), Email: "m@x.io", PasswordHash: "x", Name: "m",
		Role: "user", Active: true, Theme: "system", CreatedAt: time.Now().UTC(),
	}
	must(t, w.st.Users.Create(ctx, u))
	must(t, w.st.OrgMembers.Create(ctx, store.OrgMember{
		ID: uuid.NewString(), OrgID: w.org, UserID: u.ID, Role: "owner", CreatedAt: time.Now().UTC(),
	}))
	ms, err := w.orch.Members(ctx, w.org)
	if err != nil || len(ms) != 1 || ms[0].Email != "m@x.io" {
		t.Errorf("members = %+v, %v", ms, err)
	}
}
