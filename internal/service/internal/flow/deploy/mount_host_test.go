package deploy_test

import (
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/deploy"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/hostgrant"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A tile with host access parks before any container starts; once the
// stack is granted the host line is a plain bind and privileged is set; a
// revoke parks the next deploy while the running tile stays.
func TestHostAccessGate(t *testing.T) {
	w := setup(t)
	w.f.HostGrants = hostgrant.New(w.st.HostGrants)
	tl := w.tile
	tl.Volumes = "host:/var/run/docker.sock:/var/run/docker.sock:ro"
	tl.Devices = "/dev/dri"
	tl.Privileged = true

	_, err := w.f.Run(ctx, tl, "nginx:1", io.Discard, nil)
	n, ok := errs.IsNeedsApproval(err)
	if !ok || n.Stack != tl.StackID {
		t.Fatalf("run without a grant = %v", err)
	}
	if ask, _ := hostgrant.Parse(n.What); len(ask.Lines) != 3 || !ask.Has(hostgrant.Line(tl.Slug, hostgrant.Privileged)) {
		t.Errorf("asked for %+v (%q)", ask, n.What)
	}
	if len(w.fake.Specs) != 0 {
		t.Fatalf("a container started before approval: %+v", w.fake.Specs)
	}

	now := time.Now()
	must(t, w.st.Users.Create(ctx, store.User{ID: "adm", Email: "a@b.c", Name: "A", Role: "admin", Active: true, CreatedAt: now, UpdatedAt: now}))
	ask, _ := hostgrant.Parse(n.What)
	_, err = w.f.HostGrants.Approve(ctx, tl.StackID, "adm", ask)
	must(t, err)
	if _, err = w.f.Run(ctx, tl, "nginx:1", io.Discard, nil); err != nil {
		t.Fatalf("run with the grant: %v", err)
	}
	s := w.fake.Specs[len(w.fake.Specs)-1]
	if !slices.Contains(s.Volumes, "/var/run/docker.sock:/var/run/docker.sock:ro") || !s.Privileged {
		t.Errorf("spec = binds %v privileged %v", s.Volumes, s.Privileged)
	}

	// A privileged flag the grant does not say is never set.
	g, err := w.f.HostGrants.Row(ctx, tl.StackID)
	must(t, err)
	g.Lines = strings.ReplaceAll(g.Lines, tl.Slug+" privileged", "")
	must(t, w.st.HostGrants.Update(ctx, g))
	if _, err = w.f.Run(ctx, tl, "nginx:1", io.Discard, nil); err == nil {
		t.Fatal("privileged ran on a grant without it")
	}

	must(t, w.f.HostGrants.Revoke(ctx, tl.StackID, ""))
	if _, err = w.f.Run(ctx, tl, "nginx:1", io.Discard, nil); err == nil {
		t.Fatal("a revoked grant still deploys")
	} else if _, ok := errs.IsNeedsApproval(err); !ok {
		t.Fatalf("after revoke = %v", err)
	}
}

// Every permission is a per tile line; another tile's identical mount is not
// covered by the first tile's approved line.
func TestHostSetLines(t *testing.T) {
	a := store.Tile{Slug: "a", Volumes: "host:/srv:/srv:ro\nvol:/data", Devices: "/dev/dri", Privileged: true,
		Lan: "192.168.1.10:8123\n10.1.2.3/8\nall\nbogus", PublishedPorts: "8080:80\n5353:5353/udp\nbad", HostNetwork: true}
	want := []string{
		"a device:/dev/dri", "a host:/srv:/srv:ro", "a lan:10.0.0.0/8", "a lan:192.168.1.10:8123",
		"a network:host", "a port:5353/udp", "a port:8080", "a privileged",
	}
	if got := deploy.HostSet(a).Lines; !slices.Equal(got, want) {
		t.Fatalf("lines = %v", got)
	}
	b := store.Tile{Slug: "b", Volumes: "host:/srv:/srv:ro"}
	if m := deploy.HostSet(a, b).Missing(deploy.HostSet(a)); !slices.Equal(m, []string{"b host:/srv:/srv:ro"}) {
		t.Fatalf("tile b covered by tile a: %v", m)
	}
}
