package admin_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The Params tab edits the server scope with the same editor as an org: a
// save writes the server rows (and no org's), a secret's value never comes
// back, a refusal is a 422 in the editor, and an owner cannot post.
func TestAdminParams(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	root := s.Session(t, s.User(t, "root@x.test", true))

	tab := s.As(t, root, "GET", "/-/admin/config", nil)
	for _, want := range []string{`id="vars-editor"`, "/-/admin/params", "stackr-server.yml", `name="new_name"`} {
		if tab.Code != http.StatusOK || !strings.Contains(tab.Body.String(), want) {
			t.Fatalf("params tab = %d, no %s in\n%s", tab.Code, want, tab.Body)
		}
	}
	if strings.Contains(tab.Body.String(), "redeploys the running tiles") {
		t.Error("the server tab promises a tile redeploy it does not do")
	}

	rec := s.As(t, root, "POST", "/-/admin/params", url.Values{
		"new_collection": {"s3"}, "new_name": {"secret_key"}, "new_kind": {"secret"}, "new_value": {"LEAK-sk"},
	})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Saved.") ||
		strings.Contains(rec.Body.String(), "LEAK-sk") || rec.Header().Get("HX-Retarget") != "#vars-editor" {
		t.Fatalf("save = %d %s", rec.Code, rec.Body)
	}
	ps, err := s.Orch.Params(ctx, service.ServerParamScope, true)
	if err != nil || len(ps) != 1 || ps[0].Value != "LEAK-sk" {
		t.Fatalf("server params = %+v, %v", ps, err)
	}
	if ps, _ := s.Orch.Params(ctx, service.ParamScope{Kind: "org", ID: s.Org}, true); len(ps) != 0 {
		t.Errorf("org params = %+v; the save reached an org", ps)
	}
	if after := s.As(t, root, "GET", "/-/admin/config", nil).Body.String(); strings.Contains(after, "LEAK-sk") ||
		!strings.Contains(after, `name="secret.s3.secret_key"`) {
		t.Errorf("params tab after a save shows the secret value or lost the row:\n%s", after)
	}

	if rec := s.As(t, root, "POST", "/-/admin/params", url.Values{
		"new_collection": {"3s"}, "new_name": {"x"}, "new_kind": {"param"},
	}); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad collection = %d, want 422", rec.Code)
	}
	if rec := s.As(t, root, "POST", "/-/admin/params", url.Values{
		"param.s3.secret_key": {"open"},
	}); rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusConflict {
		t.Errorf("declassify = %d, want a refusal", rec.Code)
	}

	if rec := s.As(t, root, "POST", "/-/admin/params/delete", url.Values{"collection": {"s3"}, "name": {"secret_key"}}); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "Deleted.") {
		t.Errorf("delete = %d %s", rec.Code, rec.Body)
	}

	for _, p := range []string{"/-/admin/params", "/-/admin/params/delete"} {
		if rec := s.Do(t, "POST", p, url.Values{"new_name": {"x"}}); rec.Code != http.StatusForbidden {
			t.Errorf("owner POST %s = %d, want 403", p, rec.Code)
		}
	}
}

// A new row with no name is refused on its name field, not "Saved."; bad
// group and name are each marked at once.
func TestAdminParamsNewRowRefusals(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	rec := s.As(t, root, "POST", "/-/admin/params", url.Values{"new_collection": {"qa"}, "new_value": {"v"}, "new_kind": {"param"}})
	body := rec.Body.String()
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "Give the variable a name.") ||
		!strings.Contains(body, `aria-describedby="error-new_name"`) || !strings.Contains(body, `value="qa"`) {
		t.Errorf("nameless add = %d\n%s", rec.Code, body)
	}
	rec = s.As(t, root, "POST", "/-/admin/params", url.Values{"new_collection": {"bad coll!"}, "new_name": {"bad name!"}, "new_kind": {"param"}})
	body = rec.Body.String()
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, `aria-describedby="error-new_collection"`) ||
		!strings.Contains(body, `aria-describedby="error-new_name"`) {
		t.Errorf("bad group and name = %d\n%s", rec.Code, body)
	}
}
