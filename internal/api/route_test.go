package api_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/api"
	"github.com/FyrmForge/stackr/internal/api/handler/v1"
	"github.com/FyrmForge/stackr/internal/authz"
	"github.com/FyrmForge/stackr/internal/service"
)

// External routes are an admin's: create, list, delete; an owner is 403 on
// all three, and a refusal is a 4xx.
func TestExternalRoutes(t *testing.T) {
	w := newWorld(t)
	const body = `{"host":"pve.example.com","mode":"https","target":"10.0.0.5:8006","insecure":true}`
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/admin/routes", ""},
		{"POST", "/admin/routes", body},
		{"DELETE", "/admin/routes/x", ""},
	} {
		if code, _ := w.do(t, w.owner, c.method, c.path, c.body); code != 403 {
			t.Errorf("owner %s %s = %d, want 403", c.method, c.path, code)
		}
	}
	code, out := w.do(t, w.admin, "POST", "/admin/routes", body)
	var r service.ExternalRoute
	if code != 201 || json.Unmarshal([]byte(out), &r) != nil || r.Host != "pve.example.com" || !r.Insecure {
		t.Fatalf("create = %d %s", code, out)
	}
	if code, _ := w.do(t, w.admin, "POST", "/admin/routes", body); code != 409 {
		t.Errorf("duplicate = %d, want 409", code)
	}
	if code, _ := w.do(t, w.admin, "POST", "/admin/routes", `{"host":"a.io","mode":"tcp","target":"x"}`); code != 400 {
		t.Errorf("bad mode = %d, want 400", code)
	}
	code, out = w.do(t, w.admin, "GET", "/admin/routes", "")
	var rs []service.ExternalRoute
	if code != 200 || json.Unmarshal([]byte(out), &rs) != nil || len(rs) != 1 || rs[0].ID != r.ID {
		t.Errorf("list = %d %s", code, out)
	}
	if code, _ := w.do(t, w.admin, "DELETE", "/admin/routes/"+r.ID, ""); code != 204 && code != 200 {
		t.Errorf("delete = %d", code)
	}
	if code, _ := w.do(t, w.admin, "DELETE", "/admin/routes/"+r.ID, ""); code != 404 {
		t.Errorf("delete again = %d, want 404", code)
	}
}

// The server config routes are a contract the wave 1 workers build against:
// path, method, op and verb stay as written in serverconfig-seams.md.
func TestServerConfigRouteContract(t *testing.T) {
	type key struct{ method, path string }
	got := map[key][2]string{}
	for _, r := range api.Routes(&v1.H{}) {
		got[key{r.Method, r.Path}] = [2]string{r.Op, string(r.Verb)}
	}
	for k, want := range map[key][2]string{
		{"GET", "/admin/config-repo"}:                            {"admin.config-repo-get", "admin.read"},
		{"PUT", "/admin/config-repo"}:                            {"admin.config-repo", "serverconfig.bind"},
		{"POST", "/admin/config/plan"}:                           {"admin.config-plan", "serverconfig.bind"},
		{"POST", "/admin/config/plan-file"}:                      {"admin.config-plan-file", "serverconfig.bind"},
		{"POST", "/admin/config/plan-preview"}:                   {"admin.config-plan-preview", "serverconfig.bind"},
		{"GET", "/admin/config/plans"}:                           {"admin.config-plans", "admin.read"},
		{"GET", "/admin/config/plans/:plan"}:                     {"admin.config-plan-get", "admin.read"},
		{"POST", "/admin/config/plans/:plan/approve"}:            {"admin.config-plan-approve", "serverplan.approve"},
		{"POST", "/admin/config/plans/:plan/reject"}:             {"admin.config-plan-reject", "serverplan.approve"},
		{"GET", "/admin/config/export"}:                          {"admin.config-export", "admin.read"},
		{"POST", "/orgs/:org/domain-resources/:resource/rename"}: {"domain-resource.rename", "domain.resource"},
		{"POST", "/admin/domain-resources/:resource/rename"}:     {"admin.domain-resource-rename", "serverdefaults.set"},
		{"GET", "/admin/connectors"}:                             {"admin.connector-list", "admin.read"},
		{"POST", "/admin/connectors"}:                            {"admin.connector-begin", "connector.admin"},
		{"PUT", "/admin/connectors/:connector/name"}:             {"admin.connector-rename", "connector.admin"},
		{"PUT", "/admin/connectors/:connector/shares"}:           {"admin.connector-share", "connector.admin"},
		{"DELETE", "/admin/connectors/:connector"}:               {"admin.connector-delete", "connector.admin"},
	} {
		if got[k] != want {
			t.Errorf("%s %s = %v, want %v", k.method, k.path, got[k], want)
		}
		if authz.LevelOf(authz.Verb(want[1])) == authz.LevelAdmin != !strings.HasPrefix(k.path, "/orgs/") {
			t.Errorf("%s %s: verb %s is the wrong level for the path", k.method, k.path, want[1])
		}
	}
}
