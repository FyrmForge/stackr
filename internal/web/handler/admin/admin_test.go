package admin_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/api/stream"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The admin drawer: admin-only, every tab renders, the settings form
// writes what changed, users flip, a panel backup streams its job.
func TestAdminDrawer(t *testing.T) {
	stream.PollEvery = 10 * time.Millisecond
	s := webtest.New(t)
	ctx := context.Background()
	if rec := s.Do(t, "GET", "/-/admin", nil); rec.Code != http.StatusForbidden {
		t.Errorf("owner opens admin = %d, want 403", rec.Code)
	}
	root := s.Session(t, s.User(t, "root@x.test", true))
	for tab, want := range map[string]string{
		"settings": `id="admin-settings"`,
		"users":    "owner@acme.test",
		"update":   "/-/admin/update/check",
		"routes":   `id="tab-routes"`,
		"caddy":    `name="proxy_custom"`,
		"backups":  "No panel backups yet",
		"config":   `id="tab-config"`,
	} {
		rec := s.As(t, root, "GET", "/-/admin?tab="+tab, nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("tab %s = %d, no %s in\n%s", tab, rec.Code, want, rec.Body)
		}
	}
	settings := s.As(t, root, "GET", "/-/admin?tab=settings", nil).Body.String()
	if !strings.Contains(settings, `name="root_domain"`) {
		t.Error("root_domain is not an input; the server file may set it")
	}
	if strings.Contains(settings, `name="server_config_`) || strings.Contains(settings, `name="dns_env"`) {
		t.Error("the Settings tab shows the server file's binding or the dead dns_env knob")
	}
	if body := s.As(
		t,
		root,
		"GET",
		"/acme",
		nil,
	).Body.String(); !strings.Contains(body, `hx-get="/-/admin?tab=settings"`) {
		t.Error("no admin button in the nav")
	}

	rec := s.As(t, root, "POST", "/-/admin/routes", url.Values{"route_host": {"pve.example.com"}, "route_mode": {"https"}, "route_target": {"10.0.0.5:8006"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Route added.") || !strings.Contains(rec.Body.String(), "10.0.0.5:8006") {
		t.Fatalf("add route = %d %s", rec.Code, rec.Body)
	}
	rs, err := s.Orch.ExternalRoutes(ctx)
	if err != nil || len(rs) != 1 {
		t.Fatalf("routes = %v, %v", rs, err)
	}
	if rec := s.As(t, root, "POST", "/-/admin/routes", url.Values{"route_host": {"x.io"}, "route_mode": {"tcp"}}); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad route = %d, want 422", rec.Code)
	}
	if rec := s.As(t, root, "POST", "/-/admin/routes/"+rs[0].ID+"/delete", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Route removed.") {
		t.Errorf("remove route = %d %s", rec.Code, rec.Body)
	}

	rec = s.As(t, root, "POST", "/-/admin/settings", url.Values{"workers": {"3"}, "cpu_limit": {""}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Saved.") {
		t.Fatalf("save = %d %s", rec.Code, rec.Body)
	}
	if v, _ := s.Orch.Setting(ctx, "workers"); v != "3" {
		t.Errorf("workers = %q", v)
	}
	if rec := s.As(t, root, "POST", "/-/admin/settings", url.Values{
		"workers": {"lots"},
	}); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "not a whole number") {
		t.Errorf("bad value = %d %s", rec.Code, rec.Body)
	}

	var owner string
	us, _ := s.Orch.Users(ctx)
	for _, u := range us {
		if u.Email == "owner@acme.test" {
			owner = u.ID
		}
	}
	if rec := s.As(t, root, "POST", "/-/admin/users/"+owner+"/admin?admin=true", nil); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "Remove admin") {
		t.Errorf("make admin = %d", rec.Code)
	}

	if rec := s.As(t, root, "POST", "/-/admin/users/"+owner+"/disable", nil); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "/users/"+owner+"/enable") {
		t.Errorf("disable = %d, no enable button in\n%s", rec.Code, rec.Body)
	}
	if rec := s.As(t, root, "POST", "/-/admin/users/"+owner+"/enable", nil); rec.Code != http.StatusOK ||
		strings.Contains(rec.Body.String(), "/users/"+owner+"/enable") {
		t.Errorf("enable = %d, row did not flip in\n%s", rec.Code, rec.Body)
	}

	rec = s.As(t, root, "POST", "/-/admin/backups", nil)
	m := regexp.MustCompile(`sse-connect="(/-/jobs/[^"]+/events)"`).FindStringSubmatch(rec.Body.String())
	if rec.Code != http.StatusOK || m == nil {
		t.Fatalf("backup now = %d %s", rec.Code, rec.Body)
	}
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest("GET", m[1], nil).WithContext(cctx)
	req.AddCookie(&http.Cookie{Name: s.Orch.Sessions().CookieName(), Value: root})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "event: update") {
		t.Errorf("job stream:\n%s", w.Body)
	}

	// A fresh load of ?drawer=admin opens it on any page.
	req = httptest.NewRequest("GET", "/acme?drawer=admin&tab=users", nil)
	req.AddCookie(&http.Cookie{Name: s.Orch.Sessions().CookieName(), Value: root})
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), `hx-get="/-/admin?tab=users"`) {
		t.Errorf("fresh load has no admin drawer:\n%s", w.Body)
	}
}

// The Backups tab edits the panel backup switch, schedule, keep and
// destination, refuses a bad line (saving nothing) and shows the last run
// with where it went.
func TestAdminBackupSchedule(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	root := s.Session(t, s.User(t, "root@x.test", true))
	tab := s.As(t, root, "GET", "/-/admin?tab=backups", nil).Body.String()
	for _, want := range []string{
		`name="panel_backup_enabled"`, `name="panel_backup_schedule"`, `value="0 3 * * *"`,
		`name="panel_backup_keep"`, `name="panel_backup_dest"`, "Last run",
	} {
		if !strings.Contains(tab, want) {
			t.Errorf("backups tab has no %s", want)
		}
	}
	if !regexp.MustCompile(`name="panel_backup_enabled"[^>]*checked`).MatchString(tab) {
		t.Error("the switch is not on by default")
	}
	post := func(on, sched, keep string) *httptest.ResponseRecorder {
		f := url.Values{"panel_backup_schedule": {sched}, "panel_backup_keep": {keep}, "panel_backup_dest": {""}}
		if on != "" {
			f.Set("panel_backup_enabled", on)
		}
		return s.As(t, root, "POST", "/-/admin/backups/schedule", f)
	}
	if rec := post("1", "0 5 * * *", "7"); rec.Code != http.StatusOK {
		t.Fatalf("save = %d %s", rec.Code, rec.Body)
	}
	if v, _ := s.Orch.Setting(ctx, "panel_backup_schedule"); v != "0 5 * * *" {
		t.Errorf("schedule = %q", v)
	}
	if v, _ := s.Orch.Setting(ctx, "panel_backup_keep"); v != "7" {
		t.Errorf("keep = %q", v)
	}
	if rec := post("1", "nonsense", "9"); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad cron = %d", rec.Code)
	}
	if v, _ := s.Orch.Setting(ctx, "panel_backup_keep"); v != "7" {
		t.Errorf("keep = %q after a refused save, want 7", v)
	}
	if rec := post("", "", "7"); rec.Code != http.StatusOK {
		t.Fatalf("switch off = %d %s", rec.Code, rec.Body)
	}
	if v, _ := s.Orch.Setting(ctx, "panel_backup_enabled"); v != "false" {
		t.Errorf("enabled = %q, want false", v)
	}
	if v, _ := s.Orch.Setting(ctx, "panel_backup_schedule"); v != "0 3 * * *" {
		t.Errorf("an empty schedule = %q, want the default time", v)
	}

	// Run one: the last-run line says where it went.
	s.As(t, root, "POST", "/-/admin/backups", nil)
	deadline := time.Now().Add(20 * time.Second)
	for {
		tab = s.As(t, root, "GET", "/-/admin?tab=backups", nil).Body.String()
		if strings.Contains(tab, "done, to local") || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(tab, "done, to local") {
		t.Errorf("the last-run line does not say where it went:\n%s", tab)
	}
}

// The Caddy tab saves proxy_custom as a JSON array of routes and refuses
// anything else, keeping what was stored.
func TestAdminCaddyCustom(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	root := s.Session(t, s.User(t, "root@x.test", true))
	route := `[{"match":[{"host":["extra.example.com"]}],"handle":[{"handler":"static_response","body":"hi"}]}]`
	rec := s.As(t, root, "POST", "/-/admin/caddy", url.Values{"proxy_custom": {route}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Saved.") {
		t.Fatalf("save = %d %s", rec.Code, rec.Body)
	}
	if rec := s.As(t, root, "POST", "/-/admin/caddy", url.Values{"proxy_custom": {"{oops"}}); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "JSON array") {
		t.Errorf("bad json = %d %s", rec.Code, rec.Body)
	}
	if v, _ := s.Orch.Setting(ctx, "proxy_custom"); v != route {
		t.Errorf("stored = %q, want the last good value", v)
	}
	if tab := s.As(t, root, "GET", "/-/admin?tab=caddy", nil).Body.String(); strings.Contains(tab, "does not read it") {
		t.Error("the tab still says the proxy ignores proxy_custom")
	}
}

// serverPlan seeds a pending plan row of the server file.
func serverPlan(t *testing.T, s *webtest.Site, source, plan string) service.ServerPlan {
	t.Helper()
	p := service.ServerPlan{
		ID: uuid.NewString(), Status: "pending", Source: source, Plan: plan, Ticked: []string{},
		Summary: "1 to change, 1 removal to review, needs confirm", CreatedAt: time.Now(),
	}
	if err := s.Store.ServerPlans.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

const riskyServerPlan = `{"changes":[` +
	`{"kind":"setting","field":"panel_domain","old":"a.test","new":"b.test","impact":"panel moves to b.test; point DNS first"},` +
	`{"kind":"route-delete","field":"pve.example.com","key":"route:pve.example.com","optional":true}]}`

// The Config tab: the export link and the binding form, an unticked
// removal box and an "are you sure?" on a plan with impact lines, a local
// plan says it did not come from the repo; the approve takes the ticks and
// the confirm, and the service stamps both. Admins only.
func TestAdminConfigTab(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	rootID := s.User(t, "root@x.test", true)
	root := s.Session(t, rootID)
	if rec := s.Do(t, "GET", "/-/admin?tab=config", nil); rec.Code != http.StatusForbidden {
		t.Errorf("owner opens the config tab = %d, want 403", rec.Code)
	}
	body := s.As(t, root, "GET", "/-/admin?tab=config", nil).Body.String()
	for _, want := range []string{`id="tab-config"`, `href="/api/v1/admin/config/export"`, "stackr-server.yml"} {
		if !strings.Contains(body, want) {
			t.Errorf("tab lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Latest plan") {
		t.Error("a latest plan with none made")
	}

	p := serverPlan(t, s, "local", riskyServerPlan)
	approve := "/-/admin/config/plans/" + p.ID + "/approve"
	body = s.As(t, root, "GET", "/-/admin?tab=config", nil).Body.String()
	for _, want := range []string{
		"Latest plan", "From a local file, not the repo.", `name="ticked"`, `value="route:pve.example.com"`,
		"panel moves to b.test; point DNS first", "Are you sure?", `hx-post="` + approve + `"`,
		`hx-post="/-/admin/config/plans/` + p.ID + `/reject"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plan view lacks %q:\n%s", want, body)
		}
	}
	if regexp.MustCompile(`name="ticked"[^>]*checked`).MatchString(body) {
		t.Error("a removal is ticked by default")
	}

	if rec := s.As(t, root, "POST", approve, url.Values{"ticked": {"route:pve.example.com"}}); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "impact lines") {
		t.Errorf("approve without confirm = %d\n%s", rec.Code, rec.Body)
	}
	if rec := s.As(t, root, "POST", approve, url.Values{"ticked": {"route:ghost"}, "confirm": {"1"}}); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("approve with a stray tick = %d\n%s", rec.Code, rec.Body)
	}
	if got, err := s.Store.ServerPlans.Get(ctx, p.ID); err != nil || got.Status != "pending" || len(got.Ticked) != 0 {
		t.Fatalf("a refused approve changed the row: %+v %v", got, err)
	}
	rec := s.As(t, root, "POST", approve, url.Values{"ticked": {"route:pve.example.com"}, "confirm": {"1"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Approved: the apply is queued.") ||
		!strings.Contains(rec.Body.String(), "server-apply") {
		t.Fatalf("approve = %d\n%s", rec.Code, rec.Body)
	}
	// the approver owns the orgs the apply creates: the job carries who they are
	js, err := s.Store.Jobs.ListTouching(ctx, []string{"serverconfig"}, "server-apply", 1)
	if err != nil || len(js) != 1 || !strings.Contains(js[0].Payload, rootID) {
		t.Errorf("server-apply job = %+v %v, want the approver %s in its payload", js, err, rootID)
	}
	got, err := s.Store.ServerPlans.Get(ctx, p.ID)
	if err != nil || !got.Confirmed || len(got.Ticked) != 1 || got.Ticked[0] != "route:pve.example.com" {
		t.Errorf("row after approve = %+v %v, want confirmed with the route ticked", got, err)
	}

	// reject closes a pending plan; a blocked one offers no approve
	q := serverPlan(t, s, "repo", `{"changes":[],"blockers":["dest offsite needs server.params.s3.secret_key"]}`)
	body = s.As(t, root, "GET", "/-/admin?tab=config", nil).Body.String()
	if !strings.Contains(body, "dest offsite needs") || strings.Contains(body, "/plans/"+q.ID+"/approve") {
		t.Errorf("blocked plan:\n%s", body)
	}
	rec = s.As(t, root, "POST", "/-/admin/config/plans/"+q.ID+"/reject", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Plan rejected.") {
		t.Fatalf("reject = %d\n%s", rec.Code, rec.Body)
	}
	if got, _ := s.Store.ServerPlans.Get(ctx, q.ID); got.Status != "rejected" {
		t.Errorf("after reject = %s", got.Status)
	}
}

// The Config tab's binding form: the select lists the server connectors,
// Save binds and plans, Unbind clears the binding and rejects the plans
// nobody approved. Only an admin binds.
func TestAdminConfigBind(t *testing.T) {
	g := servicetest.NewGit(t)
	s := webtest.NewWith(t, []service.Option{g.Option()})
	ctx := context.Background()
	root := s.Session(t, s.User(t, "root@x.test", true))
	conn := s.ServerConnector(t, "shared", "whsec")
	g.Commit(t, "acme/server", "main", map[string]string{"stackr-server.yml": "version: 1\n"})

	body := s.As(t, root, "GET", "/-/admin?tab=config", nil).Body.String()
	if !strings.Contains(body, `value="`+conn+`"`) || !strings.Contains(body, "(unbind)") || strings.Contains(body, "Create a server connector first") {
		t.Fatalf("unbound tab:\n%s", body)
	}
	form := url.Values{"connector": {conn}, "repo": {"acme/server"}, "path": {"infra/server.yml"}, "auto": {"1"}}
	if rec := s.Do(t, "POST", "/-/admin/config", form); rec.Code != http.StatusForbidden {
		t.Errorf("owner binds = %d, want 403", rec.Code)
	}
	rec := s.As(t, root, "POST", "/-/admin/config", form)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Config file saved and planned.") {
		t.Fatalf("bind = %d\n%s", rec.Code, rec.Body)
	}
	bd, err := s.Orch.ServerConfigBinding(ctx)
	if err != nil || bd.ConnectorID != conn || !strings.HasSuffix(bd.Repo, "acme/server") || bd.Path != "infra/server.yml" || !bd.Auto {
		t.Errorf("binding = %+v %v", bd, err)
	}
	for _, w := range []string{"acme/server", "Plan now", "/-/admin/config/unbind", "Latest plan"} {
		if !strings.Contains(rec.Body.String(), w) {
			t.Errorf("bound tab lacks %q:\n%s", w, rec.Body)
		}
	}

	rec = s.As(t, root, "POST", "/-/admin/config/unbind", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Unbound") {
		t.Fatalf("unbind = %d\n%s", rec.Code, rec.Body)
	}
	if bd, _ := s.Orch.ServerConfigBinding(ctx); bd != (service.ServerBinding{}) {
		t.Errorf("binding after unbind = %+v", bd)
	}
	ps, err := s.Orch.ServerPlans(ctx, 5)
	if err != nil || len(ps) == 0 || ps[0].Status == "pending" {
		t.Errorf("plans after unbind = %+v %v, want none pending", ps, err)
	}
}
