package canvas_test

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service"
)

// park asks for elevated access on the stack's tile and waits for the
// request to show; it returns the stack id and the pending lines.
func park(t *testing.T, b browser, volumes, devices string, hostNet bool) (string, []string) {
	t.Helper()
	ctx := context.Background()
	tl := b.env.Tile(t, b.org)
	_, _, err := b.env.Orch.UpdateTile(ctx, tl.ID, func(t *service.Tile) error {
		t.Volumes, t.Devices, t.HostNetwork = volumes, devices, hostNet
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.env.Orch.Deploy(ctx, tl.ID); err != nil {
		t.Fatal(err)
	}
	for range 500 {
		if g, _ := b.env.Orch.HostGrant(ctx, tl.Stack); len(g.Pending) > 0 {
			return tl.Stack, g.Pending
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the deploy never parked")
	return "", nil
}

func asAdmin(b browser, t *testing.T) browser {
	b.session = b.env.Session(t, b.env.User(t, "root@x", true))
	return b
}

const approveURL = "/acme/shop/-/drawer/host-grant/approve"

// A parked host access shows on the stack's settings; an owner sees it and
// cannot approve, a server admin can, and the section then lists the grant.
func TestStackHostAccess(t *testing.T) {
	b := newBrowser(t, "owner")
	ctx := context.Background()
	stack, pending := park(t, b, "host:/var/run/docker.sock:/s", "", false)
	line := pending[0]
	const tab = "/acme/shop/-/drawer?tab=settings"
	body := b.do(t, "GET", tab, nil, true).Body.String()
	for _, w := range []string{"Docker socket", "waiting", "elevated access"} {
		if !strings.Contains(body, w) {
			t.Errorf("owner view has no %q:\n%s", w, body)
		}
	}
	if strings.Contains(body, "/host-grant/approve") {
		t.Errorf("owner is offered Approve:\n%s", body)
	}
	if rec := b.do(t, "POST", approveURL, nil, true); rec.Code != http.StatusForbidden {
		t.Errorf("owner approve = %d, want 403", rec.Code)
	}

	admin := asAdmin(b, t)
	body = admin.do(t, "GET", tab, nil, true).Body.String()
	for _, w := range []string{"/host-grant/approve", `name="grant" value="` + line + `"`, `name="pending" value="` + line + `"`} {
		if !strings.Contains(body, w) {
			t.Fatalf("admin view has no %s:\n%s", w, body)
		}
	}
	if rec := admin.do(t, "POST", approveURL, url.Values{"pending": {"x host:/:/h"}, "grant": {"x host:/:/h"}, "confirm": {"shop"}}, true); rec.Code == http.StatusOK {
		t.Fatalf("approve of a set that is not the ask = %d", rec.Code)
	}
	if g, _ := b.env.Orch.HostGrant(ctx, stack); g.Granted {
		t.Fatalf("a refused approval granted: %+v", g)
	}
	// the docker socket is full trust: the stack's name must be typed
	form := url.Values{"pending": {line}, "grant": {line}}
	if rec := admin.do(t, "POST", approveURL, form, true); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("approve without the name = %d", rec.Code)
	}
	form.Set("confirm", "shop")
	if rec := admin.do(t, "POST", approveURL, form, true); rec.Code != http.StatusOK {
		t.Fatalf("admin approve = %d %s", rec.Code, rec.Body)
	}
	body = admin.do(t, "GET", tab, nil, true).Body.String()
	if !strings.Contains(body, "Docker socket") || strings.Contains(body, "waiting") {
		t.Errorf("after approval:\n%s", body)
	}
}

// Unticking narrows the approval: the ticked lines are granted, the rest
// keeps waiting; ticking nothing is refused, not read as "all".
func TestStackApproveNarrowed(t *testing.T) {
	b := newBrowser(t, "owner")
	ctx := context.Background()
	stack, pending := park(t, b, "host:/srv:/data", "/dev/ttyUSB0", false)
	if len(pending) != 2 {
		t.Fatalf("pending = %v", pending)
	}
	admin := asAdmin(b, t)
	if rec := admin.do(t, "POST", approveURL, url.Values{"pending": pending}, true); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("nothing ticked = %d", rec.Code)
	}
	if g, _ := b.env.Orch.HostGrant(ctx, stack); g.Granted {
		t.Fatalf("an empty tick granted: %+v", g)
	}
	dev := slices.IndexFunc(pending, func(l string) bool { return strings.Contains(l, "device:") })
	if rec := admin.do(t, "POST", approveURL, url.Values{"pending": pending, "grant": {pending[dev]}}, true); rec.Code != http.StatusOK {
		t.Fatalf("narrowed approve = %d %s", rec.Code, rec.Body)
	}
	// the requeued job runs again and parks on what is left
	var g service.HostGrant
	for range 500 {
		if g, _ = b.env.Orch.HostGrant(ctx, stack); len(g.Pending) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !slices.Equal(g.Lines, []string{pending[dev]}) || !slices.Equal(g.Pending, []string{pending[1-dev]}) {
		t.Errorf("lines %v pending %v", g.Lines, g.Pending)
	}
	if body := admin.do(t, "GET", "/acme/shop/-/drawer?tab=settings", nil, true).Body.String(); !strings.Contains(body, `name="grant" value="`+pending[1-dev]+`"`) {
		t.Errorf("the rest is not offered again:\n%s", body)
	}
}

// Host networking is full trust: it is approved only with the stack's
// name typed, and unticking it needs none.
func TestStackApproveHostNetworkNeedsName(t *testing.T) {
	b := newBrowser(t, "owner")
	ctx := context.Background()
	stack, pending := park(t, b, "", "/dev/ttyUSB0", true)
	admin := asAdmin(b, t)
	if body := admin.do(t, "GET", "/acme/shop/-/drawer?tab=settings", nil, true).Body.String(); !strings.Contains(body, `name="confirm"`) {
		t.Errorf("no name field while host networking waits:\n%s", body)
	}
	net := slices.IndexFunc(pending, func(l string) bool { return strings.Contains(l, "network:host") })
	form := url.Values{"pending": pending, "grant": {pending[net]}}
	for _, typed := range []string{"", "other"} {
		form.Set("confirm", typed)
		if rec := admin.do(t, "POST", approveURL, form, true); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("host networking, name %q = %d", typed, rec.Code)
		}
	}
	if g, _ := b.env.Orch.HostGrant(ctx, stack); g.Granted {
		t.Fatalf("granted without the name: %+v", g)
	}
	form.Set("confirm", "shop")
	if rec := admin.do(t, "POST", approveURL, form, true); rec.Code != http.StatusOK {
		t.Fatalf("with the name = %d %s", rec.Code, rec.Body)
	}
	if g, _ := b.env.Orch.HostGrant(ctx, stack); !slices.Contains(g.Lines, pending[net]) {
		t.Errorf("not granted: %+v", g)
	}
}

// A tile's card names the strongest permission it holds, and says so while
// a request waits.
func TestCardAccessChip(t *testing.T) {
	b := newBrowser(t, "owner")
	stack, pending := park(t, b, "host:/srv:/data", "/dev/ttyUSB0", false)
	const canvas = "/acme/shop/dev"
	body := b.do(t, "GET", canvas, nil, true).Body.String()
	if !strings.Contains(body, "Waiting for an admin") || strings.Contains(body, ">device<") {
		t.Errorf("waiting card:\n%s", body)
	}
	admin := asAdmin(b, t)
	if rec := admin.do(t, "POST", approveURL, url.Values{"pending": pending, "grant": pending}, true); rec.Code != http.StatusOK {
		t.Fatalf("approve = %d", rec.Code)
	}
	g, _ := b.env.Orch.HostGrant(context.Background(), stack)
	if len(g.Lines) != 2 {
		t.Fatalf("lines %v", g.Lines)
	}
	body = b.do(t, "GET", canvas, nil, true).Body.String()
	if !strings.Contains(body, ">device<") || strings.Contains(body, ">folder<") || strings.Contains(body, "Waiting for an admin") {
		t.Errorf("granted card:\n%s", body)
	}
}
