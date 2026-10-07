package service

import (
	"context"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

// A route host refuses a later tile domain on the same host (or under its
// wildcard), a tile domain's host refuses a route, and create and delete
// each push the proxy.
func TestExternalRouteSquat(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	api := w.tile(t, "api", false)
	last := func() string {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.pushed[len(w.pushed)-1]
	}

	r, err := w.orch.CreateExternalRoute(ctx, ExternalRouteSpec{Host: "pve.example.com", Mode: "passthrough", Target: "10.0.0.5"})
	must(t, err)
	if !strings.Contains(last(), `"wrapper":"layer4"`) || !strings.Contains(last(), "10.0.0.5:443") {
		t.Errorf("create did not push the route: %s", last())
	}
	_, err = w.orch.CreateExternalRoute(ctx, ExternalRouteSpec{Host: "*.fyrm.io", Mode: "passthrough", Target: "10.0.0.6"})
	must(t, err)

	for _, host := range []string{"pve.example.com", "a.fyrm.io"} {
		_, err = w.orch.AttachDomain(ctx, api.ID, DomainSpec{Host: host})
		if _, ok := errs.IsConflict(err); !ok {
			t.Errorf("attach %s = %v, want conflict", host, err)
		}
	}
	d, err := w.orch.AttachDomain(ctx, api.ID, DomainSpec{Host: "api.example.com"})
	must(t, err)
	_, err = w.orch.CreateExternalRoute(ctx, ExternalRouteSpec{Host: d.Host, Mode: "http", Target: "10.0.0.7"})
	if _, ok := errs.IsConflict(err); !ok {
		t.Errorf("route on a tile domain = %v, want conflict", err)
	}
	_, err = w.orch.CreateDomainResource(ctx, "org", w.org, "res.example.com", false, "")
	must(t, err)
	_, err = w.orch.CreateExternalRoute(ctx, ExternalRouteSpec{Host: "res.example.com", Mode: "http", Target: "10.0.0.7"})
	if _, ok := errs.IsConflict(err); !ok {
		t.Errorf("route on a domain resource = %v, want conflict", err)
	}

	_, err = w.orch.CreateDomainResource(ctx, "org", w.org, "pve.example.com", false, "")
	if _, ok := errs.IsConflict(err); !ok {
		t.Errorf("domain resource on a route host = %v, want conflict", err)
	}

	must(t, w.orch.DeleteExternalRoute(ctx, r.ID))
	if strings.Contains(last(), "10.0.0.5") {
		t.Errorf("delete did not push: %s", last())
	}
	if _, err = w.orch.AttachDomain(ctx, api.ID, DomainSpec{Host: "pve.example.com"}); err != nil {
		t.Errorf("attach after delete = %v", err)
	}
}

// An auto host is a name too: a tile named like an admin's route takes it
// under a resource, on attach and on rename.
func TestAutoHostOnRouteHost(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	_, err := w.orch.CreateDomainResource(ctx, "stack", w.stack, "example.com", false, "")
	must(t, err)
	_, err = w.orch.CreateExternalRoute(ctx, ExternalRouteSpec{Host: "pve.example.com", Mode: "passthrough", Target: "10.0.0.5"})
	must(t, err)

	pve := w.tile(t, "pve", false)
	_, err = w.orch.AttachDomain(ctx, pve.ID, DomainSpec{Auto: true})
	if _, ok := errs.IsConflict(err); !ok {
		t.Errorf("auto attach on a route host = %v, want conflict", err)
	}

	web := w.tile(t, "web", false)
	d, err := w.orch.AttachDomain(ctx, web.ID, DomainSpec{Auto: true})
	must(t, err)
	if d.Host != "web.example.com" {
		t.Fatalf("auto host = %s", d.Host)
	}
	_, err = w.orch.RenameTile(ctx, web.ID, "pve")
	if _, ok := errs.IsConflict(err); !ok {
		t.Errorf("rename onto a route host = %v, want conflict", err)
	}
	got, err := w.orch.domains.Get(ctx, d.ID)
	must(t, err)
	if got.Host != "web.example.com" {
		t.Errorf("host after the refused rename = %s, want web.example.com", got.Host)
	}
}

// A pass-through route on the panel host (exact or under a wildcard) wins
// before HTTP and locks the panel out: refused at create, and a later
// panel_domain a route covers is refused too.
func TestPassthroughOnPanelHost(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	must(t, w.orch.SetSetting(ctx, "panel_domain", "panel.example.com"))
	for _, host := range []string{"panel.example.com", "*.example.com"} {
		_, err := w.orch.CreateExternalRoute(ctx, ExternalRouteSpec{Host: host, Mode: "passthrough", Target: "10.0.0.5"})
		if _, ok := errs.IsConflict(err); !ok {
			t.Errorf("passthrough on %s = %v, want conflict", host, err)
		}
	}
	_, err := w.orch.CreateExternalRoute(ctx, ExternalRouteSpec{Host: "nas.other.io", Mode: "passthrough", Target: "10.0.0.5"})
	must(t, err)
	err = w.orch.SetSetting(ctx, "panel_domain", "nas.other.io")
	if _, ok := errs.IsConflict(err); !ok {
		t.Errorf("panel_domain under a passthrough route = %v, want conflict", err)
	}
	if v, _ := w.orch.settings.Get(ctx, "panel_domain"); v != "panel.example.com" {
		t.Errorf("panel_domain = %q after the refusal", v)
	}
}

// A resource and a wildcard route one label above it refuse each other, and
// a trailing dot does not slip past the check.
func TestResourceVersusWildcardRoute(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	_, err := w.orch.CreateExternalRoute(ctx, ExternalRouteSpec{Host: "*.res.example.com", Mode: "passthrough", Target: "10.0.0.5"})
	must(t, err)
	_, err = w.orch.CreateDomainResource(ctx, "org", w.org, "res.example.com", false, "")
	if _, ok := errs.IsConflict(err); !ok {
		t.Errorf("resource under a wildcard route = %v, want conflict", err)
	}
	_, err = w.orch.CreateExternalRoute(ctx, ExternalRouteSpec{Host: "pve.example.com", Mode: "passthrough", Target: "10.0.0.5"})
	must(t, err)
	_, err = w.orch.CreateDomainResource(ctx, "org", w.org, "pve.example.com.", false, "")
	if _, ok := errs.IsConflict(err); !ok {
		t.Errorf("resource with a trailing dot on a route host = %v, want conflict", err)
	}
	_, err = w.orch.CreateExternalRoute(ctx, ExternalRouteSpec{Host: "x.io", Mode: "http", Target: "h{env.X}"})
	if _, ok := errs.IsInvalid(err); !ok {
		t.Errorf("route target with a placeholder = %v, want invalid", err)
	}
}
