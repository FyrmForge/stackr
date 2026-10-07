package canvas_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service"
)

// A parked host access shows on the stack's settings; an owner sees it and
// cannot approve, a server admin can, and the section then lists the grant.
func TestStackHostAccess(t *testing.T) {
	b := newBrowser(t, "owner")
	ctx := context.Background()
	tl := b.env.Tile(t, b.org)
	_, _, err := b.env.Orch.UpdateTile(ctx, tl.ID, func(t *service.Tile) error {
		t.Volumes = "host:/var/run/docker.sock:/s"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.env.Orch.Deploy(ctx, tl.ID); err != nil {
		t.Fatal(err)
	}
	const tab = "/acme/shop/-/drawer?tab=settings"
	var body string
	for range 500 {
		if body = b.do(t, "GET", tab, nil, true).Body.String(); strings.Contains(body, "waiting: host:/var/run/docker.sock:/s") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(body, "waiting: host:/var/run/docker.sock:/s") || strings.Contains(body, "/host-grant/approve") {
		t.Fatalf("owner view:\n%s", body)
	}
	if rec := b.do(t, "POST", "/acme/shop/-/drawer/host-grant/approve", nil, true); rec.Code != http.StatusForbidden {
		t.Errorf("owner approve = %d, want 403", rec.Code)
	}

	admin := b
	admin.session = b.env.Session(t, b.env.User(t, "root@x", true))
	if body = admin.do(t, "GET", tab, nil, true).Body.String(); !strings.Contains(body, "/host-grant/approve") {
		t.Fatalf("admin view has no Approve:\n%s", body)
	}
	if !strings.Contains(body, `&#34;pending&#34;:&#34;host:/var/run/docker.sock:/s&#34;`) {
		t.Fatalf("Approve does not carry the set shown:\n%s", body)
	}
	if rec := admin.do(t, "POST", "/acme/shop/-/drawer/host-grant/approve", url.Values{"pending": {"host:/:/h"}}, true); rec.Code == http.StatusOK {
		t.Fatalf("approve of a set that is not the ask = %d", rec.Code)
	}
	if g, _ := b.env.Orch.HostGrant(ctx, tl.Stack); g.Granted {
		t.Fatalf("a refused approval granted: %+v", g)
	}
	if rec := admin.do(t, "POST", "/acme/shop/-/drawer/host-grant/approve", url.Values{"pending": {"host:/var/run/docker.sock:/s"}}, true); rec.Code != http.StatusOK {
		t.Fatalf("admin approve = %d %s", rec.Code, rec.Body)
	}
	body = admin.do(t, "GET", tab, nil, true).Body.String()
	if !strings.Contains(body, "host:/var/run/docker.sock:/s") || strings.Contains(body, "waiting:") {
		t.Errorf("after approval:\n%s", body)
	}
}
