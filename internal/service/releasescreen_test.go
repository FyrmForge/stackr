package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A failure a later push cured leaves the window but stays on the screen,
// found by the Failed filter and marked fixed; the screen lists every
// release, filters by state and text, and scopes by org and stack.
func TestReleaseScreenKeepsFixedFailure(t *testing.T) {
	t.Parallel()
	w := seedWorld(t)
	ctx := context.Background()
	p, err := w.e.Orch.SessionPrincipal(ctx, w.user)
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"org_id":"` + w.acme + `","stack_id":"` + w.shop + `"}`
	push := func(state, msg string, ago time.Duration) {
		t.Helper()
		at := time.Now().Add(-ago)
		if err := w.e.Store.Jobs.Create(ctx, store.Job{
			ID: uuid.NewString(), Kind: "push", State: state, Error: msg, Payload: payload,
			LockSet: store.StringList{"push:" + w.shop, "push:" + w.shop + ":acme/shop@main"}, CreatedAt: at, FinishedAt: &at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	push("failed", "build web, exit status 1", 3*time.Hour)
	push("done", "", time.Hour)

	win, err := w.e.Orch.ReleaseWindow(ctx, w.shop, "")
	if err != nil || win.Failed != 0 {
		t.Fatalf("window = %+v, %v: a fixed failure is not in it", win, err)
	}
	sc, err := w.e.Orch.ReleaseScreen(ctx, p, service.ScreenQuery{State: "failed"})
	if err != nil || len(sc.Rows) != 1 || !sc.Rows[0].Fixed || sc.Rows[0].Org != "acme" || sc.Rows[0].Scope != "shop" {
		t.Fatalf("failed filter = %+v, %v", sc.Rows, err)
	}
	if sc.Rows[0].Err != "build web, exit status 1" {
		t.Errorf("err = %q", sc.Rows[0].Err)
	}
	all, _ := w.e.Orch.ReleaseScreen(ctx, p, service.ScreenQuery{Scope: "acme/shop"})
	if len(all.Rows) < 3 { // two releases and the failure
		t.Errorf("rows = %d", len(all.Rows))
	}
	if n, _ := w.e.Orch.ReleaseScreen(ctx, p, service.ScreenQuery{Scope: "beta"}); len(n.Rows) != 0 {
		t.Errorf("beta rows = %+v", n.Rows)
	}
	if n, _ := w.e.Orch.ReleaseScreen(ctx, p, service.ScreenQuery{Q: "zzz-none"}); len(n.Rows) != 0 {
		t.Errorf("search rows = %+v", n.Rows)
	}
	// a viewer outside the org sees none of it
	out := w.e.User(t, "out@x.io", false)
	po, _ := w.e.Orch.SessionPrincipal(ctx, out)
	if n, err := w.e.Orch.ReleaseScreen(ctx, po, service.ScreenQuery{}); err != nil || len(n.Rows) != 0 || len(n.Scopes) != 0 {
		t.Errorf("outsider: %+v, %v", n, err)
	}
}

// Config plans are the owners' (org.owner.read, as the org canvas and the
// API): a member sees the org's releases but not its plan rows or scope.
func TestReleaseScreenPlansAreOwnersOnly(t *testing.T) {
	t.Parallel()
	w := seedWorld(t)
	ctx := context.Background()
	if err := w.e.Store.OrgPlans.Create(ctx, store.OrgPlan{ID: uuid.NewString(), OrgID: w.acme, Status: "pending", Summary: "1 change", Plan: "{}", Ticked: []string{}, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	plans := func(u string) (rows, scopes int) {
		t.Helper()
		p, err := w.e.Orch.SessionPrincipal(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		sc, err := w.e.Orch.ReleaseScreen(ctx, p, service.ScreenQuery{Scope: "acme"})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range sc.Rows {
			if r.Kind == service.RowPlan {
				rows++
			}
		}
		for _, s := range sc.Scopes {
			if s.Key == "acme/"+service.ScopeOrgConfig {
				scopes++
			}
		}
		return
	}
	if r, s := plans(w.user); r != 1 || s != 1 {
		t.Errorf("owner sees %d plan rows, %d scopes, want 1, 1", r, s)
	}
	m := w.e.User(t, "m@x.io", false)
	w.e.Member(t, w.acme, m, "member")
	if r, s := plans(m); r != 0 || s != 0 {
		t.Errorf("member sees %d plan rows, %d scopes, want 0, 0", r, s)
	}
}

// Names for a release's source come from the members of the org only, and a
// member with no display name has none: never their email.
func TestMemberNamesNeverFallBackToEmail(t *testing.T) {
	t.Parallel()
	w := seedWorld(t)
	ctx := context.Background()
	named := w.e.User(t, "named@x.io", false)
	blank := w.e.User(t, "blank@x.io", false)
	outsider := w.e.User(t, "out2@x.io", false)
	w.e.Member(t, w.acme, named, "member")
	w.e.Member(t, w.acme, blank, "member")
	for id, name := range map[string]string{named: "Nia", blank: ""} {
		u, err := w.e.Store.Users.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		u.Name = name
		if err := w.e.Store.Users.Update(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	got, err := w.e.Orch.MemberNames(ctx, w.acme)
	if err != nil {
		t.Fatal(err)
	}
	if got[named] != "Nia" || got[blank] != "" || got[outsider] != "" {
		t.Errorf("names = %v: want Nia for the named member only, no email, no outsider", got)
	}
	if strings.Contains(got[blank], "@") {
		t.Errorf("blank member shows as %q", got[blank])
	}
}

// Jobs are placed by what the real flows write: a push payload has a
// stack_id and no org_id, a promote payload an env_id and no stack_id. The
// screen still lists the failure a later push cured, the one a later promote
// cured, and a promote under way as deploying.
func TestReleaseScreenPlacesJobsByRealPayloads(t *testing.T) {
	t.Parallel()
	w := seedWorld(t)
	ctx := context.Background()
	p, err := w.e.Orch.SessionPrincipal(ctx, w.user)
	if err != nil {
		t.Fatal(err)
	}
	rid := uuid.NewString()
	if err := w.e.Store.Releases.Create(ctx, store.Release{ID: rid, StackID: w.shop, Number: 3, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	job := func(kind, state, msg, payload string, lock []string, rel *string, ago time.Duration) {
		t.Helper()
		at := time.Now().Add(-ago)
		j := store.Job{ID: uuid.NewString(), Kind: kind, State: state, Error: msg, Payload: payload, LockSet: store.StringList(lock), ReleaseID: rel, CreatedAt: at}
		if state != "running" {
			j.FinishedAt = &at
		}
		if err := w.e.Store.Jobs.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	push := `{"stack_id":"` + w.shop + `","event":{"repo":"acme/shop","branch":"main","commit":"abc"}}`
	pl := []string{"push:" + w.shop, "push:" + w.shop + ":acme/shop@main"}
	job("push", "failed", "build web, exit status 1", push, pl, nil, 5*time.Hour)
	job("push", "done", "", push, pl, nil, 4*time.Hour)
	promote := `{"env_id":"` + w.prod + `","release_id":"` + rid + `"}`
	job("promote", "failed", "deploy api, boom", promote, []string{"env:" + w.prod}, &rid, 3*time.Hour)
	job("promote", "done", "", promote, []string{"env:" + w.prod}, &rid, 2*time.Hour)
	job("promote", "running", "", `{"env_id":"`+w.dev+`","release_id":"`+rid+`"}`, []string{"env:" + w.dev}, &rid, time.Minute)

	sc, err := w.e.Orch.ReleaseScreen(ctx, p, service.ScreenQuery{Scope: "acme/shop"})
	if err != nil {
		t.Fatal(err)
	}
	fixed, deploying := 0, 0
	for _, r := range sc.Rows {
		if r.State == service.StateFailed && r.Fixed {
			fixed++
		}
		if r.State == service.StateDeploying {
			deploying++
		}
	}
	if fixed != 2 || deploying != 1 {
		t.Errorf("fixed failures = %d, deploying = %d, want 2, 1: %+v", fixed, deploying, sc.Rows)
	}
}
