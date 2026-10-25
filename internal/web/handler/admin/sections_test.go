package admin_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	ui "github.com/FyrmForge/stackr/internal/ui/drawer/admin"
	"github.com/FyrmForge/stackr/internal/web/handler/admin"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// Every section renders its own fields and nothing aims at the drawer's
// root, which a node drawer over the page still owns: an htmx target that
// is missing never sends its request.
func TestAdminSections(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	want := map[string][]string{
		"overview":     {"Open problems", "Alerts webhook", `href="/-/setup/server/ui/domains"`},
		"users":        {"owner@acme.test"},
		"host-access":  {`id="tab-access"`, "Grants"},
		"nodes":        {`id="tab-nodes"`, "Add node"},
		"placements":   {`id="tab-placements"`, `name="share" value="all"`},
		"traffic":      {"External routes", `name="route_host"`},
		"domains":      {`name="panel_domain"`, `name="root_domain"`, `name="acme_email"`, `name="trusted_proxies"`, `name="ipv6"`, `<textarea id="proxy_custom"`, "/-/admin/caddy/sync"},
		"connectors":   {"/-/admin/connectors/new"},
		"backups":      {`name="panel_backup_schedule"`, `name="panel_backup_dest"`, "Encrypted to recovery key generation", `href="/-/admin/keys"`, "History"},
		"registry":     {`name="registry_cleanup_schedule"`, `name="registry_keep"`, "Clean up now"},
		"cleanup":      {`name="image_check_interval"`, `name="cleanup_enabled"`, `name="cleanup_schedule"`, `name="orphans_enabled"`, `name="orphans_schedule"`, `name="orphan_retention_days"`},
		"metrics":      {`name="observe_enabled"`, `name="observe_logs_enabled"`, `name="observe_access_ip"`, "Keep metrics history"},
		"alerts":       {"Webhook URL", "Heartbeat URL"},
		"keys":         {`id="master-key"`, `id="recovery"`, "Cluster root"},
		"security-log": {`id="tab-security"`},
		"org-defaults": {`name="cpu_limit"`, `name="mem_limit_mb"`, `name="protect"`, `name="protect_password"`},
		"jobs":         {`name="workers"`, `name="backup_run_concurrency"`, `name="backup_restore_concurrency"`},
		"config":       {`id="tab-config"`, `id="vars-editor"`, "Variables"},
		"update":       {`id="tab-update"`, "A dev build cannot upgrade itself"},
	}
	for _, g := range ui.Groups {
		for _, it := range g.Items {
			if _, ok := want[it.Key]; !ok {
				t.Errorf("section %s has no check", it.Key)
			}
		}
	}
	for key, ws := range want {
		rec := s.As(t, root, "GET", "/-/admin/"+key, nil)
		body := rec.Body.String()
		if rec.Code != http.StatusOK || !strings.Contains(body, `id="admin-view"`) {
			t.Errorf("%s = %d, not the view\n%s", key, rec.Code, body)
			continue
		}
		for _, w := range ws {
			if !strings.Contains(body, w) {
				t.Errorf("%s: no %s", key, w)
			}
		}
		if strings.Contains(body, `hx-target="#drawer-view"`) {
			t.Errorf("%s aims a request at the drawer's root", key)
		}
	}

	// a section's form holds its own knobs only
	if b := s.As(t, root, "GET", "/-/admin/domains", nil).Body.String(); strings.Contains(b, `name="workers"`) || strings.Contains(b, `name="cpu_limit"`) {
		t.Error("Domains shows another section's knobs")
	}
	// a flat knob has no rung above it: no Inherit choice, no "inherit" placeholder
	if b := s.As(t, root, "GET", "/-/admin/cleanup", nil).Body.String(); strings.Contains(b, `placeholder="inherit"`) || strings.Contains(b, "Inherit (") {
		t.Error("a flat knob offers to inherit")
	}
	if b := s.As(t, root, "GET", "/-/admin/org-defaults", nil).Body.String(); !strings.Contains(b, "Inherit (") {
		t.Error("an org default lost its Inherit choice")
	}

	// a plain load is the page, with the side menu and its group headings
	req := httptest.NewRequest("GET", "/-/admin/backups", nil)
	req.AddCookie(&http.Cookie{Name: s.Orch.Sessions().CookieName(), Value: root})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	for _, x := range []string{"<html", "Server settings", `aria-label="Server settings"`, ">Monitoring<", `aria-current="page"`, `href="/-/admin/security-log"`} {
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), x) {
			t.Errorf("page = %d, no %s", w.Code, x)
		}
	}

	// the drawer is gone; an unknown section is not found
	for _, p := range []string{"/-/admin", "/-/admin?tab=settings", "/-/admin/settings", "/-/admin/nope"} {
		if rec := s.As(t, root, "GET", p, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}

	// the rail's gear goes to the page
	if b := s.As(t, root, "GET", "/acme", nil).Body.String(); !strings.Contains(b, `href="/-/admin/overview"`) || strings.Contains(b, "drawer=admin") {
		t.Error("the rail does not link to Server settings")
	}
}

// Every server knob is edited in exactly one section.
func TestAdminKnobsHaveOneHome(t *testing.T) {
	s := webtest.New(t)
	ss, err := s.Orch.ServerSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	home := map[string][]string{}
	for form, keys := range admin.KnobForms {
		for _, k := range keys {
			home[k] = append(home[k], form)
		}
	}
	for _, k := range admin.OwnEditor {
		home[k] = append(home[k], "own editor")
	}
	for _, x := range ss {
		if len(home[x.Key]) != 1 {
			t.Errorf("%s is edited in %v", x.Key, home[x.Key])
		}
		delete(home, x.Key)
	}
	for k := range home {
		t.Errorf("%s is no server knob", k)
	}
}

// A section's badge counts the problems fixed there; Overview counts all.
func TestAdminBadges(t *testing.T) {
	b := admin.Badges([]service.Problem{
		{Key: "backup.panel", Severity: service.SevCritical, Link: "/-/admin/backups"},
		{Key: "disk.n1", Severity: service.SevWarning, Link: "?drawer=node:n1"},
		{Key: "node.n2", Severity: service.SevWarning, Link: "?drawer=node:n2"},
		{Key: "crash.t1", Severity: service.SevWarning, Link: "/acme/shop/prod/api"},
	})
	if o := b["overview"]; o.N != 4 || !o.Critical {
		t.Errorf("overview = %+v", o)
	}
	if x := b["backups"]; x.N != 1 || !x.Critical {
		t.Errorf("backups = %+v", x)
	}
	if x := b["nodes"]; x.N != 2 || x.Critical {
		t.Errorf("nodes = %+v", x)
	}
	if len(b) != 3 {
		t.Errorf("badges = %+v", b)
	}
}
