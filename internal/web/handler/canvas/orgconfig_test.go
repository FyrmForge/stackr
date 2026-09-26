package canvas_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/hamr/pkg/server"

	"github.com/FyrmForge/stackr/internal/api/stream"
	"github.com/FyrmForge/stackr/internal/middleware"
	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
	"github.com/FyrmForge/stackr/internal/web"
)

// configRig is acme over the git fake with a connector, an owner and a
// viewer on one server.
type configRig struct {
	owner, viewer browser
	g             *servicetest.Git
	conn          string
}

func newConfigRig(t *testing.T) configRig {
	t.Helper()
	g := servicetest.NewGit(t)
	env := servicetest.NewWith(t, []service.Option{g.Option()})
	srv, err := server.New(server.WithDevMode(true))
	if err != nil {
		t.Fatal(err)
	}
	web.RegisterRoutes(srv, &web.Deps{Orch: env.Orch, Access: middleware.NewAccess(env.Orch), DevMode: true})
	org := env.Org(t, "acme")
	o, v := env.User(t, "owner@acme.test", false), env.User(t, "viewer@acme.test", false)
	env.Member(t, org, o, "owner")
	env.Member(t, org, v, "viewer")
	return configRig{
		owner:  browser{env, srv.Echo(), org, env.Session(t, o)},
		viewer: browser{env, srv.Echo(), org, env.Session(t, v)},
		g:      g,
		conn:   env.Connector(t, org, "whsec"),
	}
}

func (r configRig) file(t *testing.T, body string) {
	t.Helper()
	r.g.Commit(t, "acme/org", "main", map[string]string{"stackr-org.yml": body})
}

func (r configRig) latest(t *testing.T) service.OrgPlan {
	t.Helper()
	ps, err := r.owner.env.Orch.OrgPlans(context.Background(), r.owner.org, 1)
	if err != nil || len(ps) == 0 {
		t.Fatalf("plans = %+v %v", ps, err)
	}
	return ps[0]
}

const regionFile = "version: 1\norg: acme\nparams:\n  app:\n    region:\n      type: param\n      value: us\n"

// The Config tab: an owner binds and the plan renders under the form with
// its answers; a viewer reads the binding but no plan and no Approve;
// reject closes a plan, approve queues the apply and shows its job.
func TestOrgConfigTab(t *testing.T) {
	r := newConfigRig(t)
	const tab = "/acme/-/drawer?tab=config"
	body := r.owner.do(t, "GET", tab, nil, true).Body.String()
	if !strings.Contains(body, `id="tab-config"`) || !strings.Contains(body, `href="/api/v1/orgs/acme/config/export"`) ||
		!strings.Contains(body, "(unbind)") || !strings.Contains(body, `value="`+r.conn+`"`) {
		t.Fatalf("unbound tab:\n%s", body)
	}

	r.file(t, regionFile)
	rec := r.owner.do(t, "POST", "/acme/-/drawer/config", url.Values{
		"connector": {r.conn},
		"repo":      {"acme/org"},
	}, true)
	body = rec.Body.String()
	first := r.latest(t)
	approve := "/acme/-/drawer/plans/" + first.ID + "/approve"
	reject := "/acme/-/drawer/plans/" + first.ID + "/reject"
	if rec.Code != http.StatusOK || !strings.Contains(body, "Config file saved and planned.") ||
		!strings.Contains(body, "What changes") || !strings.Contains(body, "app.region") ||
		!strings.Contains(body, `aria-hidden="true">+</span>`) || !strings.Contains(body, ">us</span>") ||
		!strings.Contains(body, approve) || !strings.Contains(body, reject) ||
		!strings.Contains(body, "/acme/-/drawer/unbind") || !strings.Contains(body, "Plan now") {
		t.Fatalf("bind = %d\n%s", rec.Code, body)
	}

	rec = r.viewer.do(t, "GET", tab, nil, true)
	body = rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `id="tab-config"`) || !strings.Contains(body, "acme/org") {
		t.Fatalf("viewer tab = %d\n%s", rec.Code, body)
	}
	if strings.Contains(body, "/approve") || strings.Contains(body, "Save and plan") || strings.Contains(body, "What changes") {
		t.Errorf("a viewer sees the plan or an answer:\n%s", body)
	}
	if rec := r.viewer.do(t, "POST", approve, nil, true); rec.Code != http.StatusForbidden {
		t.Errorf("viewer approve = %d, want 403", rec.Code)
	}

	rec = r.owner.do(t, "POST", reject, nil, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Plan rejected.") || strings.Contains(rec.Body.String(), approve) {
		t.Fatalf("reject = %d\n%s", rec.Code, rec.Body)
	}
	if p := r.latest(t); p.Status != "rejected" {
		t.Errorf("after reject = %s", p.Status)
	}

	rec = r.owner.do(t, "POST", "/acme/-/drawer/plan", nil, true)
	second := r.latest(t)
	if rec.Code != http.StatusOK || second.ID == first.ID || !strings.Contains(rec.Body.String(), "Planned: 1 to add.") {
		t.Fatalf("plan now = %d %+v\n%s", rec.Code, second, rec.Body)
	}
	rec = r.owner.do(t, "POST", "/acme/-/drawer/plans/"+second.ID+"/approve", nil, true)
	body = rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "Approved: the apply is queued.") ||
		!strings.Contains(body, "org-apply") || !strings.Contains(body, `sse-connect="/acme/-/jobs/`) {
		t.Fatalf("approve = %d\n%s", rec.Code, body)
	}

	// the apply's stream answers its own org, not another one
	m := regexp.MustCompile(`sse-connect="/acme(/-/jobs/[^"]+/events)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no job stream in:\n%s", body)
	}
	stream.PollEvery = 10 * time.Millisecond
	rec = r.owner.stream(t, "/acme"+m[1])
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "event: update") {
		t.Errorf("owner stream = %d\n%s", rec.Code, rec.Body)
	}
	env := r.owner.env
	u := env.User(t, "owner@other.test", false)
	other := env.Org(t, "other")
	env.Member(t, other, u, "owner")
	stranger := browser{env, r.owner.h, other, env.Session(t, u)}
	if rec := stranger.stream(t, "/other"+m[1]); rec.Code != http.StatusNotFound {
		t.Errorf("another org's stream of acme's job = %d, want 404", rec.Code)
	}
}

// stream reads an SSE route for a moment, then hangs up.
func (b browser) stream(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest("GET", path, nil).WithContext(ctx)
	req.AddCookie(&http.Cookie{Name: b.env.Orch.Sessions().CookieName(), Value: b.session})
	rec := httptest.NewRecorder()
	b.h.ServeHTTP(rec, req)
	return rec
}

// The org canvas banner: a pending plan warns and links to the tab, an
// error row says the config is invalid; a viewer gets neither.
func TestOrgPlanBanner(t *testing.T) {
	r := newConfigRig(t)
	if body := r.owner.do(t, "GET", "/acme", nil, false).Body.String(); strings.Contains(body, "Config ") {
		t.Errorf("a banner with no plan:\n%s", body)
	}
	r.file(t, regionFile)
	if _, err := r.owner.env.Orch.SetOrgConfigRepo(context.Background(), r.owner.org, r.conn, "acme/org", "", "", false); err != nil {
		t.Fatal(err)
	}
	href := `href="/acme?drawer=org:` + r.owner.org + `&amp;tab=config"`
	body := r.owner.do(t, "GET", "/acme", nil, false).Body.String()
	if !strings.Contains(body, "Config plan pending: 1 to add. View") || !strings.Contains(body, href) ||
		!strings.Contains(body, "bg-rw-warning/10") {
		t.Errorf("pending banner:\n%s", body)
	}
	if body := r.viewer.do(t, "GET", "/acme", nil, false).Body.String(); strings.Contains(body, "Config plan pending") {
		t.Error("a viewer gets the banner")
	}

	r.file(t, "version: 2\n")
	if _, err := r.owner.env.Orch.PlanOrgConfig(context.Background(), r.owner.org); err != nil {
		t.Fatal(err)
	}
	body = r.owner.do(t, "GET", "/acme", nil, false).Body.String()
	if !strings.Contains(body, "Config invalid. View details") || !strings.Contains(body, href) ||
		!strings.Contains(body, "bg-rw-danger/10") {
		t.Errorf("error banner:\n%s", body)
	}
}
