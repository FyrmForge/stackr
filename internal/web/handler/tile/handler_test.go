package tile_test

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

const drawer = "/acme/shop/dev/-/tiles/api"

// Every tab of an image tile answers a drawer fragment, never a page.
func TestEveryTabServes(t *testing.T) {
	s := webtest.New(t)
	for _, tab := range []string{
		"status",
		"logs",
		"domains",
		"env",
		"access",
		"settings",
		"jobs",
		"image",
		"runs",
		"backups",
		"nonsense",
	} {
		rec := s.Do(t, "GET", drawer+"?tab="+tab, nil)
		body := rec.Body.String()
		if rec.Code != 200 || !strings.HasPrefix(body, `<div id="drawer-view">`) || strings.Contains(body, "<html") {
			t.Errorf("tab %s = %d\n%s", tab, rec.Code, body)
		}
	}
}

// An action runs its verb and answers its tab; a refusal shows over the
// tab with 422 so htmx still swaps it.
func TestActions(t *testing.T) {
	s := webtest.New(t)
	rec := s.Do(t, "POST", drawer+"/deploy", url.Values{})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "deploy queued") ||
		!strings.Contains(rec.Body.String(), "/-/jobs/") {
		t.Errorf("deploy = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/pause", url.Values{"paused": {"true"}})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "only cron tiles") {
		t.Errorf("pause an image tile = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/env", url.Values{"env": {"GREETING=hi\nNAME=bob"}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `aria-label="Delete GREETING"`) ||
		!strings.Contains(rec.Body.String(), "saved") {
		t.Errorf("env = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/env", url.Values{"drop": {"GREETING"}})
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "Delete GREETING") ||
		!strings.Contains(rec.Body.String(), "Delete NAME") {
		t.Errorf("drop = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/env", url.Values{"env": {"NAME=bob\noops"}})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "line 2: want KEY=VALUE") {
		t.Errorf("bad env = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/settings", url.Values{"cpu_limit": {"lots"}, "mem_limit_mb": {"256"}})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "must be a number") ||
		!strings.Contains(rec.Body.String(), `value="256"`) || !strings.Contains(rec.Body.String(), `value="lots"`) {
		t.Errorf("bad settings = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/settings", url.Values{"cpu_limit": {"0.5"}, "mem_limit_mb": {""}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `value="0.5"`) {
		t.Errorf("settings = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.Do(t, "POST", drawer+"/settings", url.Values{"restart_policy": {"sometimes"}})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), `id="error-restart_policy" data-has-error="true"`) {
		t.Errorf("refused restart policy = %d\n%s", rec.Code, rec.Body)
	}
}

// Writes need the CSRF token like every form on the site.
func TestActionNeedsCSRF(t *testing.T) {
	s := webtest.New(t)
	if rec := s.DoNoCSRF(t, "POST", drawer+"/deploy"); rec.Code != 403 && rec.Code != 400 {
		t.Errorf("deploy without CSRF = %d", rec.Code)
	}
}

// Add auto domain (step 7a): refused while no domain resource is visible;
// with one, the orchestrator names the host under it and records it.
func TestAutoDomain(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	if body := s.Do(t, "GET", drawer+"?tab=settings", nil).Body.String(); !strings.Contains(body, `name="auto"`) {
		t.Errorf("no auto domain form:\n%s", body)
	}
	rec := s.Do(t, "POST", drawer+"/domains", url.Values{"auto": {"1"}})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "No domain resource to nest under") {
		t.Errorf("auto with no resource = %d\n%s", rec.Code, rec.Body)
	}
	res, err := s.Orch.CreateDomainResource(ctx, "org", s.Org, "acme.io", false, "")
	if err != nil {
		t.Fatal(err)
	}
	// the auto form has no port field: the refusal names the tile, no field
	setPort(t, s, 0)
	rec = s.Do(t, "POST", drawer+"/domains", url.Values{"auto": {"1"}})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "Set a container port on the tile first.") ||
		strings.Contains(rec.Body.String(), "the domain needs a container port") {
		t.Errorf("auto with no port = %d\n%s", rec.Code, rec.Body)
	}
	setPort(t, s, 80)
	rec = s.Do(t, "POST", drawer+"/domains", url.Values{"auto": {"1"}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "attached api.shop.acme.io") {
		t.Fatalf("auto = %d\n%s", rec.Code, rec.Body)
	}
	ds, err := s.Orch.Domains(ctx, s.Tile.ID)
	if err != nil || len(ds) != 1 || !ds[0].Auto || ds[0].ResourceID == nil || *ds[0].ResourceID != res.ID {
		t.Errorf("domains = %+v %v, want one auto row named by %s", ds, err, res.ID)
	}
}

func setPort(t *testing.T, s *webtest.Site, port int) {
	t.Helper()
	_, _, err := s.Orch.UpdateTile(context.Background(), s.Tile.ID, func(tl *service.Tile) error {
		tl.ContainerPort = port
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The Access tab: the slice_access entries (each a select that posts),
// the creds the tile holds, and an add form over the env's slice tiles.
func TestAccessTab(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	pg, err := s.Orch.CreateManagedTile(ctx, service.Tile{EnvironmentID: s.Tile.Env, Name: "pg"}, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	main, err := s.Orch.CreateSliceTile(ctx, s.Tile.Env, "main", "shop:dev:pg", "")
	if err != nil {
		t.Fatal(err)
	}
	cache, err := s.Orch.CreateSliceTile(ctx, s.Tile.Env, "cache", "shop:dev:pg", "")
	if err != nil {
		t.Fatal(err)
	}
	// cache is reached by a ref alone: it shows once bound (DECIDE 204 b).
	s.Bound(t, cache.ID, pg.ID, s.Tile.ID, "write")
	rec := s.Do(t, "POST", drawer+"/access", url.Values{
		"slice":  {"main"},
		"access": {"read"},
	})
	body := rec.Body.String()
	for _, w := range []string{
		"saved",
		`href="/acme/shop/dev?drawer=` + main.ID + `&amp;tab=overview"`,
		`name="slice" value="main"`,
		`<option value="read" selected>`,
		`href="/acme/shop/dev?drawer=` + cache.ID + `&amp;tab=overview"`,
		s.Tile.ID + "_user",
		`<option value="cache">cache</option>`,
	} {
		if rec.Code != 200 || !strings.Contains(body, w) {
			t.Errorf("access tab = %d, lacks %q", rec.Code, w)
		}
	}
	rec = s.Do(t, "POST", drawer+"/access", url.Values{
		"slice":  {"nope"},
		"access": {"read"},
	})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "no slice tile nope") {
		t.Errorf("unknown slice = %d\n%s", rec.Code, rec.Body)
	}
}
