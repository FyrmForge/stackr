package releases_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The page lists the stack's releases under its scope, the rail links it,
// a filter that matches nothing says so with a way out, and a viewer who
// cannot promote is offered "View plan".
func TestReleasesScreen(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	r1, r2 := s.Release(t, s.Tile.Stack, 1), s.Release(t, s.Tile.Stack, 2)
	// dev runs release 2, staging release 1: release 2 waits for staging
	staging, err := s.Orch.CreateEnv(ctx, s.Tile.Stack, "staging", service.EnvSpec{Type: "static", FromKind: "promote"})
	if err != nil {
		t.Fatal(err)
	}
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

	rec := s.Do(t, "GET", "/-/releases", nil)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d\n%s", rec.Code, body)
	}
	for _, want := range []string{`href="/-/releases"`, "All of ", `id="rel-screen"`, `data-live-tab`, ">shop<", ">running<", "git push", ">Search commit or message<"} {
		if want == ">Search commit or message<" {
			want = `placeholder="Search commit or message"`
		}
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q:\n%s", want, body)
		}
	}
	empty := s.Do(t, "GET", "/-/releases?scope=acme/shop&state=failed&q=zz", nil).Body.String()
	if !strings.Contains(empty, "No releases match failed, &#34;zz&#34; in ") || !strings.Contains(empty, "Clear filters") {
		t.Errorf("filtered to nothing:\n%s", empty)
	}
	viewer := s.User(t, "viewer@acme.test", false)
	s.Member(t, s.Org, viewer, "viewer")
	// the row links the review as a page of its own, for that exact release
	page := "/acme/shop/staging/-/releases/" + r2
	if !strings.Contains(body, ">Review plan<") || !strings.Contains(body, `href="`+page+`"`) {
		t.Errorf("owner's waiting row:\n%s", body)
	}
	if d := s.Do(t, "GET", page, nil); d.Code != http.StatusOK || !strings.Contains(d.Body.String(), "Release 2 for staging") || !strings.Contains(d.Body.String(), `id="drawer-view"`) {
		t.Errorf("the review page = %d:\n%s", d.Code, d.Body)
	}
	vb := s.As(t, s.Session(t, viewer), "GET", "/-/releases?state=waiting", nil).Body.String()
	if strings.Contains(vb, "Review plan") || !strings.Contains(vb, ">View plan<") {
		t.Errorf("viewer sees Review plan:\n%s", vb)
	}
}
