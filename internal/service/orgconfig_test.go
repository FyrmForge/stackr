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
	"time"

	"github.com/google/uuid"

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
    all:
      region: eu
      token: {type: secret}
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
	j, err := r.env.Orch.ApproveOrgPlan(ctx, pl.ID, service.ApproveOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if j.Kind != "org-apply" {
		t.Errorf("job kind = %s", j.Kind)
	}
	if _, err := r.env.Orch.ApproveOrgPlan(ctx, pl.ID, service.ApproveOpts{}); !isConflict(err) {
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
	r.orgFile(t, "version: 1\norg: acme\nmoved:\n  - from: stack.weblog\n    to: stack.blog\n")
	r.bind(t, false)
	pl := r.plans(t)[0]
	_, err := r.env.Orch.ApproveOrgPlan(ctx, pl.ID, service.ApproveOpts{})
	if !isConflict(err) || !strings.Contains(err.Error(), "moved: neither stack.weblog nor stack.blog exists") {
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
	if _, err := r.env.Orch.ApproveOrgPlan(ctx, pl.ID, service.ApproveOpts{}); !isConflict(err) {
		t.Errorf("approve after reject = %v, want Conflict", err)
	}
}

// With auto on, a push to the org repo plans and applies with no call.
func TestOrgAutoApply(t *testing.T) {
	ctx := context.Background()
	r := newOrgRig(t)
	r.orgFile(t, "version: 1\norg: acme\n")
	r.bind(t, true)
	sha := r.orgFile(t, "version: 1\norg: acme\nparams:\n  app:\n    all:\n      region: us\n")
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

// An auto org never applies an unlock: the plan stays pending for an owner.
func TestOrgAutoSkipsUnlock(t *testing.T) {
	r := newOrgRig(t)
	r.orgFile(t, "version: 1\norg: acme\n")
	r.bind(t, true)
	sha := r.orgFile(t, "version: 1\norg: acme\ntiers:\n  dev: {locked: false}\n")
	r.push(t, "acme/org", sha)
	eventually(t, "the plan", func() bool { ps := r.plans(t); return ps[0].Commit == sha })
	time.Sleep(300 * time.Millisecond)
	if ps := r.plans(t); ps[0].Status != "pending" {
		t.Errorf("status = %s, want pending", ps[0].Status)
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
		js, err := r.env.Orch.Jobs(ctx, 0, "queued", "running", "waiting", "done", "failed", "superseded", "cancelled")
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
	if got := kinds(); !slices.Equal(got, []string{"push", "org-plan"}) {
		t.Errorf("jobs after a stack repo push = %v, want [push org-plan]", got)
	}
}

func isConflict(err error) bool {
	var c errs.Conflict
	return errors.As(err, &c)
}

// A plan with an impact line needs the approver's confirm, and a tick must
// name one of the plan's removal rows; both are stored on the row.
func TestApproveOrgPlanContract(t *testing.T) {
	ctx := context.Background()
	r := newOrgRig(t)
	seed := func(plan string) service.OrgPlan {
		t.Helper()
		p := service.OrgPlan{
			ID: uuid.NewString(), OrgID: r.org, Plan: plan,
			Status: "pending", Ticked: []string{}, CreatedAt: time.Now(),
		}
		if err := r.env.Store.OrgPlans.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	risky := seed(`{"changes":[` +
		`{"kind":"defaults","field":"defaults","impact":"redeploys 3 tiles"},` +
		`{"kind":"share-delete","tile":"old","key":"share:old","optional":true}]}`)

	_, err := r.env.Orch.ApproveOrgPlan(ctx, risky.ID, service.ApproveOpts{})
	if !isConflict(err) || !strings.Contains(err.Error(), "This plan has impact lines; confirm to approve.") {
		t.Fatalf("unconfirmed approve = %v, want the impact refusal", err)
	}
	_, err = r.env.Orch.ApproveOrgPlan(ctx, risky.ID, service.ApproveOpts{Confirm: true, Ticked: []string{"share:media"}})
	if _, ok := errs.IsInvalid(err); !ok {
		t.Fatalf("a tick outside the plan's removals = %v, want Invalid", err)
	}
	if p, _ := r.env.Orch.OrgPlan(ctx, risky.ID); p.DecidedAt != nil {
		t.Fatal("a refused approve stamped the row")
	}
	if _, err := r.env.Orch.ApproveOrgPlan(ctx, risky.ID, service.ApproveOpts{
		Confirm: true,
		Ticked:  []string{"share:old"},
	}); err != nil {
		t.Fatal(err)
	}
	p, err := r.env.Orch.OrgPlan(ctx, risky.ID)
	if err != nil || p.DecidedAt == nil || !p.Confirmed || len(p.Ticked) != 1 || p.Ticked[0] != "share:old" {
		t.Errorf("approved row = %+v, %v; want the tick and the confirm stored", p, err)
	}

	quiet := seed(`{"changes":[{"kind":"param","field":"a.b"}]}`)
	if _, err := r.env.Orch.ApproveOrgPlan(ctx, quiet.ID, service.ApproveOpts{Ticked: []string{"share:old"}}); err == nil {
		t.Error("a tick on a plan with no removal rows passed")
	}
	if _, err := r.env.Orch.ApproveOrgPlan(ctx, quiet.ID, service.ApproveOpts{}); err != nil {
		t.Errorf("a plain plan needs no confirm: %v", err)
	}
}

// A share the file's shares: block no longer names is a removal row: it
// stays live unless the approver ticked it.
func TestOrgShareRemovalNeedsATick(t *testing.T) {
	ctx := context.Background()
	r := newOrgRig(t)
	if _, err := r.env.Orch.CreateShare(ctx, r.org, service.ShareSpec{Slug: "media", Kind: "nfs", Source: "nas:/e"}); err != nil {
		t.Fatal(err)
	}
	shares := func() int {
		ss, err := r.env.Orch.Shares(ctx, r.org)
		if err != nil {
			t.Fatal(err)
		}
		return len(ss)
	}
	r.orgFile(t, "version: 1\norg: acme\nparams:\n  app:\n    all:\n      a: one\nshares: {}\n")
	r.bind(t, false)
	pl := r.plans(t)[0]
	if _, err := r.env.Orch.ApproveOrgPlan(ctx, pl.ID, service.ApproveOpts{}); err != nil {
		t.Fatal(err)
	}
	r.applied(t, pl.ID)
	if shares() != 1 {
		t.Fatal("an unticked removal row deleted the share")
	}

	// the plan again: the param is done, the removal row is still there
	pl, err := r.env.Orch.PlanOrgConfig(ctx, r.org)
	if err != nil || !strings.Contains(pl.Plan, `"key":"share:media"`) {
		t.Fatalf("replan = %+v, %v; want the removal row", pl, err)
	}
	if _, err := r.env.Orch.ApproveOrgPlan(ctx, pl.ID, service.ApproveOpts{Ticked: []string{"share:media"}}); err != nil {
		t.Fatal(err)
	}
	r.applied(t, pl.ID)
	if shares() != 0 {
		t.Error("a ticked removal row left the share")
	}
}

// A tiered file makes the ladder, locks and per-tier blocks; the export of the
// result previews clean, and a panel edit shows as drift.
func TestOrgTiersApply(t *testing.T) {
	ctx := context.Background()
	r := newOrgRig(t)
	r.orgFile(t, `version: 1
org: acme
tiers:
  dev: {locked: false}
  prod:
params:
  smtp:
    dev|prod:
      host: smtp.example.com
    prod:
      pw: {type: secret, generate: 32}
    pr:
      host: sandbox
`)
	r.bind(t, false)
	pl := r.plans(t)[0]
	if !strings.Contains(pl.Plan, "org-wide params stop being read") {
		t.Errorf("no first-tier warning: %s", pl.Plan)
	}
	// dev is an unlocked new tier: an impact line, so the approve needs a confirm
	if _, err := r.env.Orch.ApproveOrgPlan(ctx, pl.ID, service.ApproveOpts{}); !isConflict(err) {
		t.Fatalf("approve without confirm = %v, want Conflict", err)
	}
	if _, err := r.env.Orch.ApproveOrgPlan(ctx, pl.ID, service.ApproveOpts{Confirm: true}); err != nil {
		t.Fatal(err)
	}
	r.applied(t, pl.ID)

	ts, err := r.env.Orch.Tiers(ctx, r.org)
	if err != nil || len(ts) != 2 || ts[0].Slug != "dev" || ts[0].Locked || ts[1].Slug != "prod" || !ts[1].Locked {
		t.Fatalf("tiers = %+v, %v", ts, err)
	}
	vals := func(kind, id string) map[string]string {
		ps, err := r.env.Orch.Params(ctx, service.ParamScope{Kind: kind, ID: id}, true)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		for _, p := range ps {
			m[p.Collection+"."+p.Name] = p.Value
		}
		return m
	}
	if dev := vals("tier", ts[0].ID); len(dev) != 1 || dev["smtp.host"] != "smtp.example.com" {
		t.Errorf("dev block = %v", dev)
	}
	if prod := vals("tier", ts[1].ID); prod["smtp.host"] != "smtp.example.com" || len(prod["smtp.pw"]) != 32 {
		t.Errorf("prod block = %v", prod)
	}
	if pr := vals("org_pr", r.org); pr["smtp.host"] != "sandbox" || len(pr) != 1 {
		t.Errorf("pr block = %v", pr)
	}
	if wide := vals("org", r.org); len(wide) != 0 {
		t.Errorf("org-wide = %v", wide)
	}

	out, err := r.env.Orch.ExportOrgConfig(ctx, r.org)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "type: secret") || strings.Contains(string(out), "{") {
		t.Errorf("export shape:\n%s", out)
	}
	pv, err := r.env.Orch.PreviewOrgConfig(ctx, r.org, out)
	if err != nil || len(pv.Changes) > 0 || len(pv.Blockers) > 0 {
		t.Fatalf("export previews %+v, %v\n%s", pv, err, out)
	}

	if _, err := r.env.Orch.SetParams(ctx, service.ParamScope{Kind: "tier", ID: ts[0].ID},
		[]service.ParamEntry{{Collection: "smtp", Name: "host", Kind: "param", Value: "ui.example.com"}}); err != nil {
		t.Fatal(err)
	}
	pv, err = r.env.Orch.PreviewOrgConfig(ctx, r.org, out)
	if err != nil || len(pv.Changes) != 1 || pv.Changes[0].Old != "ui.example.com" ||
		!strings.Contains(pv.Changes[0].Note, "panel has ui.example.com, file says smtp.example.com") {
		t.Errorf("drift preview = %+v, %v", pv, err)
	}

	// A rename is a moved: entry; the tier keeps its params, so only the
	// rename row shows.
	renamed := strings.NewReplacer("dev|prod:", "local|prod:", "  dev:", "  local:").Replace(string(out)) +
		"moved:\n  - from: tier.dev\n    to: tier.local\n"
	pv, err = r.env.Orch.PreviewOrgConfig(ctx, r.org, []byte(renamed))
	if err != nil || len(pv.Blockers) > 0 {
		t.Fatalf("rename preview = %+v, %v\n%s", pv, err, renamed)
	}
	var ks []string
	for _, c := range pv.Changes {
		ks = append(ks, c.Kind)
	}
	if got := strings.Join(ks, " "); got != "tier-rename param-update" {
		t.Errorf("rename rows = %s (the param-update is the drift edit above)", got)
	}
}

// A param the bound file does not set is removed with the plan, in the org
// and pr scopes alike; a secret stays until its removal row is ticked.
func TestOrgParamRemoval(t *testing.T) {
	ctx := context.Background()
	r := newOrgRig(t)
	set := func(kind string, es ...service.ParamEntry) {
		if _, err := r.env.Orch.SetParams(ctx, service.ParamScope{Kind: kind, ID: r.org}, es); err != nil {
			t.Fatal(err)
		}
	}
	set("org",
		service.ParamEntry{Collection: "app", Name: "keep", Kind: "param", Value: "k"},
		service.ParamEntry{Collection: "app", Name: "old", Kind: "param", Value: "o"},
		service.ParamEntry{Collection: "app", Name: "sec", Kind: "secret", Value: "s"})
	set("org_pr", service.ParamEntry{Collection: "app", Name: "old", Kind: "param", Value: "o"})
	names := func(kind string) string {
		ps, err := r.env.Orch.Params(ctx, service.ParamScope{Kind: kind, ID: r.org}, true)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, p := range ps {
			out = append(out, p.Name)
		}
		slices.Sort(out)
		return strings.Join(out, ",")
	}
	r.orgFile(t, "version: 1\norg: acme\nparams:\n  app:\n    all:\n      keep: k\n")
	r.bind(t, false)
	pl := r.plans(t)[0]
	if _, err := r.env.Orch.ApproveOrgPlan(ctx, pl.ID, service.ApproveOpts{}); err != nil {
		t.Fatal(err)
	}
	r.applied(t, pl.ID)
	if got := names("org"); got != "keep,sec" {
		t.Errorf("org after an unticked apply = %q, want the plain name gone and the secret kept", got)
	}
	if got := names("org_pr"); got != "" {
		t.Errorf("org_pr = %q, want the plain name gone", got)
	}
	pl, err := r.env.Orch.PlanOrgConfig(ctx, r.org)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.env.Orch.ApproveOrgPlan(ctx, pl.ID, service.ApproveOpts{Ticked: []string{"param-remove:all:app.sec"}}); err != nil {
		t.Fatal(err)
	}
	r.applied(t, pl.ID)
	if got := names("org"); got != "keep" {
		t.Errorf("org after a ticked secret removal = %q", got)
	}
}
