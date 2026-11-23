package canvas_test

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// A promote that stopped at a tile marks the env until the next promote
// does not fail: the banner and chip, the failed tile, and the tiles it
// never reached.
func TestRolloutStopShows(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	if _, err := s.Orch.CreateTile(ctx, service.Tile{StackID: s.Tile.Stack, EnvironmentID: s.Tile.Env, Name: "web", Kind: "image", ImageRef: "nginx:1", ContainerPort: 80}); err != nil {
		t.Fatal(err)
	}
	rel := s.Release(t, s.Tile.Stack, 3)
	at := time.Now()
	id := s.PromoteJob(t, s.Tile.Env, "failed", "deploy api: port 80 is taken", `{"stopped_at":"api","not_deployed":["web"]}`, rel, at)
	body := get(t, s, "/acme/shop/dev")
	for _, want := range []string{
		"Promote of release 3 stopped at api.",
		`href="/acme/shop/dev/-/jobs/` + id + `"`,
		`id="env-stop-chip" class="chip bg-rw-danger/15 text-rw-danger"`,
		"Not deployed",
		"Stopped at api.",
		"Deploy failed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("env page lacks %q:\n%s", want, body)
		}
	}
	// the banners poll carries both, out of band
	poll := get(t, s, "/acme/shop/dev/-/banners")
	if !strings.Contains(poll, "stopped at api") || !strings.Contains(poll, `id="env-stop-chip"`) || !strings.Contains(poll, `hx-swap-oob="true"`) {
		t.Errorf("banners poll:\n%s", poll)
	}
	s.PromoteJob(t, s.Tile.Env, "done", "", `{}`, rel, at.Add(time.Minute))
	body = get(t, s, "/acme/shop/dev")
	if strings.Contains(body, "stopped at") || strings.Contains(body, "Not deployed") || !strings.Contains(body, `id="env-stop-chip" class="chip bg-rw-danger/15 text-rw-danger hidden"`) {
		t.Errorf("a later promote left the failure:\n%s", body)
	}
}

// The release the env below runs and this one does not waits: the stack's env
// card says so, the empty env page names it with a link to its plan, the
// Releases tab chips it and its review says Waiting.
func TestWaitingReleaseShows(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	staging := stagingOf(t, s)
	r1, r2 := s.Release(t, s.Tile.Stack, 1), s.Release(t, s.Tile.Stack, 2)
	for env, rid := range map[string]string{s.Tile.Env: r2, staging.ID: r1} {
		row, err := s.Store.Environments.Get(ctx, env)
		if err != nil {
			t.Fatal(err)
		}
		row.ReleaseID = &rid
		if err := s.Store.Environments.Update(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	if body := get(t, s, "/acme/shop"); !strings.Contains(body, "Release 2 waiting") {
		t.Errorf("env card:\n%s", body)
	}
	body := get(t, s, "/acme/shop/staging")
	if !strings.Contains(body, "Release 2 is waiting.") || !strings.Contains(body, "Nothing runs here until it is promoted.") ||
		!strings.Contains(body, "tab=releases&amp;plan="+r2) || !strings.Contains(body, ">Review plan<") {
		t.Errorf("empty env page:\n%s", body)
	}
	body = get(t, s, "/acme/shop/staging/-/drawer?tab=releases")
	if !strings.Contains(body, ">waiting<") || !strings.Contains(body, ">Review plan<") || strings.Contains(body, ">Promote to") {
		t.Errorf("releases tab:\n%s", body)
	}
	body = get(t, s, "/acme/shop/staging/-/drawer?tab=releases&plan="+r2)
	if !strings.Contains(body, "Release 2 for staging") || !strings.Contains(body, ">Waiting<") {
		t.Errorf("review:\n%s", body)
	}
	// rolling back is its own review, in danger, with no Promote
	body = get(t, s, "/acme/shop/dev/-/drawer?tab=releases&plan="+r1)
	if !strings.Contains(body, "Roll back dev to release 1") || !strings.Contains(body, "btn-danger") || !strings.Contains(body, ">Rollback<") || strings.Contains(body, ">Promote") {
		t.Errorf("rollback review:\n%s", body)
	}
}

// A deploy still running in the env blocks a new promote and says which.
func TestReviewBlockedWhileDeploying(t *testing.T) {
	s := webtest.New(t)
	rel := s.Release(t, s.Tile.Stack, 5)
	s.JobWith(t, "promote", "running", `{"org_id":"`+s.Org+`","stack_id":"`+s.Tile.Stack+`","env_id":"`+s.Tile.Env+`"}`)
	body := get(t, s, "/acme/shop/dev/-/drawer?tab=releases&plan="+rel)
	for _, want := range []string{"Cannot promote yet", "A deploy is still running in dev.", "This page updates by itself.", `hx-trigger="every 3s"`, "disabled"} {
		if !strings.Contains(body, want) {
			t.Errorf("review lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `hx-post="/acme/shop/dev/-/drawer/promote/`) {
		t.Errorf("a blocked review can promote:\n%s", body)
	}
}

// Only a secret the plan names can be set from the review: any other name
// is refused and nothing is written.
func TestPromoteSecretRefusesUnnamed(t *testing.T) {
	s := webtest.New(t)
	rel := s.Release(t, s.Tile.Stack, 5)
	rec := s.Do(t, "POST", "/acme/shop/dev/-/drawer/promote/"+rel+"/secret?plan="+rel, url.Values{"name": {"app.key"}, "value": {"hunter2"}})
	if rec.Code != 422 && rec.Code != 400 {
		t.Errorf("status = %d\n%.400s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Error("the answer echoes the value")
	}
	ps, err := s.Orch.Params(context.Background(), service.ParamScope{Kind: "env", ID: s.Tile.Env}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.Name == "key" {
			t.Errorf("the refused secret was written: %+v", p)
		}
	}
}

// The window links to the Releases screen at its stack, and a viewer who
// cannot promote is offered "View plan", not "Review plan".
func TestReleasesWindowAllLinkAndViewOnly(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	staging := stagingOf(t, s)
	r1, r2 := s.Release(t, s.Tile.Stack, 1), s.Release(t, s.Tile.Stack, 2)
	for env, rid := range map[string]string{s.Tile.Env: r2, staging.ID: r1} {
		row, err := s.Store.Environments.Get(ctx, env)
		if err != nil {
			t.Fatal(err)
		}
		row.ReleaseID = &rid
		if err := s.Store.Environments.Update(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	body := get(t, s, "/acme/shop")
	dialog := `hx-get="/acme/shop/staging/-/releases/` + r2 + `/dialog"`
	if !strings.Contains(body, `href="/-/releases?scope=acme%2Fshop"`) || !strings.Contains(body, ">All releases<") || !strings.Contains(body, dialog) || !strings.Contains(body, `id="rel-modal"`) {
		t.Errorf("owner's window:\n%s", body)
	}
	if strings.Contains(body, "Updates by itself") || strings.Contains(body, ">Review plan for staging<") {
		t.Errorf("the window keeps a footer note or an inline review link:\n%s", body)
	}
	viewer := s.User(t, "viewer@acme.test", false)
	s.Member(t, s.Org, viewer, "viewer")
	rec := s.As(t, s.Session(t, viewer), "GET", "/acme/shop", nil)
	if b := rec.Body.String(); !strings.Contains(b, dialog) || strings.Contains(b, ">View plan for staging<") {
		t.Errorf("viewer's window:\n%s", b)
	}
	// the viewer's modal offers no Promote
	if b := s.As(t, s.Session(t, viewer), "GET", "/acme/shop/staging/-/releases/"+r2+"/dialog", nil).Body.String(); !strings.Contains(b, "Open full review") || strings.Contains(b, ">Promote<") {
		t.Errorf("viewer's modal:\n%s", b)
	}
}

// A later release that lands on the env (a push, not a promote) takes the
// failure away: the failed promote of release 3 says nothing once the env
// runs release 4.
func TestRolloutStopClearsWhenALaterReleaseLands(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	rel := s.Release(t, s.Tile.Stack, 3)
	s.PromoteJob(t, s.Tile.Env, "failed", "deploy api: port 80 is taken", `{"stopped_at":"api"}`, rel, time.Now())
	if body := get(t, s, "/acme/shop/dev"); !strings.Contains(body, "Promote of release 3 stopped at api.") {
		t.Fatalf("no failure to clear:\n%s", body)
	}
	later := s.Release(t, s.Tile.Stack, 4)
	row, err := s.Store.Environments.Get(ctx, s.Tile.Env)
	if err != nil {
		t.Fatal(err)
	}
	row.ReleaseID = &later
	if err := s.Store.Environments.Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	if body := get(t, s, "/acme/shop/dev"); strings.Contains(body, "stopped at") {
		t.Errorf("a later release left the failure:\n%s", body)
	}
}
