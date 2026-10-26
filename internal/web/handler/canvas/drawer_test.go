package canvas_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/hamr/pkg/server"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
	"github.com/FyrmForge/stackr/internal/web"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// browser is a server and one signed-in user who asks without htmx, as a
// fresh load or a typed URL does. role "admin" = a stackr admin in no org.
type browser struct {
	env     *servicetest.Env
	h       http.Handler
	org     string
	session string
}

func newBrowser(t *testing.T, role string) browser {
	t.Helper()
	env := servicetest.New(t)
	srv, err := server.New(server.WithDevMode(true))
	if err != nil {
		t.Fatal(err)
	}
	web.RegisterRoutes(srv, &web.Deps{Orch: env.Orch, Access: middleware.NewAccess(env.Orch), DevMode: true})
	org := env.Org(t, "acme")
	u := env.User(t, role+"@acme.test", role == "admin")
	if role != "admin" {
		env.Member(t, org, u, role)
	}
	return browser{env, srv.Echo(), org, env.Session(t, u)}
}

func (b browser) do(t *testing.T, method, path string, form url.Values, htmx bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	req.Header.Set("X-CSRF-Token", "tok")
	req.AddCookie(&http.Cookie{Name: "csrf", Value: "tok"})
	req.AddCookie(&http.Cookie{Name: b.env.Orch.Sessions().CookieName(), Value: b.session})
	rec := httptest.NewRecorder()
	b.h.ServeHTTP(rec, req)
	return rec
}

// A drawer route loaded without htmx (its link opened in a new tab) sends
// the browser to the page that opens it, so it never draws a bare fragment.
func TestDrawerRouteLoadsInShell(t *testing.T) {
	b := newBrowser(t, "admin")
	if err := b.env.Orch.FinishServerSetup(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"/-/admin/nodes/n1":        "/-/admin/nodes?drawer=node%3An1",
		"/acme/-/drawer?tab=audit": "/acme?drawer=org%3A" + b.org + "&tab=audit",
		// the Server settings root is its Overview, with or without the slash
		"/-/admin":  "/-/admin/overview",
		"/-/admin/": "/-/admin/overview",
	} {
		rec := b.do(t, "GET", path, nil, false)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != want {
			t.Errorf("%s = %d %q, want %q", path, rec.Code, rec.Header().Get("Location"), want)
		}
	}
	// the admin drawer is gone: ?drawer=admin opens nothing
	if body := b.do(t, "GET", "/?drawer=admin&tab=settings", nil, false).Body.String(); strings.Contains(body, `<side-drawer open`) {
		t.Errorf("?drawer=admin still opens a drawer:\n%s", body)
	}
}

// A fresh load of ?drawer=&tab= renders the page with that drawer open and
// its tab inside; a tab the role may not see opens with the refusal, and
// an id not on the canvas leaves the drawer closed.
func TestDrawerFreshLoad(t *testing.T) {
	b := newBrowser(t, "owner")
	rec := b.do(t, "GET", "/?drawer=org:"+b.org+"&tab=general", nil, false)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `<side-drawer open tab="general">`) ||
		!strings.Contains(body, `id="tab-general"`) ||
		!strings.Contains(body, `<graph-canvas`) {
		t.Fatalf("fresh load = %d\n%s", rec.Code, body)
	}
	if body := b.do(
		t,
		"GET",
		"/?drawer=org:nope&tab=general",
		nil,
		false,
	).Body.String(); !strings.Contains(body, "<side-drawer>") {
		t.Error("an unknown card opened a drawer")
	}

	v := newBrowser(t, "viewer")
	body = v.do(t, "GET", "/?drawer=org:"+v.org+"&tab=people", nil, false).Body.String()
	if !strings.Contains(body, `id="org-members"`) || strings.Contains(body, `hx-post="/acme/-/drawer/invite"`) {
		t.Error("a viewer lists members but gets no invite form")
	}
	body = v.do(t, "GET", "/?drawer=org:"+v.org+"&tab=general", nil, false).Body.String()
	if !strings.Contains(body, `id="tab-general"`) || strings.Contains(body, "/acme/-/drawer/rename") {
		t.Error("a viewer got the rename form")
	}
	// a tab the role cannot use is not drawn, and asking for it opens General
	body = v.do(t, "GET", "/acme/-/drawer?tab=connectors", nil, true).Body.String()
	if !strings.Contains(body, `id="tab-general"`) || strings.Contains(body, "tab=connectors") ||
		strings.Contains(body, "tab=domains") || !strings.Contains(body, "tab=storage") {
		t.Errorf("a viewer's strip:\n%s", body)
	}
}

// A level's own drawer (its top bar Settings) survives a reload: the card
// is drawn on the level above, not on its own canvas.
func TestOwnDrawerFreshLoad(t *testing.T) {
	b := newBrowser(t, "owner")
	tl := b.env.Tile(t, b.org)
	for _, c := range []struct{ path, tab string }{
		{
			"/acme?drawer=org:" + b.org,
			"domains",
		},
		{
			"/acme/shop?drawer=stack:" + tl.Stack,
			"settings",
		},
		{
			"/acme/shop/dev?drawer=env:" + tl.Env,
			"settings",
		},
	} {
		rec := b.do(t, "GET", c.path+"&tab="+c.tab, nil, false)
		body := rec.Body.String()
		if rec.Code != http.StatusOK ||
			!strings.Contains(body, `<side-drawer open tab="`+c.tab+`">`) ||
			!strings.Contains(body, `id="tab-`+c.tab+`"`) ||
			!strings.Contains(body, `<graph-canvas`) {
			t.Errorf("%s&tab=%s = %d\n%s", c.path, c.tab, rec.Code, body)
		}
	}
}

// Every tab of the home, org and stack level drawers answers.
func TestDrawerTabs(t *testing.T) {
	s := webtest.New(t)
	for path, tabs := range map[string][]string{
		"/acme/-/drawer":          {"general", "people", "variables", "domains", "storage", "config"},
		"/acme/shop/-/drawer":     {"releases", "settings"},
		"/acme/shop/dev/-/drawer": {"settings", "releases", "sync"},
		"/acme/-/vars":            {"editor"},
		"/acme/shop/-/vars":       {"editor"},
	} {
		for _, tab := range tabs {
			body := get(t, s, path+"?tab="+tab)
			want := `id="tab-` + tab + `"`
			if tab == "params" || tab == "variables" || tab == "editor" {
				want = `id="vars-editor"`
			}
			if !strings.Contains(body, want) || !strings.Contains(body, `id="drawer-view"`) {
				t.Errorf("%s?tab=%s: no %s in\n%s", path, tab, want, body)
			}
		}
	}
	// a level's top bar "Settings" opens its own drawer; a stack card drills
	// into the stack (v0). The org has no params, so no vars card (v0 drew
	// none for an empty scope; the editor is the drawer's params tab).
	if body := get(t, s, "/acme"); !strings.Contains(body, `hx-get="/acme/-/drawer?tab=general" hx-target="#drawer-body" hx-push-url="?drawer=org%3A`) ||
		!strings.Contains(body, `hx-get="/acme/shop"`) ||
		strings.Contains(body, `hx-get="/acme/-/vars"`) {
		t.Error("the org canvas's Settings and cards do not open their drawers, or an empty vars card is drawn")
	}
	if body := get(t, s, "/acme/shop"); !strings.Contains(body, `hx-get="/acme/shop/-/drawer?tab=settings" hx-target="#drawer-body" hx-push-url="?drawer=stack%3A`) || !strings.Contains(body, `&amp;tab=settings"`) {
		t.Error("the stack canvas's Settings does not open the stack drawer")
	}
}

// The env drawer opens on Releases (an env card); Settings is accordion
// rows, ?open= opens one, and each row sums itself up.
func TestEnvSettingsRows(t *testing.T) {
	s := webtest.New(t)
	if body := get(t, s, "/acme/shop/dev/-/drawer"); !strings.Contains(body, `id="tab-releases"`) {
		t.Error("the env drawer does not open on Releases")
	}
	body := get(t, s, "/acme/shop/dev/-/drawer?tab=settings&open=placement")
	for _, w := range []string{
		`data-acc="placement" open>`,
		`<span hidden data-acc-refresh hx-get="/acme/shop/dev/-/drawer?tab=settings" hx-trigger="refresh" hx-swap="none" hx-select-oob="#env-settings-general-head,#env-settings-deploys-head,`, // an in-row save redraws the heads
		`<summary id="env-settings-placement-head"`,
		`hx-get="/acme/shop/dev/-/placement"`,
		`id="vars-editor"`,
		`data-acc="deploys">`,
		"own uplink",
		"set</span>", // Variables: "N set" or "none set"
	} {
		if !strings.Contains(body, w) {
			t.Errorf("no %s in\n%s", w, body)
		}
	}
}

// A resource set on the env marks its row and field changed and shows in
// the summary; one inherited says where from.
func TestEnvResourcesChanged(t *testing.T) {
	s := webtest.New(t)
	if rec := s.Do(t, "POST", "/acme/shop/-/drawer/settings", url.Values{"mem_limit_mb": {"512"}}); rec.Code != http.StatusOK {
		t.Fatalf("stack settings = %d", rec.Code)
	}
	if rec := s.Do(t, "POST", "/acme/shop/dev/-/drawer/settings", url.Values{"cpu_limit": {"2"}}); rec.Code != http.StatusOK {
		t.Fatalf("env settings = %d", rec.Code)
	}
	// the saved form's answer keeps its marks: set here, and from where
	rec := s.Do(t, "POST", "/acme/shop/dev/-/drawer/settings", url.Values{"cpu_limit": {"2"}})
	if b := rec.Body.String(); !strings.Contains(b, `<div data-field data-changed><label for="env-cascade-cpu_limit"`) ||
		!strings.Contains(b, "from stack") {
		t.Errorf("the saved Resources form lost its marks:\n%s", b)
	}
	body := get(t, s, "/acme/shop/dev/-/drawer?tab=settings")
	for _, w := range []string{
		`data-acc="resources" data-changed>`,
		`<div data-field data-changed><label for="env-cascade-cpu_limit"`,
		"2 cores · 512 MB (from stack) · no password</span>",
	} {
		if !strings.Contains(body, w) {
			t.Errorf("no %s in\n%s", w, body)
		}
	}
}

// With log history off the env drawer has no Logs tab (asking for it lands on
// Releases) and the viewer route is gone.
func TestLogsTabOffWhenHistoryOff(t *testing.T) {
	s := webtest.New(t)
	body := get(t, s, "/acme/shop/dev/-/drawer?tab=logs")
	if strings.Contains(body, `tab=logs`) || !strings.Contains(body, `id="tab-releases"`) {
		t.Errorf("env drawer offers Logs:\n%s", body)
	}
	if rec := s.Do(t, "GET", "/acme/shop/dev/-/logs", nil); rec.Code != http.StatusNotFound {
		t.Errorf("viewer = %d", rec.Code)
	}
}

// The create dialogs round-trip: a refusal re-renders the form (422), a
// create redirects to the new card's page and the verbs read it back.
func TestCreateDialogs(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	if body := get(t, s, "/acme"); !strings.Contains(body, `hx-get="/acme/-/new-stack"`) ||
		strings.Contains(body, "new-org") {
		t.Error("the org canvas offers the wrong create buttons")
	}
	rec := s.Do(t, "POST", "/acme/-/new-stack", url.Values{"name": {"blog"}})
	if rec.Header().Get("HX-Redirect") != "/acme/blog" {
		t.Fatalf("create stack = %d %q %s", rec.Code, rec.Header().Get("HX-Redirect"), rec.Body)
	}
	sts, _ := s.Orch.Stacks(ctx, s.Org)
	if len(sts) != 2 {
		t.Errorf("stacks = %d", len(sts))
	}
	if rec := s.Do(t, "POST", "/acme/-/new-stack", url.Values{
		"name": {"blog"},
	}); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `id="create-stack"`) {
		t.Errorf("a taken name = %d %s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", "/acme/shop/-/new-env", url.Values{"name": {"prod"}, "from": {"promote"}})
	if rec.Header().Get("HX-Redirect") != "/acme/shop/prod" {
		t.Fatalf("create env = %d %q %s", rec.Code, rec.Header().Get("HX-Redirect"), rec.Body)
	}
	if es, _ := s.Orch.Ladder(ctx, s.Tile.Stack); len(es) != 2 || es[1].FromKind != "promote" {
		t.Errorf("ladder = %+v", es)
	}
	if rec := s.Do(t, "POST", "/setup", url.Values{"mode": {"ui"}}); rec.Code != http.StatusForbidden {
		t.Errorf("an owner created an org: %d", rec.Code)
	}

	// + org is the setup wizard's branch question (handler/setup).
	a := newBrowser(t, "admin")
	if body := a.do(t, "GET", "/", nil, true).Body.String(); !strings.Contains(body, `href="/setup"`) {
		t.Error("an admin gets no + org")
	}
}

// Drawer actions: a refusal shows over the tab (422); a param saved from
// the editor reads back and a secret's value never comes back.
func TestDrawerActions(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	rec := s.Do(t, "POST", "/acme/shop/-/drawer/delete", nil)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "to remove it and everything in it") {
		t.Errorf("delete a stack untyped = %d %s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", "/acme/-/vars", url.Values{
		"scope":          {"org"},
		"new_collection": {"db"},
		"new_name":       {"pass"},
		"new_kind":       {"secret"},
		"new_value":      {"hunter2"},
	})
	if rec.Code != http.StatusOK || rec.Header().Get("HX-Retarget") != "#vars-editor" ||
		strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatalf("save a secret = %d %s", rec.Code, rec.Body)
	}
	ps, _ := s.Orch.Params(ctx, service.ParamScope{Kind: "org", ID: s.Org}, true)
	if len(ps) != 1 || ps[0].Name != "pass" {
		t.Errorf("params = %+v", ps)
	}
	b := newBrowser(t, "owner")
	rec = b.do(t, "POST", "/acme/-/drawer/rename", url.Values{"name": {"Acme Two"}}, true)
	to := rec.Header().Get("HX-Redirect")
	if body := b.do(
		t,
		"GET",
		to,
		nil,
		false,
	).Body.String(); !strings.Contains(body, `<side-drawer open tab="general">`) ||
		!strings.Contains(body, `value="Acme Two"`) || !strings.Contains(body, `data-acc="name" open`) {
		t.Errorf("rename lands on %q without its drawer open", to)
	}
	rec = s.Do(t, "POST", "/acme/shop/dev/-/drawer/color", url.Values{"color": {"teal"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `value="teal" checked`) {
		t.Errorf("colour = %d %s", rec.Code, rec.Body)
	}
}

// The org's domains tab (step 7a): an owner sees the instance's row with
// the add form prefilled under it, adds and deletes the org's own; the
// instance's row has no delete and no org route reaches it. A viewer sees
// the rows, no form and no delete.
func TestOrgDomains(t *testing.T) {
	ctx := context.Background()
	b := newBrowser(t, "owner")
	inst, err := b.env.Orch.CreateDomainResource(ctx, "instance", "", "example.com", false, "")
	if err != nil {
		t.Fatal(err)
	}
	body := b.do(t, "GET", "/acme/-/drawer?tab=domains", nil, true).Body.String()
	if !strings.Contains(body, `value="acme.example.com"`) || strings.Contains(body, "/acme/-/drawer/domains/") {
		t.Errorf("no prefill under the instance host, or a delete on its row:\n%s", body)
	}
	rec := b.do(t, "POST", "/acme/-/drawer/domains", url.Values{
		"host":                   {"acme.io"},
		"include_env_on_default": {"1"},
		"acme_email":             {"ops@acme.io"},
	}, true)
	body = rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "Domain acme.io added.") ||
		!strings.Contains(body, "env slug on default env") || !strings.Contains(body, "ACME ops@acme.io") {
		t.Fatalf("add = %d\n%s", rec.Code, body)
	}
	rs, err := b.env.Orch.DomainResources(ctx, b.org)
	if err != nil || len(rs) != 2 || rs[0].Host != "acme.io" {
		t.Fatalf("rows = %+v %v", rs, err)
	}
	del := "/acme/-/drawer/domains/" + rs[0].ID + "/delete"
	if !strings.Contains(body, del) {
		t.Errorf("no delete on the org's row:\n%s", body)
	}
	if rec := b.do(t, "POST", "/acme/-/drawer/domains/"+inst.ID+"/delete", nil, true); rec.Code != http.StatusNotFound {
		t.Errorf("delete the instance's row through the org = %d, want 404", rec.Code)
	}
	rec = b.do(t, "POST", del, nil, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Domain resource removed.") {
		t.Errorf("delete = %d\n%s", rec.Code, rec.Body)
	}
	if rs, _ := b.env.Orch.DomainResources(ctx, b.org); len(rs) != 1 {
		t.Errorf("after delete = %+v, want the instance's row only", rs)
	}

	v := newBrowser(t, "viewer")
	if _, err := v.env.Orch.CreateDomainResource(ctx, "org", v.org, "acme.io", false, ""); err != nil {
		t.Fatal(err)
	}
	body = v.do(t, "GET", "/acme/-/drawer?tab=domains", nil, true).Body.String()
	if strings.Contains(body, "acme.io") || !strings.Contains(body, `id="tab-general"`) {
		t.Errorf("a viewer gets no domain rows:\n%s", body)
	}
	if rec := v.do(t, "POST", "/acme/-/drawer/domains", url.Values{"host": {"x.io"}}, true); rec.Code != http.StatusForbidden {
		t.Errorf("viewer add = %d, want 403", rec.Code)
	}
}

// The org drawer's defaults save (DECIDE 166): an owner posts the rung and
// reads it back; a member sees it read-only and a post is refused.
func TestOrgDefaultsSave(t *testing.T) {
	ctx := context.Background()
	b := newBrowser(t, "owner")
	rec := b.do(t, "POST", "/acme/-/drawer/settings", url.Values{"cpu_limit": {"1.5"}}, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Saved.") {
		t.Fatalf("save = %d\n%s", rec.Code, rec.Body)
	}
	ogs, err := b.env.Orch.AllOrgs(ctx)
	if err != nil || len(ogs) != 1 || !strings.Contains(ogs[0].Settings, `"cpu_limit":1.5`) {
		t.Errorf("orgs = %+v %v, want acme's rung saved", ogs, err)
	}
	body := b.do(t, "GET", "/acme/-/drawer?tab=general", nil, true).Body.String()
	if !strings.Contains(body, `name="cpu_limit" type="number" step="any" value="1.5"`) || strings.Contains(body, "needs an owner") {
		t.Errorf("the drawer does not read the rung back:\n%s", body)
	}

	m := newBrowser(t, "member")
	body = m.do(t, "GET", "/acme/-/drawer?tab=general", nil, true).Body.String()
	if !strings.Contains(body, "Changing defaults needs an owner of this organization.") {
		t.Errorf("a member's defaults are not read-only:\n%s", body)
	}
	if rec := m.do(t, "POST", "/acme/-/drawer/settings", url.Values{"cpu_limit": {"2"}}, true); rec.Code != http.StatusForbidden {
		t.Errorf("member save = %d, want 403", rec.Code)
	}
}

// A viewer gets the env, stack and org Settings read-only: the rows and
// their fields, and no form that posts.
func TestViewerSettingsReadOnly(t *testing.T) {
	s := webtest.New(t)
	v := s.User(t, "viewer@acme.test", false)
	s.Member(t, s.Org, v, "viewer")
	vs := s.Session(t, v)
	for path, row := range map[string]string{
		"/acme/shop/dev/-/drawer?tab=settings": `data-acc="deploys"`,
		"/acme/shop/-/drawer?tab=settings":     `data-acc="config"`,
		"/acme/-/drawer?tab=general":           `data-acc="defaults"`,
		"/acme/shop/dev/-/placement":           `id="ingress-node"`,
		"/acme/shop/dev/-/tiles/api/placement": `id="ingress-node"`,
		"/acme/shop/-/placement":               `id="ingress-node"`,
		"/acme/-/placement":                    `id="ingress-node"`,
	} {
		body := s.As(t, vs, "GET", path, nil).Body.String()
		if !strings.Contains(body, row) || !strings.Contains(body, "disabled") {
			t.Errorf("%s: no read-only %s\n%s", path, row, body)
		}
		if strings.Contains(body, "hx-post") || strings.Contains(body, `method="post"`) {
			t.Errorf("%s: a viewer gets a form that posts", path)
		}
	}
}

// A fresh load asking for a tab the viewer cannot see is sent to the URL
// of the tab the drawer shows.
func TestFreshLoadHiddenTabRedirects(t *testing.T) {
	b := newBrowser(t, "member")
	rec := b.do(t, "GET", "/acme?drawer=org:"+b.org+"&tab=domains&open=x", nil, false)
	if loc := rec.Header().Get("Location"); rec.Code != 303 || !strings.Contains(loc, "tab=general") || strings.Contains(loc, "open=") {
		t.Errorf("hidden tab = %d, Location %q", rec.Code, loc)
	}
	if rec := b.do(t, "GET", "/acme?drawer=org:"+b.org+"&tab=general", nil, false); rec.Code != 200 {
		t.Errorf("shown tab = %d", rec.Code)
	}
}
