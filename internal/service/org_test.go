package service

import (
	"context"
	"slices"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

// The org rung (DECIDE 166): settings and env colours save, and each save
// queues a redeploy of the org's running tiles only; bad colours are
// refused before anything moves.
func TestOrgRungRedeploysRunningTiles(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	up := w.tile(t, "api", true)
	down := w.tile(t, "worker", false)

	og, err := w.orch.SetOrgSettings(ctx, w.org, `{"cpu_limit":1}`)
	must(t, err)
	if og.Settings != `{"cpu_limit":1}` {
		t.Errorf("settings = %s", og.Settings)
	}
	deploys := func() int {
		js, err := w.orch.TileJobs(ctx, []string{up.ID, down.ID}, 10)
		must(t, err)
		for _, j := range js {
			if j.Kind != string(kindDeploy) || !slices.Contains(j.LockSet, up.ID) {
				t.Fatalf("job %+v touches more than the running tile", j)
			}
		}
		return len(js)
	}
	if n := deploys(); n != 1 {
		t.Fatalf("deploys after settings = %d, want 1", n)
	}

	if _, err := w.orch.SetOrgEnvColors(ctx, w.org, "red"); err == nil {
		t.Fatal("colours that are not JSON saved")
	} else if v, ok := errs.IsInvalid(err); !ok || v.Field != "env_colors" {
		t.Errorf("bad colours = %v, want Invalid on env_colors", err)
	}
	og, err = w.orch.SetOrgEnvColors(ctx, w.org, `{"prod":"rose"}`)
	must(t, err)
	if og.EnvColors != `{"prod":"rose"}` {
		t.Errorf("env colours = %s", og.EnvColors)
	}
	if n := deploys(); n < 1 {
		t.Errorf("deploys after colours = %d", n)
	}
}
