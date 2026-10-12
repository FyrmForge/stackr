package admin_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// The Nodes tab is admin-only: the list, the add-node steps, the node drawer
// with its tag refusal for the controller. An org owner reaches none of it.
func TestAdminNodes(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))

	rec := s.As(t, root, "GET", "/-/admin?tab=nodes", nil)
	body := rec.Body.String()
	for _, want := range []string{`id="tab-nodes"`, "controller", "Only this server so far", "Add node"} {
		if rec.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("nodes tab = %d, no %q in\n%s", rec.Code, want, body)
		}
	}

	rec = s.As(t, root, "GET", "/-/admin/nodes/add", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Get join command") {
		t.Fatalf("add step 1 = %d\n%s", rec.Code, rec.Body)
	}
	// a bad name stays on step one with the refusal
	rec = s.As(t, root, "POST", "/-/admin/nodes/add", url.Values{"name": {"Bad Name"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Get join command") ||
		!strings.Contains(rec.Body.String(), "lowercase") {
		t.Errorf("bad name = %d\n%s", rec.Code, rec.Body)
	}
	// nothing has called in: the poll has nothing to say
	if rec := s.As(t, root, "GET", "/-/admin/nodes/add/poll?name=w9", nil); rec.Code != http.StatusNoContent {
		t.Errorf("poll = %d, want 204", rec.Code)
	}

	// the controller's drawer, and the refusal to untag its ingress
	ns, err := s.Orch.Nodes(t.Context())
	if err != nil || len(ns) != 1 {
		t.Fatalf("nodes = %+v, %v", ns, err)
	}
	id := ns[0].ID
	rec = s.As(t, root, "GET", "/-/admin/nodes/"+id+"?tab=tags", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Open to orgs") ||
		!strings.Contains(rec.Body.String(), "open to every organization") {
		t.Fatalf("tags tab = %d\n%s", rec.Code, rec.Body)
	}
	rec = s.As(t, root, "POST", "/-/admin/nodes/"+id+"/tags", url.Values{"tag": {"builder"}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Set another default ingress first") {
		t.Errorf("untag ingress = %d\n%s", rec.Code, rec.Body)
	}

	for _, c := range []struct{ method, path string }{
		{"GET", "/-/admin?tab=nodes"}, {"GET", "/-/admin/nodes/add"}, {"POST", "/-/admin/nodes/add"},
		{"GET", "/-/admin/nodes/" + id}, {"POST", "/-/admin/nodes/" + id + "/tags"},
	} {
		if rec := s.Do(t, c.method, c.path, url.Values{"name": {"w1"}}); rec.Code != http.StatusForbidden {
			t.Errorf("owner %s %s = %d, want 403", c.method, c.path, rec.Code)
		}
	}
}
