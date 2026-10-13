package cluster_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/web/webtest"
)

func admin(t *testing.T, s *webtest.Site) string {
	t.Helper()
	return s.Session(t, s.User(t, "root@stackr.test", true))
}

// The admin sees the map with the controller on it and the seeded env under
// its card; the rail links to it; an org owner is refused.
func TestClusterPage(t *testing.T) {
	s := webtest.New(t)
	root := admin(t, s)

	rec := s.As(t, root, "GET", "/-/cluster", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /-/cluster = %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{"<graph-canvas", "1 node · 1 network", "acme/shop/dev", `hx-push-url="?drawer=node:`} {
		if !strings.Contains(body, want) {
			t.Errorf("cluster page lacks %q", want)
		}
	}
	if home := s.As(t, root, "GET", "/", nil).Body.String(); !strings.Contains(home, `href="/-/cluster"`) {
		t.Error("the rail has no Cluster entry for an admin")
	}
	if rec := s.Do(t, "GET", "/-/cluster", nil); rec.Code != http.StatusForbidden {
		t.Errorf("owner GET /-/cluster = %d, want 403", rec.Code)
	}
	if home := s.Do(t, "GET", "/", nil).Body.String(); strings.Contains(home, `href="/-/cluster"`) {
		t.Error("the rail shows Cluster to someone who may not open it")
	}
}

// A fresh load of ?drawer=node:<id> asks for that node's drawer.
func TestClusterDrawerLoad(t *testing.T) {
	s := webtest.New(t)
	req := httptest.NewRequest("GET", "/-/cluster?drawer=node:abc&tab=envs", nil) // a browser load, not htmx
	req.AddCookie(&http.Cookie{Name: s.Orch.Sessions().CookieName(), Value: admin(t, s)})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if body := rec.Body.String(); !strings.Contains(body, `hx-get="/-/admin/nodes/abc?tab=envs"`) {
		t.Errorf("no drawer load in\n%s", body)
	}
}

// Notes work as on every canvas; a node card's drop is kept, an env's is
// refused.
func TestClusterNotes(t *testing.T) {
	s := webtest.New(t)
	root := admin(t, s)
	rec := s.As(t, root, "POST", "/-/cluster/-/notes", url.Values{
		"kind": {"note"}, "text": {"backups run at night"}, "w": {"160"}, "h": {"80"},
	})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "backups run at night") {
		t.Fatalf("note = %d %s", rec.Code, rec.Body)
	}
	if body := s.As(t, root, "GET", "/-/cluster", nil).Body.String(); !strings.Contains(body, "backups run at night") {
		t.Error("the note did not stick")
	}
	rec = s.As(t, root, "POST", "/-/cluster/-/positions", url.Values{"node_id": {"node:x"}, "x": {"1"}, "y": {"1"}})
	if rec.Code != http.StatusNoContent {
		t.Errorf("moving a node card = %d, want 204", rec.Code)
	}
	rec = s.As(t, root, "POST", "/-/cluster/-/positions", url.Values{"node_id": {"env:x:0"}, "x": {"1"}, "y": {"1"}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("moving an env = %d, want 422", rec.Code)
	}
	if rec := s.As(t, root, "POST", "/-/cluster/-/reset", nil); rec.Code != http.StatusOK {
		t.Errorf("reset = %d", rec.Code)
	}
}
