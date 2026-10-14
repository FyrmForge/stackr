package cluster_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
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

// A fresh load of ?drawer=internet:... or tunnel:a:b asks for that drawer.
func TestClusterDrawerLoadInternetAndTunnel(t *testing.T) {
	s := webtest.New(t)
	sess := admin(t, s)
	for drawer, want := range map[string]string{
		"internet:node:abc":        `hx-get="/-/cluster/internet?from=node%3Aabc"`,
		"internet:nat:10.0.0.0/24": `hx-get="/-/cluster/internet?from=nat%3A10.0.0.0%2F24"`,
		"tunnel:a1:b2":             `hx-get="/-/cluster/tunnel?a=a1&amp;b=b2"`,
	} {
		req := httptest.NewRequest("GET", "/-/cluster?drawer="+url.QueryEscape(drawer), nil)
		req.AddCookie(&http.Cookie{Name: s.Orch.Sessions().CookieName(), Value: sess})
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if body := rec.Body.String(); !strings.Contains(body, want) {
			t.Errorf("?drawer=%s: no %s in\n%s", drawer, want, body)
		}
	}
}

// The internet drawer of the controller's card lists the envs that leave
// there (and the hosts that come in, when the card is the NAT's).
func TestClusterInternetDrawer(t *testing.T) {
	s := webtest.New(t)
	root := admin(t, s)
	s.Domain(t, s.Tile.ID, "shop.example.com")
	m, err := s.Orch.ClusterMap(context.Background())
	if err != nil || len(m.Nodes) != 1 {
		t.Fatalf("map = %+v, %v", m, err)
	}
	from := "node:" + m.Nodes[0].ID
	if n := m.Networks[0]; n.Router {
		from = "nat:" + n.ID // behind a router: the NAT card carries it
	}
	rec := s.As(t, root, "GET", "/-/cluster/internet?from="+url.QueryEscape(from), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET internet = %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{"Internet", "Ingress", "Egress", `href="/acme/shop/dev"`} {
		if !strings.Contains(body, want) {
			t.Errorf("internet drawer lacks %q", want)
		}
	}
	if strings.HasPrefix(from, "nat:") && !strings.Contains(body, "shop.example.com") {
		t.Error("the NAT's drawer lacks the ingress host")
	}
	for _, bad := range []string{"/-/cluster/internet?from=node:nope", "/-/cluster/tunnel?a=x&b=y"} {
		if rec := s.As(t, root, "GET", bad, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", bad, rec.Code)
		}
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
	// A network box keeps where it was dropped; a plain drag (no w, h) saves no size.
	if rec := s.As(t, root, "POST", "/-/cluster/-/positions", url.Values{"node_id": {"net:lan"}, "x": {"1"}, "y": {"2"}}); rec.Code != http.StatusNoContent {
		t.Errorf("dragging a box = %d, want 204", rec.Code)
	}
	if p := mustPositions(t, s)["net:lan"]; p.X != 1 || p.Y != 2 {
		t.Errorf("saved box corner = %+v, want 1,2", p)
	}
	if _, ok := mustPositions(t, s)["size:net:lan"]; ok {
		t.Error("a plain box drag saved a size")
	}
	rec = s.As(t, root, "POST", "/-/cluster/-/positions", url.Values{"node_id": {"net:lan"}, "x": {"1"}, "y": {"1"}, "w": {"700"}, "h": {"300"}})
	if rec.Code != http.StatusNoContent {
		t.Errorf("sizing a box = %d, want 204", rec.Code)
	}
	if p := mustPositions(t, s)["size:net:lan"]; p.X != 700 || p.Y != 300 {
		t.Errorf("saved size = %+v, want 700x300", p)
	}
	if rec := s.As(t, root, "POST", "/-/cluster/-/reset", nil); rec.Code != http.StatusOK {
		t.Errorf("reset = %d", rec.Code)
	}
	if _, ok := mustPositions(t, s)["size:net:lan"]; ok {
		t.Error("re-arrange kept the box size")
	}
}

func mustPositions(t *testing.T, s *webtest.Site) map[string]service.Point {
	t.Helper()
	p, err := s.Orch.ClusterPositions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return p
}
