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
	web.RegisterRoutes(srv, &web.Deps{Service: env.O, Access: middleware.NewAccess(env.O), DevMode: true})
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
	req.AddCookie(&http.Cookie{Name: b.env.O.Sessions().CookieName(), Value: b.session})
	rec := httptest.NewRecorder()
	b.h.ServeHTTP(rec, req)
	return rec
}

// A fresh load of ?drawer=&tab= renders the page with that drawer open and
// its tab inside; a tab the role may not see opens with the refusal, and
// an id not on the canvas leaves the drawer closed.
func TestDrawerFreshLoad(t *testing.T) {
	b := newBrowser(t, "owner")
	rec := b.do(t, "GET", "/?drawer=org:"+b.org+"&tab=settings", nil, false)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `<side-drawer open tab="settings">`) || !strings.Contains(body, `id="tab-settings"`) ||
		!strings.Contains(body, `<graph-canvas`) {
		t.Fatalf("fresh load = %d\n%s", rec.Code, body)
	}
	if body := b.do(t, "GET", "/?drawer=org:nope&tab=settings", nil, false).Body.String(); !strings.Contains(body, "<side-drawer>") {
		t.Error("an unknown card opened a drawer")
	}

	v := newBrowser(t, "viewer")
	body = v.do(t, "GET", "/?drawer=org:"+v.org+"&tab=members", nil, false).Body.String()
	if !strings.Contains(body, `id="tab-members"`) || strings.Contains(body, `hx-post="/acme/-/drawer/invite"`) {
		t.Error("a viewer lists members but gets no invite form")
	}
	body = v.do(t, "GET", "/?drawer=org:"+v.org+"&tab=settings", nil, false).Body.String()
	if !strings.Contains(body, `id="tab-settings"`) || strings.Contains(body, "/acme/-/drawer/rename") {
		t.Error("a viewer got the rename form")
	}
}

// Every tab of the home, org and stack level drawers answers.
func TestDrawerTabs(t *testing.T) {
	s := webtest.New(t)
	for path, tabs := range map[string][]string{
		"/acme/-/drawer":          {"settings", "members", "keys", "params", "backups"},
		"/acme/shop/-/drawer":     {"settings", "params", "releases"},
		"/acme/shop/dev/-/drawer": {"settings", "releases", "params", "order", "logs"},
		"/acme/-/vars":            {"editor"},
		"/acme/shop/-/vars":       {"editor"},
	} {
		for _, tab := range tabs {
			body := get(t, s, path+"?tab="+tab)
			want := `id="tab-` + tab + `"`
			if tab == "params" || tab == "editor" {
				want = `id="vars-editor"`
			}
			if !strings.Contains(body, want) || !strings.Contains(body, `id="drawer-view"`) {
				t.Errorf("%s?tab=%s: no %s in\n%s", path, tab, want, body)
			}
		}
	}
	// the cards on the canvases point at them
	if body := get(t, s, "/acme"); !strings.Contains(body, `hx-get="/acme/shop/-/drawer"`) || !strings.Contains(body, `hx-get="/acme/-/vars"`) {
		t.Error("the org canvas's cards do not open their drawers")
	}
}

// The create dialogs round-trip: a refusal re-renders the form (422), a
// create redirects to the new card's page and the verbs read it back.
func TestCreateDialogs(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	if body := get(t, s, "/acme"); !strings.Contains(body, `hx-get="/acme/-/new-stack"`) || strings.Contains(body, "new-org") {
		t.Error("the org canvas offers the wrong create buttons")
	}
	rec := s.Do(t, "POST", "/acme/-/new-stack", url.Values{"name": {"blog"}})
	if rec.Header().Get("HX-Redirect") != "/acme/blog" {
		t.Fatalf("create stack = %d %q %s", rec.Code, rec.Header().Get("HX-Redirect"), rec.Body)
	}
	sts, _ := s.O.Stacks(ctx, s.Org)
	if len(sts) != 2 {
		t.Errorf("stacks = %d", len(sts))
	}
	if rec := s.Do(t, "POST", "/acme/-/new-stack", url.Values{"name": {"blog"}}); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `id="create-stack"`) {
		t.Errorf("a taken name = %d %s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", "/acme/shop/-/new-env", url.Values{"name": {"prod"}, "from": {"promote"}})
	if rec.Header().Get("HX-Redirect") != "/acme/shop/prod" {
		t.Fatalf("create env = %d %q %s", rec.Code, rec.Header().Get("HX-Redirect"), rec.Body)
	}
	if es, _ := s.O.Ladder(ctx, s.Tile.Stack); len(es) != 2 || es[1].FromKind != "promote" {
		t.Errorf("ladder = %+v", es)
	}
	if rec := s.Do(t, "POST", "/-/new-org", url.Values{"name": {"beta"}}); rec.Code != http.StatusForbidden {
		t.Errorf("an owner created an org: %d", rec.Code)
	}

	a := newBrowser(t, "admin")
	if body := a.do(t, "GET", "/", nil, true).Body.String(); !strings.Contains(body, `hx-get="/-/new-org"`) {
		t.Error("an admin gets no + org")
	}
	rec = a.do(t, "POST", "/-/new-org", url.Values{"name": {"Beta"}}, true)
	if rec.Header().Get("HX-Redirect") != "/beta" {
		t.Fatalf("create org = %d %q %s", rec.Code, rec.Header().Get("HX-Redirect"), rec.Body)
	}
}

// Drawer actions: a refusal shows over the tab (422); a param saved from
// the editor reads back and a secret's value never comes back.
func TestDrawerActions(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	rec := s.Do(t, "POST", "/acme/shop/-/drawer/delete", nil)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "environments first") {
		t.Errorf("delete a stack with envs = %d %s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", "/acme/-/vars", url.Values{"new_collection": {"db"}, "new_name": {"pass"}, "new_kind": {"secret"}, "new_value": {"hunter2"}})
	if rec.Code != http.StatusOK || rec.Header().Get("HX-Retarget") != "#vars-editor" || strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatalf("save a secret = %d %s", rec.Code, rec.Body)
	}
	ps, _ := s.O.Params(ctx, service.ParamScope{Kind: "org", ID: s.Org}, true)
	if len(ps) != 1 || ps[0].Name != "pass" {
		t.Errorf("params = %+v", ps)
	}
	b := newBrowser(t, "owner")
	rec = b.do(t, "POST", "/acme/-/drawer/rename", url.Values{"name": {"Acme Two"}}, true)
	to := rec.Header().Get("HX-Redirect")
	if body := b.do(t, "GET", to, nil, false).Body.String(); !strings.Contains(body, `<side-drawer open tab="settings">`) || !strings.Contains(body, `value="Acme Two"`) {
		t.Errorf("rename lands on %q without its drawer open", to)
	}
	rec = s.Do(t, "POST", "/acme/shop/dev/-/drawer/color", url.Values{"color": {"teal"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `value="teal" checked`) {
		t.Errorf("colour = %d %s", rec.Code, rec.Body)
	}
}
