package setup_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/servicetest"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// admin is a server admin in no org yet, and their session.
func admin(t *testing.T, s *webtest.Site) (string, string) {
	t.Helper()
	id := s.User(t, "root@x.test", true)
	return id, s.Session(t, id)
}

// draft answers the branch question and returns the org the admin now
// half owns, and where the wizard sent them.
func draft(t *testing.T, s *webtest.Site, userID, session, mode string) (service.Org, string) {
	t.Helper()
	rec := s.As(t, session, "POST", "/setup", url.Values{"mode": {mode}})
	orgs, err := s.Orch.Orgs(context.Background(), userID)
	if err != nil || len(orgs) != 1 {
		t.Fatalf("orgs after POST /setup = %+v %v (%d %v)", orgs, err, rec.Code, rec.Header())
	}
	return orgs[0], rec.Header().Get("HX-Redirect")
}

func org(t *testing.T, s *webtest.Site, userID string) service.Org {
	t.Helper()
	orgs, err := s.Orch.Orgs(context.Background(), userID)
	if err != nil || len(orgs) != 1 {
		t.Fatalf("orgs = %+v %v", orgs, err)
	}
	return orgs[0]
}

// redirect posts as session and wants HX-Redirect to want.
func redirect(t *testing.T, s *webtest.Site, session, path string, form url.Values, want string) {
	t.Helper()
	rec := s.As(t, session, "POST", path, form)
	if got := rec.Header().Get("HX-Redirect"); got != want {
		t.Fatalf("POST %s = %d HX-Redirect %q, want %q\n%s", path, rec.Code, got, want, rec.Body)
	}
}

// page gets path as session and wants every one of want in the body.
func page(t *testing.T, s *webtest.Site, session, path string, want ...string) string {
	t.Helper()
	rec := s.As(t, session, "GET", path, nil)
	body := rec.Body.String()
	for _, w := range want {
		if rec.Code != http.StatusOK || !strings.Contains(body, w) {
			t.Fatalf("GET %s = %d, want %q in:\n%s", path, rec.Code, w, body)
		}
	}
	return body
}

// By hand: the branch question, a name, a domain, an invite, Finish.
// The steps close once it is finished.
func TestByHand(t *testing.T) {
	s := webtest.New(t)
	uid, sess := admin(t, s)
	page(t, s, sess, "/setup", `hx-post="/setup"`, `value="config"`, `value="ui"`)

	og, to := draft(t, s, uid, sess, "ui")
	b := "/" + og.Slug + "/-/setup"
	if to != b+"/name" || og.Name != service.DraftOrgName || og.SetupDoneAt != nil {
		t.Fatalf("draft = %+v, sent to %q", og, to)
	}
	// home carries on into the wizard
	if got := s.As(t, sess, "GET", "/", nil).Header().Get("HX-Redirect"); got != b+"/done" {
		t.Errorf("home with one draft = %q, want %s/done", got, b)
	}
	page(t, s, sess, b+"/done", "Finish setting up this organization", b+"/name")
	if rec := s.As(t, sess, "POST", b+"/done", nil); rec.Header().Get("HX-Redirect") != b+"/name" {
		t.Errorf("finish unnamed = %v", rec.Header())
	}

	page(t, s, sess, b+"/name", "Name your organization", `aria-label="Step 2 of 6"`)
	body := s.As(t, sess, "POST", b+"/name", url.Values{"name": {""}}).Body.String()
	if !strings.Contains(body, `id="setup-name"`) || !strings.Contains(body, "rw-danger") {
		t.Errorf("empty name:\n%s", body)
	}
	redirect(t, s, sess, b+"/name", url.Values{"name": {"Globex"}}, "/globex/-/setup/connector")
	b = "/globex/-/setup"

	page(t, s, sess, b+"/connector", "Connect GitHub", `name="gh_org"`, `name="csrf_token"`)
	// the connector form is a native post: its token rides in the form
	req := httptest.NewRequest("POST", b+"/connector", strings.NewReader("csrf_token=tok&gh_org="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "csrf", Value: "tok"})
	req.AddCookie(&http.Cookie{Name: s.Orch.Sessions().CookieName(), Value: sess})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="github-manifest"`) {
		t.Errorf("native connector post = %d\n%s", rec.Code, rec.Body)
	}
	page(t, s, sess, b+"/domain", "Add a domain", `name="host"`)
	redirect(t, s, sess, b+"/domain", url.Values{"host": {"globex.example.com"}}, b+"/team")
	rs, err := s.Orch.DomainResources(context.Background(), org(t, s, uid).ID)
	if err != nil || !slices.ContainsFunc(rs, func(r service.DomainResource) bool { return r.Host == "globex.example.com" }) {
		t.Errorf("domains = %+v %v", rs, err)
	}
	redirect(t, s, sess, b+"/team/members", url.Values{"email": {"new@x.test"}, "role": {"owner"}}, b+"/team")
	page(t, s, sess, b+"/team", "new@x.test", "Invited")

	page(t, s, sess, b+"/done", "Finish and go to Globex", b+"/domain", b+"/team")
	redirect(t, s, sess, b+"/done", nil, "/globex")
	if og := org(t, s, uid); og.SetupDoneAt == nil || og.Name != "Globex" {
		t.Fatalf("finished org = %+v", og)
	}
	if got := s.As(t, sess, "GET", b+"/team", nil).Header().Get("HX-Redirect"); got != "/globex?drawer=org:"+og.ID+"&tab=members" {
		t.Errorf("a finished org's step = %q", got)
	}
}

// From a config file: bind and plan, switch to by hand and back (which
// drops the binding and its plan), approve, and the apply names the org
// and binds its stack before the wizard moves on.
func TestFromConfig(t *testing.T) {
	g := servicetest.NewGit(t)
	s := webtest.NewWith(t, []service.Option{g.Option()})
	uid, sess := admin(t, s)
	og, to := draft(t, s, uid, sess, "config")
	b := "/" + og.Slug + "/-/setup"
	if to != b+"/connector" {
		t.Fatalf("config draft sent to %q", to)
	}
	conn := s.Connector(t, og.ID, "whsec")
	g.Commit(t, "acme/shop", "main", map[string]string{
		"stackr-compose.yml": "version: 1\nstack: shop\nladder:\n  - dev\nhead: main\n",
	})
	g.Commit(t, "acme/org", "main", map[string]string{
		"stackr-org.yml": "version: 1\norg: Globex\nstacks:\n  shop:\n    repo: acme/shop\n",
	})
	page(t, s, sess, b+"/config", "Config as code", "Reading repositories.", `aria-label="Step 3 of 5"`)

	bind := url.Values{
		"connector_id": {conn},
		"repo":         {"acme/org"},
		"branch":       {"main"},
		"path":         {"stackr-org.yml"},
	}
	rec := s.As(t, sess, "POST", b+"/config", bind)
	plans, err := s.Orch.OrgPlans(context.Background(), og.ID, 1)
	if err != nil || len(plans) != 1 || plans[0].Status != "pending" {
		t.Fatalf("plans after bind = %+v %v\n%s", plans, err, rec.Body)
	}
	first := plans[0]
	if got := rec.Header().Get("HX-Redirect"); got != "/"+og.ID+"/-/setup/config/plan?plan="+first.ID {
		t.Fatalf("bind sent to %q", got)
	}

	redirect(t, s, sess, b+"/mode", url.Values{"mode": {"ui"}}, b+"/name")
	if og := org(t, s, uid); og.ConfigRepo != "" || og.SetupMode != service.SetupUI {
		t.Errorf("by hand kept the binding: %+v", og)
	}
	if p, _ := s.Orch.OrgPlan(context.Background(), first.ID); p.Status != "rejected" {
		t.Errorf("by hand left the plan %s", p.Status)
	}
	redirect(t, s, sess, b+"/mode", url.Values{"mode": {"config"}}, b+"/connector")

	s.As(t, sess, "POST", b+"/config", bind)
	plans, _ = s.Orch.OrgPlans(context.Background(), og.ID, 1)
	pl := plans[0]
	plan := "/" + og.ID + "/-/setup/config/plan?plan=" + pl.ID
	approve := b + "/config/plan/" + pl.ID + "/approve"
	page(t, s, sess, plan, "Review the plan", "Globex", approve)

	rec = s.As(t, sess, "POST", approve, nil)
	wait := rec.Header().Get("HX-Redirect")
	if !strings.HasPrefix(wait, plan+"&job=") {
		t.Fatalf("approve = %d %q", rec.Code, wait)
	}
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		p, err := s.Orch.OrgPlan(context.Background(), pl.ID)
		if err != nil || p.Status == "error" {
			t.Fatalf("plan = %+v %v", p, err)
		}
		if p.Status == "applied" {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("plan still %s", p.Status)
		}
	}
	// The wait lands on the next step, under the name the file gave.
	if got := s.As(t, sess, "GET", plan, nil).Header().Get("HX-Redirect"); got != "/globex/-/setup/team" {
		t.Errorf("applied plan sent to %q", got)
	}
	og = org(t, s, uid)
	sts, err := s.Orch.Stacks(context.Background(), og.ID)
	if og.Name != "Globex" || err != nil || len(sts) != 1 || sts[0].Slug != "shop" {
		t.Fatalf("applied org = %+v, stacks %+v %v", og, sts, err)
	}
	b = "/globex/-/setup"
	page(t, s, sess, b+"/done", "Finish and go to Globex", b+"/config")
	redirect(t, s, sess, b+"/done", nil, "/globex")
}

// Inside an unfinished org the owner is sent to the summary; a member
// sees the holding page, and a drawer of theirs navigates to it.
func TestGate(t *testing.T) {
	s := webtest.New(t)
	uid, sess := admin(t, s)
	og, _ := draft(t, s, uid, sess, "ui")
	member := s.User(t, "m@x.test", false)
	s.Member(t, og.ID, member, "member")
	msess := s.Session(t, member)

	if got := s.As(t, sess, "GET", "/"+og.Slug, nil).Header().Get("HX-Redirect"); got != "/"+og.Slug+"/-/setup/done" {
		t.Errorf("owner in an unfinished org = %q", got)
	}
	// a second owner, no admin, walks the same wizard
	co := s.User(t, "co@x.test", false)
	s.Member(t, og.ID, co, "owner")
	cosess := s.Session(t, co)
	b := "/" + og.Slug + "/-/setup"
	if got := s.As(t, cosess, "GET", "/"+og.Slug, nil).Header().Get("HX-Redirect"); got != b+"/done" {
		t.Errorf("co-owner in an unfinished org = %q", got)
	}
	page(t, s, cosess, b+"/done", "Finish setting up this organization")
	redirect(t, s, cosess, b+"/domain", url.Values{"host": {"co.example.com"}}, b+"/team")
	redirect(t, s, cosess, b+"/mode", url.Values{"mode": {"config"}}, b+"/connector")
	if got := s.As(t, msess, "GET", "/"+og.Slug+"/-/drawer", nil).Header().Get("HX-Redirect"); got != "/"+og.Slug {
		t.Errorf("member drawer = %q", got)
	}
	req := httptest.NewRequest("GET", "/"+og.Slug, nil)
	req.AddCookie(&http.Cookie{Name: s.Orch.Sessions().CookieName(), Value: msess})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "is still being set up") {
		t.Errorf("member page = %d\n%s", rec.Code, rec.Body)
	}
	if rec := s.As(t, msess, "GET", "/"+og.Slug+"/-/setup/done", nil); rec.Code != http.StatusForbidden {
		t.Errorf("member on the wizard = %d, want 403", rec.Code)
	}
	// the org the member is also in stays open
	s.Member(t, s.Org, member, "member")
	if rec := s.As(t, msess, "GET", "/acme", nil); rec.Code != http.StatusOK || rec.Header().Get("HX-Redirect") != "" {
		t.Errorf("a finished org = %d %v", rec.Code, rec.Header())
	}
}

// /setup for someone who may not create an org: to their org, or told to
// ask for an invite.
func TestStartNonAdmin(t *testing.T) {
	s := webtest.New(t)
	if got := s.Do(t, "GET", "/setup", nil).Header().Get("HX-Redirect"); got != "/acme" {
		t.Errorf("member /setup = %q", got)
	}
	lone := s.Session(t, s.User(t, "lone@x.test", false))
	body := page(t, s, lone, "/setup", "invite link")
	if strings.Contains(body, `hx-post="/setup"`) {
		t.Errorf("non-admin got the branch question:\n%s", body)
	}
	if rec := s.As(t, lone, "POST", "/setup", url.Values{"mode": {"ui"}}); rec.Code != http.StatusForbidden {
		t.Errorf("non-admin POST /setup = %d", rec.Code)
	}
}
