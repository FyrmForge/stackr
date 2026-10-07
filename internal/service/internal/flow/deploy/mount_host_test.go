package deploy_test

import (
	"io"
	"slices"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
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
	if ask, _ := hostgrant.Parse(n.What); !ask.Privileged || len(ask.Lines) != 2 {
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
	g.Privileged = false
	must(t, w.st.HostGrants.Update(ctx, g))
	if _, err = w.f.Run(ctx, tl, "nginx:1", io.Discard, nil); err == nil {
		t.Fatal("privileged ran on a grant without it")
	}

	must(t, w.f.HostGrants.Revoke(ctx, tl.StackID))
	if _, err = w.f.Run(ctx, tl, "nginx:1", io.Discard, nil); err == nil {
		t.Fatal("a revoked grant still deploys")
	} else if _, ok := errs.IsNeedsApproval(err); !ok {
		t.Fatalf("after revoke = %v", err)
	}
}
