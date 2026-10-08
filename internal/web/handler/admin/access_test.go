package admin_test

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/web/webtest"
)

// parkTwo parks two tiles of the stack on a device each and returns the
// pending lines, api's first.
func parkTwo(t *testing.T, s *webtest.Site) []string {
	t.Helper()
	ctx := context.Background()
	other, err := s.Orch.CreateTile(ctx, service.Tile{
		StackID: s.Tile.Stack, EnvironmentID: s.Tile.Env, Name: "cache", Kind: "image", ImageRef: "redis:7", ContainerPort: 6379,
	})
	if err != nil {
		t.Fatal(err)
	}
	for id, dev := range map[string]string{s.Tile.ID: "/dev/a", other.ID: "/dev/b"} {
		if _, _, err := s.Orch.UpdateTile(ctx, id, func(t *service.Tile) error { t.Devices = dev; return nil }); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Orch.Deploy(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	waitPending(t, s, 2)
	g, _ := s.Orch.HostGrant(ctx, s.Tile.Stack)
	return g.Pending
}

func waitPending(t *testing.T, s *webtest.Site, n int) service.HostGrant {
	t.Helper()
	for range 500 {
		if g, _ := s.Orch.HostGrant(context.Background(), s.Tile.Stack); len(g.Pending) == n {
			return g
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pending never became %d", n)
	return service.HostGrant{}
}

// The Elevated access tab: admins only; it lists the waiting stack with the
// checkbox approve, then every grant per tile with filters and a revoke.
func TestAdminAccessTab(t *testing.T) {
	s := webtest.New(t)
	ctx := context.Background()
	if rec := s.Do(t, "GET", "/-/admin?tab=access", nil); rec.Code != http.StatusForbidden {
		t.Errorf("owner opens the tab = %d, want 403", rec.Code)
	}
	rootID := s.User(t, "root@x.test", true)
	root := s.Session(t, rootID)
	pending := parkTwo(t, s)
	const tab = "/-/admin?tab=access"
	body := s.As(t, root, "GET", tab, nil).Body.String()
	for _, w := range []string{"Elevated access", "Waiting", "acme / shop", `name="grant" value="` + pending[0] + `"`, `name="stack" value="` + s.Tile.Stack + `"`} {
		if !strings.Contains(body, w) {
			t.Errorf("no %q in\n%s", w, body)
		}
	}
	users := s.As(t, root, "GET", "/-/admin?tab=users", nil).Body.String()
	if u, a, r := strings.Index(users, ">Users<"), strings.Index(users, ">Elevated access<"), strings.Index(users, ">Routes<"); u >= a || a >= r {
		t.Errorf("the tab is not third (users %d, access %d, routes %d)", u, a, r)
	}

	// the rail badge: the admin sees the count, the owner does not
	if body := s.As(t, root, "GET", "/acme", nil).Body.String(); !strings.Contains(body, `id="rail-waiting"`) || !strings.Contains(body, "tab=access") {
		t.Errorf("no rail badge for the admin:\n%s", body)
	}
	if body := s.Do(t, "GET", "/acme", nil).Body.String(); strings.Contains(body, "rail-waiting") {
		t.Error("the owner sees the badge")
	}

	// approve one line from the tab; the other keeps waiting
	rec := s.As(t, root, "POST", "/-/admin/access/approve", url.Values{"stack": {s.Tile.Stack}, "pending": pending, "grant": {pending[0]}})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d", rec.Code)
	}
	if rec := s.As(t, root, "POST", "/-/admin/access/approve", url.Values{"stack": {s.Tile.Stack}, "pending": pending}); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("nothing ticked = %d", rec.Code)
	}
	g := waitPending(t, s, 1)
	if !slices.Equal(g.Lines, pending[:1]) || !slices.Equal(g.Pending, pending[1:]) {
		t.Fatalf("lines %v pending %v", g.Lines, g.Pending)
	}
	if _, err := s.Orch.ApproveHostGrant(ctx, s.Tile.Stack, rootID, g.Pending, nil); err != nil {
		t.Fatal(err)
	}

	// every grant, by tile, with who approved
	body = s.As(t, root, "GET", tab, nil).Body.String()
	for _, w := range []string{"acme / shop /", ">api<", ">cache<", "/dev/a", "/dev/b", "approved by", "/access/revoke?stack=" + s.Tile.Stack + "&amp;tile=cache"} {
		if !strings.Contains(body, w) {
			t.Errorf("no %q in\n%s", w, body)
		}
	}
	for q, want := range map[string]string{
		"&q=/dev/b":    "/dev/b",
		"&perm=device": "/dev/a",
		"&org=nope":    "Nothing is granted.",
		"&perm=docker": "Nothing is granted.",
		"&q=redis%20x": "Nothing is granted.",
	} {
		if b := s.As(t, root, "GET", tab+q, nil).Body.String(); !strings.Contains(b, want) {
			t.Errorf("%s: no %q", q, want)
		}
	}
	if b := s.As(t, root, "GET", tab+"&q=/dev/b", nil).Body.String(); strings.Contains(b, "/dev/a") {
		t.Error("the search keeps a tile it does not match")
	}

	// revoke one tile: the other keeps its line; the whole stack is not an option
	if rec := s.As(t, root, "POST", "/-/admin/access/revoke?stack="+s.Tile.Stack, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("revoke with no tile = %d", rec.Code)
	}
	if rec := s.Do(t, "POST", "/-/admin/access/revoke?stack="+s.Tile.Stack+"&tile=cache", nil); rec.Code != http.StatusForbidden {
		t.Errorf("owner revokes = %d", rec.Code)
	}
	if rec := s.As(t, root, "POST", "/-/admin/access/revoke?stack="+s.Tile.Stack+"&tile=cache", nil); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d", rec.Code)
	}
	g, _ = s.Orch.HostGrant(ctx, s.Tile.Stack)
	if len(g.Lines) != 1 || !strings.HasPrefix(g.Lines[0], "api ") {
		t.Errorf("lines after revoking cache: %v", g.Lines)
	}
}
