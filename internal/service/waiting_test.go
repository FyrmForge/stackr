package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
	"github.com/google/uuid"
)

// Waiting on you: an owner sees the value a parked deploy waits for; access
// parked for approval is an admin's group and the owner's muted line.
func TestWaitingOnYou(t *testing.T) {
	env := servicetest.New(t)
	ctx := context.Background()
	org := env.Org(t, "acme")
	owner := env.User(t, "owner@acme.test", false)
	env.Member(t, org, owner, "owner")
	admin := env.User(t, "admin@acme.test", true)
	tl := env.Tile(t, org)

	pOwner, err := env.Orch.SessionPrincipal(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	pAdmin, _ := env.Orch.SessionPrincipal(ctx, admin)
	if w, err := env.Orch.WaitingOnYou(ctx, pOwner); err != nil || w.Count() != 0 || len(w.Others) != 0 {
		t.Fatalf("nothing waits: %+v, %v", w, err)
	}

	env.WaitingJob(t, "gate.password", `{"tile_id":"`+tl.ID+`"}`)
	env.WaitingJob(t, "elevated access: share media", `{"tile_id":"`+tl.ID+`","host_access":{"stack":"`+tl.Stack+`","what":"share media"}}`)

	w, err := env.Orch.WaitingOnYou(ctx, pOwner)
	if err != nil || w.Count() != 1 || w.Groups[0].Title != "Values to set" || w.Groups[0].Items[0].Act != "Set" {
		t.Fatalf("owner = %+v, %v", w, err)
	}
	if len(w.Others) != 1 || w.Others[0].Sub != "1 tile waits for a server admin" {
		t.Errorf("the owner's muted line = %+v", w.Others)
	}
	if wa, _ := env.Orch.WaitingOnYou(ctx, pAdmin); len(wa.Groups) == 0 || wa.Groups[len(wa.Groups)-1].Title != "Access to approve" || len(wa.Others) != 0 {
		t.Errorf("admin = %+v", wa)
	}
}

// A wait on an org value links to the org's variables (the env's tab cannot
// set it), and to an owner only: an env writer gets the muted line.
func TestWaitingOrgParam(t *testing.T) {
	env := servicetest.New(t)
	ctx := context.Background()
	org := env.Org(t, "acme")
	owner := env.User(t, "owner@acme.test", false)
	env.Member(t, org, owner, "owner")
	tl := env.Tile(t, org)
	env.WaitingJob(t, "org.gate.password", `{"tile_id":"`+tl.ID+`"}`)

	p, err := env.Orch.SessionPrincipal(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	w, err := env.Orch.WaitingOnYou(ctx, p)
	if err != nil || w.Count() != 1 {
		t.Fatalf("owner = %+v, %v", w, err)
	}
	it := w.Groups[0].Items[0]
	if !strings.Contains(it.Href, "?drawer=org%3A") || !strings.HasSuffix(it.Href, "&tab=variables") || strings.Contains(it.Href, "/shop/") {
		t.Errorf("link = %q, want the org's variables", it.Href)
	}
}

// The badge's count is the cheap path and is served for a few seconds; the
// list is always fresh.
func TestWaitingCountIsCached(t *testing.T) {
	env := servicetest.New(t)
	ctx := context.Background()
	org := env.Org(t, "acme")
	owner := env.User(t, "owner@acme.test", false)
	env.Member(t, org, owner, "owner")
	tl := env.Tile(t, org)
	p, err := env.Orch.SessionPrincipal(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if n := env.Orch.WaitingCount(ctx, p); n != 0 {
		t.Fatalf("count = %d", n)
	}
	env.WaitingJob(t, "gate.password", `{"tile_id":"`+tl.ID+`"}`)
	if n := env.Orch.WaitingCount(ctx, p); n != 0 {
		t.Errorf("count = %d, want the cached 0", n)
	}
	if w, _ := env.Orch.WaitingOnYou(ctx, p); w.Count() != 1 {
		t.Errorf("the list is stale: %+v", w)
	}
	// after a write the acting tab reads fresh (item 55)
	env.Orch.ForgetWaitingCounts()
	if n := env.Orch.WaitingCount(ctx, p); n != 1 {
		t.Errorf("count = %d after a forget, want 1", n)
	}
}

// A release row says what promoting it does, not what the rung below runs.
func TestWaitingReleaseRowWords(t *testing.T) {
	t.Parallel()
	w := seedWorld(t)
	p, err := w.e.Orch.SessionPrincipal(context.Background(), w.user)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := w.e.Orch.WaitingOnYou(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range wt.Groups {
		if g.Title != "Releases to promote" {
			continue
		}
		if sub := g.Items[0].Sub; sub == "" || strings.Contains(sub, "runs release") {
			t.Errorf("release row sub = %q", sub)
		}
		return
	}
	t.Fatalf("no release row: %+v", wt)
}

// A release waiting on the bottom env (rung 0, nothing below it) is listed by
// its own number and does not panic the page.
func TestWaitingReleaseAtBottomEnv(t *testing.T) {
	t.Parallel()
	e := servicetest.New(t)
	ctx := context.Background()
	org := e.Org(t, "acme")
	u := e.User(t, "o@acme.test", false)
	e.Member(t, org, u, "owner")
	tl := e.Tile(t, org)
	now := time.Now()
	line := "env:" + tl.Env
	built := "r1"
	for _, r := range []store.Release{
		{ID: "r1", StackID: tl.Stack, Number: 1, CreatedAt: now},
		{ID: "r2", StackID: tl.Stack, Number: 2, CreatedAt: now, BuiltOn: &built, Line: &line},
	} {
		if err := e.Store.Releases.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		if err := e.Store.ReleaseTiles.Create(ctx, store.ReleaseTile{ID: "t" + r.ID, ReleaseID: r.ID, Slug: "api", Repo: "nginx:1", Digest: "sha256:" + r.ID}); err != nil {
			t.Fatal(err)
		}
	}
	row, err := e.Store.Environments.Get(ctx, tl.Env)
	if err != nil {
		t.Fatal(err)
	}
	row.ReleaseID = &built
	if err := e.Store.Environments.Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	p, err := e.Orch.SessionPrincipal(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	w, err := e.Orch.WaitingOnYou(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range w.Groups {
		if g.Title == "Releases to promote" && strings.HasSuffix(g.Items[0].Title, "release 2") {
			return
		}
	}
	t.Fatalf("no row naming release 2: %+v", w)
}

// A release row carries what the Waiting page draws: the release, the env it
// waits for and the one below; PromoteClean checks each pair on its own.
func TestWaitingReleaseRowAndPromoteClean(t *testing.T) {
	t.Parallel()
	w := seedWorld(t)
	ctx := context.Background()
	p, err := w.e.Orch.SessionPrincipal(ctx, w.user)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := w.e.Orch.WaitingOnYou(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	var rel *service.WaitRelease
	for _, g := range wt.Groups {
		if g.Title == "Releases to promote" {
			rel = g.Items[0].Rel
		}
	}
	if rel == nil || rel.ID == "" || rel.Number != 2 || rel.EnvID != w.prod || rel.From != "dev" || rel.At.IsZero() {
		t.Fatalf("release row = %+v", rel)
	}
	// dev is the top rung: nothing waits there, so a pair for it is not the newest waiting one
	outs := w.e.Orch.PromoteClean(ctx, p, []service.PromotePair{{ReleaseID: rel.ID, EnvID: w.dev}})
	if outs[0].Skip != service.SkipNewer || outs[0].Err != nil {
		t.Errorf("a release not waiting for the env: %+v", outs[0])
	}
	// someone who may not write the env gets an error per pair, never a job
	stranger, err := w.e.Orch.SessionPrincipal(ctx, w.e.User(t, "x@y.io", false))
	if err != nil {
		t.Fatal(err)
	}
	outs = w.e.Orch.PromoteClean(ctx, stranger, []service.PromotePair{{ReleaseID: rel.ID, EnvID: w.prod}})
	if outs[0].Err == nil || outs[0].Job.ID != "" {
		t.Errorf("a stranger promoted: %+v", outs[0])
	}
}

// The worker's clean-only guard re-checks that the release is still the env's
// newest waiting one: a newer cut after the enqueue refuses it.
func TestCleanGuardRefusesAfterNewerRelease(t *testing.T) {
	t.Parallel()
	w := seedWorld(t)
	ctx := context.Background()
	rels, err := w.e.Store.Releases.ListByStack(ctx, w.shop)
	if err != nil {
		t.Fatal(err)
	}
	var two string
	for _, r := range rels {
		if r.Number == 2 {
			two = r.ID
		}
	}
	if err := w.e.Orch.CleanGuardRefusal(ctx, w.prod, two); err != nil {
		t.Fatalf("newest waiting refused: %v", err)
	}
	// a release cut for prod's own line on top of what prod runs
	prod, err := w.e.Store.Environments.Get(ctx, w.prod)
	if err != nil {
		t.Fatal(err)
	}
	line := "env:" + w.prod
	if err := w.e.Store.Releases.Create(ctx, store.Release{ID: "rel-3", StackID: w.shop, Number: 3, CreatedAt: time.Now(), BuiltOn: prod.ReleaseID, Line: &line}); err != nil {
		t.Fatal(err)
	}
	if err := w.e.Store.ReleaseTiles.Create(ctx, store.ReleaseTile{ID: "t-rel-3", ReleaseID: "rel-3", Slug: "api", Repo: "nginx:1", Digest: "sha256:3"}); err != nil {
		t.Fatal(err)
	}
	err = w.e.Orch.CleanGuardRefusal(ctx, w.prod, two)
	if !errors.Is(err, errs.ErrRefused) || !strings.Contains(err.Error(), service.SkipNewer) {
		t.Fatalf("want refused %q, got %v", service.SkipNewer, err)
	}
}

// Every pair of a batch is authorized on its own: the one the person may
// promote is judged, each mismatched or foreign one is not found, and no job
// is queued for those.
func TestPromoteCleanAuthorizesEachPair(t *testing.T) {
	t.Parallel()
	w := seedWorld(t)
	ctx := context.Background()
	e := w.e
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	rel := func(stack string, n int) string {
		id := "rel-" + stack + "-" + string(rune('a'+n))
		must(e.Store.Releases.Create(ctx, store.Release{ID: id, StackID: stack, Number: n, CreatedAt: time.Now()}))
		return id
	}
	// a second stack in acme, and a stack in beta, each with an env
	other, err := e.Orch.CreateStack(ctx, w.acme, "other", "")
	must(err)
	oenv, err := e.Orch.CreateEnv(ctx, other.ID, "dev", service.EnvSpec{Type: "static", FromKind: "branch", FromBranch: "main"})
	must(err)
	bstack, err := e.Orch.CreateStack(ctx, w.beta, "bshop", "")
	must(err)
	benv, err := e.Orch.CreateEnv(ctx, bstack.ID, "dev", service.EnvSpec{Type: "static", FromKind: "branch", FromBranch: "main"})
	must(err)
	shopRel := rel(w.shop, 3)
	otherRel := rel(other.ID, 1)

	owner, err := e.Orch.SessionPrincipal(ctx, w.user)
	must(err)
	member := e.User(t, "acme@x.io", false)
	e.Member(t, w.acme, member, "owner")
	acmeOwner, err := e.Orch.SessionPrincipal(ctx, member)
	must(err)
	scoped := *acmeOwner // a key limited to the "other" stack
	scoped.Access.Key, scoped.Access.KeyStack = true, other.ID

	queued := func() int {
		js, err := e.Store.Jobs.ListByState(ctx, "queued")
		must(err)
		return len(js)
	}
	for _, c := range []struct {
		name  string
		by    *service.Principal
		pairs []service.PromotePair
		deny  []bool // per pair: not found and no job
	}{
		{"mixed batch", owner, []service.PromotePair{{ReleaseID: shopRel, EnvID: w.prod}, {ReleaseID: shopRel, EnvID: oenv.ID}, {ReleaseID: otherRel, EnvID: w.prod}}, []bool{false, true, true}},
		{"stack-scoped key", &scoped, []service.PromotePair{{ReleaseID: otherRel, EnvID: oenv.ID}, {ReleaseID: shopRel, EnvID: w.prod}}, []bool{false, true}},
		{"cross-org env", acmeOwner, []service.PromotePair{{ReleaseID: shopRel, EnvID: w.prod}, {ReleaseID: shopRel, EnvID: benv.ID}}, []bool{false, true}},
		{"cross-stack release and env", owner, []service.PromotePair{{ReleaseID: otherRel, EnvID: w.prod}, {ReleaseID: shopRel, EnvID: oenv.ID}}, []bool{true, true}},
	} {
		before := queued()
		outs := e.Orch.PromoteClean(ctx, c.by, c.pairs)
		granted := 0
		for i, o := range outs {
			if c.deny[i] {
				if !errors.Is(o.Err, errs.ErrNotFound) || o.Job.ID != "" || o.Skip != "" {
					t.Errorf("%s pair %d: %+v", c.name, i, o)
				}
				continue
			}
			if errors.Is(o.Err, errs.ErrNotFound) {
				t.Errorf("%s pair %d was refused: %+v", c.name, i, o)
			}
			if o.Job.ID != "" {
				granted++
			}
		}
		if got := queued() - before; got != granted {
			t.Errorf("%s: %d jobs queued, %d for allowed pairs", c.name, got, granted)
		}
	}
}

// A release waiting on two envs shows its promote status on the env that is
// promoting, not on both.
func TestWaitingLivePromoteIsPerEnv(t *testing.T) {
	t.Parallel()
	w := seedWorld(t)
	ctx := context.Background()
	p, err := w.e.Orch.SessionPrincipal(ctx, w.user)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := w.e.Orch.WaitingOnYou(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	var rel *service.WaitRelease
	for _, g := range wt.Groups {
		if g.Title == "Releases to promote" {
			rel = g.Items[0].Rel
		}
	}
	if rel == nil {
		t.Fatal("no release row")
	}
	now := time.Now()
	if err := w.e.Store.Jobs.Create(ctx, store.Job{
		ID: uuid.NewString(), Kind: "promote", State: "running", ReleaseID: &rel.ID, CreatedAt: now, StartedAt: &now,
		Payload: `{"env_id":"` + w.dev + `","release_id":"` + rel.ID + `"}`, LockSet: store.StringList{"env:" + w.dev},
	}); err != nil {
		t.Fatal(err)
	}
	wt, err = w.e.Orch.WaitingOnYou(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range wt.Groups {
		if g.Title == "Releases to promote" && g.Items[0].Rel.Status != "" {
			t.Errorf("a promote on dev marked the %s row %q", g.Items[0].Rel.Env, g.Items[0].Rel.Status)
		}
	}
	if err := w.e.Store.Jobs.Create(ctx, store.Job{
		ID: uuid.NewString(), Kind: "promote", State: "running", ReleaseID: &rel.ID, CreatedAt: now, StartedAt: &now,
		Payload: `{"env_id":"` + w.prod + `","release_id":"` + rel.ID + `"}`, LockSet: store.StringList{"env:" + w.prod},
	}); err != nil {
		t.Fatal(err)
	}
	wt, _ = w.e.Orch.WaitingOnYou(ctx, p)
	for _, g := range wt.Groups {
		if g.Title == "Releases to promote" && g.Items[0].Rel.Status != "Deploying" {
			t.Errorf("status = %q, want Deploying", g.Items[0].Rel.Status)
		}
	}
}

// A doubled Promote click queues one job: the second pair is skipped as live.
// And once the env runs the release, the worker's guard has nothing to refuse.
func TestPromoteCleanDoubleClick(t *testing.T) {
	t.Parallel()
	w := seedWorld(t)
	ctx := context.Background()
	p, err := w.e.Orch.SessionPrincipal(ctx, w.user)
	if err != nil {
		t.Fatal(err)
	}
	rels, err := w.e.Store.Releases.ListByStack(ctx, w.shop)
	if err != nil {
		t.Fatal(err)
	}
	var two string
	for _, r := range rels {
		if r.Number == 2 {
			two = r.ID
		}
	}
	pair := []service.PromotePair{{ReleaseID: two, EnvID: w.prod}}
	if _, err := w.e.Orch.Promote(ctx, p, w.prod, two); err != nil { // the first click's job
		t.Fatal(err)
	}
	if second := w.e.Orch.PromoteClean(ctx, p, pair)[0]; second.Skip != service.SkipLive || second.Job.ID != "" {
		t.Errorf("second click = %+v, want skip %q", second, service.SkipLive)
	}
	prod, err := w.e.Store.Environments.Get(ctx, w.prod)
	if err != nil {
		t.Fatal(err)
	}
	prod.ReleaseID = &two
	if err := w.e.Store.Environments.Update(ctx, prod); err != nil {
		t.Fatal(err)
	}
	if err := w.e.Orch.CleanGuardRefusal(ctx, w.prod, two); err != nil {
		t.Errorf("env already runs the release: guard = %v", err)
	}
}
