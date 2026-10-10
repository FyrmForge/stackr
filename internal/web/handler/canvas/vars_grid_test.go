package canvas_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

func value(t *testing.T, s *webtest.Site, kind, id string) map[string]string {
	t.Helper()
	ps, err := s.Orch.Params(context.Background(), service.ParamScope{Kind: kind, ID: id}, true)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, p := range ps {
		out[p.Collection+"."+p.Name] = p.Value
	}
	return out
}

// An org with no tiers gets a grid of two columns: all (the org scope) and
// pr (org_pr); a cell writes its scope.
func TestOrgUntieredGrid(t *testing.T) {
	s := webtest.New(t)
	_, _ = s.Orch.SetParams(context.Background(), service.ParamScope{Kind: "org", ID: s.Org}, []service.ParamEntry{{Collection: "smtp", Name: "host", Kind: "param", Value: "h"}})
	body := get(t, s, "/acme/-/vars")
	for _, w := range []string{"data-wide", ">all<", ">pr<", `value="org"`, `value="org_pr"`} {
		if !strings.Contains(body, w) {
			t.Errorf("no %q in untiered grid", w)
		}
	}
	if strings.Contains(body, `id="param-editor"`) {
		t.Error("untiered org still shows the single editor")
	}
	for scope, kind := range map[string]string{"org": "org", "org_pr": "org_pr"} {
		rec := s.Do(t, "POST", "/acme/-/vars", url.Values{"scope": {scope}, "param.smtp.host": {scope}})
		if rec.Code != http.StatusOK {
			t.Fatalf("set %s = %d %s", scope, rec.Code, rec.Body)
		}
		if got := value(t, s, kind, s.Org); got["smtp.host"] != scope {
			t.Errorf("%s = %v", scope, got)
		}
	}
	s.Do(t, "POST", "/acme/-/vars/delete", url.Values{"scope": {"org"}, "collection": {"smtp"}, "name": {"host"}})
	if got := value(t, s, "org", s.Org); got["smtp.host"] != "" {
		t.Errorf("not unset: %v", got)
	}
	if rec := s.Do(t, "POST", "/acme/-/vars", url.Values{"param.a.b": {"x"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("no scope = %d", rec.Code)
	}
}

// A tiered org gets a grid: a column per tier plus pr, a lock on a locked
// tier, secrets masked with no value in the page; a cell writes its scope.
func TestOrgTieredGrid(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	dev, err := s.Orch.CreateTier(ctx, s.Org, "dev")
	if err != nil {
		t.Fatal(err)
	}
	prod, err := s.Orch.CreateTier(ctx, s.Org, "prod")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.Orch.SetParams(ctx, service.ParamScope{Kind: "tier", ID: dev.ID}, []service.ParamEntry{
		{Collection: "smtp", Name: "host", Kind: "param", Value: "smtp.dev"},
		{Collection: "smtp", Name: "pass", Kind: "secret", Value: "hunter2"},
	})
	body := get(t, s, "/acme/-/vars")
	for _, w := range []string{"data-wide", "smtp.dev", ">dev<", ">prod<", ">pr<", "Locked: read only from inside prod", `value="tier:` + dev.ID + `"`, "Replace", "unset"} {
		if !strings.Contains(body, w) {
			t.Errorf("no %q in grid", w)
		}
	}
	if strings.Contains(body, "hunter2") || strings.Contains(body, "Reveal") {
		t.Error("a secret value or a reveal reached the page")
	}

	rec := s.Do(t, "POST", "/acme/-/vars", url.Values{"scope": {"tier:" + prod.ID}, "param.smtp.host": {"smtp.prod"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("set a cell = %d %s", rec.Code, rec.Body)
	}
	if got := value(t, s, "tier", prod.ID); got["smtp.host"] != "smtp.prod" {
		t.Errorf("prod = %v", got)
	}
	if got := value(t, s, "tier", dev.ID); got["smtp.host"] != "smtp.dev" {
		t.Errorf("dev changed: %v", got)
	}
	s.Do(t, "POST", "/acme/-/vars", url.Values{"scope": {"org_pr"}, "param.smtp.host": {"localhost"}})
	if got := value(t, s, "org_pr", s.Org); got["smtp.host"] != "localhost" {
		t.Errorf("org_pr = %v", got)
	}
	// a blank secret box keeps the stored value
	s.Do(t, "POST", "/acme/-/vars", url.Values{"scope": {"tier:" + dev.ID}, "secret.smtp.pass": {""}})
	if got := value(t, s, "tier", dev.ID); got["smtp.pass"] != "hunter2" {
		t.Errorf("secret lost: %v", got)
	}
	// Unset drops that scope only
	s.Do(t, "POST", "/acme/-/vars/delete", url.Values{"scope": {"tier:" + dev.ID}, "collection": {"smtp"}, "name": {"host"}})
	if got := value(t, s, "tier", dev.ID); got["smtp.host"] != "" {
		t.Errorf("not unset: %v", got)
	}
	// a scope the level does not show is refused
	rec = s.Do(t, "POST", "/acme/-/vars", url.Values{"scope": {"env:" + s.Tile.Env}, "param.a.b": {"x"}})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("foreign scope = %d", rec.Code)
	}
	// the Variables card counts the new scopes
	if !strings.Contains(get(t, s, "/acme"), `hx-get="/acme/-/vars"`) {
		t.Error("no Variables card on a tiered org with params")
	}
}

// The stack grid has a column per static env and pr; a cell sets the env
// scope or stack_pr; Add writes every column with a value; secrets never
// render.
func TestStackGrid(t *testing.T) {
	s := webtest.New(t)
	body := get(t, s, "/acme/shop/-/vars")
	for _, w := range []string{"data-wide", `name="add_value.env:` + s.Tile.Env + `"`, `name="add_value.stack_pr"`, "+ Add group"} {
		if !strings.Contains(body, w) {
			t.Errorf("no %q in stack grid", w)
		}
	}
	rec := s.Do(t, "POST", "/acme/shop/-/vars", url.Values{
		"add_group": {"app"}, "add_name": {"key"}, "add_kind": {"secret"},
		"add_secret.env:" + s.Tile.Env: {"s3cret"}, "add_secret.stack_pr": {"sandbox"},
	})
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "s3cret") {
		t.Fatalf("add = %d %s", rec.Code, rec.Body)
	}
	if got := value(t, s, "env", s.Tile.Env); got["app.key"] != "s3cret" {
		t.Errorf("env = %v", got)
	}
	if got := value(t, s, "stack_pr", s.Tile.Stack); got["app.key"] != "sandbox" {
		t.Errorf("stack_pr = %v", got)
	}
	if body := get(t, s, "/acme/shop/-/vars"); strings.Contains(body, "s3cret") || !strings.Contains(body, "app") {
		t.Error("secret reached the page, or the group is missing")
	}
	// a name plain in one column and secret in another keeps each cell's kind
	s.Do(t, "POST", "/acme/shop/-/vars", url.Values{"add_group": {"app"}, "add_name": {"mix"}, "add_kind": {"secret"}, "add_secret.env:" + s.Tile.Env: {"s3cret"}})
	rec = s.Do(t, "POST", "/acme/shop/-/vars", url.Values{"add_group": {"app"}, "add_name": {"mix"}, "add_kind": {"param"}, "add_value.stack_pr": {"plainval"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("mixed add = %d %s", rec.Code, rec.Body)
	}
	if body := rec.Body.String(); !strings.Contains(body, "plainval") || strings.Contains(body, "s3cret") ||
		!strings.Contains(body, `name="param.app.mix"`) || !strings.Contains(body, `name="secret.app.mix"`) {
		t.Errorf("mixed row did not keep each cell's kind:\n%s", body)
	}
	// no value anywhere is refused
	rec = s.Do(t, "POST", "/acme/shop/-/vars", url.Values{"add_group": {"app"}, "add_name": {"other"}, "add_kind": {"param"}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("empty add = %d", rec.Code)
	}
	// the stack-level scope is gone: a stack-scoped write has no column
	rec = s.Do(t, "POST", "/acme/shop/-/vars", url.Values{"scope": {"stack"}, "param.a.b": {"x"}})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("stack scope = %d", rec.Code)
	}
	// the env drawer's params is that env's column alone
	body = get(t, s, "/acme/shop/dev/-/vars")
	if !strings.Contains(body, `add_value.env:`+s.Tile.Env) || strings.Contains(body, "add_value.stack_pr") {
		t.Errorf("env editor is not one column:\n%s", body)
	}
}
