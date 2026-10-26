package setup_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	hamrmw "github.com/FyrmForge/hamr/pkg/middleware"
	"github.com/FyrmForge/hamr/pkg/server"
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
	"github.com/FyrmForge/stackr/internal/web/handler/setup"
	"github.com/FyrmForge/stackr/internal/web/render"
)

// srvSite is the server wizard and the gate over one stand-in page, since
// server.go mounts them in the real site.
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
	site.GET("/settings/github/installed/:id", func(c echo.Context) error { return c.String(http.StatusOK, "installed") })
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
	// GitHub hands the browser back mid-wizard
	if rec := s.do(adm, "GET", "/settings/github/installed/x", nil, false); rec.Code != http.StatusOK {
		t.Errorf("admin GitHub return = %d, want it passed", rec.Code)
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
		{"ui", "/ui/domains", []string{`name="root_domain"`, "Step 1 of 7"}},
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
			if got := rec.Header().Get("HX-Redirect"); got != setup.ServerBase+"/"+tc.mode {
				t.Fatalf("mode redirect = %q", got)
			}
			if tc.mode == "ui" {
				if got := s.do(adm, "GET", setup.ServerBase+"/ui", nil, false).Header().Get("Location"); got != setup.ServerBase+tc.step {
					t.Fatalf("/ui = %q, want the first screen", got)
				}
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
	if got := rec.Header().Get("HX-Redirect"); got != setup.ServerBase+"/ui/certificates" {
		t.Fatalf("domains = %d %q\n%s", rec.Code, got, rec.Body)
	}
	if v, _ := s.env.Orch.Setting(context.Background(), "root_domain"); v != "example.test" {
		t.Errorf("root_domain = %q", v)
	}
	if !strings.Contains(s.do(adm, "GET", setup.ServerBase+"/ui/domains", nil, false).Body.String(), `value="example.test"`) {
		t.Error("saved domain is not shown again")
	}
	rec = s.do(adm, "POST", setup.ServerBase+"/domains", url.Values{"root_domain": {"kept.test"}, "acme_email": {"not an email"}}, true)
	refused(t, rec, `value="kept.test"`, `value="not an email"`, `id="error-acme_email"`)
}

// The stepper names every step; a done one links back, the rest do not.
func TestServerWizardStepper(t *testing.T) {
	s := newSrvSite(t)
	adm := s.env.Session(t, s.env.User(t, "root@x.test", true))
	body := s.do(adm, "GET", setup.ServerBase+"/ui/backups", nil, false).Body.String()
	if i, j := strings.Index(body, "<nav"), strings.Index(body, "</nav>"); i >= 0 && j > i {
		body = body[i:j]
	}
	for _, w := range []string{
		`href="/-/setup/server/ui/domains"`, `href="/-/setup/server/ui/observability"`, ">Observability</span>", ">Nodes</span>", `aria-current="step"`,
	} {
		if !strings.Contains(body, w) {
			t.Errorf("stepper lacks %s", w)
		}
	}
	if strings.Contains(body, `href="/-/setup/server/ui/nodes"`) {
		t.Error("a step not reached yet is a link")
	}
}

// refused checks a refused save came back as its step, swapped into the
// shell: 422, what was typed still there, the message where it belongs.
func refused(t *testing.T, rec *httptest.ResponseRecorder, want ...string) {
	t.Helper()
	if rec.Code != http.StatusUnprocessableEntity || rec.Header().Get("HX-Retarget") != "#setup-shell" {
		t.Errorf("refused save = %d, retarget %q; want 422 into the shell", rec.Code, rec.Header().Get("HX-Retarget"))
	}
	for _, w := range want {
		if !strings.Contains(rec.Body.String(), w) {
			t.Errorf("refused save lacks %s", w)
		}
	}
}

// Every screen draws under the stepper; all but domains can be skipped to
// the next; an unknown one is not found. Finished, they still open (the
// Setup guide) and the gate lets the admin go.
func TestServerWizardSteps(t *testing.T) {
	s := newSrvSite(t)
	adm := s.env.Session(t, s.env.User(t, "root@x.test", true))
	steps := []string{"domains", "certificates", "git", "observability", "backups", "nodes", "summary"}
	check := func() {
		t.Helper()
		for i, step := range steps {
			rec := s.do(adm, "GET", setup.ServerBase+"/ui/"+step, nil, false)
			body := rec.Body.String()
			if rec.Code != http.StatusOK || !strings.Contains(body, fmt.Sprintf("Step %d of 7", i+1)) {
				t.Fatalf("GET %s = %d\n%s", step, rec.Code, body)
			}
			skip := strings.Contains(body, ">Skip<")
			if step == "domains" && skip {
				t.Error("domains offers Skip")
			}
			// domains moves on by saving
			if step != "domains" && i+1 < len(steps) && !strings.Contains(body, `href="`+setup.ServerBase+"/ui/"+steps[i+1]+`"`) {
				t.Errorf("%s has no way on to %s", step, steps[i+1])
			}
			if (step == "certificates" || step == "git" || step == "observability" || step == "backups") && !skip {
				t.Errorf("%s offers no Skip", step)
			}
		}
	}
	check()
	if rec := s.do(adm, "GET", setup.ServerBase+"/ui/nope", nil, false); rec.Code != http.StatusNotFound {
		t.Errorf("unknown step = %d", rec.Code)
	}
	if rec := s.do(adm, "GET", "/", nil, false); rec.Code != http.StatusSeeOther {
		t.Errorf("unfinished admin / = %d, want the gate", rec.Code)
	}
	rec := s.do(adm, "POST", setup.ServerBase+"/done", url.Values{}, true)
	if got := rec.Header().Get("HX-Redirect"); got != "/" {
		t.Fatalf("finish = %q", got)
	}
	check()
	if body := s.do(adm, "GET", setup.ServerBase+"/ui/summary", nil, false).Body.String(); !strings.Contains(body, `href="/-/admin/connectors"`) {
		t.Errorf("finished summary does not link to Admin:\n%s", body)
	}
	if rec := s.do(adm, "GET", "/", nil, false); rec.Code != http.StatusOK {
		t.Errorf("finished admin / = %d", rec.Code)
	}
	if rec := s.do(adm, "GET", setup.ServerBase+"?pick=1", nil, false); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "From a server file") {
		t.Errorf("finished ?pick=1 = %d, want the branch question", rec.Code)
	}
}

// Observability saves the switch and its knobs, and refuses a bad number.
func TestServerWizardObserve(t *testing.T) {
	s := newSrvSite(t)
	ctx := context.Background()
	adm := s.env.Session(t, s.env.User(t, "root@x.test", true))
	if !strings.Contains(s.do(adm, "GET", setup.ServerBase+"/ui/observability", nil, false).Body.String(), `name="observe_enabled" value="1" checked`) {
		t.Error("observability is not on by default")
	}
	rec := s.do(adm, "POST", setup.ServerBase+"/observe", url.Values{
		"observe_metrics_retention": {"10"}, "observe_min_free": {"3"},
		"observe_logs_retention": {"2"}, "observe_logs_max_size": {"4"},
	}, true)
	if got := rec.Header().Get("HX-Redirect"); got != setup.ServerBase+"/ui/backups" {
		t.Fatalf("observe = %d %q", rec.Code, got)
	}
	for k, want := range map[string]string{"observe_enabled": "false", "observe_metrics_retention": "10", "observe_logs_max_size": "4", "observe_access_ip": "true"} {
		if v, _ := s.env.Orch.Setting(ctx, k); v != want {
			t.Errorf("%s = %q, want %q", k, v, want)
		}
	}
	rec = s.do(adm, "POST", setup.ServerBase+"/observe", url.Values{"observe_enabled": {"1"}, "observe_metrics_retention": {"lots"}}, true)
	refused(t, rec, `value="lots"`, `id="error-observe_metrics_retention"`, "Use a whole number")
	if v, _ := s.env.Orch.Setting(ctx, "observe_enabled"); v != "false" {
		t.Errorf("a refused save wrote observe_enabled = %q", v)
	}
}

// Backups adds a destination offered to every org, then points the panel
// backup at it.
func TestServerWizardBackups(t *testing.T) {
	s := newSrvSite(t)
	ctx := context.Background()
	adm := s.env.Session(t, s.env.User(t, "root@x.test", true))
	if !strings.Contains(s.do(adm, "GET", setup.ServerBase+"/ui/backups", nil, false).Body.String(), `name="shared" value="1" checked`) {
		t.Error("Let orgs use it is not ticked")
	}
	rec := s.do(adm, "POST", setup.ServerBase+"/dest", url.Values{
		"dest_name": {"offsite"}, "endpoint": {"https://s3.example.test"}, "bucket": {"b"}, "region": {"r"},
		"access_key": {"ak"}, "secret_key": {"sk"}, "shared": {"1"},
	}, true)
	back := rec.Header().Get("HX-Redirect")
	ds, _ := s.env.Orch.GlobalBackupDests(ctx)
	var id string
	for _, d := range ds {
		if d.Name == "offsite" {
			id = d.ID
			if !d.Shared {
				t.Error("destination is not shared")
			}
		}
	}
	if id == "" {
		t.Fatal("destination not saved")
	}
	if back != setup.ServerBase+"/ui/backups?dest="+id {
		t.Fatalf("dest = %d %q", rec.Code, back)
	}
	// the panel backup form picks what was just added, not This box
	if body := s.do(adm, "GET", back, nil, false).Body.String(); !strings.Contains(body, `<option value="`+id+`" selected>`) {
		t.Error("the new destination is not picked for the panel backup")
	}
	rec = s.do(adm, "POST", setup.ServerBase+"/panel-backup", url.Values{"panel_backup_enabled": {"1"}, "panel_backup_schedule": {"0 4 * * *"}, "panel_backup_dest": {id}}, true)
	if got := rec.Header().Get("HX-Redirect"); got != setup.ServerBase+"/ui/nodes" {
		t.Fatalf("panel backup = %d %q", rec.Code, got)
	}
	for k, want := range map[string]string{"panel_backup_dest": id, "panel_backup_schedule": "0 4 * * *", "panel_backup_keep": "14"} {
		if v, _ := s.env.Orch.Setting(ctx, k); v != want {
			t.Errorf("%s = %q, want %q", k, v, want)
		}
	}
	rec = s.do(adm, "POST", setup.ServerBase+"/panel-backup", url.Values{"panel_backup_schedule": {"whenever"}}, true)
	refused(t, rec, `value="whenever"`, `id="error-panel_backup_schedule"`)
	if regexp.MustCompile(`name="panel_backup_enabled"[^>]*checked`).MatchString(rec.Body.String()) {
		t.Error("the switch posted off came back on")
	}
	// a refused destination keeps every field, the secret key too
	rec = s.do(adm, "POST", setup.ServerBase+"/dest", url.Values{
		"dest_name": {"second"}, "endpoint": {"ftp://nope"}, "bucket": {"b2"}, "access_key": {"ak2"}, "secret_key": {"sk2"},
	}, true)
	refused(t, rec, `value="second"`, `value="ftp://nope"`, `value="sk2"`, `id="error-endpoint"`)
	if regexp.MustCompile(`name="shared"[^>]*checked`).MatchString(rec.Body.String()) {
		t.Error("Let orgs use it, posted off, came back on")
	}
}

// An existing App needs a key that parses, and a refusal never shows a
// secret back; saved, it is a server connector shared with every org and
// the step shows its webhook URL and install page. Creating one starts
// GitHub's manifest flow, shared from the start.
func TestServerWizardGitHub(t *testing.T) {
	s := newSrvSite(t)
	ctx := context.Background()
	adm := s.env.Session(t, s.env.User(t, "root@x.test", true))
	add := setup.ServerBase + "/git/new/github"
	form := url.Values{"alt": {"1"}, "app_id": {"42"}, "slug": {"my-app"}, "webhook_secret": {"whsec"}, "private_key": {"not a key"}}
	rec := s.do(adm, "POST", add, form, true)
	if b := rec.Body.String(); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(b, `value="my-app"`) ||
		!strings.Contains(b, `id="error-private_key"`) || strings.Contains(b, "whsec") || strings.Contains(b, "not a key") {
		t.Errorf("refused save = %d\n%s", rec.Code, b)
	}
	if cs, _ := s.env.Orch.ServerConnectors(ctx); len(cs) != 0 {
		t.Fatalf("a bad key was saved: %+v", cs)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	form.Set("private_key", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})))
	if rec := s.do(adm, "POST", add, form, true); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "PRIVATE KEY-----") {
		t.Fatalf("save = %d\n%s", rec.Code, rec.Body)
	}
	cs, _ := s.env.Orch.ServerConnectors(ctx)
	if len(cs) != 1 || !cs[0].ShareAll || cs[0].Name != "GitHub · my-app" {
		t.Fatalf("connectors = %+v, want one shared with all", cs)
	}
	body := s.do(adm, "GET", setup.ServerBase+"/ui/git", nil, false).Body.String()
	for _, w := range []string{"/hooks/connectors/" + cs[0].ID, "github.com/apps/my-app/installations/new?state="} {
		if !strings.Contains(body, w) {
			t.Errorf("git step lacks %s", w)
		}
	}
	if strings.Contains(body, "PRIVATE KEY-----\n") || strings.Contains(body, "whsec") {
		t.Error("the step shows a secret back")
	}

	rec = s.do(adm, "POST", add, url.Values{"owner": {""}}, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="connector-out"`) {
		t.Fatalf("begin = %d\n%s", rec.Code, rec.Body)
	}
	cs, _ = s.env.Orch.ServerConnectors(ctx)
	if len(cs) != 2 || !cs[0].ShareAll || !cs[1].ShareAll {
		t.Errorf("manifest connector is not shared with all: %+v", cs)
	}
}

// A Create on GitHub walked away from is no App: the step keeps Skip, and
// the sweep removes the half-made connector after an hour.
func TestServerWizardPendingApp(t *testing.T) {
	s := newSrvSite(t)
	ctx := context.Background()
	adm := s.env.Session(t, s.env.User(t, "root@x.test", true))
	s.do(adm, "POST", setup.ServerBase+"/git/new/github", url.Values{"owner": {""}}, true)
	body := s.do(adm, "GET", setup.ServerBase+"/ui/git", nil, false).Body.String()
	if !strings.Contains(body, ">Skip</a>") || strings.Contains(body, ">Continue</a>") {
		t.Errorf("a pending App turned Skip into Continue:\n%s", body)
	}
	if n, err := s.env.Orch.SweepPendingConnectors(ctx, time.Now()); err != nil || n != 0 {
		t.Errorf("fresh sweep = %d, %v; want none", n, err)
	}
	if n, err := s.env.Orch.SweepPendingConnectors(ctx, time.Now().Add(2*time.Hour)); err != nil || n != 1 {
		t.Errorf("sweep after two hours = %d, %v; want 1", n, err)
	}
	if cs, _ := s.env.Orch.ServerConnectors(ctx); len(cs) != 0 {
		t.Errorf("connectors after the sweep = %+v", cs)
	}
}

// Certificates adds a Namecheap connector (a refusal keeps the user and IP,
// never the key), lists its domains, manages one for every org (no org
// exists yet), and the summary names it. Skip until a connector is added.
func TestServerWizardCertificates(t *testing.T) {
	s := newSrvSite(t)
	ctx := context.Background()
	s.env.DNS.Zones = []service.DNSZone{{Name: "acme.dev", OurDNS: true}, {Name: "away.com"}}
	adm := s.env.Session(t, s.env.User(t, "root@x.test", true))
	step := setup.ServerBase + "/ui/certificates"

	body := s.do(adm, "GET", step, nil, false).Body.String()
	for _, w := range []string{`hx-get="/-/setup/server/connectors/new"`, "+ Add Namecheap", "No connectors yet.", ">Skip</a>", "you point its DNS yourself"} {
		if !strings.Contains(body, w) {
			t.Fatalf("step lacks %s\n%s", w, body)
		}
	}
	// the add dialog opens straight on Namecheap: no kind picker, no Back to one
	body = s.do(adm, "GET", setup.ServerBase+"/connectors/new", nil, true).Body.String()
	for _, w := range []string{"<confirm-dialog open>", `hx-post="/-/setup/server/connectors/new/namecheap"`, "Controls your whole Namecheap account.", `hx-get="` + step + `"`} {
		if !strings.Contains(body, w) {
			t.Fatalf("dialog lacks %s\n%s", w, body)
		}
	}
	if strings.Contains(body, "?pick=1") {
		t.Errorf("the wizard's dialog goes back to a picker:\n%s", body)
	}
	bad := url.Values{"user": {"jo"}, "key": {servicetest.BadKey}, "client_ip": {"203.0.113.7"}}
	rec := s.do(adm, "POST", setup.ServerBase+"/connectors/new/namecheap", bad, true)
	// a refusal answers into the open dialog, not the shell
	if b := rec.Body.String(); rec.Code != http.StatusUnprocessableEntity || rec.Header().Get("HX-Retarget") != "" ||
		!strings.Contains(b, `value="jo"`) || !strings.Contains(b, `value="203.0.113.7"`) || !strings.Contains(b, "refused the API key") {
		t.Errorf("refused add = %d\n%s", rec.Code, b)
	}
	if strings.Contains(rec.Body.String(), servicetest.BadKey) {
		t.Error("the refused key came back")
	}
	rec = s.do(adm, "POST", setup.ServerBase+"/connectors/new/namecheap", url.Values{"user": {"jo"}, "key": {"good-key"}, "client_ip": {"203.0.113.7"}}, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Step 5 of 5: Domains") {
		t.Fatalf("add = %d\n%s", rec.Code, rec.Body)
	}
	cs, _ := s.env.Orch.DNSConnectors(ctx, nil)
	if len(cs) != 1 {
		t.Fatalf("connectors = %v", cs)
	}
	base := setup.ServerBase + "/dns/" + cs[0].ID
	body = s.do(adm, "GET", step, nil, false).Body.String()
	if !strings.Contains(body, `hx-get="`+setup.ServerBase+"/connectors/"+cs[0].ID+`/settings?tab=domains"`) || !strings.Contains(body, ">Continue</a>") || !strings.Contains(body, "No domains managed yet.") {
		t.Errorf("step after add:\n%s", body)
	}
	// a closing dialog refreshes the list from the step, Continue with it
	body = s.do(adm, "GET", step, nil, true).Body.String()
	list := strings.Index(body, `id="connectors-list"`)
	if list < 0 || strings.Index(body, ">Continue</a>") < list || strings.Index(body, `id="connector-modal"`) < strings.Index(body, ">Continue</a>") {
		t.Errorf("the step's list does not carry Continue:\n%s", body)
	}
	body = s.do(adm, "GET", setup.ServerBase+"/connectors/"+cs[0].ID+"/settings", nil, true).Body.String()
	if !strings.Contains(body, `hx-post="`+base+`/domains"`) || strings.Contains(body, "/delete") {
		t.Errorf("settings:\n%s", body)
	}
	rec = s.do(adm, "POST", base+"/manage", url.Values{"domain": {"acme.dev"}}, true)
	if b := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(b, `name="scope" value="all" checked`) || !strings.Contains(b, "No organizations yet.") {
		t.Fatalf("manage = %d\n%s", rec.Code, b)
	}
	if strings.Contains(s.do(adm, "GET", step, nil, false).Body.String(), "No domains managed yet.") {
		t.Error("the no-domains line stays once one is managed")
	}
	if !strings.Contains(s.do(adm, "GET", setup.ServerBase+"/ui/summary", nil, false).Body.String(), "wildcards and DNS records for acme.dev") {
		t.Error("summary does not name the managed domain")
	}
}
