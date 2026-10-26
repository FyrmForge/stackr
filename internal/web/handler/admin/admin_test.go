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

// The Server settings page: admin-only, the settings forms write what
// changed, users flip, a panel backup streams its job.
func TestAdminPage(t *testing.T) {
	stream.PollEvery = 10 * time.Millisecond
	s := webtest.New(t)
	ctx := context.Background()
	if rec := s.Do(t, "GET", "/-/admin/overview", nil); rec.Code != http.StatusForbidden {
		t.Errorf("owner opens admin = %d, want 403", rec.Code)
	}
	root := s.Session(t, s.User(t, "root@x.test", true))
	domains := s.As(t, root, "GET", "/-/admin/domains", nil).Body.String()
	if !strings.Contains(domains, `name="root_domain"`) {
		t.Error("root_domain is not an input; the server file may set it")
	}
	if strings.Contains(domains, `name="server_config_`) || strings.Contains(domains, `name="dns_env"`) {
		t.Error("Domains shows the server file's binding or the dead dns_env knob")
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

	// a section's form writes its own knobs only
	rec = s.As(t, root, "POST", "/-/admin/settings/jobs", url.Values{"workers": {"3"}, "cpu_limit": {"2"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Saved.") || !strings.Contains(rec.Body.String(), `id="admin-jobs"`) {
		t.Fatalf("save = %d %s", rec.Code, rec.Body)
	}
	if v, _ := s.Orch.Setting(ctx, "workers"); v != "3" {
		t.Errorf("workers = %q", v)
	}
	if d, _ := s.Orch.SettingDefaults(ctx); strings.Contains(d.JSON(), "cpu_limit") {
		t.Errorf("the Jobs form wrote cpu_limit: %s", d.JSON())
	}
	if rec := s.As(t, root, "POST", "/-/admin/settings/jobs", url.Values{
		"workers": {"lots"},
	}); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Use a whole number") {
		t.Errorf("bad value = %d %s", rec.Code, rec.Body)
	}
	if rec := s.As(t, root, "POST", "/-/admin/settings/nope", url.Values{"workers": {"2"}}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown form = %d, want 404", rec.Code)
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
	// A finished job's stream ends by itself, so no deadline can cut the
	// request short before its first event (one did, under a loaded run).
	jobID := strings.TrimSuffix(strings.TrimPrefix(m[1], "/-/jobs/"), "/events")
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		j, err := s.Orch.GetJob(ctx, jobID)
		if err != nil {
			t.Fatal(err)
		}
		if j.FinishedAt != nil {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("backup job still %s", j.State)
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req := httptest.NewRequest("GET", m[1], nil).WithContext(cctx)
	req.AddCookie(&http.Cookie{Name: s.Orch.Sessions().CookieName(), Value: root})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "event: update") {
		t.Errorf("job stream:\n%s", w.Body)
	}

	// a node's drawer opens on any page from a fresh load
	req = httptest.NewRequest("GET", "/acme?drawer=node:n1&tab=envs", nil)
	req.AddCookie(&http.Cookie{Name: s.Orch.Sessions().CookieName(), Value: root})
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), `hx-get="/-/admin/nodes/n1?tab=envs"`) {
		t.Errorf("fresh load has no node drawer:\n%s", w.Body)
	}
}

// The Backups tab edits the panel backup switch, schedule, keep and
// destination, refuses a bad line (saving nothing) and shows the last run
// with where it went.
func TestAdminBackupSchedule(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	root := s.Session(t, s.User(t, "root@x.test", true))
	tab := s.As(t, root, "GET", "/-/admin/backups", nil).Body.String()
	for _, want := range []string{
		`name="panel_backup_enabled"`, `name="panel_backup_schedule"`, `value="0 3 * * *"`,
		`name="panel_backup_keep"`, `name="panel_backup_dest"`, "Last run",
	} {
		if !strings.Contains(tab, want) {
			t.Errorf("backups tab has no %s", want)
		}
	}
	if !regexp.MustCompile(`name="share" value="all" checked`).MatchString(tab) {
		t.Error("a new destination is not offered to every org by default")
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

	// A destination just added is the panel backup form's pick.
	rec := s.As(t, root, "POST", "/-/admin/dests", url.Values{
		"dest_name": {"offsite"}, "endpoint": {"https://s3.example.test"}, "bucket": {"b"}, "region": {"r"},
		"access_key": {"ak"}, "secret_key": {"sk"},
	})
	if !regexp.MustCompile(`<option value="[^"]+" selected>offsite</option>`).MatchString(rec.Body.String()) {
		t.Errorf("the new destination is not picked for the panel backup:\n%s", rec.Body)
	}

	// Run one: the last-run line says where it went.
	s.As(t, root, "POST", "/-/admin/backups", nil)
	deadline := time.Now().Add(20 * time.Second)
	for {
		tab = s.As(t, root, "GET", "/-/admin/backups", nil).Body.String()
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
// A refused save (422) draws its form as typed, and only that form: a
// sibling form posting the same field name keeps what is stored.
func TestAdminRefusedKeepsTyped(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	refused := func(path string, form url.Values, want ...string) string {
		t.Helper()
		rec := s.As(t, root, "POST", path, form)
		body := rec.Body.String()
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s = %d, want 422\n%s", path, rec.Code, body)
		}
		for _, w := range want {
			if !strings.Contains(body, w) {
				t.Errorf("%s: no %s in\n%s", path, w, body)
			}
		}
		return body
	}
	refused("/-/admin/settings/jobs", url.Values{"workers": {"-5"}}, `value="-5"`)
	refused("/-/admin/caddy", url.Values{"proxy_custom": {"{oops"}}, ">{oops</textarea>")
	refused("/-/admin/backups/schedule", url.Values{"panel_backup_schedule": {"not cron"}, "panel_backup_keep": {"4"}},
		`value="not cron"`, `value="4"`)
	refused("/-/admin/routes", url.Values{"route_host": {"bad host"}, "route_mode": {"https"}, "route_target": {"10.0.0.1:1"}, "route_insecure": {"1"}},
		`value="bad host"`, `value="https" selected`, `value="10.0.0.1:1"`)
	// the webhook's refused URL stays in the webhook field, not the heartbeat's
	body := refused("/-/admin/alerts", url.Values{"url": {"nope"}}, `value="nope"`)
	if strings.Count(body, `value="nope"`) != 1 || strings.Index(body, `value="nope"`) > strings.Index(body, "Heartbeat URL") {
		t.Errorf("the refused webhook URL is not in the webhook field alone:\n%s", body)
	}
}

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
	if tab := s.As(t, root, "GET", "/-/admin/domains", nil).Body.String(); strings.Contains(tab, "does not read it") {
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
	if rec := s.Do(t, "GET", "/-/admin/config", nil); rec.Code != http.StatusForbidden {
		t.Errorf("owner opens the config tab = %d, want 403", rec.Code)
	}
	body := s.As(t, root, "GET", "/-/admin/config", nil).Body.String()
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
	body = s.As(t, root, "GET", "/-/admin/config", nil).Body.String()
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
	body = s.As(t, root, "GET", "/-/admin/config", nil).Body.String()
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

	body := s.As(t, root, "GET", "/-/admin/config", nil).Body.String()
	if !strings.Contains(body, `value="`+conn+`"`) || !strings.Contains(body, `<option value="">None</option>`) || strings.Contains(body, "Create a server connector first") {
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

// The Update tab lists the workers and Update workers queues the rollout; a
// non-admin gets neither.
func TestAdminUpdateWorkers(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	s.Worker(t, "h4", "fp")
	rec := s.As(t, root, "GET", "/-/admin/update", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Workers") || !strings.Contains(rec.Body.String(), "h4") {
		t.Fatalf("update tab = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.As(t, root, "POST", "/-/admin/update/workers", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Worker rollout queued.") {
		t.Fatalf("update workers = %d\n%s", rec.Code, rec.Body)
	}
	// the queued rollout's stream refreshes the tab when it ends
	if b := rec.Body.String(); !strings.Contains(b, "data-live") || !strings.Contains(b, `hx-trigger="sse:end"`) {
		t.Errorf("update workers: no live tab or end refresh\n%s", b)
	}
	if rec := s.Do(t, "POST", "/-/admin/update/workers", nil); rec.Code != http.StatusForbidden {
		t.Errorf("owner update workers = %d, want 403", rec.Code)
	}
}

// While an upgrade is underway the Update tab polls itself through the
// panel's restart and offers no buttons; once over it settles.
func TestAdminUpdateUnderway(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	ctx := context.Background()
	j := service.Job{ID: uuid.NewString(), Kind: "upgrade", State: "running", Payload: "{}", CreatedAt: time.Now()}
	if err := s.Store.Jobs.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	b := s.As(t, root, "GET", "/-/admin/update", nil).Body.String()
	for _, want := range []string{"data-live", `hx-trigger="every 3s"`, "Upgrading.", "Reconnecting..."} {
		if !strings.Contains(b, want) {
			t.Errorf("underway: no %q in\n%s", want, b)
		}
	}
	if strings.Contains(b, "/-/admin/update/check") || strings.Contains(b, "/-/admin/update/workers") {
		t.Errorf("underway tab offers a button\n%s", b)
	}
	now := time.Now()
	j.State, j.FinishedAt = "failed", &now
	if err := s.Store.Jobs.Update(ctx, j); err != nil {
		t.Fatal(err)
	}
	b = s.As(t, root, "GET", "/-/admin/update", nil).Body.String()
	if strings.Contains(b, "every 3s") || strings.Contains(b, "data-live") {
		t.Errorf("settled tab still live\n%s", b)
	}
}

// Keys carries the recovery key section: a fresh box's key is
// unsaved, showing it needs the admin's password, and rotating is refused
// until it is saved.
func TestAdminRecoveryKey(t *testing.T) {
	s := webtest.New(t)
	id := s.User(t, "root@x.test", true)
	if err := s.Orch.SetPassword(context.Background(), id, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	root := s.Session(t, id)
	tab := s.As(t, root, "GET", "/-/admin/keys", nil).Body.String()
	for _, want := range []string{"Recovery key", "data-recovery-warning", "/-/admin/backups/recovery/show"} {
		if !strings.Contains(tab, want) {
			t.Errorf("keys has no %s", want)
		}
	}
	post := func(action, pw string) *httptest.ResponseRecorder {
		return s.As(t, root, "POST", "/-/admin/backups/recovery/"+action, url.Values{"password": {pw}})
	}
	if rec := post("show", "wrong"); rec.Code != http.StatusUnprocessableEntity || strings.Contains(rec.Body.String(), "data-recovery-key") {
		t.Errorf("show with a wrong password = %d", rec.Code)
	}
	if rec := post("rotate", "correct horse battery"); rec.Code != http.StatusUnprocessableEntity || strings.Contains(rec.Body.String(), "data-recovery-key") {
		t.Errorf("rotate while unsaved = %d", rec.Code)
	}
	if rec := post("show", "correct horse battery"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `hx-history="false" data-recovery-key`) {
		t.Errorf("show with the password = %d", rec.Code)
	}
}

// An answer's words sit in the card that posted: a save's note under its
// form, a refusal about one field under that field, nothing over the top
// of the section.
func TestAdminAnswerInItsCard(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	body := s.As(t, root, "POST", "/-/admin/caddy", url.Values{"proxy_custom": {"[]"}}).Body.String()
	form, note := strings.Index(body, `hx-post="/-/admin/caddy"`), strings.Index(body, `role="status" hx-history="false">Saved.`)
	if form < 0 || note < form {
		t.Errorf("the note is not under the form that saved (form %d, note %d):\n%s", form, note, body)
	}
	rec := s.As(t, root, "POST", "/-/admin/dests", url.Values{
		"dest_name": {"offsite"}, "endpoint": {"https://s3.example.test"}, "bucket": {"b"}, "secret_key": {"sk"},
	})
	body = rec.Body.String()
	if rec.Code != http.StatusUnprocessableEntity || !regexp.MustCompile(`id="error-access_key" data-has-error="true"[^>]*>[^<]+`).MatchString(body) {
		t.Errorf("the refusal is not on the access key field: %d\n%s", rec.Code, body)
	}
	if strings.Contains(body, `banner banner-danger`) {
		t.Errorf("a refusal a field shows is drawn again as a banner:\n%s", body)
	}
	// a field refusal shows under its own form's field only, never a
	// same-named field in another card
	for _, path := range []string{"/-/admin/alerts", "/-/admin/alerts/heartbeat"} {
		body = s.As(t, root, "POST", path, url.Values{"url": {""}}).Body.String()
		if n := strings.Count(body, `data-has-error="true"`); n > 1 {
			t.Errorf("%s: the refusal shows under %d fields", path, n)
		}
	}
	// Turn off takes its own button away: the note lands in the card above it.
	s.As(t, root, "POST", "/-/admin/alerts/heartbeat", url.Values{"url": {"https://hb.example.test/ping"}})
	body = s.As(t, root, "POST", "/-/admin/alerts/heartbeat/clear", nil).Body.String()
	form, note = strings.Index(body, `hx-post="/-/admin/alerts/heartbeat"`), strings.Index(body, "Heartbeat is off.")
	if form < 0 || note < form || !strings.Contains(body, "Status: off.") {
		t.Errorf("turning the heartbeat off is not told in its card (form %d, note %d):\n%s", form, note, body)
	}
}

// A settings form refused on several knobs marks each one at once, and a
// save's answer sits after its Save button (data-acted), not over the form.
func TestAdminSettingsEveryRefusalAndNoteBySave(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	rec := s.As(t, root, "POST", "/-/admin/settings/jobs", url.Values{
		"workers": {"-5"}, "backup_run_concurrency": {"-5"}, "backup_restore_concurrency": {"-5"},
	})
	body := rec.Body.String()
	if rec.Code != http.StatusUnprocessableEntity || strings.Count(body, `aria-invalid="true"`) != 3 {
		t.Errorf("refused = %d, %d fields marked, want 3\n%s", rec.Code, strings.Count(body, `aria-invalid="true"`), body)
	}
	rec = s.As(t, root, "POST", "/-/admin/settings/jobs", url.Values{"workers": {"3"}})
	body = rec.Body.String()
	save, note := strings.Index(body, ">Save</button>"), strings.Index(body, "data-acted")
	if rec.Code != http.StatusOK || save < 0 || note < save || !strings.Contains(body[note:], "Saved.") {
		t.Errorf("saved = %d, note at %d, Save at %d\n%s", rec.Code, note, save, body)
	}
}
