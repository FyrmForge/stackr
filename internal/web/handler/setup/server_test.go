package setup_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/server"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
	"github.com/FyrmForge/stackr/internal/web/handler/setup"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// srvSite is the server wizard and the gate over one stand-in page, since
// server.go (the integrator's) is what mounts them in the real site.
type srvSite struct {
	env *servicetest.Env
	h   http.Handler
}

func newSrvSite(t *testing.T) srvSite {
	t.Helper()
	env := servicetest.New(t)
	srv, err := server.New(server.WithDevMode(true))
	if err != nil {
		t.Fatal(err)
	}
	a := middleware.NewAccess(env.Orch)
	site := srv.Echo().Group("")
	site.Use(hamrmw.FlashWithConfig(hamrmw.FlashConfig{}))
	site.Use(hamrmw.CSRFWithConfig(hamrmw.CSRFConfig{}))
	site.Use(a.LoadSession())
	site.Use(render.Theme)
	site.Use(setup.ServerPending(env.Orch))
	site.GET("/", func(c echo.Context) error { return c.String(http.StatusOK, "home") })
	setup.NewHandler(env.Orch).MountServer(site, a)
	return srvSite{env, srv.Echo()}
}

func (s srvSite) do(session, method, path string, form url.Values, htmx bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	req.Header.Set("X-CSRF-Token", "tok")
	req.AddCookie(&http.Cookie{Name: "csrf", Value: "tok"})
	req.AddCookie(&http.Cookie{Name: s.env.Orch.Sessions().CookieName(), Value: session})
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	return rec
}

// Only an admin is sent to the wizard, and only while it is unfinished.
func TestServerPendingGate(t *testing.T) {
	s := newSrvSite(t)
	adm := s.env.Session(t, s.env.User(t, "root@x.test", true))
	org := s.env.Org(t, "acme")
	u := s.env.User(t, "owner@x.test", false)
	s.env.Member(t, org, u, "owner")
	owner := s.env.Session(t, u)

	if rec := s.do(adm, "GET", "/", nil, false); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != setup.ServerBase {
		t.Errorf("admin / = %d %q, want redirect to the wizard", rec.Code, rec.Header().Get("Location"))
	}
	if rec := s.do(adm, "GET", "/", nil, true); rec.Code != http.StatusOK {
		t.Errorf("admin htmx / = %d, want it passed", rec.Code)
	}
	if rec := s.do(owner, "GET", "/", nil, false); rec.Code != http.StatusOK {
		t.Errorf("owner / = %d, want it passed", rec.Code)
	}
	if rec := s.do(owner, "GET", setup.ServerBase, nil, false); rec.Code == http.StatusOK {
		t.Errorf("owner reached the server wizard: %d", rec.Code)
	}
	if err := s.env.Orch.FinishServerSetup(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if rec := s.do(adm, "GET", "/", nil, false); rec.Code != http.StatusOK {
		t.Errorf("admin / after finish = %d, want it passed", rec.Code)
	}
}

// Both branches end with done stamped; the page has no restore option.
func TestServerWizardBranches(t *testing.T) {
	for _, tc := range []struct {
		mode, step string
		want       []string
	}{
		{"ui", "/ui", []string{`name="root_domain"`, `name="dest_name"`, "Open placements"}},
		{"config", "/config", []string{`name="repo"`, `hx-post="/-/setup/server/bind"`}},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			s := newSrvSite(t)
			adm := s.env.Session(t, s.env.User(t, "root@x.test", true))
			body := s.do(adm, "GET", setup.ServerBase, nil, false).Body.String()
			for _, w := range []string{`value="ui"`, `value="config"`} {
				if !strings.Contains(body, w) {
					t.Errorf("branch page lacks %s", w)
				}
			}
			if strings.Contains(strings.ToLower(body), "restore") {
				t.Error("branch page offers a restore")
			}
			rec := s.do(adm, "POST", setup.ServerBase+"/mode", url.Values{"mode": {tc.mode}}, true)
			if got := rec.Header().Get("HX-Redirect"); got != setup.ServerBase+tc.step {
				t.Fatalf("mode redirect = %q", got)
			}
			rec = s.do(adm, "GET", setup.ServerBase+tc.step, nil, false)
			for _, w := range tc.want {
				if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), w) {
					t.Fatalf("GET %s = %d, want %q in:\n%s", tc.step, rec.Code, w, rec.Body)
				}
			}
			rec = s.do(adm, "POST", setup.ServerBase+"/done", url.Values{}, true)
			if got := rec.Header().Get("HX-Redirect"); got != "/" {
				t.Fatalf("done redirect = %q", got)
			}
			if st, err := s.env.Orch.ServerSetupState(context.Background()); err != nil || !st.Done {
				t.Errorf("state = %+v, %v; want done", st, err)
			}
		})
	}
}

// The by-hand page saves the domains through the settings verb.
func TestServerWizardDomains(t *testing.T) {
	s := newSrvSite(t)
	adm := s.env.Session(t, s.env.User(t, "root@x.test", true))
	s.do(adm, "POST", setup.ServerBase+"/mode", url.Values{"mode": {"ui"}}, true)
	rec := s.do(adm, "POST", setup.ServerBase+"/domains", url.Values{"root_domain": {"example.test"}, "panel_domain": {"stackr.example.test"}}, true)
	if got := rec.Header().Get("HX-Redirect"); got != setup.ServerBase+"/ui" {
		t.Fatalf("domains = %d %q\n%s", rec.Code, got, rec.Body)
	}
	if v, _ := s.env.Orch.Setting(context.Background(), "root_domain"); v != "example.test" {
		t.Errorf("root_domain = %q", v)
	}
	if !strings.Contains(s.do(adm, "GET", setup.ServerBase+"/ui", nil, false).Body.String(), `value="example.test"`) {
		t.Error("saved domain is not shown again")
	}
}
