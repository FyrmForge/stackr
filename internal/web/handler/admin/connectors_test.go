package admin_test

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The Connectors tab is admin-only: it lists the server's connectors, starts
// one, renames, shares with named orgs or all, and removes. An org owner
// reaches none of it.
func TestAdminConnectors(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	srv := s.ServerConnector(t, "main", "sec")
	root := s.Session(t, s.User(t, "root@x.test", true))
	const base = "/-/admin/connectors/"

	// the list: one row, its summary and cogwheel, and the add button
	rec := s.As(t, root, "GET", "/-/admin?tab=connectors", nil)
	body := rec.Body.String()
	for _, want := range []string{
		`id="tab-connectors"`, `id="connectors-list"`, "main", "Not shared yet",
		`hx-get="/-/admin/connectors/` + srv + `/settings"`, `aria-label="Settings for main"`,
		`hx-get="/-/admin/connectors/new"`, "+ Add connector",
	} {
		if rec.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("connectors tab = %d, no %q in\n%s", rec.Code, want, body)
		}
	}
	if strings.Contains(body, "Every owner of every organization") || strings.Contains(body, `name="github_org"`) {
		t.Errorf("the list carries a form:\n%s", body)
	}

	// the cogwheel: sharing, the GitHub link, Remove
	rec = s.As(t, root, "GET", base+srv+"/settings", nil)
	body = rec.Body.String()
	for _, want := range []string{
		"<confirm-dialog open>", "https://github.com/apps/stackr-main/installations/new",
		"All organizations", "Every owner of every organization", `name="org" value="` + s.Org + `"`,
		`hx-post="/-/admin/connectors/` + srv + `/delete"`, `hx-trigger="close from:#connector-dialog"`,
	} {
		if rec.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("settings = %d, no %q in\n%s", rec.Code, want, body)
		}
	}

	// the add dialog: the kind picker, then GitHub's first screen
	rec = s.As(t, root, "GET", base+"new", nil)
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, "<confirm-dialog open>") ||
		!strings.Contains(body, `hx-get="/-/admin/connectors/new/github"`) || !strings.Contains(body, `hx-get="/-/admin/connectors/new/namecheap"`) {
		t.Fatalf("add dialog = %d\n%s", rec.Code, body)
	}
	if body := s.As(t, root, "GET", base+"new?pick=1", nil).Body.String(); strings.Contains(body, "<confirm-dialog") || !strings.Contains(body, "Namecheap") {
		t.Errorf("Back to the picker:\n%s", body)
	}
	rec = s.As(t, root, "GET", base+"new/github", nil)
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(body, `name="github_org"`) ||
		!strings.Contains(body, `hx-post="/-/admin/connectors"`) || !strings.Contains(body, "Step 1 of 2") {
		t.Fatalf("GitHub step = %d\n%s", rec.Code, body)
	}

	// the owner of an org is no admin
	for _, c := range []struct{ method, path string }{
		{"GET", "/-/admin?tab=connectors"},
		{"GET", base + "new"},
		{"GET", base + "new/github"},
		{"GET", base + srv + "/settings"},
		{"POST", "/-/admin/connectors"},
		{"POST", base + srv + "/share"},
		{"POST", base + srv + "/name"},
		{"POST", base + srv + "/delete"},
	} {
		if rec := s.Do(t, c.method, c.path, url.Values{"all": {"1"}, "name": {"x"}}); rec.Code != http.StatusForbidden {
			t.Errorf("owner %s %s = %d, want 403", c.method, c.path, rec.Code)
		}
	}
	if cs, _ := s.Orch.ServerConnectors(ctx); len(cs) != 1 || cs[0].ShareAll || len(cs[0].OrgIDs) != 0 || cs[0].Name != "main" {
		t.Fatalf("the owner changed a connector: %+v", cs)
	}

	rec = s.As(t, root, "POST", base+srv+"/share", url.Values{"org": {s.Org}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Saved.") ||
		!strings.Contains(rec.Body.String(), "Shared with 1 organization") {
		t.Fatalf("share with an org = %d\n%s", rec.Code, rec.Body)
	}
	if cs, _ := s.Orch.ServerConnectors(ctx); !slices.Equal(cs[0].OrgIDs, []string{s.Org}) {
		t.Errorf("shares = %+v", cs)
	}
	rec = s.As(t, root, "POST", base+srv+"/share", url.Values{"all": {"1"}, "org": {s.Org}})
	if cs, _ := s.Orch.ServerConnectors(ctx); rec.Code != http.StatusOK || !cs[0].ShareAll || len(cs[0].OrgIDs) != 0 {
		t.Errorf("share all = %d %+v", rec.Code, cs)
	}
	if rec := s.As(t, root, "POST", base+srv+"/share", url.Values{"org": {"nope"}}); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("share with an unknown org = %d, want 422", rec.Code)
	}

	rec = s.As(t, root, "POST", base+srv+"/name", url.Values{"name": {"primary"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Renamed.") || !strings.Contains(rec.Body.String(), "primary") {
		t.Errorf("rename = %d\n%s", rec.Code, rec.Body)
	}
	if rec := s.As(t, root, "POST", base+srv+"/name", url.Values{"name": {" "}}); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("blank rename = %d, want 422", rec.Code)
	}

	// begin: the manifest form that posts to GitHub
	rec = s.As(t, root, "POST", "/-/admin/connectors", url.Values{"github_org": {"acme-inc"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Continue on GitHub") ||
		!strings.Contains(rec.Body.String(), `name="manifest"`) || !strings.Contains(rec.Body.String(), "Step 2 of 2") ||
		strings.Contains(rec.Body.String(), `id="drawer-view"`) {
		t.Fatalf("begin = %d\n%s", rec.Code, rec.Body)
	}
	cs, _ := s.Orch.ServerConnectors(ctx)
	if len(cs) != 2 {
		t.Fatalf("connectors after begin = %+v", cs)
	}
	for _, k := range cs {
		if k.ID != srv && !k.ShareAll {
			t.Errorf("a connector begun in Admin is not shared with every org: %+v", k)
		}
	}
	rec = s.As(t, root, "GET", "/-/admin?tab=connectors", nil)
	if !strings.Contains(rec.Body.String(), "pending") {
		t.Errorf("a pending connector is not marked:\n%s", rec.Body)
	}
	// set up server: GitHub's install return lands on the Connectors tab
	if rec := s.As(t, root, "GET", "/settings/github/installed/"+cs[1].ID, nil); rec.Header().Get("Location") != "/?drawer=admin&tab=connectors" {
		t.Errorf("installed = %d %q", rec.Code, rec.Header().Get("Location"))
	}

	rec = s.As(t, root, "POST", base+srv+"/delete", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Connector removed.") {
		t.Errorf("remove = %d\n%s", rec.Code, rec.Body)
	}
	if cs, _ := s.Orch.ServerConnectors(ctx); len(cs) != 1 {
		t.Errorf("connectors after remove = %+v", cs)
	}
}
