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

	rec := s.As(t, root, "GET", "/-/admin?tab=connectors", nil)
	body := rec.Body.String()
	for _, want := range []string{
		`id="tab-connectors"`, "main", "https://github.com/apps/stackr-main/installations/new",
		"All organizations", "Every owner of every organization", `name="org" value="` + s.Org + `"`,
		`hx-post="/-/admin/connectors"`,
	} {
		if rec.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("connectors tab = %d, no %q in\n%s", rec.Code, want, body)
		}
	}

	// the owner of an org is no admin
	for _, c := range []struct{ method, path string }{
		{"GET", "/-/admin?tab=connectors"},
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
		!strings.Contains(rec.Body.String(), `value="`+s.Org+`" checked`) {
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
		!strings.Contains(rec.Body.String(), `name="manifest"`) {
		t.Fatalf("begin = %d\n%s", rec.Code, rec.Body)
	}
	cs, _ := s.Orch.ServerConnectors(ctx)
	if len(cs) != 2 {
		t.Fatalf("connectors after begin = %+v", cs)
	}
	rec = s.As(t, root, "GET", "/-/admin?tab=connectors", nil)
	if !strings.Contains(rec.Body.String(), "pending") {
		t.Errorf("a pending connector is not marked:\n%s", rec.Body)
	}

	rec = s.As(t, root, "POST", base+srv+"/delete", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Connector removed.") {
		t.Errorf("remove = %d\n%s", rec.Code, rec.Body)
	}
	if cs, _ := s.Orch.ServerConnectors(ctx); len(cs) != 1 {
		t.Errorf("connectors after remove = %+v", cs)
	}
}
