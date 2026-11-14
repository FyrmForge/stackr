package service

import (
	"context"
	"errors"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
)

// LastPGExtPlan reads the plan stored under pgextplan.<instance id>: nil when
// there is none, an error when it is unreadable.
func TestLastPGExtPlan(t *testing.T) {
	t.Parallel()
	r := newPGExtRig(t)
	ctx := context.Background()
	p, err := r.orch.LastPGExtPlan(ctx, "mi-none")
	if p != nil || err != nil {
		t.Fatalf("no plan: %v, %v", p, err)
	}
	must(t, r.orch.store.Settings.Set(ctx, pgextplanKey+"mi1", `{"ID":"p1","Hash":"h","Lines":["a"],"Restart":true}`))
	if p, err = r.orch.LastPGExtPlan(ctx, "mi1"); err != nil || p == nil || p.ID != "p1" || !p.Restart || len(p.Lines) != 1 {
		t.Fatalf("stored plan: %+v, %v", p, err)
	}
	must(t, r.orch.store.Settings.Set(ctx, pgextplanKey+"mi2", `{`))
	if _, err = r.orch.LastPGExtPlan(ctx, "mi2"); err == nil {
		t.Error("an unreadable plan read as nothing")
	}
}

// The web holds the tile id; the plan is stored under its instance.
func TestLastPGExtPlanOf(t *testing.T) {
	t.Parallel()
	r := newPGExtRig(t)
	ctx := context.Background()
	_, m, err := r.orch.instanceOf(ctx, r.pg.ID)
	must(t, err)
	must(t, r.orch.store.Settings.Set(ctx, pgextplanKey+m.ID, `{"ID":"p9"}`))
	if p, err := r.orch.LastPGExtPlanOf(ctx, r.pg.ID); err != nil || p == nil || p.ID != "p9" {
		t.Fatalf("by tile: %+v, %v", p, err)
	}
}

// The stub rebuild leaves a missing image to fail the way it always did.
func TestRebuildOperatedStub(t *testing.T) {
	t.Parallel()
	r := newPGExtRig(t)
	if err := r.orch.rebuildOperated(context.Background(), r.pg, nil); !errors.Is(err, docker.ErrNoClusterImage) {
		t.Errorf("rebuildOperated = %v", err)
	}
}
