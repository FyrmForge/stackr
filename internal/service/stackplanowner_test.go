package service_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// A stack plan that changes pin, ingress or exits is an org owner's to
// approve, as the Placement tab is: a push's auto promote and a member's
// promote are blocked and leave the exits alone; an owner's applies them.
// A plan without such a change stays a member's.
func TestStackPlanPlacingNeedsOwner(t *testing.T) {
	ctx := context.Background()
	g := servicetest.NewGit(t)
	env := servicetest.NewWith(t, []service.Option{g.Option()})
	org := env.Org(t, "acme")
	conn := env.Connector(t, org, "whsec")
	st, err := env.Orch.CreateStack(ctx, org, "shop", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Orch.SetConfigRepo(ctx, st.ID, conn, "acme/shop", "", ""); err != nil {
		t.Fatal(err)
	}
	push := func(file string) {
		t.Helper()
		sha := g.Commit(t, "acme/shop", "main", map[string]string{"stackr-compose.yml": file})
		body := []byte(`{"ref":"refs/heads/main","after":"` + sha + `",` +
			`"repository":{"clone_url":"https://github.com/acme/shop.git","default_branch":"main"},` +
			`"commits":[{"modified":["stackr-compose.yml"]}]}`)
		mac := hmac.New(sha256.New, []byte("whsec"))
		mac.Write(body)
		if err := env.Orch.Webhook(ctx, conn, hook("push", "sha256="+hex.EncodeToString(mac.Sum(nil))), body); err != nil {
			t.Fatal(err)
		}
	}
	as := func(role string) *service.Principal {
		return &service.Principal{Access: authz.User{ID: role, Active: true, Roles: map[string]string{org: role}}}
	}
	member, owner := as("member"), as("owner")
	exits := func() int {
		t.Helper()
		rs, err := env.Store.ExitLadders.List(ctx, "stack", st.ID)
		if err != nil {
			t.Fatal(err)
		}
		return len(rs)
	}
	ended := func(id string) service.Job {
		t.Helper()
		var j service.Job
		eventually(t, "job "+id+" ended", func() bool {
			if j, err = env.Store.Jobs.Get(ctx, id); err != nil {
				t.Fatal(err)
			}
			return j.State == "done" || j.State == "failed"
		})
		return j
	}
	promoteJobs := func() []service.Job {
		t.Helper()
		js, err := env.Store.Jobs.ListByState(ctx, "queued", "running", "done", "failed", "waiting")
		if err != nil {
			t.Fatal(err)
		}
		var out []service.Job
		for _, j := range js {
			if j.Kind == "promote" {
				out = append(out, j)
			}
		}
		return out
	}

	// the push's own promote into dev (auto) is blocked: nothing moved
	push("version: 1\nstack: shop\nladder:\n  - dev\nhead: main\nexits: [own]\n")
	eventually(t, "the push's promote", func() bool { return len(promoteJobs()) == 1 })
	if j := ended(promoteJobs()[0].ID); j.State != "failed" || !strings.Contains(j.Error, promote.NeedsOwner) {
		t.Errorf("auto promote = %s %q, want failed with %q", j.State, j.Error, promote.NeedsOwner)
	}
	if exits() != 0 {
		t.Fatal("the auto promote set the stack's exits")
	}
	rs, err := env.Orch.Releases(ctx, st.ID)
	if err != nil || len(rs) != 1 {
		t.Fatalf("releases = %v %v", rs, err)
	}
	es, err := env.Orch.Ladder(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	dev, rel := es[0].ID, rs[0].ID

	// a member: the dry run says why, the promote is refused in its job
	pp, err := env.Orch.PlanPromote(ctx, member, dev, rel)
	if err != nil {
		t.Fatal(err)
	}
	if pp.CanDeploy || len(pp.Plan.Blockers) != 1 || pp.Plan.Blockers[0] != promote.NeedsOwner {
		t.Errorf("member plan = %+v, want blocked on the owner", pp.Plan)
	}
	j, err := env.Orch.Promote(ctx, member, dev, rel)
	if err != nil {
		t.Fatal(err)
	}
	if j = ended(j.ID); j.State != "failed" || !strings.Contains(j.Error, promote.NeedsOwner) || exits() != 0 {
		t.Errorf("member promote = %s %q exits=%d, want refused", j.State, j.Error, exits())
	}

	// an owner: no blocker, the exits land
	if pp, err = env.Orch.PlanPromote(ctx, owner, dev, rel); err != nil || !pp.CanDeploy {
		t.Fatalf("owner plan = %+v %v, want deployable", pp.Plan, err)
	}
	if j, err = env.Orch.Promote(ctx, owner, dev, rel); err != nil {
		t.Fatal(err)
	}
	if j = ended(j.ID); j.State != "done" || exits() != 1 {
		t.Errorf("owner promote = %s %q exits=%d, want applied", j.State, j.Error, exits())
	}

	// a plan without a placing change is a member's again
	if pp, err = env.Orch.PlanPromote(ctx, member, dev, rel); err != nil || !pp.CanDeploy {
		t.Errorf("member plan with the exits in place = %+v %v, want deployable", pp.Plan, err)
	}
}
