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
	if !strings.Contains(body, `hx-get="`+page+`/dialog"`) || !strings.Contains(body, `id="rel-modal"`) || strings.Contains(body, ">Review plan<") {
		t.Errorf("owner's waiting row:\n%s", body)
	}
	if d := s.Do(t, "GET", page, nil); d.Code != http.StatusOK || !strings.Contains(d.Body.String(), "Release 2 for staging") || !strings.Contains(d.Body.String(), `id="drawer-view"`) {
		t.Errorf("the review page = %d:\n%s", d.Code, d.Body)
	}
	vb := s.As(t, s.Session(t, viewer), "GET", "/-/releases?state=waiting", nil).Body.String()
	if strings.Contains(vb, "Review plan") || !strings.Contains(vb, `hx-get="`+page+`/dialog"`) {
		t.Errorf("viewer's waiting row:\n%s", vb)
	}
}

// The title is in the top bar, not the page; 50 rows fit a page, more bring a
// pager whose links page the list and whose filters start over at page 1.
func TestReleasesScreenPages(t *testing.T) {
	s := webtest.New(t)
	for n := 1; n <= 50; n++ {
		s.Release(t, s.Tile.Stack, n)
	}
	small := s.Do(t, "GET", "/-/releases", nil).Body.String()
	if !strings.Contains(small, `<h1 class="text-lg font-semibold text-rw-text tracking-tight">Releases</h1>`) || strings.Contains(small, "<h2") {
		t.Errorf("title belongs in the top bar:\n%s", small)
	}
	if strings.Contains(small, `aria-label="Pagination"`) {
		t.Errorf("50 rows need no pager:\n%s", small)
	}
	s.Release(t, s.Tile.Stack, 51)
	p1 := s.Do(t, "GET", "/-/releases", nil).Body.String()
	if !strings.Contains(p1, "1-50 of 51") || !strings.Contains(p1, `>Older</a>`) || strings.Contains(p1, `>Newer</a>`) || !strings.Contains(p1, `hx-get="/-/releases?page=2"`) {
		t.Errorf("page 1:\n%s", p1)
	}
	if !strings.Contains(p1, `hx-get="/-/releases?state=failed"`) && strings.Contains(p1, "page=2&") {
		t.Errorf("filter links must drop the page:\n%s", p1)
	}
	for _, u := range []string{"/-/releases?page=2", "/-/releases?page=9"} {
		p2 := s.Do(t, "GET", u, nil).Body.String()
		if !strings.Contains(p2, "51-51 of 51") || !strings.Contains(p2, `>Newer</a>`) || strings.Contains(p2, `>Older</a>`) {
			t.Errorf("%s:\n%s", u, p2)
		}
	}
	if bad := s.Do(t, "GET", "/-/releases?page=x", nil).Body.String(); !strings.Contains(bad, "1-50 of 51") {
		t.Errorf("a bad page is page 1:\n%s", bad)
	}
}
