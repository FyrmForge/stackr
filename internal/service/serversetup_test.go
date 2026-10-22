package service_test

import (
	"context"
	"slices"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/servicetest"
)

// A fresh install keeps metrics history on, wizard or not.
func TestObserveOnByDefault(t *testing.T) {
	orch := servicetest.New(t).Orch
	if v, err := orch.Setting(context.Background(), "observe_enabled"); err != nil || v != "true" {
		t.Errorf("observe_enabled = %q, %v; want true", v, err)
	}
}

// A fresh server is pending; a branch is picked, then finishing stamps it.
func TestServerSetup(t *testing.T) {
	orch, ctx := servicetest.New(t).Orch, context.Background()
	s, err := orch.ServerSetupState(ctx)
	if err != nil || s.Done || s.Mode != "" {
		t.Fatalf("fresh = %+v, %v; want pending, no mode", s, err)
	}
	if _, err := orch.SetServerSetupMode(ctx, nil, "restore"); err == nil {
		t.Error("a restore branch was accepted")
	}
	for mode, first := range map[string]string{"ui": "domains", "config": "file"} {
		s, err = orch.SetServerSetupMode(ctx, nil, mode)
		if err != nil || s.Mode != mode || !slices.Contains(s.Steps, first) {
			t.Errorf("mode %s = %+v, %v", mode, s, err)
		}
	}
	if err := orch.FinishServerSetup(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if s, err = orch.ServerSetupState(ctx); err != nil || !s.Done {
		t.Errorf("after finish = %+v, %v; want done", s, err)
	}
}

// The summary names a domain once when the panel sits on the root domain.
func TestServerSetupSummaryDomains(t *testing.T) {
	orch, ctx := servicetest.New(t).Orch, context.Background()
	for _, c := range []struct{ panel, root, want string }{
		{"x.test", "x.test", "x.test"},
		{"stkr.x.test", "x.test", "stkr.x.test, x.test"},
		{"", "x.test", "x.test"},
	} {
		if err := orch.SetSettings(ctx, map[string]string{"panel_domain": c.panel, "root_domain": c.root}); err != nil {
			t.Fatal(err)
		}
		ls, err := orch.ServerSetupSummary(ctx)
		if err != nil || ls[0].Detail != c.want {
			t.Errorf("panel %q root %q: %+v, %v; want %q", c.panel, c.root, ls[0], err, c.want)
		}
	}
}
