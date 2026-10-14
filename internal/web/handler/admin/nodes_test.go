package admin_test

import (
	"net/http"
	"net/url"
	"runtime"
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
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Get join command") ||
		!strings.Contains(rec.Body.String(), "Every node starts with egress.") {
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
	rec = s.As(t, root, "POST", "/-/admin/nodes/"+id+"/tags", url.Values{"tag": {"builder", "egress"}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Set another default ingress first") {
		t.Errorf("untag ingress = %d\n%s", rec.Code, rec.Body)
	}

	// without egress a node needs a default exit; with one the tab shows it picked
	h4 := s.Worker(t, "h4", "fp")
	if rec := s.As(t, root, "POST", "/-/admin/nodes/"+h4+"/tags", url.Values{"tag": {"egress"}}); rec.Code != http.StatusOK {
		t.Fatalf("tag h4 = %d\n%s", rec.Code, rec.Body)
	}
	noEgress := url.Values{"tag": {"ingress"}}
	rec = s.As(t, root, "POST", "/-/admin/nodes/"+id+"/tags", noEgress)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Pick a default exit") {
		t.Errorf("no exit = %d\n%s", rec.Code, rec.Body)
	}
	noEgress.Set("exit", "h4")
	rec = s.As(t, root, "POST", "/-/admin/nodes/"+id+"/tags", noEgress)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<option value="h4" selected`) {
		t.Errorf("with exit = %d\n%s", rec.Code, rec.Body)
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

// The node drawer's overview is four cards: "1 core" and the arch. Tunnel rows
// need a live mesh, so the ui package's test pins those.
func TestAdminNodeOverview(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	s.Docker.Host.CPUs = 1
	s.Worker(t, "h4", "fp")
	ns, err := s.Orch.Nodes(t.Context())
	if err != nil || len(ns) != 2 {
		t.Fatalf("nodes = %+v, %v", ns, err)
	}
	rec := s.As(t, root, "GET", "/-/admin/nodes/"+ns[0].ID, nil)
	body := rec.Body.String()
	for _, want := range []string{"1 core", runtime.GOARCH, "just now","Addresses", "Machine", "Tunnels"} {
		if rec.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("overview = %d, no %q in\n%s", rec.Code, want, body)
		}
	}
	if strings.Contains(body, "1 cores") {
		t.Errorf("1 cores\n%s", body)
	}
}

// The default ingress select posts to a DNS preview that lists the panel's
// domain first, then asks for the node's name; a refusal lands in the banner.
func TestAdminIngressPreview(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	h4 := s.Worker(t, "h4", "fp")
	if err := s.Orch.SetSetting(t.Context(), "panel_domain", "panel.example.test"); err != nil {
		t.Fatal(err)
	}
	// a node not tagged ingress cannot be previewed
	if rec := s.As(t, root, "POST", "/-/admin/nodes/default-ingress/preview", url.Values{"node": {h4}}); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("untagged preview = %d\n%s", rec.Code, rec.Body)
	}
	if rec := s.As(t, root, "POST", "/-/admin/nodes/"+h4+"/tags", url.Values{"tag": {"ingress", "egress"}}); rec.Code != http.StatusOK {
		t.Fatalf("tag h4 = %d\n%s", rec.Code, rec.Body)
	}
	if body := s.As(t, root, "GET", "/-/admin?tab=nodes", nil).Body.String(); !strings.Contains(body, `hx-post="/-/admin/nodes/default-ingress/preview"`) {
		t.Errorf("the select does not post to the preview\n%s", body)
	}
	rec := s.As(t, root, "POST", "/-/admin/nodes/default-ingress/preview", url.Values{"node": {h4}})
	body := rec.Body.String()
	for _, want := range []string{"panel.example.test", "The panel moves to h4.", "Type", "h4"} {
		if rec.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("preview = %d, no %q in\n%s", rec.Code, want, body)
		}
	}
	// the typed name is checked on the server, and a refusal is the banner
	rec = s.As(t, root, "POST", "/-/admin/nodes/default-ingress", url.Values{"node": {h4}, "confirm": {"nope"}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "type h4 to confirm") {
		t.Errorf("wrong name = %d\n%s", rec.Code, rec.Body)
	}
}

// Break glass offers a pre-ticked "Back up every node first". Ticked, it only
// queues the backups; unticked it breaks glass as before.
func TestAdminBreakGlassBackupFirst(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	body := s.As(t, root, "GET", "/-/admin?tab=nodes", nil).Body.String()
	for _, want := range []string{"Back up every node first", `name="backup"`, "checked"} {
		if !strings.Contains(body, want) {
			t.Fatalf("no %q in\n%s", want, body)
		}
	}
	rec := s.As(t, root, "POST", "/-/admin/nodes/trust/break-glass", url.Values{"confirm": {"break glass"}, "backup": {"on"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Backups queued; break glass once they finish.") ||
		strings.Contains(rec.Body.String(), "Break glass done.") {
		t.Fatalf("ticked = %d\n%s", rec.Code, rec.Body)
	}
	js, err := s.Store.Jobs.ListByState(t.Context(), "queued", "running", "waiting")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, j := range js {
		found = found || j.Kind == "node-backup"
	}
	if !found {
		t.Errorf("no node-backup job queued: %+v", js)
	}
	rec = s.As(t, root, "POST", "/-/admin/nodes/trust/break-glass", url.Values{"confirm": {"break glass"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Break glass done.") {
		t.Errorf("unticked = %d\n%s", rec.Code, rec.Body)
	}
}

// A worker's drawer offers Back up node and the remove steps; both answers
// keep the drawer open and show the job they queued.
func TestAdminNodeBackupAndRemove(t *testing.T) {
	s := webtest.New(t)
	root := s.Session(t, s.User(t, "root@x.test", true))
	h4 := s.Worker(t, "h4", "fp")
	rec := s.As(t, root, "GET", "/-/admin/nodes/"+h4+"?tab=security", nil)
	for _, want := range []string{"Stops everything on h4, backs it up, checks the backups, then cleans the box and removes it. A failed backup stops here and starts the apps again.", "the backups, and the volumes and images on the box"} {
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("security = %d, no %q in\n%s", rec.Code, want, rec.Body)
		}
	}
	rec = s.As(t, root, "GET", "/-/admin/nodes/"+h4, nil)
	if !strings.Contains(rec.Body.String(), "Back up node") || !strings.Contains(rec.Body.String(), "No node backups yet.") {
		t.Fatalf("overview = %d\n%s", rec.Code, rec.Body)
	}
	for _, c := range []struct{ path, note string }{
		{"/backup", "Node backup queued."}, {"/remove", "Node removal queued."},
	} {
		rec = s.As(t, root, "POST", "/-/admin/nodes/"+h4+c.path, nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), c.note) || !strings.Contains(rec.Body.String(), "/-/jobs/") {
			t.Errorf("%s = %d, want %q and a job in\n%s", c.path, rec.Code, c.note, rec.Body)
		}
	}
}
