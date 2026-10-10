package canvas_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

func memberSession(t *testing.T, s *webtest.Site) string {
	t.Helper()
	u := s.User(t, "member@acme.test", false)
	s.Member(t, s.Org, u, "member")
	return s.Session(t, u)
}

// An owner edits the ladder; a member sees chips and no verbs, and a verb
// posted by a member is refused.
func TestOrgTiersSection(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	for _, n := range []string{"dev", "prod"} {
		if _, err := s.Orch.CreateTier(ctx, s.Org, n); err != nil {
			t.Fatal(err)
		}
	}
	owner := s.Do(t, "GET", "/acme/-/drawer?tab=settings", nil).Body.String()
	for _, want := range []string{
		`hx-post="/acme/-/drawer/tiers/dev/lock"`, `name="slugs"`, "A new tier starts locked.",
		`hx-post="/acme/-/drawer/tiers"`, "in 1 stack", "Delete tier dev?",
	} {
		if !strings.Contains(owner, want) {
			t.Errorf("owner view lacks %q", want)
		}
	}
	if strings.Contains(owner, "Set in the org file") {
		t.Error("banner without a bound org file")
	}
	m := memberSession(t, s)
	body := s.As(t, m, "GET", "/acme/-/drawer?tab=settings", nil).Body.String()
	if !strings.Contains(body, "Only an owner can change tiers.") || strings.Contains(body, "/tiers") || !strings.Contains(body, ">locked<") {
		t.Errorf("member view:\n%s", body)
	}
	if rec := s.As(t, m, "POST", "/acme/-/drawer/tiers", url.Values{"slug": {"qa"}}); rec.Code != http.StatusForbidden {
		t.Errorf("member add tier = %d", rec.Code)
	}
}

// Each tier verb goes through the drawer and the tab answers with the result.
func TestOrgTiersVerbs(t *testing.T) {
	s := webtest.New(t)
	post := func(path string, f url.Values) string {
		t.Helper()
		rec := s.Do(t, "POST", "/acme/-/drawer/tiers"+path, f)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s = %d %s", path, rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	post("", url.Values{"slug": {"dev"}})
	post("", url.Values{"slug": {"prod"}})
	post("/dev/lock", nil) // unchecked: open
	ts, _ := s.Orch.Tiers(context.Background(), s.Org)
	if len(ts) != 2 || ts[0].Locked || !ts[1].Locked {
		t.Fatalf("tiers = %+v", ts)
	}
	post("/order", url.Values{"slugs": {"prod", "dev"}})
	post("/prod/rename", url.Values{"name": {"live"}})
	if rec := s.Do(t, "POST", "/acme/-/drawer/tiers/dev/delete", nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("delete of a tier an env is in = %d", rec.Code) // shop/dev
	}
	body := post("/live/delete", nil)
	ts, _ = s.Orch.Tiers(context.Background(), s.Org)
	if len(ts) != 1 || ts[0].Slug != "dev" || strings.Contains(body, ">live<") {
		t.Errorf("tiers = %+v", ts)
	}
}

// A bound org file shows the drift banner, and edits still go through.
func TestOrgTiersManaged(t *testing.T) {
	r := newConfigRig(t)
	if _, err := r.owner.env.Orch.CreateTier(context.Background(), r.owner.org, "dev"); err != nil {
		t.Fatal(err)
	}
	r.file(t, regionFile)
	r.owner.do(t, "POST", "/acme/-/drawer/config", url.Values{"connector": {r.conn}, "repo": {"acme/org"}}, true)
	body := r.owner.do(t, "GET", "/acme/-/drawer?tab=settings", nil, true).Body.String()
	if !strings.Contains(body, "Set in the org file. Edits here show as drift on the next plan.") || !strings.Contains(body, "/tiers/dev/lock") {
		t.Errorf("managed view:\n%s", body)
	}
}

// The env drawer's Lock section: a toggle off-tier, fixed on a tiered env,
// and the lock route re-renders the tab it came from.
func TestEnvLockSection(t *testing.T) {
	s := webtest.New(t)
	d := "/acme/shop/dev/-/drawer"
	body := s.Do(t, "GET", d+"?tab=settings", nil).Body.String()
	for _, want := range []string{"Other envs can't read this env's values with [env] refs while locked.", `hx-post="` + d + `/lock"`, `checked`} {
		if !strings.Contains(body, want) {
			t.Errorf("off-tier lock lacks %q", want)
		}
	}
	rec := s.Do(t, "POST", d+"/lock", url.Values{"tab": {"releases"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="tab-releases"`) {
		t.Fatalf("lock = %d\n%s", rec.Code, rec.Body)
	}
	if e, _ := s.Orch.Envs(context.Background(), s.Tile.Stack); e[0].Locked {
		t.Error("lock not cleared")
	}
	if _, err := s.Orch.CreateTier(context.Background(), s.Org, "dev"); err != nil {
		t.Fatal(err)
	}
	body = s.Do(t, "GET", d+"?tab=settings", nil).Body.String()
	if !strings.Contains(body, "dev's lock is the org's tier lock. Change it in the org's tiers.") || strings.Contains(body, "/lock\"") {
		t.Errorf("tiered lock:\n%s", body)
	}
	if rec := s.Do(t, "POST", d+"/lock", nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("lock on a tiered env = %d", rec.Code)
	}
}
