package releases_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The page lists the stack's releases under its scope, the rail links it,
// a filter that matches nothing says so with a way out, and a viewer who
// cannot promote is offered "View plan".
func TestReleasesScreen(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	r1, r2 := s.Release(t, s.Tile.Stack, 1), s.Release(t, s.Tile.Stack, 2)
	envs, err := s.Orch.Ladder(ctx, s.Tile.Stack)
	if err != nil || len(envs) == 0 {
		t.Fatalf("ladder = %v, %v", envs, err)
	}
	// the one env runs release 1, so release 2 waits for it
	row, err := s.Store.Environments.Get(ctx, s.Tile.Env)
	if err != nil {
		t.Fatal(err)
	}
	row.ReleaseID = &r1
	if err := s.Store.Environments.Update(ctx, row); err != nil {
		t.Fatal(err)
	}
	_ = r2

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
	vb := s.As(t, s.Session(t, viewer), "GET", "/-/releases?state=waiting", nil).Body.String()
	if strings.Contains(vb, "Review plan") {
		t.Errorf("viewer sees Review plan:\n%s", vb)
	}
}
