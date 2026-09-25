package service_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// orgRig is an org with a connected connector over the git fake.
type orgRig struct {
	env  *servicetest.Env
	g    *servicetest.Git
	org  string
	conn string
}

func newOrgRig(t *testing.T) orgRig {
	t.Helper()
	g := servicetest.NewGit(t)
	env := servicetest.NewWith(t, []service.Option{g.Option()})
	org := env.Org(t, "acme")
	return orgRig{
		env:  env,
		g:    g,
		org:  org,
		conn: env.Connector(t, org, "whsec"),
	}
}

// orgFile commits stackr-org.yml to acme/org.
func (r orgRig) orgFile(t *testing.T, body string) string {
	t.Helper()
	return r.g.Commit(t, "acme/org", "main", map[string]string{"stackr-org.yml": body})
}

// bind binds the org to acme/org, which plans at once.
func (r orgRig) bind(t *testing.T, auto bool) {
	t.Helper()
	if _, err := r.env.Orch.SetOrgConfigRepo(context.Background(), r.org, r.conn, "acme/org", "", "", auto); err != nil {
		t.Fatal(err)
	}
}

func (r orgRig) plans(t *testing.T) []service.OrgPlan {
	t.Helper()
	ps, err := r.env.Orch.OrgPlans(context.Background(), r.org, 50)
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

// push delivers a signed GitHub push of sha on main of repo.
func (r orgRig) push(t *testing.T, repo, sha string) {
	t.Helper()
	body := []byte(`{"ref":"refs/heads/main","after":"` + sha + `",` +
		`"repository":{"clone_url":"https://github.com/` + repo + `.git","default_branch":"main"},` +
		`"commits":[{"modified":["stackr-org.yml"]}]}`)
	mac := hmac.New(sha256.New, []byte("whsec"))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if err := r.env.Orch.Webhook(context.Background(), r.conn, "push", sig, body); err != nil {
		t.Fatal(err)
	}
}

func (r orgRig) applied(t *testing.T, id string) {
	t.Helper()
	var last service.OrgPlan
	eventually(t, "plan "+id+" applied", func() bool {
		p, err := r.env.Orch.OrgPlan(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		last = p
		return p.Status == "applied" || p.Status == "error"
	})
	if last.Status != "applied" {
		t.Fatalf("plan %s = %s: %s", id, last.Status, last.Error)
	}
}

// A plan from the branch head stores a pending row at that commit and
// supersedes the one before; a file that does not parse stores an error
// row; a preview stores nothing.
func TestOrgPlanStoresRows(t *testing.T) {
	ctx := context.Background()
	r := newOrgRig(t)
	if _, err := r.env.Orch.PreviewOrgConfig(ctx, r.org, []byte("version: 1\norg: acme\n")); !isConflict(err) {
		t.Errorf("preview of an unbound org = %v, want Conflict", err)
	}

	first := r.orgFile(t, "version: 1\norg: Acme Inc\n")
	r.bind(t, false)
	ps := r.plans(t)
	if len(ps) != 1 || ps[0].Status != "pending" || ps[0].Commit != first {
		t.Fatalf("after bind plans = %+v, want one pending at %s", ps, first)
	}

	second := r.orgFile(t, "version: 1\norg: Acme Two\n")
	p, err := r.env.Orch.PlanOrgConfig(ctx, r.org)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != "pending" || p.Commit != second || !strings.Contains(p.Plan, "Acme Two") {
		t.Errorf("plan = %+v, want pending at %s renaming to Acme Two", p, second)
	}
	ps = r.plans(t)
	if len(ps) != 2 || ps[1].Status != "superseded" {
		t.Errorf("plans = %+v, want the first superseded", ps)
	}

	r.orgFile(t, "version: 2\norg: acme\n")
	bad, err := r.env.Orch.PlanOrgConfig(ctx, r.org)
	if err != nil {
		t.Fatal(err)
	}
	if bad.Status != "error" || !strings.Contains(bad.Error, "version") {
		t.Errorf("bad file = %s %q, want an error row naming the version", bad.Status, bad.Error)
	}

	n := len(r.plans(t))
	pv, err := r.env.Orch.PreviewOrgConfig(ctx, r.org, []byte("version: 1\norg: Acme Three\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pv.Changes) != 1 || pv.Changes[0].Kind != "org" {
		t.Errorf("preview = %+v, want the one rename", pv)
	}
	if got := len(r.plans(t)); got != n {
		t.Errorf("preview stored a row: %d plans, want %d", got, n)
	}
	if _, err := r.env.Orch.PreviewOrgConfig(ctx, r.org, []byte("version: 7\n")); err == nil {
		t.Error("a preview of a bad file passed")
	}
}

// Approve creates and binds the stack, sets params and colours, marks the
// plan applied and pushes the new stack; a second approve is refused; the
// export of the applied org previews clean.
func TestApproveOrgPlanApplies(t *testing.T) {
	ctx := context.Background()
	r := newOrgRig(t)
	r.g.Commit(t, "acme/shop", "main", map[string]string{
		"stackr-compose.yml": "version: 1\nstack: shop\nladder:\n  - dev\nhead: main\n",
	})
	r.orgFile(t, `version: 1
org: acme
params:
  app:
    region:
      type: param
      value: eu
    token:
      type: secret
env_colors:
  dev: sky
stacks:
  shop:
    repo: acme/shop
domains:
  - host: Shop.Example.com
    include_env_on_default: true
`)
	r.bind(t, false)
	pl := r.plans(t)[0]
	j, err := r.env.Orch.ApproveOrgPlan(ctx, pl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Kind != "org-apply" {
		t.Errorf("job kind = %s", j.Kind)
	}
	if _, err := r.env.Orch.ApproveOrgPlan(ctx, pl.ID); !isConflict(err) {
		t.Errorf("second approve = %v, want Conflict", err)
	}
	r.applied(t, pl.ID)

	sts, err := r.env.Orch.Stacks(ctx, r.org)
	if err != nil {
		t.Fatal(err)
	}
	if len(sts) != 1 || sts[0].Slug != "shop" || sts[0].ConfigRepo != "https://github.com/acme/shop" {
		t.Fatalf("stacks = %+v, want shop bound to acme/shop", sts)
	}
	ps, err := r.env.Orch.Params(ctx, service.ParamScope{Kind: "org", ID: r.org}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].Name != "region" || ps[0].Value != "eu" {
		t.Errorf("org params = %+v, want app.region = eu (the secret is declared, not stored)", ps)
	}
	og, err := r.env.Store.Orgs.Get(ctx, r.org)
	if err != nil {
		t.Fatal(err)
	}
	if og.EnvColors != `{"dev":"sky"}` {
		t.Errorf("env colours = %s", og.EnvColors)
	}
	ds, err := r.env.Orch.DomainResources(ctx, r.org)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].Host != "shop.example.com" || !ds[0].IncludeEnvOnDefault {
		t.Errorf("domain resources = %+v, want shop.example.com with the env flag", ds)
	}
	eventually(t, "the stack file's dev env", func() bool {
		es, err := r.env.Orch.Envs(ctx, sts[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		return len(es) == 1 && es[0].Slug == "dev"
	})

	out, err := r.env.Orch.ExportOrgConfig(ctx, r.org)
	if err != nil {
		t.Fatal(err)
	}
	pv, err := r.env.Orch.PreviewOrgConfig(ctx, r.org, out)
	if err != nil {
		t.Fatalf("export does not preview: %v\n%s", err, out)
	}
	if len(pv.Changes) > 0 || len(pv.Blockers) > 0 {
		t.Errorf("export previews %+v, want clean\n%s", pv, out)
	}
	if strings.Contains(string(out), "{") {
		t.Errorf("export has flow style:\n%s", out)
	}
}

// A blocked plan is refused with its blocker; reject ends a pending plan.
func TestBlockedAndRejectedOrgPlans(t *testing.T) {
	ctx := context.Background()
	r := newOrgRig(t)
	r.orgFile(t, "version: 1\norg: acme\nshared:\n  pg:\n    engine: postgres\n    host: db/prod\n")
	r.bind(t, false)
	pl := r.plans(t)[0]
	_, err := r.env.Orch.ApproveOrgPlan(ctx, pl.ID)
	if !isConflict(err) || !strings.Contains(err.Error(), "shared.pg: host db/prod is not an env") {
		t.Errorf("approve of a blocked plan = %v, want Conflict with the blocker", err)
	}

	r.orgFile(t, "version: 1\norg: Acme Inc\n")
	pl, err = r.env.Orch.PlanOrgConfig(ctx, r.org)
	if err != nil {
		t.Fatal(err)
	}
	rej, err := r.env.Orch.RejectOrgPlan(ctx, pl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rej.Status != "rejected" {
		t.Errorf("rejected plan = %s", rej.Status)
	}
	if _, err := r.env.Orch.ApproveOrgPlan(ctx, pl.ID); !isConflict(err) {
		t.Errorf("approve after reject = %v, want Conflict", err)
	}
}

// With auto on, a push to the org repo plans and applies with no call.
func TestOrgAutoApply(t *testing.T) {
	ctx := context.Background()
	r := newOrgRig(t)
	r.orgFile(t, "version: 1\norg: acme\n")
	r.bind(t, true)
	sha := r.orgFile(t, "version: 1\norg: acme\nparams:\n  app:\n    region:\n      type: param\n      value: us\n")
	r.push(t, "acme/org", sha)
	eventually(t, "the auto apply", func() bool {
		ps := r.plans(t)
		return ps[0].Commit == sha && ps[0].Status == "applied"
	})
	vs, err := r.env.Orch.Params(ctx, service.ParamScope{Kind: "org", ID: r.org}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 1 || vs[0].Value != "us" {
		t.Errorf("org params = %+v, want app.region = us", vs)
	}
}

// A shared instance is made in its host env, scoped to the org, deployed.
func TestOrgSharedInstance(t *testing.T) {
	ctx := context.Background()
	r := newOrgRig(t)
	st, err := r.env.Orch.CreateStack(ctx, r.org, "db", "")
	if err != nil {
		t.Fatal(err)
	}
	prod, err := r.env.Orch.CreateEnv(ctx, st.ID, "prod", service.EnvSpec{
		Type:       "static",
		FromKind:   "branch",
		FromBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	r.orgFile(t, "version: 1\norg: acme\nshared:\n  pg:\n    engine: postgres\n    host: db/prod\n    shm_size_mb: 256\n")
	r.bind(t, false)
	pl := r.plans(t)[0]
	if _, err := r.env.Orch.ApproveOrgPlan(ctx, pl.ID); err != nil {
		t.Fatal(err)
	}
	r.applied(t, pl.ID)

	ms, err := r.env.Orch.ManagedInstances(ctx, prod.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].Engine != "postgres" || ms[0].ScopeKind != "org" || ms[0].ScopeID != r.org {
		t.Fatalf("instances = %+v, want one org-scoped postgres", ms)
	}
	ts, err := r.env.Orch.Tiles(ctx, prod.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 1 || ts[0].Slug != "pg" || ts[0].ShmSizeMB != 256 {
		t.Fatalf("tiles = %+v, want pg with 256 MB shm", ts)
	}
	js, err := r.env.Orch.TileJobs(ctx, []string{ts[0].ID}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(js, func(j service.Job) bool { return j.Kind == "deploy" }) {
		t.Errorf("jobs = %+v, want a deploy of pg", js)
	}

	out, err := r.env.Orch.ExportOrgConfig(ctx, r.org)
	if err != nil {
		t.Fatal(err)
	}
	pv, err := r.env.Orch.PreviewOrgConfig(ctx, r.org, out)
	if err != nil {
		t.Fatal(err)
	}
	if len(pv.Changes) > 0 || len(pv.Blockers) > 0 {
		t.Errorf("export previews %+v, want clean\n%s", pv, out)
	}
}

// Task 8: a push to the org repo queues the org plan and nothing else; a
// push to a stack repo queues no org plan.
func TestWebhookQueuesOrgPlan(t *testing.T) {
	ctx := context.Background()
	r := newOrgRig(t)
	r.orgFile(t, "version: 1\norg: acme\n")
	r.bind(t, false)
	st, err := r.env.Orch.CreateStack(ctx, r.org, "shop", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.env.Orch.SetConfigRepo(ctx, st.ID, r.conn, "acme/shop", "", ""); err != nil {
		t.Fatal(err)
	}
	kinds := func() []string {
		js, err := r.env.Orch.Jobs(ctx, "queued", "running", "waiting", "done", "failed", "superseded", "cancelled")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, j := range js {
			out = append(out, j.Kind)
		}
		return out
	}

	r.push(t, "acme/org", r.orgFile(t, "version: 1\norg: Acme Inc\n"))
	if got := kinds(); !slices.Equal(got, []string{"org-plan"}) {
		t.Fatalf("jobs after an org repo push = %v, want [org-plan]", got)
	}
	r.push(t, "acme/shop", r.g.Commit(t, "acme/shop", "main", map[string]string{"x": "1"}))
	if got := kinds(); !slices.Equal(got, []string{"org-plan", "push"}) {
		t.Errorf("jobs after a stack repo push = %v, want [org-plan push]", got)
	}
}

func isConflict(err error) bool {
	var c errs.Conflict
	return errors.As(err, &c)
}
